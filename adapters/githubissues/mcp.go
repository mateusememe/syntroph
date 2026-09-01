package githubissues

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

const MCPProviderID = "github-mcp"

var requiredMCPTools = map[string]toolRequirement{
	"get_label":         {Properties: []string{"owner", "repo", "name"}},
	"label_write":       {Properties: []string{"owner", "repo", "method", "name", "color", "description"}, Methods: []string{"create", "update"}},
	"issue_read":        {Properties: []string{"owner", "repo", "method", "issue_number"}, Methods: []string{"get", "get_comments"}},
	"issue_write":       {Properties: []string{"owner", "repo", "method", "title", "body", "labels", "issue_number", "state", "state_reason"}, Methods: []string{"create", "update"}},
	"add_issue_comment": {Properties: []string{"owner", "repo", "issue_number", "body"}},
	"list_issues":       {Properties: []string{"owner", "repo", "state", "labels", "cursor"}},
}

type toolRequirement struct {
	Properties []string
	Methods    []string
}

type MCPProvider struct {
	repository string
	owner      string
	repo       string
	command    []string
	opts       MCPOptions
	bindings   *storage.BindingStore
	operations sync.Mutex
	now        func() time.Time
	client     *mcpClient
	prepared   bool
	prepareErr error
	commandRun bool
}

func NewMCP(repository string, command []string, storageRoot string, opts MCPOptions) (*MCPProvider, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, errors.New("GitHub Issues MCP repository must be owner/name")
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, fmt.Errorf("%w: storage.mcp.command is required", storage.ErrPrerequisiteMissing)
	}
	bindings, err := storage.NewBindingStore(storageRoot)
	if err != nil {
		return nil, err
	}
	return &MCPProvider{repository: repository, owner: parts[0], repo: parts[1], command: append([]string(nil), command...), opts: opts, bindings: bindings, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (p *MCPProvider) Mirror(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	if err := diary.Validate(); err != nil {
		return pending(r, err)
	}
	return p.run(ctx, r, func(client *mcpClient) storage.MirrorResult {
		binding, ok, err := p.bindings.Load(ctx, r.Key)
		if err != nil {
			return pending(r, err)
		}
		var remote issue
		if ok {
			if strings.HasPrefix(string(binding.RemoteRevision), "comment:") {
				return p.compareCorrection(ctx, client, r, diary.Content, binding)
			}
			remote, err = p.getIssue(ctx, client, binding.RemoteID)
		} else {
			remote, err = p.findMarked(ctx, client, r.Key)
			if errors.Is(err, errNotFound) {
				err = nil
			}
		}
		if err != nil {
			return p.failed(r, err)
		}
		if remote.Number != 0 {
			return p.finishExisting(ctx, client, r, diary, remote)
		}
		if err := p.ensureLabels(ctx, client); err != nil {
			return p.failed(r, err)
		}
		remote, err = p.createIssue(ctx, client, diary)
		if err != nil {
			return p.failed(r, err)
		}
		created := remote
		remote, err = p.closeIssue(ctx, client, remote.Number)
		if err != nil {
			return p.failedWithRemote(r, created, err)
		}
		return mirroredIssue(r, remote, diary.Content)
	})
}

func (p *MCPProvider) Status(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	return p.run(ctx, r, func(client *mcpClient) storage.MirrorResult {
		binding, ok, err := p.bindings.Load(ctx, r.Key)
		if err != nil {
			return pending(r, err)
		}
		if !ok {
			return pending(r, errors.New("remote binding not found; run explicit storage recovery"))
		}
		if strings.HasPrefix(string(binding.RemoteRevision), "comment:") {
			return p.compareCorrection(ctx, client, r, diary.Content, binding)
		}
		remote, err := p.getIssue(ctx, client, binding.RemoteID)
		if err != nil {
			return p.failed(r, err)
		}
		return p.compare(r, diary.Content, remote)
	})
}

func (p *MCPProvider) Resolve(ctx context.Context, diary storage.SessionDiary, choice storage.Resolution, observedRevision string) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	if choice != storage.KeepLocal && choice != storage.KeepRemote {
		return mcpConflict(r, issue{}, errors.New("resolution must be keep-local or keep-remote"))
	}
	return p.run(ctx, r, func(client *mcpClient) storage.MirrorResult {
		binding, ok, err := p.bindings.Load(ctx, r.Key)
		if err != nil {
			return pending(r, err)
		}
		if !ok {
			return pending(r, errors.New("remote binding not found"))
		}
		effective, err := p.effectiveRemote(ctx, client, binding)
		if err != nil {
			return p.failed(r, err)
		}
		r.RemoteID, r.RemoteURL, r.RemoteRev, r.ExpectedRev = binding.RemoteID, binding.URL, effective.revision, observedRevision
		r.RemoteContent, r.EffectiveRemoteHash = effective.content, hash(effective.content)
		if observedRevision == "" || observedRevision != effective.revision {
			r.State, r.FailureClass, r.Cause = storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict
			return r
		}
		if choice == storage.KeepRemote {
			r.State = storage.Mirrored
			return r
		}
		number, err := strconv.Atoi(binding.RemoteID)
		if err != nil || number <= 0 {
			return pending(r, errors.New("remote binding contains an invalid Issue number"))
		}
		correction, err := p.findCorrection(ctx, client, number, diary)
		if errors.Is(err, errNotFound) {
			correction, err = p.createCorrection(ctx, client, number, diary)
		}
		if err != nil {
			return p.failed(r, err)
		}
		r.State, r.RemoteRev, r.EffectiveRemoteHash = storage.Mirrored, commentRevision(correction), hash(diary.Content)
		r.RemoteID, r.RemoteURL = binding.RemoteID, binding.URL
		return r
	})
}

