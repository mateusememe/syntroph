package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/core"
)

// closeWithSkillInvocations is a small helper around SessionCloser.Close
// that builds a JSON Session Artifact carrying the given records, so each
// test only has to state the records under reconciliation.
func closeWithSkillInvocations(t *testing.T, closer core.SessionCloser, commitSHA string, records []core.SkillInvocationRecord) (core.SessionDiary, error) {
	t.Helper()
	data := jsonArtifactWithSkillInvocations(t, records)
	diary, _, err := closer.Close(context.Background(), core.SessionCloseRequest{
		RepositoryID: "repo-1",
		CommitSHA:    commitSHA,
		Author:       "matt",
		Artifact:     data,
		Format:       core.ArtifactJSON,
		Now:          time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
	})
	return diary, err
}

func newTestSessionCloser(t *testing.T, bus *core.EventBus) core.SessionCloser {
	t.Helper()
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	return core.SessionCloser{Memory: memory, Bus: bus}
}

func TestSessionCloseMarksSkillInvocationJournalVerifiedWhenItMatchesPreparedEvidence(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}
	bundle, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name: "mattpocock/code-review", InvocationID: "inv-verified", Runtime: "codex",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	record := core.SkillInvocationRecord{
		InvocationID:    "inv-verified",
		PackageIdentity: bundle.PackageIdentity(),
		BundleHash:      bundle.BundleHash(),
		Runtime:         bundle.Runtime(),
		Outcome:         core.SkillInvocationOutcomeSucceeded,
		Summary:         "reviewed the diff",
	}

	closer := newTestSessionCloser(t, bus)
	diary, err := closeWithSkillInvocations(t, closer, "sha-verified", []core.SkillInvocationRecord{record})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(diary.SkillInvocations) != 1 {
		t.Fatalf("len(diary.SkillInvocations) = %d, want 1", len(diary.SkillInvocations))
	}
	got := diary.SkillInvocations[0]
	if got.Provenance != core.SkillInvocationJournalVerified {
		t.Fatalf("Provenance = %q, want %q", got.Provenance, core.SkillInvocationJournalVerified)
	}
	wantRef := "inv-verified:prepared"
	if len(got.EventReferences) != 1 || got.EventReferences[0] != wantRef {
		t.Fatalf("EventReferences = %v, want [%q]", got.EventReferences, wantRef)
	}
	_ = journal
}

func TestSessionCloseMarksSkillInvocationRuntimeDeclaredWithoutJournalEvidence(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	closer := newTestSessionCloser(t, bus)
	record := validSkillInvocationRecord()
	record.InvocationID = "inv-unknown-to-journal"

	diary, err := closeWithSkillInvocations(t, closer, "sha-unknown", []core.SkillInvocationRecord{record})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	got := diary.SkillInvocations[0]
	if got.Provenance != core.SkillInvocationRuntimeDeclared {
		t.Fatalf("Provenance = %q, want %q", got.Provenance, core.SkillInvocationRuntimeDeclared)
	}
	if len(got.EventReferences) != 0 {
		t.Fatalf("EventReferences = %v, want none", got.EventReferences)
	}
}

func TestSessionCloseWithoutBusTreatsEverySkillInvocationAsRuntimeDeclared(t *testing.T) {
	memory := core.LocalMemoryStore{Root: filepath.Join(t.TempDir(), ".syntroph", "memory")}
	closer := core.SessionCloser{Memory: memory}
	record := validSkillInvocationRecord()

	diary, err := closeWithSkillInvocations(t, closer, "sha-no-bus", []core.SkillInvocationRecord{record})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if diary.SkillInvocations[0].Provenance != core.SkillInvocationRuntimeDeclared {
		t.Fatalf("Provenance = %q, want %q", diary.SkillInvocations[0].Provenance, core.SkillInvocationRuntimeDeclared)
	}
}

