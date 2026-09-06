package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/adapters/storageadapter"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

type readyGraph struct{}

func (readyGraph) Resolve(_ context.Context, req core.GraphResolveRequest) (core.GraphResolution, error) {
	return core.GraphResolution{References: req.References, GraphSnapshotID: "snapshot-1"}, nil
}

type recoveryClient struct {
	mu   sync.Mutex
	docs map[string]storage.RemoteDocument
	mode string
	puts int
}

func (c *recoveryClient) Get(_ context.Context, backend storage.Backend, repository, key string) (storage.RemoteDocument, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.mode {
	case "transient":
		return storage.RemoteDocument{}, errors.New("temporary outage")
	case "prerequisite":
		return storage.RemoteDocument{}, storage.ErrPrerequisiteMissing
	case "conflict":
		content := "human remote edit"
		return storage.RemoteDocument{ID: "77", URL: "https://example.test/issues/77", Revision: issueRevision("77", content), Content: content}, nil
	}
	doc, ok := c.docs[string(backend)+":"+repository+":"+key]
	if !ok {
		return storage.RemoteDocument{NotFound: true}, nil
	}
	return doc, nil
}

func (c *recoveryClient) Put(_ context.Context, backend storage.Backend, repository, key, content, _ string) (storage.RemoteDocument, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	doc := storage.RemoteDocument{ID: "42", URL: "https://example.test/issues/42", Revision: issueRevision("42", content), Content: content}
	c.docs[string(backend)+":"+repository+":"+key] = doc
	return doc, nil
}

func issueRevision(id, content string) string {
	sum := sha256.Sum256([]byte(content))
	return "issue:" + id + ":2026-08-31T00:00:00Z:" + hex.EncodeToString(sum[:])
}

func newRecoveryCloser(t *testing.T, root string, client *recoveryClient) (core.SessionCloser, *storage.ManagedMirror, *core.SagaJournal) {
	t.Helper()
	journal, err := core.NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := core.NewEventBus(journal)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := storage.NewMirror(storage.BackendIssues, "mateusememe/syntroph", client)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := storage.NewManagedMirror(filepath.Join(root, "storage"), "fake-issues", remote)
	if err != nil {
		t.Fatal(err)
	}
	closer := core.SessionCloser{
		Memory: core.LocalMemoryStore{Root: filepath.Join(root, "memory")},
		Graph:  readyGraph{}, Bus: bus, Storage: storageadapter.Adapter{Provider: managed},
	}
	return closer, managed, journal
}

