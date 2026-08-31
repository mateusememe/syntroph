// Package githubwiki mirrors immutable Session Diaries to GitHub's Git-backed
// Wiki repository without owning credentials or changing Git configuration.
package githubwiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

const ProviderID = "github-wiki-git"

type Author struct {
	Name  string
	Email string
}

type Options struct {
	Repository     string
	RepositoryRoot string
	RemoteURL      string
	WebURL         string
	GitExecutable  string
	Author         Author
}

type Provider struct {
	options    Options
	beforePush func()
}

// ManagedProvider delays creation of local recovery projections until the
// first external operation, keeping `doctor storage` strictly non-mutating.
type ManagedProvider struct {
	root    string
	remote  *Provider
	once    sync.Once
	managed *storage.ManagedMirror
	err     error
}

func NewManaged(root string, options Options) (*ManagedProvider, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("Syntroph storage root is required")
	}
	remote, err := New(options)
	if err != nil {
		return nil, err
	}
	return &ManagedProvider{root: root, remote: remote}, nil
}

func (p *ManagedProvider) initialize() {
	p.once.Do(func() { p.managed, p.err = storage.NewManagedMirror(p.root, ProviderID, p.remote) })
}

func (p *ManagedProvider) Mirror(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.initialize()
	if p.err != nil {
		return p.remote.failure(diary, storage.StorageSyncPending, storage.FailureTransient, p.err)
	}
	return p.managed.Mirror(ctx, diary)
}

func (p *ManagedProvider) Status(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.initialize()
	if p.err != nil {
		return p.remote.failure(diary, storage.StorageSyncPending, storage.FailureTransient, p.err)
	}
	return p.managed.Status(ctx, diary)
}

func (p *ManagedProvider) Resolve(ctx context.Context, diary storage.SessionDiary, choice storage.Resolution, observed string) storage.MirrorResult {
	p.initialize()
	if p.err != nil {
		return p.remote.failure(diary, storage.StorageSyncPending, storage.FailureTransient, p.err)
	}
	return p.managed.Resolve(ctx, diary, choice, observed)
}

func New(options Options) (*Provider, error) {
	options.Repository = strings.TrimSpace(options.Repository)
	options.RemoteURL = strings.TrimSpace(options.RemoteURL)
	options.WebURL = strings.TrimRight(strings.TrimSpace(options.WebURL), "/")
	options.GitExecutable = strings.TrimSpace(options.GitExecutable)
	if options.GitExecutable == "" {
		options.GitExecutable = "git"
	}
	if options.Repository == "" || options.RemoteURL == "" {
		return nil, errors.New("GitHub Wiki provider requires repository and remote URL")
	}
	if options.WebURL == "" {
		options.WebURL = "https://github.com/" + options.Repository + "/wiki"
	}
	if (strings.TrimSpace(options.Author.Name) == "") != (strings.TrimSpace(options.Author.Email) == "") {
		return nil, errors.New("Wiki commit author requires both name and email")
	}
	return &Provider{options: options}, nil
}

func (p *Provider) Mirror(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	return p.apply(ctx, diary, "", "")
}

func (p *Provider) Status(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	return p.inspect(ctx, diary)
}

func (p *Provider) Resolve(ctx context.Context, diary storage.SessionDiary, choice storage.Resolution, observed string) storage.MirrorResult {
	if choice != storage.KeepLocal && choice != storage.KeepRemote {
		return p.failure(diary, storage.StorageSyncConflict, storage.FailureConflict, errors.New("resolution must be keep-local or keep-remote"))
	}
	if observed == "" || !strings.HasPrefix(observed, "git:") {
		return p.failure(diary, storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict)
	}
	return p.apply(ctx, diary, choice, observed)
}

func (p *Provider) inspect(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	return p.apply(ctx, diary, "status", "")
}