func TestSessionCloseRejectsSkillInvocationRecordContradictingJournalEvidence(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}
	bundle, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name: "mattpocock/code-review", InvocationID: "inv-contradicted", Runtime: "codex",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	base := core.SkillInvocationRecord{
		InvocationID:    "inv-contradicted",
		PackageIdentity: bundle.PackageIdentity(),
		Runtime:         bundle.Runtime(),
		Outcome:         core.SkillInvocationOutcomeSucceeded,
	}

	tests := []struct {
		name   string
		mutate func(core.SkillInvocationRecord) core.SkillInvocationRecord
	}{
		{
			name: "name",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.PackageIdentity.Name = "other-package"
				return r
			},
		},
		{
			name: "package hash",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.PackageIdentity.PackageHash = testOtherPackageHash
				return r
			},
		},
		{
			name: "runtime",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.Runtime = "claude"
				return r
			},
		},
		{
			name: "arguments",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.Arguments = map[string]any{"fixed_point": "main"}
				return r
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closer := newTestSessionCloser(t, bus)
			_, err := closeWithSkillInvocations(t, closer, "sha-contradiction-"+tt.name+string(rune('a'+i)), []core.SkillInvocationRecord{tt.mutate(base)})
			if err == nil || !errors.Is(err, core.ErrSkillInvocationContradiction) {
				t.Fatalf("Close() error = %v, want ErrSkillInvocationContradiction", err)
			}
		})
	}
	_ = journal
}

func TestSessionCloseRejectsResultContradictingCompletedJournalEvidence(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}
	bundle, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name: "mattpocock/code-review", InvocationID: "inv-result", Runtime: "codex",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	completedPayload, err := json.Marshal(core.SkillInvocationCompletedPayload{
		InvocationID: "inv-result", PackageIdentity: bundle.PackageIdentity(), BundleHash: bundle.BundleHash(), Runtime: bundle.Runtime(),
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	completedEvent := core.Event{
		EventID: "inv-result:invocation-completed", Type: core.SkillEventInvocationCompleted,
		OccurredAt: time.Now().UTC(), RepositoryID: "repo-1", SagaID: "inv-result", CorrelationID: "inv-result",
		SchemaVersion: core.SkillEventSchemaVersion, Payload: completedPayload,
	}
	if err := journal.AppendEvent(context.Background(), completedEvent); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	record := core.SkillInvocationRecord{
		InvocationID: "inv-result", PackageIdentity: bundle.PackageIdentity(), Runtime: bundle.Runtime(),
		Outcome: core.SkillInvocationOutcomeFailed, // journal says succeeded
	}
	closer := newTestSessionCloser(t, bus)
	_, err = closeWithSkillInvocations(t, closer, "sha-result", []core.SkillInvocationRecord{record})
	if err == nil || !errors.Is(err, core.ErrSkillInvocationContradiction) {
		t.Fatalf("Close() error = %v, want ErrSkillInvocationContradiction", err)
	}
}

// A prepare-failed event proves an invocation never produced a bundle, so a
// record claiming success for that same invocation ID is a result
// contradiction even though no identity evidence exists to compare.
func TestSessionCloseRejectsSuccessClaimAgainstPrepareFailedEvidence(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: stubSkillPort{err: errors.New("boom")}, Bus: bus}
	_, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name: "mattpocock/code-review", InvocationID: "inv-prepare-failed",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err == nil {
		t.Fatal("Prepare() unexpectedly succeeded")
	}

	record := validSkillInvocationRecord()
	record.InvocationID = "inv-prepare-failed"
	record.Outcome = core.SkillInvocationOutcomeSucceeded

	closer := newTestSessionCloser(t, bus)
	_, err = closeWithSkillInvocations(t, closer, "sha-prepare-failed", []core.SkillInvocationRecord{record})
	if err == nil || !errors.Is(err, core.ErrSkillInvocationContradiction) {
		t.Fatalf("Close() error = %v, want ErrSkillInvocationContradiction", err)
	}
}

