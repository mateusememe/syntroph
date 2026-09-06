package core

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

// mirrorClient is a deterministic authenticated-client seam. It models both
// GitHub Wiki and Issues without credentials or network access.
type mirrorClient struct {
	docs    map[string]storage.RemoteDocument
	puts    int
	failGet bool
}

func (c *mirrorClient) Get(_ context.Context, backend storage.Backend, repository, key string) (storage.RemoteDocument, error) {
	if c.failGet {
		return storage.RemoteDocument{}, errors.New("remote unavailable")
	}
	doc, ok := c.docs[string(backend)+":"+repository+":"+key]
	if !ok {
		return storage.RemoteDocument{NotFound: true}, nil
	}
	return doc, nil
}

func (c *mirrorClient) Put(_ context.Context, backend storage.Backend, repository, key, content, expectedRevision string) (storage.RemoteDocument, error) {
	c.puts++
	doc := storage.RemoteDocument{ID: key, Revision: "rev-" + string(rune('0'+c.puts)), Content: content}
	c.docs[string(backend)+":"+repository+":"+key] = doc
	return doc, nil
}

func TestMVPEndToEndSessionCloseAndExplicitRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	memory := LocalMemoryStore{Root: filepath.Join(root, ".syntroph", "memory")}
	graph, err := NewLocalGraphPort(filepath.Join(root, ".syntroph", "graph"))
	if err != nil {
		t.Fatal(err)
	}
	// Seed an active snapshot so explicit references are resolved before the
	// diary is persisted and retain graph snapshot provenance.
	snapshot, err := graph.Publish(ctx, GraphSnapshot{
		RepositoryID: "github.com/mateusememe/syntroph",
		CommitSHA:    "abc123",
		References:   []CodeReference{{Path: "core/session.go", Symbol: "SessionCloser", Kind: "type"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	journal, err := NewSagaJournal(filepath.Join(root, ".syntroph", "journal"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := NewEventBus(journal)
	if err != nil {
		t.Fatal(err)
	}
	client := &mirrorClient{docs: map[string]storage.RemoteDocument{}}
	mirror, err := storage.NewMirror(storage.BackendWiki, "mateusememe/syntroph", client)
	if err != nil {
		t.Fatal(err)
	}
	var mirrored int
	if err := bus.Subscribe("session.closed", "github-wiki-mirror", func(ctx context.Context, event Event) error {
		var diary SessionDiary
		if err := json.Unmarshal(event.Payload, &diary); err != nil {
			return err
		}
		result := mirror.Mirror(ctx, storage.SessionDiary{
			SessionID: diary.SessionID, RepositoryID: diary.RepositoryID,
			CommitSHA: diary.CommitSHA, ArtifactHash: diary.ArtifactHash,
			Content: diary.Summary,
		})
		if result.Pending() {
			return result.Cause
		}
		mirrored++
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	closer := SessionCloser{Memory: memory, Graph: graph, Bus: bus}
	diary, delivery, err := closer.Close(ctx, SessionCloseRequest{
		RepositoryID: "github.com/mateusememe/syntroph", CommitSHA: "abc123",
		Author: "mateusememe", Runtime: "codex", Now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		Format: ArtifactJSON, Artifact: []byte(`{"title":"MVP","summary":"closed session","code_references":[{"path":"core/session.go","symbol":"SessionCloser","kind":"type"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if diary.GraphState != GraphReady || diary.GraphSnapshotID != snapshot.ID || diary.CodeReferences[0].Confidence != ConfidenceAuthoritative {
		t.Fatalf("graph provenance was not retained: %+v", diary)
	}
	if mirrored != 1 || len(delivery.Attempts) != 1 || delivery.Attempts[0].Outcome != "succeeded" {
		t.Fatalf("unexpected delivery: mirrored=%d delivery=%+v", mirrored, delivery)
	}
	if got, ok, err := memory.FindByIdempotencyKey(ctx, diary.IdempotencyKey); err != nil || !ok || got.SessionID != diary.SessionID {
		t.Fatalf("canonical diary missing: ok=%v err=%v", ok, err)
	}
	records, err := journal.ReadSaga(ctx, diary.SessionID)
	if err != nil || len(records) != 2 || records[0].Kind != "event" || records[1].Kind != "attempt" {
		t.Fatalf("causal journal incomplete: records=%+v err=%v", records, err)
	}
	if pending, err := InspectRecovery(ctx, journal); err != nil || len(pending) != 0 {
		t.Fatalf("successful saga reported as pending: %+v err=%v", pending, err)
	}
}

func TestMVPEndToEndPendingGraphAndIdempotentScopedRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	memory := LocalMemoryStore{Root: filepath.Join(root, "memory")}
	graph, err := NewLocalGraphPort(filepath.Join(root, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := NewEventBus(journal)
	if err != nil {
		t.Fatal(err)
	}
	client := &mirrorClient{docs: map[string]storage.RemoteDocument{}, failGet: true}
	mirror, _ := storage.NewMirror(storage.BackendIssues, "mateusememe/syntroph", client)
	if err := bus.Subscribe("session.closed", "github-issues-mirror", func(ctx context.Context, event Event) error {
		var diary SessionDiary
		if err := json.Unmarshal(event.Payload, &diary); err != nil {
			return err
		}
		result := mirror.Mirror(ctx, storage.SessionDiary{SessionID: diary.SessionID, RepositoryID: diary.RepositoryID, CommitSHA: diary.CommitSHA, ArtifactHash: diary.ArtifactHash, Content: diary.Summary})
		return result.Cause
	}); err != nil {
		t.Fatal(err)
	}
	closer := SessionCloser{Memory: memory, Graph: graph, Bus: bus}
	diary, delivery, err := closer.Close(ctx, SessionCloseRequest{RepositoryID: "repo", CommitSHA: "sha", Format: ArtifactJSON, Artifact: []byte(`{"summary":"capture","code_references":[{"path":"missing.go"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if diary.GraphState != GraphResolutionPending || len(delivery.Attempts) != 1 || delivery.Attempts[0].Outcome != "failed" {
		t.Fatalf("pending obligations were lost: diary=%+v delivery=%+v", diary, delivery)
	}
	pending, err := InspectRecovery(ctx, journal)
	if err != nil || len(pending) != 2 || !containsRecoveryState(pending, "HandlerPending") || !containsRecoveryState(pending, GraphResolutionPending) {
		t.Fatalf("expected explainable pending recovery: %+v err=%v", pending, err)
	}
	// Explicit retry is scoped to the storage handler. Once connectivity is
	// restored, redelivery is safe: the mirror's key prevents a duplicate Put.
	client.failGet = false
	if _, err := bus.Publish(ctx, Event{EventID: diary.SessionID, Type: "session.closed", OccurredAt: diary.CreatedAt, RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, SchemaVersion: 1, Payload: mustJSON(diary)}); err != nil {
		t.Fatal(err)
	}
	if pending, err := InspectRecovery(ctx, journal); err != nil || len(pending) != 1 || pending[0].State != GraphResolutionPending {
		t.Fatalf("storage retry should clear only storage obligation: %+v err=%v", pending, err)
	}
	client.failGet = true
	if _, err := bus.Publish(ctx, Event{EventID: diary.SessionID, Type: "session.closed", OccurredAt: diary.CreatedAt, RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, SchemaVersion: 1, Payload: mustJSON(diary)}); err != nil {
		t.Fatal(err)
	}
	if client.puts != 1 {
		t.Fatalf("redelivery duplicated remote effect: puts=%d", client.puts)
	}
}

// Small wrappers keep the integration test focused on the public event seam.
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func containsRecoveryState(items []RecoveryItem, state string) bool {
	for _, item := range items {
		if item.State == state {
			return true
		}
	}
	return false
}
