package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/adapters/githubwiki"
	"github.com/mateusememe/syntroph/adapters/storageadapter"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

type countingStorageProvider struct {
	mirrorCalls      int
	resolveCalls     int
	resolveChoice    storage.Resolution
	resolveRevision  string
	mirrorResult     storage.MirrorResult
	resolutionResult storage.MirrorResult
}

func (p *countingStorageProvider) Mirror(_ context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.mirrorCalls++
	if p.mirrorResult.State != "" {
		result := p.mirrorResult
		result.Key = diary.Key()
		return result
	}
	return storage.MirrorResult{State: storage.Mirrored, Backend: storage.BackendIssues, Provider: "test-provider", Key: diary.Key()}
}

func (p *countingStorageProvider) Resolve(_ context.Context, _ storage.SessionDiary, choice storage.Resolution, revision string) storage.MirrorResult {
	p.resolveCalls++
	p.resolveChoice, p.resolveRevision = choice, revision
	if p.resolutionResult.State != "" {
		return p.resolutionResult
	}
	return storage.MirrorResult{State: storage.Mirrored}
}

func TestSyncRecoveryIsReadOnlyAndExplainsExplicitRetry(t *testing.T) {
	dir := t.TempDir()
	journal, err := core.NewSagaJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(core.SessionDiary{GraphState: core.GraphResolutionPending})
	if err := journal.AppendEvent(context.Background(), core.Event{EventID: "e", Type: "session.closed", OccurredAt: time.Now().UTC(), RepositoryID: "r", SagaID: "s", CorrelationID: "s", SchemaVersion: 1, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"sync", "--journal=" + dir, "recovery"}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no external effects") || !strings.Contains(out.String(), "sync retry --graph") {
		t.Fatalf("unexpected recovery view: %s", out.String())
	}
}

func TestSyncRetryRequiresExactlyOneScope(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"sync", "retry"}, &out, os.Stderr); err == nil {
		t.Fatal("expected scope validation")
	}
	if err := run([]string{"sync", "retry", "--graph"}, &out, os.Stderr); err != nil || !strings.Contains(out.String(), "graph synchronization") {
		t.Fatalf("retry: %v %s", err, out.String())
	}
}

func TestSyncResolveShowsDiffAndRequiresExplicitChoice(t *testing.T) {
	dir := t.TempDir()
	local, remote := filepath.Join(dir, "local"), filepath.Join(dir, "remote")
	if err := os.WriteFile(local, []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remote, []byte("remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"sync", "resolve", "conflict-1", "--local-file", local, "--remote-file", remote, "--keep-remote"}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "local") || !strings.Contains(out.String(), "remote") || !strings.Contains(out.String(), "keep-remote") {
		t.Fatalf("missing diff/resolution: %s", out.String())
	}
	b, _ := os.ReadFile(remote)
	if string(b) != "remote\n" {
		t.Fatal("keep-remote changed remote content")
	}
}

func TestSyncRecoveryShowsRemoteEvidenceAndClearsOnlyVerifiedOrphanLock(t *testing.T) {
	root := t.TempDir()
	journalDir := filepath.Join(root, "journal")
	journal, err := core.NewSagaJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	payload, _ := json.Marshal(core.StorageResult{
		State: "StorageSyncConflict", Backend: "issues", Provider: "fake-issues", Key: key,
		RemoteID: "42", RemoteURL: "https://example.test/issues/42",
		RemoteRev:    "issue:42:2026-08-31T00:00:00Z:" + strings.Repeat("b", 64),
		FailureClass: "conflict", ConflictSnapshot: filepath.Join(root, "storage", "conflicts", key, "remote.md"), Error: "remote diverged",
	})
	if err := journal.AppendEvent(context.Background(), core.Event{EventID: "s:storage", Type: "storage.sync.conflict", OccurredAt: time.Now().UTC(), RepositoryID: "r", SagaID: "s", CorrelationID: "s", SchemaVersion: 1, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"sync", "--journal=" + journalDir, "recovery"}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fake-issues", "issues/42", "issue:42", "conflict", "remote.md", "sync resolve s"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("recovery output missing %q: %s", want, out.String())
		}
	}

	locks, err := storage.NewMirrorLocks(filepath.Join(root, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	lockDir := filepath.Join(root, "storage", "locks")
	owner, _ := json.Marshal(storage.MirrorLockOwner{PID: 999999, StartedAt: time.Now().Add(-time.Hour).UTC(), OwnerID: "orphan-owner"})
	if err := os.WriteFile(filepath.Join(lockDir, key+".lock"), owner, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"sync", "--journal=" + journalDir, "recovery", "--clear-lock", key, "--root", root}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No external effect") {
		t.Fatalf("orphan cleanup output: %s", out.String())
	}
	if _, ok, err := locks.Owner(context.Background(), key); err != nil || ok {
		t.Fatalf("orphan lock remains: ok=%v err=%v", ok, err)
	}
}

func TestDoctorStorageReportsPrerequisitesWithoutProviderEffects(t *testing.T) {
	repositoryRoot, syntrophRoot := configuredTestRepository(t, "storage:\n  backend: issues\n  provider: github-rest\n")
	t.Setenv("GITHUB_TOKEN", "must-not-be-used")
	t.Setenv("SYNTROPH_GITHUB_TOKEN", "")
	provider := &countingStorageProvider{}
	providerBuilds := 0
	previous := storageProviderBuilder
	storageProviderBuilder = func(config.ResolvedStorage) storageadapter.Provider {
		providerBuilds++
		return provider
	}
	t.Cleanup(func() { storageProviderBuilder = previous })

	var out bytes.Buffer
	err := run([]string{"doctor", "storage", "--root", syntrophRoot}, &out, os.Stderr)
	if err == nil {
		t.Fatal("missing explicit token should fail doctor")
	}
	for _, want := range []string{"github-rest", "mateusememe/syntroph", "SYNTROPH_GITHUB_TOKEN is not set", "syntroph sync recovery", "syntroph sync retry --storage", "no remote writes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output missing %q: %s", want, out.String())
		}
	}
	if providerBuilds != 0 || provider.mirrorCalls != 0 {
		t.Fatalf("doctor constructed %d providers and performed %d provider effects", providerBuilds, provider.mirrorCalls)
	}
	entries, err := os.ReadDir(syntrophRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("doctor mutated repository state: root=%s entries=%v repository=%s", syntrophRoot, entries, repositoryRoot)
	}
}

