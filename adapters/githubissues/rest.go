// Package githubissues implements the GitHub Issues REST storage provider.
package githubissues

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

const (
	ProviderID       = "github-rest"
	APIVersion       = "2026-03-10"
	markerPrefix     = "<!-- syntroph-idempotency-key:"
	maxResponseBytes = 16 << 20
)

type Sleeper func(context.Context, time.Duration) error

type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	UserAgent  string
	Sleep      Sleeper
	Jitter     func(time.Duration) time.Duration
	Now        func() time.Time
}

type Provider struct {
	repository string
	token      string
	baseURL    string
	userAgent  string
	client     *http.Client
	sleep      Sleeper
	jitter     func(time.Duration) time.Duration
	now        func() time.Time
	bindings   *storage.BindingStore
	operations sync.Mutex
	mutations  sync.Mutex
	scan       sync.Mutex
	lastNext   string
}

func New(repository, token, storageRoot string, opts Options) (*Provider, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, errors.New("GitHub Issues repository must be owner/name")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: SYNTROPH_GITHUB_TOKEN is empty", storage.ErrPrerequisiteMissing)
	}
	bindings, err := storage.NewBindingStore(storageRoot)
	if err != nil {
		return nil, err
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = "syntroph/dev"
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	jitter := opts.Jitter
	if jitter == nil {
		jitter = func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Int63n(int64(max) + 1))
		}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Provider{repository: repository, token: token, baseURL: baseURL, userAgent: userAgent, client: client, sleep: sleep, jitter: jitter, now: now, bindings: bindings}, nil
}

type label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

var reservedLabels = []label{
	{Name: "syntroph-memory", Color: "00e5ff", Description: "Immutable Syntroph session memory."},
	{Name: "syntroph-session", Color: "a855f7", Description: "Syntroph session diary mirror."},
}

type issue struct {
	Number      int             `json:"number"`
	HTMLURL     string          `json:"html_url"`
	Body        string          `json:"body"`
	UpdatedAt   time.Time       `json:"updated_at"`
	State       string          `json:"state"`
	StateReason string          `json:"state_reason"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type comment struct {
	ID        int64     `json:"id"`
	HTMLURL   string    `json:"html_url"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p *Provider) Mirror(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	if err := diary.Validate(); err != nil {
		return pending(r, err)
	}
	binding, ok, err := p.bindings.Load(ctx, r.Key)
	if err != nil {
		return pending(r, err)
	}
	var remote issue
	if ok {
		if strings.HasPrefix(string(binding.RemoteRevision), "comment:") {
			return p.compareCorrection(ctx, r, diary.Content, binding)
		}
		remote, err = p.getIssue(ctx, binding.RemoteID)
	} else {
		remote, err = p.findMarked(ctx, r.Key)
		if errors.Is(err, errNotFound) {
			err = nil
		}
	}
	if err != nil {
		return p.failed(r, err)
	}
	if remote.Number != 0 {
		return p.finishExisting(ctx, r, diary, remote)
	}
	if err := p.ensureLabels(ctx); err != nil {
		return p.failed(r, err)
	}
	remote, err = p.createIssue(ctx, diary)
	if err != nil {
		return p.failed(r, err)
	}
	// If the close response is lost, the exact marker lets an explicit retry
	// reconstruct this same Issue instead of creating another one.
	created := remote
	remote, err = p.closeIssue(ctx, remote.Number)
	if err != nil {
		return p.failedWithRemote(r, created, err)
	}
	return mirroredIssue(r, remote, diary.Content)
}

func (p *Provider) Status(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	binding, ok, err := p.bindings.Load(ctx, r.Key)
	if err != nil {
		return pending(r, err)
	}
	if !ok {
		return pending(r, errors.New("remote binding not found; run explicit storage recovery"))
	}
	if strings.HasPrefix(string(binding.RemoteRevision), "comment:") {
		return p.compareCorrection(ctx, r, diary.Content, binding)
	}
	remote, err := p.getIssue(ctx, binding.RemoteID)
	if err != nil {
		return p.failed(r, err)
	}
	return p.compare(r, diary.Content, remote)
}

