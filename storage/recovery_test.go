package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBindingStoreIsAtomicPrivateAndContainsOnlyOperationalMetadata(t *testing.T) {
	root := t.TempDir()
	store, err := NewBindingStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	binding := RemoteBinding{
		IdempotencyKey: key, Backend: BackendIssues, Provider: "fake-issues",
		RemoteID: "42", URL: "https://example.test/issues/42",
		RemoteRevision: RemoteRevision("issue:42:2026-08-31T00:00:00Z:" + strings.Repeat("b", 64)),
		LocalHash:      strings.Repeat("c", 64), EffectiveRemoteHash: strings.Repeat("d", 64),
		UpdatedAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Load(context.Background(), key)
	if err != nil || !ok || got != binding {
		t.Fatalf("load binding: got=%+v ok=%v err=%v", got, ok, err)
	}
	path, _ := store.Path(key)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("binding mode = %o, want 600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "remote_content") || strings.Contains(string(data), "Session Diary") {
		t.Fatalf("binding leaked remote content: %s", data)
	}
}

func TestConflictSnapshotIsPrivateAndSeparatedFromBinding(t *testing.T) {
	store, err := NewConflictStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Write(context.Background(), strings.Repeat("e", 64), "issue:7:2026-08-31T00:00:00Z:"+strings.Repeat("f", 64), "human remote edit")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %o, want 600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "human remote edit" {
		t.Fatalf("snapshot = %q", data)
	}
}

func TestMirrorLockRejectsConcurrencyAndRequiresVerifiedExplicitCleanup(t *testing.T) {
	locks, err := NewMirrorLocks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("1", 64)
	first, err := locks.Acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locks.Acquire(context.Background(), key); !errors.Is(err, ErrMirrorInProgress) {
		t.Fatalf("second acquire = %v, want already in progress", err)
	}
	if err := locks.ClearOrphan(context.Background(), key); err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("live lock cleanup = %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	orphan, err := locks.Acquire(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	locks.processAlive = func(int) bool { return false }
	if err := locks.ClearOrphan(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := locks.Owner(context.Background(), key); err != nil || ok {
		t.Fatalf("orphan remains: ok=%v err=%v", ok, err)
	}
	// The original owner no longer owns a path after explicit recovery.
	if err := orphan.Release(); err != nil {
		t.Fatal(err)
	}
}

type controlledRemote struct {
	mu        sync.Mutex
	result    MirrorResult
	calls     int
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (r *controlledRemote) Mirror(context.Context, SessionDiary) MirrorResult {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if r.started != nil {
		r.startOnce.Do(func() { close(r.started) })
		<-r.release
	}
	return r.result
}
func (r *controlledRemote) Status(context.Context, SessionDiary) MirrorResult { return r.result }
func (r *controlledRemote) Resolve(context.Context, SessionDiary, Resolution, string) MirrorResult {
	return r.result
}

func TestManagedMirrorPersistsConflictEvidenceAndPreventsDuplicateEffect(t *testing.T) {
	diary := diary()
	localHash := contentHash(diary.Content)
	remoteContent := "human edit"
	revision := "issue:9:2026-08-31T00:00:00Z:" + contentHash(remoteContent)
	remote := &controlledRemote{result: MirrorResult{
		State: StorageSyncConflict, Backend: BackendIssues, Key: diary.Key(),
		RemoteID: "9", RemoteURL: "https://example.test/issues/9", RemoteRev: revision,
		LocalHash: localHash, EffectiveRemoteHash: contentHash(remoteContent), RemoteContent: remoteContent,
		FailureClass: FailureConflict, Cause: ErrConflict,
	}, started: make(chan struct{}), release: make(chan struct{})}
	managed, err := NewManagedMirror(t.TempDir(), "fake-issues", remote)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan MirrorResult, 1)
	go func() { results <- managed.Mirror(context.Background(), diary) }()
	<-remote.started
	concurrent := managed.Mirror(context.Background(), diary)
	if !concurrent.AlreadyInProgress || concurrent.FailureClass != FailureAlreadyInProgress {
		t.Fatalf("concurrent result = %+v", concurrent)
	}
	close(remote.release)
	conflict := <-results
	if conflict.State != StorageSyncConflict || conflict.ConflictSnapshot == "" {
		t.Fatalf("conflict result = %+v", conflict)
	}
	if info, err := os.Stat(conflict.ConflictSnapshot); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("conflict snapshot: info=%v err=%v", info, err)
	}
	binding, ok, err := managed.Bindings.Load(context.Background(), diary.Key())
	if err != nil || !ok || binding.RemoteRevision != RemoteRevision(revision) || binding.EffectiveRemoteHash != contentHash(remoteContent) {
		t.Fatalf("conflict binding: %+v ok=%v err=%v", binding, ok, err)
	}
	remote.mu.Lock()
	calls := remote.calls
	remote.mu.Unlock()
	if calls != 1 {
		t.Fatalf("remote effects = %d, want 1", calls)
	}
}

func TestRemoteRevisionValidation(t *testing.T) {
	valid := []RemoteRevision{
		RemoteRevision("issue:42:2026-08-31T00:00:00Z:" + strings.Repeat("a", 64)),
		RemoteRevision("comment:7:2026-08-31T00:00:00Z:" + strings.Repeat("b", 64)),
		RemoteRevision("git:abcdef1"),
	}
	for _, revision := range valid {
		if err := revision.Validate(); err != nil {
			t.Errorf("valid revision %q: %v", revision, err)
		}
	}
	for _, revision := range []RemoteRevision{"r1", "issue:missing", "git:not-a-sha"} {
		if err := revision.Validate(); err == nil {
			t.Errorf("invalid revision %q accepted", revision)
		}
	}
}

func TestAtomicWriteNeverLeavesTemporaryProjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binding.json")
	if err := atomicWrite(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "binding.json" {
		t.Fatalf("temporary projection leaked: %+v", entries)
	}
}
