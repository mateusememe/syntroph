package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type preflightStorage struct {
	ready       bool
	mirrorCalls int
}

func (s *preflightStorage) Preflight(context.Context) StoragePreflightResult {
	if s.ready {
		return StoragePreflightResult{Ready: true, Backend: "issues", Provider: "github-rest", Repository: "mateusememe/syntroph"}
	}
	return StoragePreflightResult{
		Backend: "issues", Provider: "github-rest", Repository: "mateusememe/syntroph",
		Cause: errors.New("SYNTROPH_GITHUB_TOKEN is not set"),
	}
}

func (s *preflightStorage) Mirror(context.Context, SessionDiary) StorageResult {
	s.mirrorCalls++
	return StorageResult{State: "mirrored"}
}

func TestSessionCloseWritesImmutableDiaryAndIsIdempotent(t *testing.T) {
	store := LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := SessionCloser{Memory: store}
	req := SessionCloseRequest{RepositoryID: "github.com/example/repo", CommitSHA: "abc123", Author: "matt", Runtime: "codex", Now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC), Format: ArtifactJSON, Artifact: []byte(`{"title":"First session","summary":"Implemented the diary","decisions":["Use local canonical storage"],"lessons":["Keep retries explicit"],"related":["[[prior-session]]"],"code_references":[{"path":"core/session.go","symbol":"SessionCloser","kind":"type"}]}`)}
	first, _, err := closer.Close(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := closer.Close(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID != second.SessionID || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("retry created a different diary: %#v %#v", first, second)
	}
	entries, err := filepath.Glob(filepath.Join(store.Root, "2026", "08", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one diary, got %d", len(entries))
	}
	b, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{"session_id:", "repository_id: github.com/example/repo", "commit_sha: abc123", "artifact_hash:", "author: matt", "runtime: codex", "[[prior-session]]", "core/session.go", "SessionCloser"} {
		if !strings.Contains(text, want) {
			t.Errorf("diary missing %q", want)
		}
	}
}

func TestSessionCloseDifferentArtifactsAtSameCommitRemainDistinct(t *testing.T) {
	store := LocalMemoryStore{Root: filepath.Join(t.TempDir(), "memory")}
	closer := SessionCloser{Memory: store}
	base := SessionCloseRequest{RepositoryID: "repo", CommitSHA: "sha", Author: "a", Format: ArtifactJSON}
	first, _, err := closer.Close(context.Background(), SessionCloseRequest{RepositoryID: base.RepositoryID, CommitSHA: base.CommitSHA, Author: base.Author, Format: ArtifactJSON, Artifact: []byte(`{"summary":"one"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := closer.Close(context.Background(), SessionCloseRequest{RepositoryID: base.RepositoryID, CommitSHA: base.CommitSHA, Author: base.Author, Format: ArtifactJSON, Artifact: []byte(`{"summary":"two"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID == second.SessionID {
		t.Fatal("different artifacts were conflated")
	}
}

func TestRenderSessionDiaryNormalizesRelatedWikiLinks(t *testing.T) {
	content := RenderSessionDiary(SessionDiary{SessionID: "s", IdempotencyKey: "k", RepositoryID: "r", CommitSHA: "c", ArtifactHash: "h", CreatedAt: time.Now().UTC(), Title: "Diary", Summary: "summary", Related: []string{"prior-session", "[[already-linked]]"}})
	if !strings.Contains(content, "related: [[already-linked]],[[prior-session]]") {
		t.Fatalf("related links were not rendered as wikilinks: %s", content)
	}
}

func TestParseMarkdownAndManualSummary(t *testing.T) {
	a, err := ParseSessionArtifact([]byte("# Closing\n\nA useful summary.\n\n## Decisions\n- Use ports\n\n## Lessons\n- Retry explicitly"), ArtifactMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "Closing" || a.Summary != "A useful summary." || len(a.Decisions) != 1 || len(a.Lessons) != 1 {
		t.Fatalf("unexpected markdown artifact: %#v", a)
	}
	m, err := ManualSessionArtifact("manual summary")
	if err != nil || m.Summary != "manual summary" {
		t.Fatalf("unexpected manual artifact: %#v %v", m, err)
	}
}

func TestSessionClosePersistsDiaryAndPrerequisiteBeforeSkippingMirror(t *testing.T) {
	root := t.TempDir()
	journal, err := NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := NewEventBus(journal)
	if err != nil {
		t.Fatal(err)
	}
	remote := &preflightStorage{}
	store := LocalMemoryStore{Root: filepath.Join(root, "memory")}
	diary, _, err := (SessionCloser{Memory: store, Bus: bus, Storage: remote}).Close(context.Background(), SessionCloseRequest{
		RepositoryID: "github.com/mateusememe/syntroph", CommitSHA: "abc123", Author: "matt",
		Format: ArtifactJSON, Artifact: []byte(`{"summary":"safe preflight"}`),
	})
	if err != nil {
		t.Fatalf("missing remote prerequisite failed local close: %v", err)
	}
	if remote.mirrorCalls != 0 {
		t.Fatalf("preflight failure performed %d provider writes", remote.mirrorCalls)
	}
	if _, ok, err := store.FindByIdempotencyKey(context.Background(), diary.IdempotencyKey); err != nil || !ok {
		t.Fatalf("durable local diary missing: ok=%v err=%v", ok, err)
	}
	records, err := journal.ReadSaga(context.Background(), diary.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0].Event == nil || records[0].Event.Type != "session.closed" || records[2].Event == nil || records[2].Event.Type != "storage.prerequisite.missing" {
		t.Fatalf("unexpected durable preflight sequence: %+v", records)
	}
	items, err := InspectRecovery(context.Background(), journal)
	if err != nil || len(items) != 1 || items[0].State != "StoragePrerequisiteMissing" || items[0].NextAction != "syntroph doctor storage" {
		t.Fatalf("unexpected recovery view: %+v err=%v", items, err)
	}
}