func (p *Provider) Resolve(ctx context.Context, diary storage.SessionDiary, choice storage.Resolution, observedRevision string) storage.MirrorResult {
	p.operations.Lock()
	defer p.operations.Unlock()
	r := p.baseResult(diary)
	if choice != storage.KeepLocal && choice != storage.KeepRemote {
		return conflict(r, issue{}, errors.New("resolution must be keep-local or keep-remote"))
	}
	binding, ok, err := p.bindings.Load(ctx, r.Key)
	if err != nil {
		return pending(r, err)
	}
	if !ok {
		return pending(r, errors.New("remote binding not found"))
	}
	effective, err := p.effectiveRemote(ctx, binding)
	if err != nil {
		return p.failed(r, err)
	}
	currentRev := effective.revision
	r.RemoteID, r.RemoteURL, r.RemoteRev, r.ExpectedRev = binding.RemoteID, binding.URL, currentRev, observedRevision
	r.RemoteContent, r.EffectiveRemoteHash = effective.content, hash(effective.content)
	if observedRevision == "" || observedRevision != currentRev {
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
	c, err := p.findCorrection(ctx, number, diary)
	if errors.Is(err, errNotFound) {
		c, err = p.createCorrection(ctx, number, diary)
	}
	if err != nil {
		return p.failed(r, err)
	}
	r.State, r.RemoteRev, r.EffectiveRemoteHash = storage.Mirrored, commentRevision(c), hash(diary.Content)
	// Preserve the Issue as the Remote Mirror identity; the revision identifies
	// the append-only correction that is now effective.
	r.RemoteID, r.RemoteURL = binding.RemoteID, binding.URL
	return r
}

func (p *Provider) finishExisting(ctx context.Context, r storage.MirrorResult, diary storage.SessionDiary, remote issue) storage.MirrorResult {
	compared := p.compare(r, diary.Content, remote)
	if compared.State != storage.Mirrored {
		return compared
	}
	if remote.State != "closed" || remote.StateReason != "completed" {
		closed, err := p.closeIssue(ctx, remote.Number)
		if err != nil {
			return p.failedWithRemote(r, remote, err)
		}
		return mirroredIssue(r, closed, diary.Content)
	}
	return compared
}

func (p *Provider) compare(r storage.MirrorResult, local string, remote issue) storage.MirrorResult {
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
	return conflict(r, remote, storage.ErrConflict)
}

type effectiveRemote struct {
	revision string
	content  string
	exact    bool
}

func (p *Provider) effectiveRemote(ctx context.Context, binding storage.RemoteBinding) (effectiveRemote, error) {
	revision := string(binding.RemoteRevision)
	if strings.HasPrefix(revision, "comment:") {
		id, err := revisionID(revision, "comment")
		if err != nil {
			return effectiveRemote{}, err
		}
		correction, err := p.getComment(ctx, id)
		if err != nil {
			return effectiveRemote{}, err
		}
		content, exact := stripCorrectionMarker(correction.Body, binding.IdempotencyKey)
		if !exact {
			content = correction.Body
		}
		return effectiveRemote{revision: commentRevision(correction), content: content, exact: exact}, nil
	}
	remote, err := p.getIssue(ctx, binding.RemoteID)
	if err != nil {
		return effectiveRemote{}, err
	}
	content, exact := stripDiaryMarker(remote.Body, binding.IdempotencyKey)
	if !exact {
		content = remote.Body
	}
	return effectiveRemote{revision: issueRevision(remote), content: content, exact: exact}, nil
}

func (p *Provider) compareCorrection(ctx context.Context, r storage.MirrorResult, local string, binding storage.RemoteBinding) storage.MirrorResult {
	effective, err := p.effectiveRemote(ctx, binding)
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

func revisionID(revision, kind string) (string, error) {
	prefix := kind + ":"
	if !strings.HasPrefix(revision, prefix) {
		return "", fmt.Errorf("remote revision is not a %s revision", kind)
	}
	id, _, ok := strings.Cut(strings.TrimPrefix(revision, prefix), ":")
	if !ok || id == "" {
		return "", fmt.Errorf("invalid %s revision", kind)
	}
	return id, nil
}

func (p *Provider) baseResult(d storage.SessionDiary) storage.MirrorResult {
	return storage.MirrorResult{Backend: storage.BackendIssues, Provider: ProviderID, Key: d.Key(), LocalHash: hash(d.Content)}
}

func mirroredIssue(r storage.MirrorResult, i issue, content string) storage.MirrorResult {
	r.State, r.RemoteID, r.RemoteURL, r.RemoteRev = storage.Mirrored, strconv.Itoa(i.Number), i.HTMLURL, issueRevision(i)
	r.EffectiveRemoteHash = hash(content)
	return r
}

func conflict(r storage.MirrorResult, i issue, err error) storage.MirrorResult {
	r.State, r.FailureClass, r.Cause = storage.StorageSyncConflict, storage.FailureConflict, err
	if i.Number != 0 {
		r.RemoteID, r.RemoteURL, r.RemoteRev, r.RemoteContent, r.EffectiveRemoteHash = strconv.Itoa(i.Number), i.HTMLURL, issueRevision(i), i.Body, hash(i.Body)
		if content, exact := stripDiaryMarker(i.Body, r.Key); exact {
			r.RemoteContent, r.EffectiveRemoteHash = content, hash(content)
		}
	}
	return r
}
func pending(r storage.MirrorResult, err error) storage.MirrorResult {
	r.State, r.FailureClass, r.Cause = storage.StorageSyncPending, storage.FailureTransient, err
	return r
}

var errNotFound = errors.New("remote mirror not found")

type providerError struct {
	class   storage.FailureClass
	status  int
	message string
}

func (e *providerError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("GitHub REST status %d: %s", e.status, e.message)
	}
	return e.message
}
func (e *providerError) Unwrap() error {
	if e.class == storage.FailurePrerequisite {
		return storage.ErrPrerequisiteMissing
	}
	if e.class == storage.FailureConflict {
		return storage.ErrConflict
	}
	return storage.ErrUnavailable
}

func (p *Provider) failed(r storage.MirrorResult, err error) storage.MirrorResult {
	var pe *providerError
	if errors.As(err, &pe) {
		switch pe.class {
		case storage.FailurePrerequisite:
			r.State, r.FailureClass = storage.StoragePrerequisiteMissing, storage.FailurePrerequisite
		case storage.FailureConflict:
			r.State, r.FailureClass = storage.StorageSyncConflict, storage.FailureConflict
		default:
			r.State, r.FailureClass = storage.StorageSyncPending, storage.FailureTransient
		}
	} else if errors.Is(err, storage.ErrPrerequisiteMissing) {
		r.State, r.FailureClass = storage.StoragePrerequisiteMissing, storage.FailurePrerequisite
	} else {
		r.State, r.FailureClass = storage.StorageSyncPending, storage.FailureTransient
	}
	r.Cause = err
	return r
}
func (p *Provider) failedWithRemote(r storage.MirrorResult, i issue, err error) storage.MirrorResult {
	r.RemoteID, r.RemoteURL = strconv.Itoa(i.Number), i.HTMLURL
	return p.failed(r, err)
}

func (p *Provider) ensureLabels(ctx context.Context) error {
	for _, want := range reservedLabels {
		var got label
		path := p.repoPath("labels/" + url.PathEscape(want.Name))
		status, _, err := p.request(ctx, http.MethodGet, path, nil, &got)
		if err != nil {
			return err
		}
		if status == http.StatusNotFound {
			status, body, createErr := p.request(ctx, http.MethodPost, p.repoPath("labels"), want, &got)
			if createErr != nil {
				if status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(string(body)), "already_exists") {
					_, _, createErr = p.request(ctx, http.MethodGet, path, nil, &got)
				}
				if createErr != nil {
					return createErr
				}
			}
		}
		if !strings.EqualFold(got.Color, want.Color) || got.Description != want.Description {
			update := map[string]string{"color": want.Color, "description": want.Description}
			if _, _, err := p.request(ctx, http.MethodPatch, path, update, &got); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Provider) createIssue(ctx context.Context, d storage.SessionDiary) (issue, error) {
	date := diaryDate(d.Content)
	if date == "" {
		date = p.now().UTC().Format(time.DateOnly)
	}
	payload := map[string]any{"title": fmt.Sprintf("[Syntroph] %s — %s", date, d.SessionID), "body": diaryBody(d.Content, d.Key()), "labels": []string{"syntroph-memory", "syntroph-session"}}
	var out issue
	_, _, err := p.request(ctx, http.MethodPost, p.repoPath("issues"), payload, &out)
	return out, err
}
func (p *Provider) closeIssue(ctx context.Context, number int) (issue, error) {
	var out issue
	_, _, err := p.request(ctx, http.MethodPatch, p.repoPath("issues/"+strconv.Itoa(number)), map[string]string{"state": "closed", "state_reason": "completed"}, &out)
	return out, err
}
func (p *Provider) createCorrection(ctx context.Context, number int, d storage.SessionDiary) (comment, error) {
	var out comment
	body := correctionBody(d.Content, d.Key())
	_, _, err := p.request(ctx, http.MethodPost, p.repoPath("issues/"+strconv.Itoa(number)+"/comments"), map[string]string{"body": body}, &out)
	return out, err
}
func (p *Provider) getIssue(ctx context.Context, id string) (issue, error) {
	var out issue
	status, _, err := p.request(ctx, http.MethodGet, p.repoPath("issues/"+url.PathEscape(id)), nil, &out)
	if err == nil && status == http.StatusNotFound {
		return issue{}, &providerError{class: storage.FailurePrerequisite, status: status, message: "Issue mirror is unavailable or not visible with the configured token"}
	}
	return out, err
}
func (p *Provider) getComment(ctx context.Context, id string) (comment, error) {
	var out comment
	status, _, err := p.request(ctx, http.MethodGet, p.repoPath("issues/comments/"+url.PathEscape(id)), nil, &out)
	if err == nil && status == http.StatusNotFound {
		return comment{}, &providerError{class: storage.FailurePrerequisite, status: status, message: "Issue correction is unavailable or not visible with the configured token"}
	}
	return out, err
}

// findCorrection makes keep-local safe to redeliver after a crash between the
// comment write and the atomic binding update. The marker version is the hash
// of the canonical local diary, so only the exact correction is reused.
func (p *Provider) findCorrection(ctx context.Context, number int, d storage.SessionDiary) (comment, error) {
	p.scan.Lock()
	defer p.scan.Unlock()
	next := p.repoPath("issues/"+strconv.Itoa(number)+"/comments") + "?per_page=100"
	for next != "" {
		var comments []comment
		p.lastNext = ""
		_, _, err := p.request(ctx, http.MethodGet, next, nil, &comments)
		if err != nil {
			return comment{}, err
		}
		for _, candidate := range comments {
			content, exact := stripCorrectionMarker(candidate.Body, d.Key())
			if exact && content == d.Content {
				return candidate, nil
			}
		}
		next = p.lastNext
	}
	return comment{}, errNotFound
}

func (p *Provider) findMarked(ctx context.Context, key string) (issue, error) {
	p.scan.Lock()
	defer p.scan.Unlock()
	for _, state := range []string{"closed", "open"} {
		next := p.repoPath("issues") + "?state=" + state + "&labels=syntroph-memory%2Csyntroph-session&per_page=100"
		for next != "" {
			var issues []issue
			p.lastNext = ""
			_, _, err := p.request(ctx, http.MethodGet, next, nil, &issues)
			if err != nil {
				return issue{}, err
			}
			for _, candidate := range issues {
				if len(candidate.PullRequest) != 0 && string(candidate.PullRequest) != "null" {
					continue
				}
				if hasDiaryMarker(candidate.Body, key) {
					return candidate, nil
				}
			}
			next = p.lastNext
		}
	}
	return issue{}, errNotFound
}

func (p *Provider) repoPath(suffix string) string { return "/repos/" + p.repository + "/" + suffix }

// request executes one REST call. Mutations are serialized and approved
// transient statuses receive at most three total attempts.
func (p *Provider) request(ctx context.Context, method, path string, payload, out any) (int, []byte, error) {
	if method != http.MethodGet {
		p.mutations.Lock()
		defer p.mutations.Unlock()
	}
	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		reqURL := path
		if !strings.HasPrefix(reqURL, "http://") && !strings.HasPrefix(reqURL, "https://") {
			reqURL = p.baseURL + reqURL
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(encoded))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+p.token)
		req.Header.Set("X-GitHub-Api-Version", APIVersion)
		req.Header.Set("User-Agent", p.userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return 0, nil, &providerError{class: storage.FailureTransient, message: "GitHub REST request failed: " + err.Error()}
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return resp.StatusCode, body, &providerError{class: storage.FailureTransient, status: resp.StatusCode, message: readErr.Error()}
		}
		if len(body) > maxResponseBytes {
			return resp.StatusCode, nil, &providerError{class: storage.FailureTransient, status: resp.StatusCode, message: "GitHub REST response exceeds limit"}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(body) != 0 {
				if err := json.Unmarshal(body, out); err != nil {
					return resp.StatusCode, body, &providerError{class: storage.FailureTransient, status: resp.StatusCode, message: "decode GitHub REST response: " + err.Error()}
				}
			}
			p.lastNext = parseNext(resp.Header.Get("Link"), p.baseURL)
			return resp.StatusCode, body, nil
		}
		if resp.StatusCode == http.StatusNotFound && method == http.MethodGet {
			return resp.StatusCode, body, nil
		}
		class, retry := classify(resp.StatusCode, resp.Header, body)
		if retry && attempt < 2 {
			if err := p.sleep(ctx, retryDelay(resp.Header, attempt, p.now, p.jitter)); err != nil {
				return resp.StatusCode, body, err
			}
			continue
		}
		return resp.StatusCode, body, &providerError{class: class, status: resp.StatusCode, message: safeMessage(resp.Header, body)}
	}
	panic("unreachable")
}

