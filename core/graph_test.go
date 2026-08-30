package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalGraphPortPublishesContentAddressedSnapshotAndResolvesReferences(t *testing.T) {
	port, err := NewLocalGraphPort(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := port.Publish(context.Background(), GraphSnapshot{RepositoryID: "repo", CommitSHA: "sha", References: []CodeReference{{Path: "core/session.go", Symbol: "SessionCloser", Kind: "type"}}})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID == "" {
		t.Fatal("expected content-addressed snapshot id")
	}
	if _, err := os.Stat(filepath.Join(port.Root, "snapshots", snapshot.ID+".json")); err != nil {
		t.Fatal(err)
	}
	resolution, err := port.Resolve(context.Background(), GraphResolveRequest{RepositoryID: "repo", CommitSHA: "sha", References: []CodeReference{{Path: "core/session.go", Symbol: "SessionCloser", Kind: "type"}, {Path: "missing.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.GraphSnapshotID != snapshot.ID || resolution.References[0].Confidence != ConfidenceAuthoritative || resolution.References[1].Confidence != ConfidenceUnresolved {
		t.Fatalf("unexpected resolution: %#v", resolution)
	}
}

func TestLocalGraphPortReportsUnavailableAndStaleSnapshots(t *testing.T) {
	port, err := NewLocalGraphPort(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Resolve(context.Background(), GraphResolveRequest{RepositoryID: "repo", CommitSHA: "sha"}); !errors.Is(err, ErrGraphUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
	if _, err := port.Publish(context.Background(), GraphSnapshot{RepositoryID: "repo", CommitSHA: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := port.Resolve(context.Background(), GraphResolveRequest{RepositoryID: "repo", CommitSHA: "new"}); !errors.Is(err, ErrGraphStale) {
		t.Fatalf("expected stale, got %v", err)
	}
}

func TestSessionClosePersistsDiaryWhenGraphUnavailable(t *testing.T) {
	store := LocalMemoryStore{Root: filepath.Join(t.TempDir(), "memory")}
	port, err := NewLocalGraphPort(filepath.Join(t.TempDir(), "graph"))
	if err != nil {
		t.Fatal(err)
	}
	closer := SessionCloser{Memory: store, Graph: port}
	diary, _, err := closer.Close(context.Background(), SessionCloseRequest{RepositoryID: "repo", CommitSHA: "sha", Format: ArtifactJSON, Artifact: []byte(`{"summary":"capture","code_references":[{"path":"core/session.go"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if diary.GraphState != GraphResolutionPending || len(diary.CodeReferences) != 1 || diary.CodeReferences[0].Confidence != ConfidenceUnresolved {
		t.Fatalf("unexpected pending diary: %#v", diary)
	}
	if _, ok, err := store.FindByIdempotencyKey(context.Background(), diary.IdempotencyKey); err != nil || !ok {
		t.Fatalf("diary was not persisted: ok=%v err=%v", ok, err)
	}
}
