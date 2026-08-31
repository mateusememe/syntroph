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