func classify(status int, h http.Header, body []byte) (storage.FailureClass, bool) {
	message := strings.ToLower(string(body))
	secondary := strings.Contains(message, "secondary rate limit") || strings.Contains(message, "secondary rate")
	if status == 429 || status == 502 || status == 503 || status == 504 || (status == 403 && (secondary || (h.Get("Retry-After") != "" && h.Get("X-RateLimit-Remaining") != "0"))) {
		return storage.FailureTransient, true
	}
	if status == 403 && h.Get("X-RateLimit-Remaining") == "0" {
		return storage.FailureTransient, false
	}
	if status == 409 {
		return storage.FailureConflict, false
	}
	if status == 401 || status == 403 || status == 404 || status == 410 || status == 422 {
		return storage.FailurePrerequisite, false
	}
	return storage.FailureTransient, false
}

func retryDelay(h http.Header, attempt int, now func() time.Time, jitter func(time.Duration) time.Duration) time.Duration {
	if value := h.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second
		}
		if when, err := http.ParseTime(value); err == nil {
			d := when.Sub(now())
			if d > 0 {
				return d
			}
			return 0
		}
	}
	base := 100 * time.Millisecond * time.Duration(1<<attempt)
	return base + jitter(base/2)
}
func safeMessage(h http.Header, body []byte) string {
	var value struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &value)
	if value.Message == "" {
		value.Message = "GitHub REST request failed"
	}
	diagnostics := []string{}
	for _, k := range []string{"X-GitHub-Request-Id", "X-Accepted-GitHub-Permissions", "X-RateLimit-Remaining"} {
		if v := h.Get(k); v != "" {
			diagnostics = append(diagnostics, strings.ToLower(k)+"="+v)
		}
	}
	if len(diagnostics) > 0 {
		value.Message += " (" + strings.Join(diagnostics, ", ") + ")"
	}
	return value.Message
}
func parseNext(link, base string) string {
	for _, part := range strings.Split(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			start, end := strings.Index(part, "<"), strings.Index(part, ">")
			if start >= 0 && end > start {
				next := part[start+1 : end]
				if strings.HasPrefix(next, base) {
					return strings.TrimPrefix(next, base)
				}
				return next
			}
		}
	}
	return ""
}