func closeRecoverySession(t *testing.T, closer core.SessionCloser) core.SessionDiary {
	t.Helper()
	diary, _, err := closer.Close(context.Background(), core.SessionCloseRequest{
		RepositoryID: "github.com/mateusememe/syntroph", CommitSHA: "abc123", Author: "mateusememe",
		Now: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), Format: core.ArtifactJSON,
		Artifact: []byte(`{"title":"Recovery","summary":"durable remote recovery"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return diary
}

func TestSessionClosePersistsRemoteBindingJournalAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	client := &recoveryClient{docs: make(map[string]storage.RemoteDocument)}
	closer, managed, journal := newRecoveryCloser(t, root, client)
	diary := closeRecoverySession(t, closer)

	binding, ok, err := managed.Bindings.Load(context.Background(), diary.IdempotencyKey)
	if err != nil || !ok {
		t.Fatalf("binding missing: %+v ok=%v err=%v", binding, ok, err)
	}
	if binding.Provider != "fake-issues" || binding.RemoteID != "42" || binding.URL == "" || binding.RemoteRevision == "" || binding.LocalHash == "" || binding.EffectiveRemoteHash == "" {
		t.Fatalf("incomplete binding: %+v", binding)
	}
	records, err := journal.ReadSaga(context.Background(), diary.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || records[0].Event == nil || records[0].Event.Type != "session.closed" || records[1].Event == nil || records[1].Event.Type != "storage.sync.requested" || records[2].Attempt == nil || records[3].Event == nil || records[3].Event.Type != "storage.sync.succeeded" {
		t.Fatalf("unexpected durable sequence: %+v", records)
	}
	var evidence core.StorageResult
	if err := json.Unmarshal(records[3].Event.Payload, &evidence); err != nil || evidence.RemoteID != binding.RemoteID || evidence.RemoteRev != string(binding.RemoteRevision) || evidence.LocalHash != binding.LocalHash {
		t.Fatalf("journal cannot rebuild binding: evidence=%+v err=%v", evidence, err)
	}

	restarted, err := core.NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := core.InspectRecovery(context.Background(), restarted); err != nil || len(pending) != 0 {
		t.Fatalf("restart reported completed mirror pending: %+v err=%v", pending, err)
	}
	// Re-running session close is a local idempotent replay, not an automatic
	// external recovery attempt.
	closeRecoverySession(t, closer)
	client.mu.Lock()
	puts := client.puts
	client.mu.Unlock()
	if puts != 1 {
		t.Fatalf("idempotent replay performed %d remote writes", puts)
	}
}

func TestSessionCloseClassifiesRecoverableStorageOutcomes(t *testing.T) {
	tests := []struct {
		name, mode, state, class, next string
		wantSnapshot                   bool
	}{
		{name: "transient", mode: "transient", state: "StorageSyncPending", class: "transient", next: "syntroph sync retry --storage"},
		{name: "prerequisite", mode: "prerequisite", state: "StoragePrerequisiteMissing", class: "prerequisite_missing", next: "syntroph doctor storage"},
		{name: "conflict", mode: "conflict", state: "StorageSyncConflict", class: "conflict", next: "syntroph sync resolve", wantSnapshot: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			client := &recoveryClient{docs: make(map[string]storage.RemoteDocument), mode: tt.mode}
			closer, managed, journal := newRecoveryCloser(t, root, client)
			diary := closeRecoverySession(t, closer)
			if _, ok, err := (core.LocalMemoryStore{Root: filepath.Join(root, "memory")}).FindByIdempotencyKey(context.Background(), diary.IdempotencyKey); err != nil || !ok {
				t.Fatalf("local canonical diary lost: ok=%v err=%v", ok, err)
			}
			items, err := core.InspectRecovery(context.Background(), journal)
			if err != nil || len(items) != 1 {
				t.Fatalf("recovery items: %+v err=%v", items, err)
			}
			item := items[0]
			if item.State != tt.state || item.Provider != "fake-issues" || item.FailureClass != tt.class || !strings.Contains(item.NextAction, tt.next) {
				t.Fatalf("recovery classification: %+v", item)
			}
			if tt.wantSnapshot {
				if item.RemoteID != "77" || item.RemoteRevision == "" || item.ConflictSnapshot == "" {
					t.Fatalf("conflict evidence missing: %+v", item)
				}
				if _, ok, err := managed.Bindings.Load(context.Background(), diary.IdempotencyKey); err != nil || !ok {
					t.Fatalf("conflict binding missing: ok=%v err=%v", ok, err)
				}
			}
			if tt.mode == "transient" {
				client.mu.Lock()
				client.mode = ""
				client.mu.Unlock()
				// Reopening the same session after a crash/restart is local replay,
				// never implicit confirmation of an external retry.
				closeRecoverySession(t, closer)
				client.mu.Lock()
				puts := client.puts
				client.mu.Unlock()
				if puts != 0 {
					t.Fatalf("replay after transient failure performed %d remote writes", puts)
				}
			}
		})
	}
}

func TestExplicitRetryReusesKeyAndProviderAndResolutionClearsConflict(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	client := &recoveryClient{docs: make(map[string]storage.RemoteDocument), mode: "transient"}
	closer, managed, journal := newRecoveryCloser(t, root, client)
	diary := closeRecoverySession(t, closer)
	client.mu.Lock()
	client.mode = ""
	client.mu.Unlock()
	result := (storageadapter.Adapter{Provider: managed}).MirrorEvent(ctx, diary.SessionID+":storage-retry", diary)
	if result.State != "mirrored" || result.Key != diary.IdempotencyKey || result.Provider != "fake-issues" {
		t.Fatalf("explicit retry changed identity: %+v", result)
	}
	retryEvent := core.Event{EventID: diary.SessionID + ":storage-retry", Type: "storage.sync.succeeded", OccurredAt: time.Now().UTC(), RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, CausationID: diary.SessionID + ":storage", SchemaVersion: 1, Payload: mustJSON(result)}
	if err := journal.AppendAttempt(ctx, core.HandlerAttempt{EventID: retryEvent.EventID, SagaID: diary.SessionID, HandlerID: "storage-retry", AttemptedAt: time.Now().UTC(), Outcome: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.AppendEvent(ctx, retryEvent); err != nil {
		t.Fatal(err)
	}
	if items, err := core.InspectRecovery(ctx, journal); err != nil || len(items) != 0 {
		t.Fatalf("explicit retry did not clear pending state: %+v err=%v", items, err)
	}

	conflictRoot := t.TempDir()
	conflictClient := &recoveryClient{docs: make(map[string]storage.RemoteDocument), mode: "conflict"}
	conflictCloser, _, conflictJournal := newRecoveryCloser(t, conflictRoot, conflictClient)
	conflictDiary := closeRecoverySession(t, conflictCloser)
	resolved := core.StorageResult{State: "mirrored", Provider: "fake-issues", Key: conflictDiary.IdempotencyKey, RemoteID: "77", RemoteRev: issueRevision("77", "human remote edit")}
	if err := conflictJournal.AppendEvent(ctx, core.Event{EventID: conflictDiary.SessionID + ":storage-resolved", Type: "storage.sync.resolved", OccurredAt: time.Now().UTC(), RepositoryID: conflictDiary.RepositoryID, SagaID: conflictDiary.SessionID, CorrelationID: conflictDiary.SessionID, CausationID: conflictDiary.SessionID + ":storage", SchemaVersion: 1, Payload: mustJSON(resolved)}); err != nil {
		t.Fatal(err)
	}
	if items, err := core.InspectRecovery(ctx, conflictJournal); err != nil || len(items) != 0 {
		t.Fatalf("explicit resolution did not clear conflict: %+v err=%v", items, err)
	}
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
