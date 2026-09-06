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
	"list_issues":       {Properties: []string{"owner", "repo", "state", "labels", "after"}},
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

type mcpIssueOperations struct {
	provider *MCPProvider
	client   *mcpClient
}

func (o mcpIssueOperations) ensureLabels(ctx context.Context) error {
	return o.provider.ensureLabels(ctx, o.client)
}
func (o mcpIssueOperations) createIssue(ctx context.Context, d storage.SessionDiary) (issue, error) {
	return o.provider.createIssue(ctx, o.client, d)
}
func (o mcpIssueOperations) closeIssue(ctx context.Context, number int) (issue, error) {
	return o.provider.closeIssue(ctx, o.client, number)
}
func (o mcpIssueOperations) getIssue(ctx context.Context, id string) (issue, error) {
	return o.provider.getIssue(ctx, o.client, id)
}
func (o mcpIssueOperations) effectiveRemote(ctx context.Context, b storage.RemoteBinding) (effectiveRemote, error) {
	return o.provider.effectiveRemote(ctx, o.client, b)
}
func (o mcpIssueOperations) findCorrection(ctx context.Context, number int, d storage.SessionDiary, predecessor string) (comment, error) {
	return o.provider.findCorrection(ctx, o.client, number, d, predecessor)
}
func (o mcpIssueOperations) createCorrection(ctx context.Context, number int, d storage.SessionDiary, predecessor string) (comment, error) {
	return o.provider.createCorrection(ctx, o.client, number, d, predecessor)
}
func (o mcpIssueOperations) findMarked(ctx context.Context, key string) (issue, error) {
	return o.provider.findMarked(ctx, o.client, key)
}
func (o mcpIssueOperations) failed(r storage.MirrorResult, err error) storage.MirrorResult {
	return o.provider.failed(r, err)
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
	return p.mirror(ctx, diary, false)
}

func (p *MCPProvider) Recover(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	return p.mirror(ctx, diary, true)
}

func (p *MCPProvider) mirror(ctx context.Context, diary storage.SessionDiary, recovery bool) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	if err := diary.Validate(); err != nil {
		return pending(r, err)
	}
	return p.run(ctx, r, func(client *mcpClient) storage.MirrorResult {
		ops := mcpIssueOperations{provider: p, client: client}
		return (issueSemantics{provider: MCPProviderID, bindings: p.bindings, ops: ops}).mirror(ctx, diary, recovery)
	})
}

func (p *MCPProvider) Status(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	return p.run(ctx, r, func(client *mcpClient) storage.MirrorResult {
		ops := mcpIssueOperations{provider: p, client: client}
		return (issueSemantics{provider: MCPProviderID, bindings: p.bindings, ops: ops}).status(ctx, diary)
	})
}

func (p *MCPProvider) Resolve(ctx context.Context, diary storage.SessionDiary, choice storage.Resolution, observedRevision string) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	return p.run(ctx, r, func(client *mcpClient) storage.MirrorResult {
		ops := mcpIssueOperations{provider: p, client: client}
		return (issueSemantics{provider: MCPProviderID, bindings: p.bindings, ops: ops}).resolve(ctx, diary, choice, observedRevision)
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
				return effectiveRemote{revision: commentRevision(correction), content: content, exact: false}, nil
			}
			marker, _ := parseCorrectionMarker(correction.Body, binding.IdempotencyKey)
			predecessor, matches, predecessorErr := p.predecessor(ctx, client, binding, comments, marker.Predecessor)
			if predecessorErr != nil {
				return effectiveRemote{}, predecessorErr
			}
			if !matches {
				return predecessor, nil
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

func (p *MCPProvider) predecessor(ctx context.Context, client *mcpClient, binding storage.RemoteBinding, comments []comment, revision string) (effectiveRemote, bool, error) {
	if strings.HasPrefix(revision, "issue:") {
		remote, err := p.getIssue(ctx, client, binding.RemoteID)
		if err != nil {
			return effectiveRemote{}, false, err
		}
		content, exact := stripDiaryMarker(remote.Body, binding.IdempotencyKey)
		if !exact {
			content = remote.Body
		}
		actual := issueRevision(remote)
		return effectiveRemote{revision: actual, content: content, exact: exact}, actual == revision, nil
	}
	if strings.HasPrefix(revision, "comment:") {
		id, err := revisionID(revision, "comment")
		if err != nil {
			return effectiveRemote{}, false, err
		}
		for _, previous := range comments {
			if strconv.FormatInt(previous.ID, 10) != id {
				continue
			}
			content, exact := stripCorrectionMarker(previous.Body, binding.IdempotencyKey)
			if !exact {
				content = previous.Body
			}
			actual := commentRevision(previous)
			return effectiveRemote{revision: actual, content: content, exact: exact}, actual == revision, nil
		}
		return effectiveRemote{}, false, errNotFound
	}
	return effectiveRemote{}, false, errors.New("correction predecessor has an unsupported remote revision")
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

func (p *MCPProvider) createCorrection(ctx context.Context, client *mcpClient, number int, diary storage.SessionDiary, predecessor string) (comment, error) {
	var out comment
	err := client.CallTool(ctx, "add_issue_comment", p.args(map[string]any{"issue_number": number, "body": correctionBody(diary.Content, diary.Key(), predecessor)}), &out)
	return out, err
}

func (p *MCPProvider) findCorrection(ctx context.Context, client *mcpClient, number int, diary storage.SessionDiary, predecessor string) (comment, error) {
	comments, err := p.listComments(ctx, client, number)
	if err != nil {
		return comment{}, err
	}
	for _, candidate := range comments {
		content, exact := stripCorrectionMarker(candidate.Body, diary.Key())
		marker, markerOK := parseCorrectionMarker(candidate.Body, diary.Key())
		if exact && markerOK && marker.Predecessor == predecessor && content == diary.Content {
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
	after := ""
	for {
		arguments := p.args(map[string]any{"state": "closed", "labels": []string{"syntroph-memory", "syntroph-session"}})
		if after != "" {
			arguments["after"] = after
		}
		var page issueListResult
		if err := client.CallTool(ctx, "list_issues", arguments, &page); err != nil {
			return issue{}, err
		}
		for _, candidate := range page.Issues {
			if candidate.State != "closed" {
				continue
			}
			if len(candidate.PullRequest) != 0 && string(candidate.PullRequest) != "null" {
				continue
			}
			if hasDiaryMarker(candidate.Body, key) {
				return candidate, nil
			}
		}
		after = page.cursor()
		if after == "" {
			break
		}
	}
	return issue{}, errNotFound
}

type issueListResult struct {
	Issues     []issue `json:"issues"`
	NextCursor string  `json:"nextCursor"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		NextCursor  string `json:"nextCursor"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

func (r issueListResult) cursor() string {
	if r.NextCursor != "" {
		return r.NextCursor
	}
	if r.PageInfo.HasNextPage {
		if r.PageInfo.NextCursor != "" {
			return r.PageInfo.NextCursor
		}
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