func diaryBody(content, key string) string { return content + "\n\n" + markerPrefix + key + " -->\n" }
func hasDiaryMarker(body, key string) bool {
	return strings.Contains(body, markerPrefix+key+" -->")
}
func stripDiaryMarker(body, key string) (string, bool) {
	marker := "\n\n" + markerPrefix + key + " -->\n"
	if !strings.HasSuffix(body, marker) {
		return "", false
	}
	return strings.TrimSuffix(body, marker), true
}
func correctionBody(content, key string) string {
	return content + "\n\n<!-- syntroph-remote-correction:key=" + key + ";version=" + hash(content) + " -->\n"
}
func stripCorrectionMarker(body, key string) (string, bool) {
	suffixStart := "\n\n<!-- syntroph-remote-correction:key=" + key + ";version="
	i := strings.LastIndex(body, suffixStart)
	if i < 0 || !strings.HasSuffix(body, " -->\n") {
		return "", false
	}
	version := strings.TrimSuffix(body[i+len(suffixStart):], " -->\n")
	content := body[:i]
	if version != hash(content) {
		return "", false
	}
	return content, true
}
func hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
func issueRevision(i issue) string {
	return fmt.Sprintf("issue:%d:%s:%s", i.Number, i.UpdatedAt.UTC().Format(time.RFC3339Nano), hash(i.Body))
}
func commentRevision(c comment) string {
	return fmt.Sprintf("comment:%d:%s:%s", c.ID, c.UpdatedAt.UTC().Format(time.RFC3339Nano), hash(c.Body))
}
func diaryDate(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "created_at: ") {
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(strings.TrimPrefix(line, "created_at: "))); err == nil {
				return parsed.UTC().Format(time.DateOnly)
			}
		}
	}
	return ""
}