func TestSessionCloseAcceptsFailedSkillInvocationAsHistoricalEvidenceWithoutError(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	closer := newTestSessionCloser(t, bus)
	record := validSkillInvocationRecord()
	record.InvocationID = "inv-failed-runtime"
	record.Outcome = core.SkillInvocationOutcomeFailed
	record.Summary = "the runtime reported a failure"

	diary, err := closeWithSkillInvocations(t, closer, "sha-failed", []core.SkillInvocationRecord{record})
	if err != nil {
		t.Fatalf("Close() error = %v, a failed invocation record must not fail diary persistence", err)
	}
	if diary.SkillInvocations[0].Outcome != core.SkillInvocationOutcomeFailed {
		t.Fatalf("Outcome = %q, want %q", diary.SkillInvocations[0].Outcome, core.SkillInvocationOutcomeFailed)
	}

	// Replaying the identical close is still a pure idempotent no-op --
	// the failed record created no pending retry obligation to trip on.
	second, err := closeWithSkillInvocations(t, closer, "sha-failed", []core.SkillInvocationRecord{record})
	if err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if second.SessionID != diary.SessionID {
		t.Fatalf("second.SessionID = %q, want %q (idempotent replay)", second.SessionID, diary.SessionID)
	}
}

func TestSessionCloseEmitsSkillInvocationLinkedEventWithoutRewritingPriorEvents(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}
	bundle, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name: "mattpocock/code-review", InvocationID: "inv-link", Runtime: "codex",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	before := readSagaEvents(t, journal, "inv-link")
	if len(before) != 2 {
		t.Fatalf("len(before) = %d, want 2 (requested, prepared)", len(before))
	}

	record := core.SkillInvocationRecord{
		InvocationID: "inv-link", PackageIdentity: bundle.PackageIdentity(), Runtime: bundle.Runtime(),
		Outcome: core.SkillInvocationOutcomeSucceeded,
	}
	closer := newTestSessionCloser(t, bus)
	diary, err := closeWithSkillInvocations(t, closer, "sha-link", []core.SkillInvocationRecord{record})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	after := readSagaEvents(t, journal, "inv-link")
	if len(after) != 3 {
		t.Fatalf("len(after) = %d, want 3 (requested, prepared, linked)", len(after))
	}
	for i := range before {
		beforeBytes, _ := json.Marshal(before[i])
		afterBytes, _ := json.Marshal(after[i])
		if string(beforeBytes) != string(afterBytes) {
			t.Fatalf("prior event %d was rewritten: before=%s after=%s", i, beforeBytes, afterBytes)
		}
	}
	linked := after[2]
	if linked.Type != core.SkillEventInvocationLinked {
		t.Fatalf("linked.Type = %q, want %q", linked.Type, core.SkillEventInvocationLinked)
	}
	if linked.SagaID != "inv-link" {
		t.Fatalf("linked.SagaID = %q, want invocation id", linked.SagaID)
	}
	if linked.CorrelationID != diary.SessionID {
		t.Fatalf("linked.CorrelationID = %q, want diary session id %q", linked.CorrelationID, diary.SessionID)
	}
	var payload core.SkillInvocationLinkedPayload
	if err := json.Unmarshal(linked.Payload, &payload); err != nil {
		t.Fatalf("json.Unmarshal(linked.Payload) error = %v", err)
	}
	if payload.InvocationID != "inv-link" || payload.LinkedFrom != diary.SessionID {
		t.Fatalf("payload = %+v, want invocation id %q linked from %q", payload, "inv-link", diary.SessionID)
	}

	// Replaying the identical close does not append a second linked event.
	if _, err := closeWithSkillInvocations(t, closer, "sha-link", []core.SkillInvocationRecord{record}); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	replayed := readSagaEvents(t, journal, "inv-link")
	if len(replayed) != 3 {
		t.Fatalf("len(replayed) = %d, want 3 (idempotent replay must not duplicate the linked event)", len(replayed))
	}
}