func (p *Provider) apply(ctx context.Context, diary storage.SessionDiary, mode storage.Resolution, observed string) storage.MirrorResult {
	base := p.base(diary)
	if err := diary.Validate(); err != nil {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, err)
	}
	path, expected, err := wikiPage(diary)
	if err != nil {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, err)
	}
	dir, err := os.MkdirTemp("", "syntroph-wiki-*")
	if err != nil {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, err)
	}
	defer os.RemoveAll(dir)
	if _, err = p.git(ctx, "clone", "--quiet", "--no-tags", p.options.RemoteURL, dir); err != nil {
		return p.gitFailure(base, err, true)
	}
	branch, err := p.git(ctx, "-C", dir, "symbolic-ref", "--short", "HEAD")
	if err != nil || strings.TrimSpace(branch) == "" {
		return p.fail(base, storage.StoragePrerequisiteMissing, storage.FailurePrerequisite, fmt.Errorf("%w: GitHub Wiki must be enabled and initialized with its first page", storage.ErrPrerequisiteMissing))
	}
	branch = strings.TrimSpace(branch)
	if _, err = p.git(ctx, "-C", dir, "fetch", "--quiet", "--no-tags", "origin"); err != nil {
		return p.gitFailure(base, err, true)
	}
	remoteRef := "refs/remotes/origin/" + branch
	remoteHead, err := p.git(ctx, "-C", dir, "rev-parse", remoteRef)
	if err != nil || strings.TrimSpace(remoteHead) == "" {
		return p.fail(base, storage.StoragePrerequisiteMissing, storage.FailurePrerequisite, fmt.Errorf("%w: GitHub Wiki must be enabled and initialized with its first page", storage.ErrPrerequisiteMissing))
	}
	if _, err = p.git(ctx, "-C", dir, "reset", "--hard", "--quiet", remoteRef); err != nil {
		return p.gitFailure(base, err, false)
	}
	head, err := p.git(ctx, "-C", dir, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) == "" {
		return p.fail(base, storage.StoragePrerequisiteMissing, storage.FailurePrerequisite, fmt.Errorf("%w: GitHub Wiki must be enabled and initialized with its first page", storage.ErrPrerequisiteMissing))
	}
	head = strings.TrimSpace(head)
	if head != strings.TrimSpace(remoteHead) {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, errors.New("local Wiki checkout does not match observed remote HEAD"))
	}
	base.RemoteRev = "git:" + head
	base.ExpectedRev = observed
	base.RemoteID = filepath.ToSlash(path)
	base.RemoteURL = p.options.WebURL + "/" + strings.TrimSuffix(filepath.ToSlash(path), ".md")
	pagePath := filepath.Join(dir, filepath.FromSlash(path))
	remote, readErr := os.ReadFile(pagePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, readErr)
	}
	if readErr == nil {
		base.RemoteContent = string(remote)
		base.EffectiveRemoteHash = hash(string(remote))
		if !hasExactFrontmatterMarker(string(remote), diary.Key()) && (mode == "" || mode == "status") {
			base.UnverifiedIdentity = true
			return p.fail(base, storage.StorageSyncConflict, storage.FailureConflict, fmt.Errorf("%w: deterministic Wiki path does not carry the expected Syntroph marker", storage.ErrConflict))
		}
	}
	if observed != "" && observed != base.RemoteRev {
		return p.fail(base, storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict)
	}
	if mode == storage.KeepRemote {
		if readErr != nil {
			return p.fail(base, storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict)
		}
		base.State = storage.Mirrored
		return base
	}
	if readErr == nil && string(remote) == expected {
		base.State = storage.Mirrored
		return base
	}
	if readErr == nil && mode != storage.KeepLocal {
		return p.fail(base, storage.StorageSyncConflict, storage.FailureConflict, storage.ErrConflict)
	}
	if mode == "status" || mode == storage.KeepLocal && readErr != nil {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, errors.New("Wiki page not found"))
	}
	author, err := p.author(ctx)
	if err != nil {
		return p.fail(base, storage.StoragePrerequisiteMissing, storage.FailurePrerequisite, err)
	}
	if err := os.MkdirAll(filepath.Dir(pagePath), 0o755); err != nil {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, err)
	}
	if err := os.WriteFile(pagePath, []byte(expected), 0o644); err != nil {
		return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, err)
	}
	if _, err = p.git(ctx, "-C", dir, "add", "--", filepath.ToSlash(path)); err != nil {
		return p.gitFailure(base, err, false)
	}
	message := "syntroph: mirror session " + diary.SessionID
	if _, err = p.git(ctx, "-C", dir, "-c", "user.name="+author.Name, "-c", "user.email="+author.Email, "commit", "--quiet", "--no-gpg-sign", "-m", message); err != nil {
		return p.gitFailure(base, err, false)
	}
	if p.beforePush != nil {
		p.beforePush()
	}
	if _, err = p.git(ctx, "-C", dir, "push", "--porcelain", "origin", "HEAD:refs/heads/"+branch); err != nil {
		if isNonFastForward(err) {
			return p.refreshConflict(ctx, base, dir, branch, path)
		}
		return p.gitFailure(base, err, false)
	}
	newHead, err := p.git(ctx, "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return p.gitFailure(base, err, false)
	}
	base.State = storage.Mirrored
	base.RemoteRev = "git:" + strings.TrimSpace(newHead)
	base.EffectiveRemoteHash = hash(expected)
	base.RemoteContent = ""
	return base
}

