package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