func (p *MCPProvider) run(ctx context.Context, base storage.MirrorResult, operation func(*mcpClient) storage.MirrorResult) storage.MirrorResult {
	client, err := p.prepare(ctx)
	if err != nil {
		result := p.failed(base, err)
		if !p.commandRun {
			p.resetClient()
		}
		return result
	}
	result := operation(client)
	if !p.commandRun {
		if closeErr := p.closeClient(); closeErr != nil && result.Cause == nil {
			result.State, result.FailureClass, result.Cause = storage.StorageSyncPending, storage.FailureTransient, closeErr
		}
	}
	return result
}

// BeginCommand keeps one negotiated MCP child alive across all remote
// operations performed by a single Syntroph command. Standalone provider
// calls retain the safer start/use/close lifecycle automatically.
func (p *MCPProvider) BeginCommand() {
	p.operations.Lock()
	defer p.operations.Unlock()
	p.commandRun = true
}

// EndCommand gracefully closes the command-scoped MCP child, if one was
// needed. Authentication and all other process state remain provider-owned.
func (p *MCPProvider) EndCommand() error {
	p.operations.Lock()
	defer p.operations.Unlock()
	p.commandRun = false
	return p.closeClient()
}

func (p *MCPProvider) prepare(ctx context.Context) (*mcpClient, error) {
	if p.prepared {
		return p.client, p.prepareErr
	}
	p.prepared = true
	client, err := newMCPClient(p.command, p.opts)
	if err == nil {
		err = client.Negotiate(ctx)
	}
	if err == nil {
		var tools []mcpTool
		tools, err = client.ListTools(ctx)
		if err == nil {
			if validationErr := validateMCPTools(tools); validationErr != nil {
				err = &mcpProviderError{class: storage.FailurePrerequisite, message: validationErr.Error()}
			}
		}
	}
	if err != nil {
		if client != nil {
			_ = client.Close()
		}
		p.prepareErr = err
		return nil, err
	}
	p.client = client
	return client, nil
}

func (p *MCPProvider) closeClient() error {
	var err error
	if p.client != nil {
		err = p.client.Close()
	}
	p.resetClient()
	return err
}

func (p *MCPProvider) resetClient() {
	p.client, p.prepareErr, p.prepared = nil, nil, false
}

func (p *MCPProvider) baseResult(d storage.SessionDiary) storage.MirrorResult {
	return storage.MirrorResult{Backend: storage.BackendIssues, Provider: MCPProviderID, Key: d.Key(), LocalHash: hash(d.Content)}
}