func (p *Provider) author(ctx context.Context) (Author, error) {
	a := Author{Name: strings.TrimSpace(p.options.Author.Name), Email: strings.TrimSpace(p.options.Author.Email)}
	if a.Name != "" && a.Email != "" {
		return a, nil
	}
	if strings.TrimSpace(p.options.RepositoryRoot) == "" {
		return Author{}, fmt.Errorf("%w: configure storage.wiki.commit_author or Git user.name and user.email", storage.ErrPrerequisiteMissing)
	}
	name, nameErr := p.git(ctx, "-C", p.options.RepositoryRoot, "config", "--get", "user.name")
	email, emailErr := p.git(ctx, "-C", p.options.RepositoryRoot, "config", "--get", "user.email")
	if nameErr != nil || emailErr != nil || strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		return Author{}, fmt.Errorf("%w: configure storage.wiki.commit_author or Git user.name and user.email", storage.ErrPrerequisiteMissing)
	}
	return Author{Name: strings.TrimSpace(name), Email: strings.TrimSpace(email)}, nil
}

func (p *Provider) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, p.options.GitExecutable, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", &gitError{args: args, output: string(out), cause: err}
	}
	return string(out), nil
}

type gitError struct {
	args   []string
	output string
	cause  error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.cause, strings.TrimSpace(e.output))
}
func (e *gitError) Unwrap() error { return e.cause }

func (p *Provider) gitFailure(base storage.MirrorResult, err error, clone bool) storage.MirrorResult {
	message := strings.ToLower(err.Error())
	if isNonFastForward(err) {
		return p.fail(base, storage.StorageSyncConflict, storage.FailureConflict, fmt.Errorf("%w: remote Wiki changed after observed HEAD", storage.ErrConflict))
	}
	if clone || strings.Contains(message, "authentication failed") || strings.Contains(message, "permission denied") || strings.Contains(message, "repository not found") || strings.Contains(message, "could not read username") || strings.Contains(message, "does not appear to be a git repository") {
		return p.fail(base, storage.StoragePrerequisiteMissing, storage.FailurePrerequisite, fmt.Errorf("%w: enable the GitHub Wiki, create its first page, and verify Git credential-helper or SSH access: %v", storage.ErrPrerequisiteMissing, err))
	}
	return p.fail(base, storage.StorageSyncPending, storage.FailureTransient, err)
}

func isNonFastForward(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "non-fast-forward") || strings.Contains(message, "fetch first") || strings.Contains(message, "[rejected]")
}