func TestJSONPrototypeDoesNotConfigureRuntimeStorage(t *testing.T) {
	_, root := configuredTestRepository(t, "storage:\n  backend: issues\n  provider: github-rest\n")
	if err := os.Remove(filepath.Join(root, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"storage":{"backend":"issues","provider":"github-rest"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	providerBuilds := 0
	previous := storageProviderBuilder
	storageProviderBuilder = func(config.ResolvedStorage) storageadapter.Provider {
		providerBuilds++
		return &countingStorageProvider{}
	}
	t.Cleanup(func() { storageProviderBuilder = previous })
	var out bytes.Buffer
	if err := run([]string{"doctor", "storage", "--root", root}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if providerBuilds != 0 || !strings.Contains(out.String(), "disabled") {
		t.Fatalf("config.json affected runtime: builds=%d output=%s", providerBuilds, out.String())
	}
}

func TestDoctorStorageReportsMCPAndWikiProcessGuidance(t *testing.T) {
	t.Run("missing MCP command", func(t *testing.T) {
		_, root := configuredTestRepository(t, "storage:\n  backend: issues\n  provider: github-mcp\n")
		var out bytes.Buffer
		if err := run([]string{"doctor", "storage", "--root", root}, &out, os.Stderr); err == nil {
			t.Fatal("missing MCP process should fail doctor")
		}
		if !strings.Contains(out.String(), "storage.mcp.command is required") || !strings.Contains(out.String(), "owns its own authentication") {
			t.Fatalf("MCP guidance missing: %s", out.String())
		}
	})
	t.Run("Wiki initialization", func(t *testing.T) {
		_, root := configuredTestRepository(t, "storage:\n  backend: wiki\n  provider: github-wiki-git\n  wiki:\n    commit_author:\n      name: Syntroph\n      email: syntroph@example.com\n")
		var out bytes.Buffer
		if err := run([]string{"doctor", "storage", "--root", root}, &out, os.Stderr); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"github-wiki-git", "Wiki initialization", "at least one page", "no remote command", "configuration was not changed"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("Wiki doctor output missing %q: %s", want, out.String())
			}
		}
	})
}

func TestSessionCloseInvalidStorageConfigKeepsLocalDiaryAndPerformsZeroProviderWrites(t *testing.T) {
	_, root := configuredTestRepository(t, "storage:\n  backend: wiki\n  provider: github-rest\n")
	providerBuilds := 0
	previous := storageProviderBuilder
	storageProviderBuilder = func(config.ResolvedStorage) storageadapter.Provider {
		providerBuilds++
		return &countingStorageProvider{}
	}
	t.Cleanup(func() { storageProviderBuilder = previous })

	var out bytes.Buffer
	if err := run([]string{"session", "close", "--summary", "local knowledge survives", "--repository", "github.com/mateusememe/syntroph", "--commit", "abc123", "--author", "matt", "--root", root}, &out, os.Stderr); err != nil {
		t.Fatalf("invalid remote configuration failed local close: %v", err)
	}
	if providerBuilds != 0 {
		t.Fatalf("invalid configuration constructed %d providers", providerBuilds)
	}
	diaries, err := filepath.Glob(filepath.Join(root, "memory", "*", "*", "*.md"))
	if err != nil || len(diaries) != 1 {
		t.Fatalf("local diary not durable: %v err=%v", diaries, err)
	}
	journal, err := core.NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := core.InspectRecovery(context.Background(), journal)
	if err != nil || len(items) != 1 || items[0].State != "StoragePrerequisiteMissing" || items[0].Provider != "" {
		t.Fatalf("invalid configuration was not journaled safely: %+v err=%v", items, err)
	}
}

func TestSupportedStorageConfigurationReachesProviderNeutralMirrorSeam(t *testing.T) {
	_, root := configuredTestRepository(t, "storage:\n  backend: issues\n  provider: github-rest\n")
	t.Setenv("SYNTROPH_GITHUB_TOKEN", "injected-for-test")
	provider := &countingStorageProvider{}
	previous := storageProviderBuilder
	storageProviderBuilder = func(resolved config.ResolvedStorage) storageadapter.Provider {
		if resolved.Repository != "mateusememe/syntroph" || resolved.Provider != config.ProviderGitHubREST {
			t.Fatalf("unexpected resolved provider: %+v", resolved)
		}
		return provider
	}
	t.Cleanup(func() { storageProviderBuilder = previous })

	var out bytes.Buffer
	if err := run([]string{"session", "close", "--summary", "mirror seam", "--repository", "github.com/mateusememe/syntroph", "--commit", "def456", "--author", "matt", "--root", root}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if provider.mirrorCalls != 1 {
		t.Fatalf("supported configuration reached provider %d times", provider.mirrorCalls)
	}
}

func TestWikiConfigurationBuildsGitProviderWithoutMutatingDoctorState(t *testing.T) {
	_, root := configuredTestRepository(t, "storage:\n  backend: wiki\n  provider: github-wiki-git\n  wiki:\n    git_executable: git\n    commit_author:\n      name: Syntroph\n      email: syntroph@example.test\n")
	previous := wikiStorageProviderBuilder
	builds := 0
	wikiStorageProviderBuilder = func(resolved config.ResolvedStorage, gotRoot, repositoryRoot, origin string) storageadapter.Provider {
		builds++
		if resolved.Repository != "mateusememe/syntroph" || gotRoot != root || repositoryRoot != filepath.Dir(root) || origin != "git@github.com:mateusememe/syntroph.git" {
			t.Fatalf("unexpected Wiki builder inputs: resolved=%+v root=%s repository=%s origin=%s", resolved, gotRoot, repositoryRoot, origin)
		}
		return &countingStorageProvider{}
	}
	t.Cleanup(func() { wikiStorageProviderBuilder = previous })

	var out bytes.Buffer
	if err := run([]string{"doctor", "storage", "--root", root}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if builds != 1 || !strings.Contains(out.String(), "No authentication or remote write") {
		t.Fatalf("Wiki doctor builds=%d output=%s", builds, out.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("doctor mutated state: entries=%v err=%v", entries, err)
	}
}

func TestWikiRemoteURLPreservesOriginTransportAndUsesSafeCrossRepositoryDefault(t *testing.T) {
	for _, tc := range []struct {
		name, origin, want string
	}{
		{name: "SSH", origin: "git@github.com:acme/repo.git", want: "git@github.com:acme/repo.wiki.git"},
		{name: "HTTPS", origin: "https://github.com/acme/repo.git", want: "https://github.com/acme/repo.wiki.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := wikiRemoteURL(tc.origin, config.ResolvedStorage{Repository: "acme/repo"})
			if got != tc.want {
				t.Fatalf("Wiki URL = %q, want %q", got, tc.want)
			}
		})
	}
	for _, tc := range []struct{ origin, want string }{
		{origin: "git@github.com:acme/repo.git", want: "git@github.com:central/memory.wiki.git"},
		{origin: "ssh://git@github.com/acme/repo.git", want: "ssh://git@github.com/central/memory.wiki.git"},
		{origin: "https://github.com/acme/repo.git", want: "https://github.com/central/memory.wiki.git"},
	} {
		cross := wikiRemoteURL(tc.origin, config.ResolvedStorage{Repository: "central/memory", CrossRepository: true})
		if cross != tc.want {
			t.Errorf("cross-repository Wiki URL for %q = %q, want %q", tc.origin, cross, tc.want)
		}
	}
}

func TestWikiProviderWiresSessionCloseRetryAndResolveThroughConfiguredStorage(t *testing.T) {
	repositoryRoot, root := configuredTestRepository(t, "storage:\n  backend: wiki\n  provider: github-wiki-git\n  wiki:\n    git_executable: git\n    commit_author:\n      name: Syntroph\n      email: syntroph@example.test\n")
	missingRemote := filepath.Join(t.TempDir(), "missing.wiki.git")
	remote := missingRemote
	previous := wikiStorageProviderBuilder
	wikiStorageProviderBuilder = func(resolved config.ResolvedStorage, gotRoot, gotRepositoryRoot, _ string) storageadapter.Provider {
		provider, err := githubwiki.NewManaged(filepath.Join(gotRoot, "storage"), githubwiki.Options{
			Repository: resolved.Repository, RepositoryRoot: gotRepositoryRoot, RemoteURL: remote,
			WebURL: "https://github.com/" + resolved.Repository + "/wiki", GitExecutable: resolved.Wiki.GitExecutable,
			Author: githubwiki.Author{Name: resolved.Wiki.CommitAuthor.Name, Email: resolved.Wiki.CommitAuthor.Email},
		})
		if err != nil {
			t.Fatal(err)
		}
		return provider
	}
	t.Cleanup(func() { wikiStorageProviderBuilder = previous })

	var out bytes.Buffer
	if err := run([]string{"session", "close", "--summary", "Wiki CLI recovery", "--repository", "github.com/mateusememe/syntroph", "--commit", "wiki123", "--author", "matt", "--root", root}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	var closed struct {
		Diary core.SessionDiary `json:"diary"`
	}
	if err := json.Unmarshal(out.Bytes(), &closed); err != nil || closed.Diary.SessionID == "" {
		t.Fatalf("decode closed diary: %+v err=%v output=%s", closed, err, out.String())
	}
	journalDir := filepath.Join(root, "journal")
	journal, err := core.NewSagaJournal(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	items, err := core.InspectRecovery(context.Background(), journal)
	if err != nil || len(items) != 1 || items[0].State != "StoragePrerequisiteMissing" {
		t.Fatalf("missing Wiki recovery = %+v err=%v", items, err)
	}

	remote = initializedCLIWiki(t)
	out.Reset()
	if err := run([]string{"sync", "--journal=" + journalDir, "retry", "--storage"}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 saga(s)") {
		t.Fatalf("storage retry output: %s", out.String())
	}
	if items, err = core.InspectRecovery(context.Background(), journal); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.State, "Storage") {
			t.Fatalf("Wiki retry did not clear storage recovery: %+v", items)
		}
	}
	inspect := filepath.Join(t.TempDir(), "inspect")
	cliGit(t, "clone", "-q", remote, inspect)
	pages, err := filepath.Glob(filepath.Join(inspect, "Sessions", "*", "*", closed.Diary.SessionID+"-*.md"))
	if err != nil || len(pages) != 1 {
		t.Fatalf("mirrored Wiki page = %v err=%v", pages, err)
	}
	pageRelative, _ := filepath.Rel(inspect, pages[0])
	original, err := os.ReadFile(pages[0])
	if err != nil {
		t.Fatal(err)
	}
	editCLIWikiPage(t, remote, filepath.ToSlash(pageRelative), string(original)+"\nHuman remote edit.\n")
	revision := "git:" + strings.TrimSpace(cliGit(t, "--git-dir", remote, "rev-parse", "HEAD"))
	storagePort := configuredStorage(root, repositoryRoot)
	eventAware, ok := storagePort.(core.EventAwareStoragePort)
	if !ok {
		t.Fatal("configured Wiki storage does not expose event-aware mirroring")
	}
	conflict := eventAware.MirrorEvent(context.Background(), closed.Diary.SessionID+":storage-conflict-check", closed.Diary)
	if conflict.State != "StorageSyncConflict" || conflict.RemoteRev != revision || conflict.ConflictSnapshot == "" {
		t.Fatalf("Wiki conflict was not persisted for explicit resolution: %+v", conflict)
	}
	conflictPayload, err := json.Marshal(conflict)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.AppendEvent(context.Background(), core.Event{
		EventID: closed.Diary.SessionID + ":storage-conflict-check", Type: "storage.sync.conflict",
		OccurredAt: time.Now().UTC(), RepositoryID: closed.Diary.RepositoryID, SagaID: closed.Diary.SessionID,
		CorrelationID: closed.Diary.SessionID, CausationID: closed.Diary.SessionID, SchemaVersion: 1, Payload: conflictPayload,
	}); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := run([]string{"sync", "resolve", closed.Diary.SessionID, "--keep-local", "--revision", revision, "--root", root}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "keep-local") {
		t.Fatalf("storage resolve output: %s", out.String())
	}
	cliGit(t, "-C", inspect, "pull", "-q", "--ff-only")
	resolved, err := os.ReadFile(pages[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resolved), "Human remote edit") || !strings.Contains(string(resolved), "Wiki CLI recovery") {
		t.Fatalf("resolved Wiki content:\n%s", resolved)
	}
	if repositoryRoot != filepath.Dir(root) {
		t.Fatalf("test repository root mismatch: %s root=%s", repositoryRoot, root)
	}
}

func initializedCLIWiki(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "repo.wiki.git")
	cliGit(t, "init", "-q", "--bare", remote)
	seed := filepath.Join(base, "seed")
	cliGit(t, "clone", "-q", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "Home.md"), []byte("# Home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, "-C", seed, "add", "Home.md")
	cliGit(t, "-C", seed, "-c", "user.name=Seed", "-c", "user.email=seed@example.test", "commit", "-q", "-m", "initialize wiki")
	cliGit(t, "-C", seed, "push", "-q", "origin", "HEAD")
	return remote
}

func editCLIWikiPage(t *testing.T, remote, page, content string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "edit")
	cliGit(t, "clone", "-q", remote, clone)
	path := filepath.Join(clone, filepath.FromSlash(page))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, "-C", clone, "add", "--", page)
	cliGit(t, "-C", clone, "-c", "user.name=Human", "-c", "user.email=human@example.test", "commit", "-q", "-m", "human edit")
	cliGit(t, "-C", clone, "push", "-q", "origin", "HEAD")
}

func cliGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func TestStorageRetryReplaysPrerequisiteFailureAndJournalsBindingResult(t *testing.T) {
	_, root := configuredTestRepository(t, "storage:\n  backend: issues\n  provider: github-rest\n")
	t.Setenv("SYNTROPH_GITHUB_TOKEN", "injected-for-test")
	provider := &countingStorageProvider{mirrorResult: storage.MirrorResult{
		State: storage.Mirrored, Backend: storage.BackendIssues, Provider: "github-rest",
		RemoteID: "41", RemoteURL: "https://github.test/issues/41",
		RemoteRev: "issue:41:2026-08-31T12:00:00Z:" + strings.Repeat("b", 64),
		LocalHash: strings.Repeat("a", 64), EffectiveRemoteHash: strings.Repeat("a", 64),
	}}
	previous := storageProviderBuilder
	storageProviderBuilder = func(config.ResolvedStorage) storageadapter.Provider { return provider }
	t.Cleanup(func() { storageProviderBuilder = previous })

	diary := recoveryTestDiary("retry-prerequisite")
	journal, err := core.NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	appendSessionAndStorageResult(t, journal, diary, core.StorageResult{State: "StoragePrerequisiteMissing", Backend: "issues", Provider: "github-rest", Key: diary.IdempotencyKey, FailureClass: "prerequisite_missing", Error: "token was missing"})

	var out bytes.Buffer
	if err := run([]string{"sync", "--journal=" + filepath.Join(root, "journal"), "retry", "--storage"}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if provider.mirrorCalls != 1 || !strings.Contains(out.String(), "1 saga") {
		t.Fatalf("calls=%d output=%s", provider.mirrorCalls, out.String())
	}
	items, err := core.InspectRecovery(context.Background(), journal)
	if err != nil || len(items) != 0 {
		t.Fatalf("retry did not resolve recovery: items=%+v err=%v", items, err)
	}
	records, err := journal.ReadSaga(context.Background(), diary.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	foundBindingResult := false
	for _, record := range records {
		if record.Event != nil && record.Event.Type == "storage.sync.succeeded" && strings.Contains(string(record.Event.Payload), `"remote_id":"41"`) {
			foundBindingResult = true
		}
	}
	if !foundBindingResult {
		t.Fatal("retry journal omitted the provider binding result")
	}
}

func TestConfiguredStorageResolveDisplaysPersistedDiffBeforeProviderEffect(t *testing.T) {
	_, root := configuredTestRepository(t, "storage:\n  backend: issues\n  provider: github-rest\n")
	t.Setenv("SYNTROPH_GITHUB_TOKEN", "injected-for-test")
	revision := "issue:42:2026-08-31T12:00:00Z:" + strings.Repeat("c", 64)
	provider := &countingStorageProvider{resolutionResult: storage.MirrorResult{
		State: storage.Mirrored, Backend: storage.BackendIssues, Provider: "github-rest",
		RemoteID: "42", RemoteURL: "https://github.test/issues/42", RemoteRev: "comment:7:2026-08-31T12:01:00Z:" + strings.Repeat("d", 64),
		LocalHash: strings.Repeat("a", 64), EffectiveRemoteHash: strings.Repeat("a", 64),
	}}
	previous := storageProviderBuilder
	storageProviderBuilder = func(config.ResolvedStorage) storageadapter.Provider { return provider }
	t.Cleanup(func() { storageProviderBuilder = previous })

	diary := recoveryTestDiary("resolve-conflict")
	journal, err := core.NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(root, "storage", "conflicts", diary.IdempotencyKey, "issue.remote.md")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, []byte("human remote edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendSessionAndStorageResult(t, journal, diary, core.StorageResult{
		State: "StorageSyncConflict", Backend: "issues", Provider: "github-rest", Key: diary.IdempotencyKey,
		RemoteID: "42", RemoteURL: "https://github.test/issues/42", RemoteRev: revision,
		FailureClass: "conflict", ConflictSnapshot: snapshot, Error: "remote diverged",
	})

	var out bytes.Buffer
	if err := run([]string{"sync", "--journal=" + filepath.Join(root, "journal"), "resolve", diary.SessionID, "--keep-local", "--root", root}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if provider.resolveCalls != 1 || provider.resolveChoice != storage.KeepLocal || provider.resolveRevision != revision {
		t.Fatalf("resolve calls=%d choice=%s revision=%s", provider.resolveCalls, provider.resolveChoice, provider.resolveRevision)
	}
	for _, want := range []string{"--- local", diary.Summary, "--- remote", "human remote edit", "keep-local"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("resolve output missing %q: %s", want, out.String())
		}
	}
	items, err := core.InspectRecovery(context.Background(), journal)
	if err != nil || len(items) != 0 {
		t.Fatalf("resolution remains pending: items=%+v err=%v", items, err)
	}
}

func recoveryTestDiary(sessionID string) core.SessionDiary {
	return core.SessionDiary{
		SessionID: sessionID, IdempotencyKey: strings.Repeat("e", 64), RepositoryID: "github.com/mateusememe/syntroph",
		CommitSHA: "abc123", ArtifactHash: strings.Repeat("f", 64), Author: "matt", CreatedAt: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC), Title: "Session diary", Summary: "durable local knowledge",
	}
}

func appendSessionAndStorageResult(t *testing.T, journal *core.SagaJournal, diary core.SessionDiary, result core.StorageResult) {
	t.Helper()
	payload, _ := json.Marshal(diary)
	if err := journal.AppendEvent(context.Background(), core.Event{EventID: diary.SessionID, Type: "session.closed", OccurredAt: diary.CreatedAt, RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, SchemaVersion: 1, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(result)
	if err := journal.AppendEvent(context.Background(), core.Event{EventID: diary.SessionID + ":storage", Type: storageResultEventType(result.State), OccurredAt: diary.CreatedAt, RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, CausationID: diary.SessionID, SchemaVersion: 1, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func configuredTestRepository(t *testing.T, contents string) (string, string) {
	t.Helper()
	repositoryRoot := t.TempDir()
	for _, args := range [][]string{{"init", "-q", repositoryRoot}, {"-C", repositoryRoot, "remote", "add", "origin", "git@github.com:mateusememe/syntroph.git"}} {
		cmd := exec.Command("git", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	syntrophRoot := filepath.Join(repositoryRoot, ".syntroph")
	if err := os.MkdirAll(syntrophRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(syntrophRoot, "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return repositoryRoot, syntrophRoot
}