func (p *MCPProvider) finishExisting(ctx context.Context, client *mcpClient, r storage.MirrorResult, diary storage.SessionDiary, remote issue) storage.MirrorResult {
	compared := p.compare(r, diary.Content, remote)
	if compared.State != storage.Mirrored {
		return compared
	}
	if remote.State != "closed" || remote.StateReason != "completed" {
		closed, err := p.closeIssue(ctx, client, remote.Number)
		if err != nil {
			return p.failedWithRemote(r, remote, err)
		}
		return mirroredIssue(r, closed, diary.Content)
	}
	return compared
}

func (p *MCPProvider) compare(r storage.MirrorResult, local string, remote issue) storage.MirrorResult {
	remoteContent, exact := stripDiaryMarker(remote.Body, r.Key)
	r.RemoteID, r.RemoteURL, r.RemoteRev = strconv.Itoa(remote.Number), remote.HTMLURL, issueRevision(remote)
	r.RemoteContent, r.EffectiveRemoteHash = remote.Body, hash(remote.Body)
	if exact {
		r.RemoteContent, r.EffectiveRemoteHash = remoteContent, hash(remoteContent)
	}
	if exact && remoteContent == local {
		r.State, r.EffectiveRemoteHash, r.RemoteContent = storage.Mirrored, hash(local), ""
		return r
	}
	return mcpConflict(r, remote, storage.ErrConflict)
}

func (p *MCPProvider) compareCorrection(ctx context.Context, client *mcpClient, r storage.MirrorResult, local string, binding storage.RemoteBinding) storage.MirrorResult {
	effective, err := p.effectiveRemote(ctx, client, binding)
	if err != nil {
		return p.failed(r, err)
	}
	r.RemoteID, r.RemoteURL, r.RemoteRev = binding.RemoteID, binding.URL, effective.revision
	r.RemoteContent, r.EffectiveRemoteHash = effective.content, hash(effective.content)
	if effective.exact && effective.content == local {
		r.State, r.RemoteContent = storage.Mirrored, ""
		return r
	}
	r.State, r.FailureClass, r.Cause = storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict
	return r
}

func (p *MCPProvider) effectiveRemote(ctx context.Context, client *mcpClient, binding storage.RemoteBinding) (effectiveRemote, error) {
	revision := string(binding.RemoteRevision)
	if strings.HasPrefix(revision, "comment:") {
		id, err := revisionID(revision, "comment")
		if err != nil {
			return effectiveRemote{}, err
		}
		number, err := strconv.Atoi(binding.RemoteID)
		if err != nil {
			return effectiveRemote{}, errors.New("remote binding contains an invalid Issue number")
		}
		comments, err := p.listComments(ctx, client, number)
		if err != nil {
			return effectiveRemote{}, err
		}
		for _, correction := range comments {
			if strconv.FormatInt(correction.ID, 10) != id {
				continue
			}
			content, exact := stripCorrectionMarker(correction.Body, binding.IdempotencyKey)
			if !exact {
				content = correction.Body
			}
			return effectiveRemote{revision: commentRevision(correction), content: content, exact: exact}, nil
		}
		return effectiveRemote{}, errNotFound
	}
	remote, err := p.getIssue(ctx, client, binding.RemoteID)
	if err != nil {
		return effectiveRemote{}, err
	}
	content, exact := stripDiaryMarker(remote.Body, binding.IdempotencyKey)
	if !exact {
		content = remote.Body
	}
	return effectiveRemote{revision: issueRevision(remote), content: content, exact: exact}, nil
}

