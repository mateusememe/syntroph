package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

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
