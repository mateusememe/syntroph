package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/core"
)

// fakeGraphPort is a deterministic, in-memory GraphPort test double. resolve
// is called once per Resolve invocation so a test can both assert how many
// times -- and with what deduplicated batch -- Resolve was called, and
// control whether it succeeds or fails.
type fakeGraphPort struct {
	calls    int
	requests []core.GraphResolveRequest
	resolve  func(core.GraphResolveRequest) (core.GraphResolution, error)
}

func (f *fakeGraphPort) Resolve(_ context.Context, req core.GraphResolveRequest) (core.GraphResolution, error) {
	f.calls++
	f.requests = append(f.requests, req)
	return f.resolve(req)
}

func resolveAllAuthoritative(snapshotID string) func(core.GraphResolveRequest) (core.GraphResolution, error) {
	return func(req core.GraphResolveRequest) (core.GraphResolution, error) {
		out := make([]core.CodeReference, len(req.References))
		for i, ref := range req.References {
			ref.RepositoryID = req.RepositoryID
			ref.CommitSHA = req.CommitSHA
			ref.Confidence = core.ConfidenceAuthoritative
			ref.GraphSnapshotID = snapshotID
			out[i] = ref
		}
		return core.GraphResolution{References: out, GraphSnapshotID: snapshotID}, nil
	}
}

func closeGraphArtifactSession(t *testing.T, closer core.SessionCloser, repositoryRoot string, a core.SessionArtifact) (core.SessionDiary, error) {
	t.Helper()
	payload := struct {
		Title            string                       `json:"title"`
		Summary          string                       `json:"summary"`
		CodeReferences   []core.CodeReference         `json:"code_references,omitempty"`
		SkillInvocations []core.SkillInvocationRecord `json:"skill_invocations,omitempty"`
	}{Title: "Session", Summary: "did things", CodeReferences: a.CodeReferences, SkillInvocations: a.SkillInvocations}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	diary, _, err := closer.Close(context.Background(), core.SessionCloseRequest{
		RepositoryID:   "repo-1",
		CommitSHA:      "sha-graph",
		Author:         "matt",
		RepositoryRoot: repositoryRoot,
		Artifact:       data,
		Format:         core.ArtifactJSON,
		Now:            time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
	})
	return diary, err
}

func TestSessionCloseResolvesTopLevelAndNestedCodeReferencesInOneDeduplicatedGraphBatch(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	graph := &fakeGraphPort{resolve: resolveAllAuthoritative("snap-1")}
	closer := core.SessionCloser{Memory: memory, Bus: bus, Graph: graph}

	shared := core.CodeReference{Path: "core/skill.go", Symbol: "SkillPort"}
	invocationA := validSkillInvocationRecord()
	invocationA.InvocationID = "inv-a"
	invocationA.CodeReferences = []core.CodeReference{shared, {Path: "core/skill_invocation.go", Symbol: "SkillInvocationRecord"}}
	invocationB := validSkillInvocationRecord()
	invocationB.InvocationID = "inv-b"
	invocationB.CodeReferences = []core.CodeReference{shared}

	a := core.SessionArtifact{
		CodeReferences:   []core.CodeReference{{Path: "core/session.go", Symbol: "SessionCloser"}, shared},
		SkillInvocations: []core.SkillInvocationRecord{invocationA, invocationB},
	}

	diary, err := closeGraphArtifactSession(t, closer, t.TempDir(), a)
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if graph.calls != 1 {
		t.Fatalf("GraphPort.Resolve called %d times, want exactly 1", graph.calls)
	}
	// Four occurrences (2 top-level + 2 + 1 nested = 5, minus the two
	// duplicates of `shared`) collapse to three unique (path, symbol, kind)
	// tuples in the single dispatched batch.
	if got := len(graph.requests[0].References); got != 3 {
		t.Fatalf("deduplicated batch size = %d, want 3: %+v", got, graph.requests[0].References)
	}

	if diary.GraphState != core.GraphReady || diary.GraphSnapshotID != "snap-1" {
		t.Fatalf("unexpected graph state: state=%q snapshot=%q", diary.GraphState, diary.GraphSnapshotID)
	}
	for _, ref := range diary.CodeReferences {
		if ref.Confidence != core.ConfidenceAuthoritative || ref.GraphSnapshotID != "snap-1" {
			t.Fatalf("top-level reference not resolved: %+v", ref)
		}
	}
	for _, inv := range diary.SkillInvocations {
		for _, ref := range inv.CodeReferences {
			if ref.Confidence != core.ConfidenceAuthoritative || ref.GraphSnapshotID != "snap-1" {
				t.Fatalf("nested reference on %s not resolved: %+v", inv.InvocationID, ref)
			}
		}
	}
}

