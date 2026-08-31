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

	"github.com/mateusememe/syntroph/adapters/storageadapter"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

type countingStorageProvider struct{ mirrorCalls int }

func (p *countingStorageProvider) Mirror(_ context.Context, diary storage.SessionDiary) storage.MirrorResult {
	p.mirrorCalls++
	return storage.MirrorResult{State: storage.Mirrored, Backend: storage.BackendIssues, Provider: "test-provider", Key: diary.Key()}
}

func (p *countingStorageProvider) Resolve(context.Context, storage.SessionDiary, storage.Resolution, string) storage.MirrorResult {
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