func (p *MCPProvider) ensureLabels(ctx context.Context, client *mcpClient) error {
	for _, want := range reservedLabels {
		var got label
		err := client.CallTool(ctx, "get_label", p.args(map[string]any{"name": want.Name}), &got)
		if err != nil && !mcpNotFound(err) {
			return err
		}
		method := ""
		if mcpNotFound(err) {
			method = "create"
		} else if !strings.EqualFold(got.Color, want.Color) || got.Description != want.Description {
			method = "update"
		}
		if method != "" {
			arguments := p.args(map[string]any{"method": method, "name": want.Name, "color": want.Color, "description": want.Description})
			if err := client.CallTool(ctx, "label_write", arguments, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *MCPProvider) createIssue(ctx context.Context, client *mcpClient, diary storage.SessionDiary) (issue, error) {
	date := diaryDate(diary.Content)
	if date == "" {
		date = p.now().Format(time.DateOnly)
	}
	arguments := p.args(map[string]any{
		"method": "create", "title": fmt.Sprintf("[Syntroph] %s — %s", date, diary.SessionID),
		"body": diaryBody(diary.Content, diary.Key()), "labels": []string{"syntroph-memory", "syntroph-session"},
	})
	var out issue
	err := client.CallTool(ctx, "issue_write", arguments, &out)
	return out, err
}

func (p *MCPProvider) closeIssue(ctx context.Context, client *mcpClient, number int) (issue, error) {
	var out issue
	err := client.CallTool(ctx, "issue_write", p.args(map[string]any{"method": "update", "issue_number": number, "state": "closed", "state_reason": "completed"}), &out)
	return out, err
}

func (p *MCPProvider) getIssue(ctx context.Context, client *mcpClient, id string) (issue, error) {
	number, err := strconv.Atoi(id)
	if err != nil {
		return issue{}, errors.New("remote binding contains an invalid Issue number")
	}
	var out issue
	err = client.CallTool(ctx, "issue_read", p.args(map[string]any{"method": "get", "issue_number": number}), &out)
	return out, err
}

func (p *MCPProvider) createCorrection(ctx context.Context, client *mcpClient, number int, diary storage.SessionDiary) (comment, error) {
	var out comment
	err := client.CallTool(ctx, "add_issue_comment", p.args(map[string]any{"issue_number": number, "body": correctionBody(diary.Content, diary.Key())}), &out)
	return out, err
}

func (p *MCPProvider) findCorrection(ctx context.Context, client *mcpClient, number int, diary storage.SessionDiary) (comment, error) {
	comments, err := p.listComments(ctx, client, number)
	if err != nil {
		return comment{}, err
	}
	for _, candidate := range comments {
		content, exact := stripCorrectionMarker(candidate.Body, diary.Key())
		if exact && content == diary.Content {
			return candidate, nil
		}
	}
	return comment{}, errNotFound
}

func (p *MCPProvider) listComments(ctx context.Context, client *mcpClient, number int) ([]comment, error) {
	var all []comment
	for page := 1; ; page++ {
		var out struct {
			Comments []comment `json:"comments"`
		}
		if err := client.CallTool(ctx, "issue_read", p.args(map[string]any{"method": "get_comments", "issue_number": number, "page": page, "perPage": 100}), &out); err != nil {
			return nil, err
		}
		all = append(all, out.Comments...)
		if len(out.Comments) < 100 {
			return all, nil
		}
	}
}

func (p *MCPProvider) findMarked(ctx context.Context, client *mcpClient, key string) (issue, error) {
	for _, state := range []string{"closed", "open"} {
		cursor := ""
		for {
			arguments := p.args(map[string]any{"state": state, "labels": []string{"syntroph-memory", "syntroph-session"}})
			if cursor != "" {
				arguments["cursor"] = cursor
			}
			var page issueListResult
			if err := client.CallTool(ctx, "list_issues", arguments, &page); err != nil {
				return issue{}, err
			}
			for _, candidate := range page.Issues {
				if len(candidate.PullRequest) != 0 && string(candidate.PullRequest) != "null" {
					continue
				}
				if hasDiaryMarker(candidate.Body, key) {
					return candidate, nil
				}
			}
			cursor = page.cursor()
			if cursor == "" {
				break
			}
		}
	}
	return issue{}, errNotFound
}

type issueListResult struct {
	Issues     []issue `json:"issues"`
	NextCursor string  `json:"nextCursor"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

func (r issueListResult) cursor() string {
	if r.NextCursor != "" {
		return r.NextCursor
	}
	if r.PageInfo.HasNextPage {
		return r.PageInfo.EndCursor
	}
	return ""
}

func (p *MCPProvider) args(values map[string]any) map[string]any {
	values["owner"], values["repo"] = p.owner, p.repo
	return values
}

func (p *MCPProvider) failed(result storage.MirrorResult, err error) storage.MirrorResult {
	class := classifyMCPError(err)
	switch class {
	case storage.FailurePrerequisite:
		result.State = storage.StoragePrerequisiteMissing
	case storage.FailureConflict:
		result.State = storage.StorageSyncConflict
	default:
		result.State = storage.StorageSyncPending
	}
	result.FailureClass, result.Cause = class, err
	return result
}

func (p *MCPProvider) failedWithRemote(result storage.MirrorResult, remote issue, err error) storage.MirrorResult {
	result.RemoteID, result.RemoteURL = strconv.Itoa(remote.Number), remote.HTMLURL
	return p.failed(result, err)
}

func mcpConflict(result storage.MirrorResult, remote issue, err error) storage.MirrorResult {
	result.State, result.FailureClass, result.Cause = storage.StorageSyncConflict, storage.FailureConflict, err
	if remote.Number != 0 {
		result.RemoteID, result.RemoteURL, result.RemoteRev = strconv.Itoa(remote.Number), remote.HTMLURL, issueRevision(remote)
		result.RemoteContent, result.EffectiveRemoteHash = remote.Body, hash(remote.Body)
		if content, exact := stripDiaryMarker(remote.Body, result.Key); exact {
			result.RemoteContent, result.EffectiveRemoteHash = content, hash(content)
		}
	}
	return result
}

type mcpProviderError struct {
	class   storage.FailureClass
	message string
}

func (e *mcpProviderError) Error() string { return e.message }
func (e *mcpProviderError) Unwrap() error {
	if e.class == storage.FailurePrerequisite {
		return storage.ErrPrerequisiteMissing
	}
	if e.class == storage.FailureConflict {
		return storage.ErrConflict
	}
	return storage.ErrUnavailable
}

func classifyMCPError(err error) storage.FailureClass {
	var provider *mcpProviderError
	if errors.As(err, &provider) {
		return provider.class
	}
	if errors.Is(err, storage.ErrPrerequisiteMissing) {
		return storage.FailurePrerequisite
	}
	var protocol *mcpProtocolError
	if errors.As(err, &protocol) {
		return storage.FailurePrerequisite
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"unauthorized", "forbidden", "permission", "read-only", "readonly", "authentication", "not authenticated", "validation failed", "not found"} {
		if strings.Contains(message, fragment) {
			return storage.FailurePrerequisite
		}
	}
	if strings.Contains(message, "conflict") {
		return storage.FailureConflict
	}
	return storage.FailureTransient
}

func mcpNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

func validateMCPTools(tools []mcpTool) error {
	available := make(map[string]mcpTool, len(tools))
	for _, tool := range tools {
		available[tool.Name] = tool
	}
	var problems []string
	for name, requirement := range requiredMCPTools {
		tool, ok := available[name]
		if !ok {
			problems = append(problems, "missing "+name)
			continue
		}
		properties := schemaProperties(tool.InputSchema)
		for _, property := range requirement.Properties {
			if _, ok := properties[property]; !ok {
				problems = append(problems, fmt.Sprintf("%s schema lacks %s", name, property))
			}
		}
		if len(requirement.Methods) != 0 {
			methods := schemaMethodEnums(tool.InputSchema)
			for _, method := range requirement.Methods {
				if !methods[method] {
					problems = append(problems, fmt.Sprintf("%s schema lacks method=%s", name, method))
				}
			}
		}
	}
	if len(problems) != 0 {
		return fmt.Errorf("MCP server lacks required GitHub issue capabilities (%s); configure the official server with toolsets issues,labels and non-interactive provider-owned authentication", strings.Join(problems, "; "))
	}
	return nil
}

func schemaProperties(schema map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if properties, ok := node["properties"].(map[string]any); ok {
				for name, property := range properties {
					out[name] = property
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(schema)
	return out
}

func schemaMethodEnums(schema map[string]any) map[string]bool {
	methods := map[string]bool{}
	properties := schemaProperties(schema)
	var walk func(any)
	walk = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if enum, ok := node["enum"].([]any); ok {
				for _, value := range enum {
					if method, ok := value.(string); ok {
						methods[method] = true
					}
				}
			}
			if constant, ok := node["const"].(string); ok {
				methods[constant] = true
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(properties["method"])
	return methods
}