func TestSessionCloseGraphPortFailurePreservesNestedReferencesAndMarksPending(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	graph := &fakeGraphPort{resolve: func(core.GraphResolveRequest) (core.GraphResolution, error) {
		return core.GraphResolution{}, errors.New("graph unavailable")
	}}
	closer := core.SessionCloser{Memory: memory, Bus: bus, Graph: graph}

	invocation := validSkillInvocationRecord()
	invocation.InvocationID = "inv-pending"
	invocation.CodeReferences = []core.CodeReference{{Path: "core/skill_invocation.go", Symbol: "SkillInvocationRecord"}}

	a := core.SessionArtifact{
		CodeReferences:   []core.CodeReference{{Path: "core/session.go", Symbol: "SessionCloser"}},
		SkillInvocations: []core.SkillInvocationRecord{invocation},
	}

	diary, err := closeGraphArtifactSession(t, closer, t.TempDir(), a)
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if diary.GraphState != core.GraphResolutionPending || diary.GraphSnapshotID != "" {
		t.Fatalf("unexpected graph state: state=%q snapshot=%q", diary.GraphState, diary.GraphSnapshotID)
	}
	if len(diary.CodeReferences) != 1 || diary.CodeReferences[0].Path != "core/session.go" || diary.CodeReferences[0].Confidence != core.ConfidenceUnresolved {
		t.Fatalf("top-level reference not preserved: %+v", diary.CodeReferences)
	}
	if len(diary.SkillInvocations) != 1 {
		t.Fatalf("SkillInvocations = %+v, want 1", diary.SkillInvocations)
	}
	got := diary.SkillInvocations[0].CodeReferences
	if len(got) != 1 || got[0].Path != "core/skill_invocation.go" || got[0].Symbol != "SkillInvocationRecord" || got[0].Confidence != core.ConfidenceUnresolved || got[0].GraphSnapshotID != "" {
		t.Fatalf("nested reference not preserved as pending: %+v", got)
	}
}

func TestSessionCloseVerifiesExistingArtifactHashMediaTypeAndSize(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := core.SessionCloser{Memory: memory, Bus: bus}

	repositoryRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repositoryRoot, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("# Review\n\nLooks good, ship it.\n")
	artifactPath := filepath.Join(repositoryRoot, "reports", "review.md")
	if err := os.WriteFile(artifactPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])
	wantMediaType := http.DetectContentType(content)

	invocation := validSkillInvocationRecord()
	invocation.Artifacts = []core.SkillInvocationArtifact{{Path: "reports/review.md"}}
	a := core.SessionArtifact{SkillInvocations: []core.SkillInvocationRecord{invocation}}

	diary, err := closeGraphArtifactSession(t, closer, repositoryRoot, a)
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	got := diary.SkillInvocations[0].Artifacts[0]
	if got.Availability != core.ArtifactAvailabilityVerified {
		t.Fatalf("Availability = %q, want verified", got.Availability)
	}
	if got.SHA256 != wantHash {
		t.Fatalf("SHA256 = %q, want %q", got.SHA256, wantHash)
	}
	if got.MediaType != wantMediaType {
		t.Fatalf("MediaType = %q, want %q", got.MediaType, wantMediaType)
	}
	if got.SizeBytes != int64(len(content)) {
		t.Fatalf("SizeBytes = %d, want %d", got.SizeBytes, len(content))
	}
	if got.Path != "reports/review.md" {
		t.Fatalf("Path = %q, want the declared repository-relative path", got.Path)
	}
}

func TestSessionCloseMarksMissingArtifactAvailabilityWithoutFailingClose(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := core.SessionCloser{Memory: memory, Bus: bus}

	invocation := validSkillInvocationRecord()
	invocation.Artifacts = []core.SkillInvocationArtifact{{Path: "reports/never-produced.md"}}
	a := core.SessionArtifact{SkillInvocations: []core.SkillInvocationRecord{invocation}}

	diary, err := closeGraphArtifactSession(t, closer, t.TempDir(), a)
	if err != nil {
		t.Fatalf("Close() error = %v, want diary persistence to succeed for a missing artifact", err)
	}
	got := diary.SkillInvocations[0].Artifacts[0]
	if got.Availability != core.ArtifactAvailabilityMissing {
		t.Fatalf("Availability = %q, want missing", got.Availability)
	}
	if got.SHA256 != "" || got.MediaType != "" || got.SizeBytes != 0 {
		t.Fatalf("unexpected metadata recorded for a missing artifact: %+v", got)
	}
}