func TestSessionCloseSkillInvocationRenderingNeverPromotesNestedObservations(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	closer := newTestSessionCloser(t, bus)

	record := validSkillInvocationRecord()
	record.InvocationID = "inv-nested"
	record.Observations = core.SkillObservation{
		Decisions: []string{"nested decision"},
		Lessons:   []string{"nested lesson"},
	}

	data, err := json.Marshal(struct {
		Title            string                       `json:"title"`
		Summary          string                       `json:"summary"`
		Decisions        []string                     `json:"decisions"`
		Lessons          []string                     `json:"lessons"`
		SkillInvocations []core.SkillInvocationRecord `json:"skill_invocations"`
	}{
		Title: "Session", Summary: "did things",
		Decisions: []string{"top decision"}, Lessons: []string{"top lesson"},
		SkillInvocations: []core.SkillInvocationRecord{record},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	diary, _, err := closer.Close(context.Background(), core.SessionCloseRequest{
		RepositoryID: "repo-1", CommitSHA: "sha-nested", Author: "matt",
		Artifact: data, Format: core.ArtifactJSON, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(diary.Decisions) != 1 || diary.Decisions[0] != "top decision" {
		t.Fatalf("diary.Decisions = %v, want only the top-level decision", diary.Decisions)
	}
	if len(diary.Lessons) != 1 || diary.Lessons[0] != "top lesson" {
		t.Fatalf("diary.Lessons = %v, want only the top-level lesson", diary.Lessons)
	}
	if len(diary.SkillInvocations) != 1 || len(diary.SkillInvocations[0].Observations.Decisions) != 1 {
		t.Fatalf("diary.SkillInvocations = %+v, want the nested decision preserved", diary.SkillInvocations)
	}

	rendered := core.RenderSessionDiary(diary)
	if !containsInOrder(rendered, "## Skill Invocations", "- Decision: nested decision", "- Lesson: nested lesson") {
		t.Fatalf("rendered diary missing indented nested observations:\n%s", rendered)
	}
	decisionsSection := sectionBody(rendered, "## Decisions")
	if containsSubstring(decisionsSection, "nested decision") {
		t.Fatalf("top-level Decisions section leaked a nested skill decision:\n%s", decisionsSection)
	}
	lessonsSection := sectionBody(rendered, "## Lessons")
	if containsSubstring(lessonsSection, "nested lesson") {
		t.Fatalf("top-level Lessons section leaked a nested skill lesson:\n%s", lessonsSection)
	}
}

// Nested code reference resolution and artifact verification (issue #27)
// are covered by session_graph_artifacts_test.go. Without a configured
// GraphPort, nested code references simply pass through unresolved.
func TestSessionCloseCarriesNestedCodeReferencesThroughUnresolvedWithoutGraphPort(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	closer := newTestSessionCloser(t, bus)

	record := validSkillInvocationRecord()
	record.InvocationID = "inv-opaque"
	record.CodeReferences = []core.CodeReference{{Path: "core/skill.go", Symbol: "SkillPort"}}

	diary, err := closeWithSkillInvocations(t, closer, "sha-opaque", []core.SkillInvocationRecord{record})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	got := diary.SkillInvocations[0]
	if len(got.CodeReferences) != 1 || got.CodeReferences[0].Path != "core/skill.go" {
		t.Fatalf("CodeReferences = %+v, want passed through unchanged", got.CodeReferences)
	}
	if got.CodeReferences[0].GraphSnapshotID != "" {
		t.Fatalf("CodeReferences[0].GraphSnapshotID = %q, want unresolved without a configured GraphPort", got.CodeReferences[0].GraphSnapshotID)
	}
}

func containsInOrder(s string, parts ...string) bool {
	rest := s
	for _, p := range parts {
		idx := indexOf(rest, p)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(p):]
	}
	return true
}

func containsSubstring(s, substr string) bool { return indexOf(s, substr) >= 0 }

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// sectionBody returns the text between a "## " heading and the next "## "
// heading (or end of string), so a test can assert what a specific
// top-level Session Diary section does and does not contain.
func sectionBody(rendered, heading string) string {
	start := indexOf(rendered, heading)
	if start < 0 {
		return ""
	}
	start += len(heading)
	rest := rendered[start:]
	next := indexOf(rest, "\n## ")
	if next < 0 {
		return rest
	}
	return rest[:next]
}