func (p *Provider) refreshConflict(ctx context.Context, base storage.MirrorResult, dir, branch, path string) storage.MirrorResult {
	if _, err := p.git(ctx, "-C", dir, "fetch", "--quiet", "--no-tags", "origin", branch); err != nil {
		return p.fail(base, storage.StorageSyncConflict, storage.FailureConflict, fmt.Errorf("%w: remote Wiki changed after observed HEAD", storage.ErrConflict))
	}
	if head, err := p.git(ctx, "-C", dir, "rev-parse", "FETCH_HEAD"); err == nil {
		base.RemoteRev = "git:" + strings.TrimSpace(head)
	}
	if content, err := p.git(ctx, "-C", dir, "show", "FETCH_HEAD:"+filepath.ToSlash(path)); err == nil {
		base.RemoteContent = content
		base.EffectiveRemoteHash = hash(content)
		base.UnverifiedIdentity = !hasExactFrontmatterMarker(content, base.Key)
	}
	return p.fail(base, storage.StorageSyncConflict, storage.FailureConflict, fmt.Errorf("%w: remote Wiki changed after observed HEAD", storage.ErrConflict))
}

func (p *Provider) base(d storage.SessionDiary) storage.MirrorResult {
	return storage.MirrorResult{Backend: storage.BackendWiki, Provider: ProviderID, Key: d.Key(), LocalHash: hash(d.Content)}
}
func (p *Provider) failure(d storage.SessionDiary, state storage.MirrorState, class storage.FailureClass, err error) storage.MirrorResult {
	return p.fail(p.base(d), state, class, err)
}
func (p *Provider) fail(r storage.MirrorResult, state storage.MirrorState, class storage.FailureClass, err error) storage.MirrorResult {
	r.State, r.FailureClass, r.Cause = state, class, err
	return r
}

var createdAtPattern = regexp.MustCompile(`(?m)^created_at:\s*([^\r\n]+)\s*$`)

func hasExactFrontmatterMarker(content, key string) bool {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return false
	}
	end := strings.Index(content[len("---\n"):], "\n---")
	if end < 0 {
		return false
	}
	frontmatter := content[len("---\n") : len("---\n")+end]
	wantKey := "idempotency_key: " + key
	wantMarker := "syntroph_remote_binding: " + key
	foundKey, foundMarker := false, false
	for _, line := range strings.Split(frontmatter, "\n") {
		switch strings.TrimSpace(line) {
		case wantKey:
			foundKey = true
		case wantMarker:
			foundMarker = true
		}
	}
	return foundKey && foundMarker
}

func wikiPage(d storage.SessionDiary) (string, string, error) {
	match := createdAtPattern.FindStringSubmatch(d.Content)
	if len(match) != 2 {
		return "", "", errors.New("Session Diary frontmatter requires created_at")
	}
	created, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(match[1]))
	if err != nil {
		return "", "", fmt.Errorf("parse Session Diary created_at: %w", err)
	}
	if len(d.ArtifactHash) < 12 {
		return "", "", errors.New("Session Diary artifact hash requires at least 12 characters")
	}
	key := d.Key()
	marker := "syntroph_remote_binding: " + key
	content := d.Content
	if !strings.Contains(content, "idempotency_key: "+key) {
		return "", "", errors.New("Session Diary frontmatter idempotency key does not match diary")
	}
	if !strings.Contains(content, marker) {
		firstBreak := strings.IndexByte(content, '\n')
		if firstBreak < 0 || strings.TrimSpace(content[:firstBreak]) != "---" {
			return "", "", errors.New("Session Diary requires YAML frontmatter")
		}
		content = content[:firstBreak+1] + marker + "\n" + content[firstBreak+1:]
	}
	name := d.SessionID + "-" + d.ArtifactHash[:12] + ".md"
	return filepath.ToSlash(filepath.Join("Sessions", created.UTC().Format("2006"), created.UTC().Format("01"), name)), content, nil
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