func TestSessionCloseRejectsAbsoluteArtifactPath(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := core.SessionCloser{Memory: memory, Bus: bus}

	invocation := validSkillInvocationRecord()
	invocation.Artifacts = []core.SkillInvocationArtifact{{Path: "/etc/passwd"}}
	a := core.SessionArtifact{SkillInvocations: []core.SkillInvocationRecord{invocation}}

	if _, err := closeGraphArtifactSession(t, closer, t.TempDir(), a); !errors.Is(err, core.ErrSkillInvocationArtifactUnsafe) {
		t.Fatalf("Close() error = %v, want ErrSkillInvocationArtifactUnsafe", err)
	}
	assertNoDiaryPersisted(t, memory)
}

func TestSessionCloseRejectsTraversalArtifactPath(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := core.SessionCloser{Memory: memory, Bus: bus}

	invocation := validSkillInvocationRecord()
	invocation.Artifacts = []core.SkillInvocationArtifact{{Path: "../outside.md"}}
	a := core.SessionArtifact{SkillInvocations: []core.SkillInvocationRecord{invocation}}

	if _, err := closeGraphArtifactSession(t, closer, t.TempDir(), a); !errors.Is(err, core.ErrSkillInvocationArtifactUnsafe) {
		t.Fatalf("Close() error = %v, want ErrSkillInvocationArtifactUnsafe", err)
	}
	assertNoDiaryPersisted(t, memory)
}

func TestSessionCloseRejectsSymlinkEscapingArtifactPathAndNeverReadsTheHostFile(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := core.SessionCloser{Memory: memory, Bus: bus}

	repositoryRoot := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret host file contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repositoryRoot, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	invocation := validSkillInvocationRecord()
	invocation.Artifacts = []core.SkillInvocationArtifact{{Path: "escape.txt"}}
	a := core.SessionArtifact{SkillInvocations: []core.SkillInvocationRecord{invocation}}

	if _, err := closeGraphArtifactSession(t, closer, repositoryRoot, a); !errors.Is(err, core.ErrSkillInvocationArtifactUnsafe) {
		t.Fatalf("Close() error = %v, want ErrSkillInvocationArtifactUnsafe", err)
	}
	assertNoDiaryPersisted(t, memory)
}

// TestSessionDiaryNeverCarriesArtifactContents renders a Session Diary --
// the same value both the local memory store and any remote mirror
// persist -- and confirms the artifact's actual content never appears in
// it, only its metadata (issue #27 acceptance: remote mirrors carry
// artifact metadata, never bodies).
func TestSessionDiaryNeverCarriesArtifactContents(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := core.SessionCloser{Memory: memory, Bus: bus}

	repositoryRoot := t.TempDir()
	secretMarker := "THIS-EXACT-ARTIFACT-BODY-MUST-NEVER-APPEAR-IN-THE-DIARY"
	if err := os.WriteFile(filepath.Join(repositoryRoot, "review.md"), []byte(secretMarker), 0o644); err != nil {
		t.Fatal(err)
	}

	invocation := validSkillInvocationRecord()
	invocation.Artifacts = []core.SkillInvocationArtifact{{Path: "review.md"}}
	a := core.SessionArtifact{SkillInvocations: []core.SkillInvocationRecord{invocation}}

	diary, err := closeGraphArtifactSession(t, closer, repositoryRoot, a)
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if diary.SkillInvocations[0].Artifacts[0].Availability != core.ArtifactAvailabilityVerified {
		t.Fatalf("artifact was not verified: %+v", diary.SkillInvocations[0].Artifacts[0])
	}
	rendered := core.RenderSessionDiary(diary)
	if containsSubstring(rendered, secretMarker) {
		t.Fatalf("rendered Session Diary carried the artifact's body, want metadata only:\n%s", rendered)
	}
}

func assertNoDiaryPersisted(t *testing.T, memory core.LocalMemoryStore) {
	t.Helper()
	var found int
	_ = filepath.Walk(memory.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".md" {
			found++
		}
		return nil
	})
	if found != 0 {
		t.Fatalf("expected no diary to be persisted, found %d", found)
	}
}
