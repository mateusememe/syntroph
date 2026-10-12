package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/core"
)

// journalCheckingPort wraps an inner SkillPort and records, at the moment
// Prepare runs, whether the saga's prepare-requested event is already
// durable in the journal. This proves SkillPreparer journals the request
// before performing the (safe, but still observed) preparation effect.
type journalCheckingPort struct {
	journal      *core.SagaJournal
	inner        core.SkillPort
	sawRequested bool
}

func (p *journalCheckingPort) Prepare(ctx context.Context, request core.SkillPrepareRequest) (core.SkillBundle, error) {
	records, err := p.journal.ReadSaga(ctx, request.InvocationID)
	if err == nil {
		for _, record := range records {
			if record.Kind == "event" && record.Event != nil && record.Event.Type == core.SkillEventPrepareRequested {
				p.sawRequested = true
			}
		}
	}
	return p.inner.Prepare(ctx, request)
}

// stubSkillPort returns a fixed bundle or error, regardless of request,
// letting tests exercise SkillPreparer's journaling behavior in isolation
// from InMemorySkillPort's own validation rules.
type stubSkillPort struct {
	bundle core.SkillBundle
	err    error
}

func (p stubSkillPort) Prepare(context.Context, core.SkillPrepareRequest) (core.SkillBundle, error) {
	return p.bundle, p.err
}

func newTestSkillJournalBus(t *testing.T) (*core.SagaJournal, *core.EventBus) {
	t.Helper()
	journal, err := core.NewSagaJournal(t.TempDir())
	if err != nil {
		t.Fatalf("NewSagaJournal() error = %v", err)
	}
	bus, err := core.NewEventBus(journal)
	if err != nil {
		t.Fatalf("NewEventBus() error = %v", err)
	}
	return journal, bus
}

func newTestSkillPort(t *testing.T) core.SkillPort {
	t.Helper()
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, nil, nil)
	if err != nil {
		t.Fatalf("NewInMemorySkillPort() error = %v", err)
	}
	return port
}

func readSagaEvents(t *testing.T, journal *core.SagaJournal, sagaID string) []core.Event {
	t.Helper()
	records, err := journal.ReadSaga(context.Background(), sagaID)
	if err != nil {
		t.Fatalf("ReadSaga(%q) error = %v", sagaID, err)
	}
	var events []core.Event
	for _, record := range records {
		if record.Kind == "event" && record.Event != nil {
			events = append(events, *record.Event)
		}
	}
	return events
}

func readSagaAttempts(t *testing.T, journal *core.SagaJournal, sagaID string) []core.HandlerAttempt {
	t.Helper()
	records, err := journal.ReadSaga(context.Background(), sagaID)
	if err != nil {
		t.Fatalf("ReadSaga(%q) error = %v", sagaID, err)
	}
	var attempts []core.HandlerAttempt
	for _, record := range records {
		if record.Kind == "attempt" && record.Attempt != nil {
			attempts = append(attempts, *record.Attempt)
		}
	}
	return attempts
}

func TestSkillPreparerJournalsPrepareRequestedBeforePrepareRuns(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	checker := &journalCheckingPort{journal: journal, inner: newTestSkillPort(t)}
	preparer := core.SkillPreparer{Port: checker, Bus: bus}

	_, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-order",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !checker.sawRequested {
		t.Fatal("prepare-requested was not durable before Port.Prepare ran")
	}
}

func TestSkillPreparerJournalsFullLifecycleOnSuccess(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}

	bundle, delivery, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-success",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1", SessionID: "session-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if bundle.InvocationID() != "inv-success" {
		t.Fatalf("bundle.InvocationID() = %q, want %q", bundle.InvocationID(), "inv-success")
	}
	if delivery.EventID != "inv-success:prepare-requested" {
		t.Fatalf("delivery.EventID = %q, want %q", delivery.EventID, "inv-success:prepare-requested")
	}
	if len(delivery.Attempts) != 1 || delivery.Attempts[0].Outcome != "succeeded" {
		t.Fatalf("delivery.Attempts = %+v, want one succeeded attempt", delivery.Attempts)
	}

	events := readSagaEvents(t, journal, "inv-success")
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2 (requested, prepared)", len(events))
	}
	requested, prepared := events[0], events[1]

	if requested.Type != core.SkillEventPrepareRequested {
		t.Fatalf("events[0].Type = %q, want %q", requested.Type, core.SkillEventPrepareRequested)
	}
	if requested.EventID != "inv-success:prepare-requested" {
		t.Fatalf("requested.EventID = %q", requested.EventID)
	}
	if requested.SagaID != "inv-success" {
		t.Fatalf("requested.SagaID = %q, want invocation id", requested.SagaID)
	}
	if requested.CorrelationID != "session-1" {
		t.Fatalf("requested.CorrelationID = %q, want session id", requested.CorrelationID)
	}
	if requested.CausationID != "" {
		t.Fatalf("requested.CausationID = %q, want empty (root event)", requested.CausationID)
	}
	if requested.RepositoryID != "repo-1" {
		t.Fatalf("requested.RepositoryID = %q", requested.RepositoryID)
	}
	if requested.SchemaVersion != core.SkillEventSchemaVersion {
		t.Fatalf("requested.SchemaVersion = %d, want %d", requested.SchemaVersion, core.SkillEventSchemaVersion)
	}

	if prepared.Type != core.SkillEventPrepared {
		t.Fatalf("events[1].Type = %q, want %q", prepared.Type, core.SkillEventPrepared)
	}
	if prepared.EventID != "inv-success:prepared" {
		t.Fatalf("prepared.EventID = %q", prepared.EventID)
	}
	if prepared.CausationID != requested.EventID {
		t.Fatalf("prepared.CausationID = %q, want %q", prepared.CausationID, requested.EventID)
	}
	if prepared.CorrelationID != "session-1" {
		t.Fatalf("prepared.CorrelationID = %q, want session id", prepared.CorrelationID)
	}

	var payload core.SkillPreparedPayload
	if err := json.Unmarshal(prepared.Payload, &payload); err != nil {
		t.Fatalf("unmarshal prepared payload: %v", err)
	}
	if payload.InvocationID != "inv-success" {
		t.Fatalf("payload.InvocationID = %q", payload.InvocationID)
	}
	if payload.BundleHash != bundle.BundleHash() {
		t.Fatalf("payload.BundleHash = %q, want %q", payload.BundleHash, bundle.BundleHash())
	}
	if payload.PackageIdentity != bundle.PackageIdentity() {
		t.Fatalf("payload.PackageIdentity = %+v, want %+v", payload.PackageIdentity, bundle.PackageIdentity())
	}
	if payload.SessionID != "session-1" {
		t.Fatalf("payload.SessionID = %q", payload.SessionID)
	}
}

func TestSkillPreparerCorrelationFallsBackToInvocationIDWithoutSession(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}

	_, delivery, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-no-session",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if delivery.EventID != "inv-no-session:prepare-requested" {
		t.Fatalf("delivery.EventID = %q", delivery.EventID)
	}
}

func TestSkillPreparerReplayWithSameInvocationIDProducesStableEventIDs(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}

	request := core.SkillPrepareRequest{Name: "mattpocock/code-review", InvocationID: "inv-replay"}
	firstBundle, firstDelivery, err := preparer.Prepare(context.Background(), request, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("first Prepare() error = %v", err)
	}
	secondBundle, secondDelivery, err := preparer.Prepare(context.Background(), request, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("second Prepare() error = %v", err)
	}

	if firstDelivery.EventID != secondDelivery.EventID {
		t.Fatalf("delivery event ids differ: %q vs %q", firstDelivery.EventID, secondDelivery.EventID)
	}
	if firstBundle.BundleHash() != secondBundle.BundleHash() {
		t.Fatalf("technical replay produced different bundle hashes: %q vs %q", firstBundle.BundleHash(), secondBundle.BundleHash())
	}

	events := readSagaEvents(t, journal, "inv-replay")
	if len(events) != 4 {
		t.Fatalf("len(events) = %d, want 4 (two requested + two prepared, same ids)", len(events))
	}
	if events[0].EventID != events[2].EventID {
		t.Fatalf("requested event ids not stable across replay: %q vs %q", events[0].EventID, events[2].EventID)
	}
	if events[1].EventID != events[3].EventID {
		t.Fatalf("prepared event ids not stable across replay: %q vs %q", events[1].EventID, events[3].EventID)
	}
}

func TestSkillPreparerJournalsPrepareFailedWithClassificationAndRedaction(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		class core.SkillPrepareFailureClass
	}{
		{"not-found", core.ErrSkillNotFound, core.SkillPrepareFailureNotFound},
		{"unsupported", core.ErrUnsupportedSkill, core.SkillPrepareFailureUnsupportedPackage},
		{"ambiguous", core.ErrSkillNameAmbiguous, core.SkillPrepareFailureAmbiguousName},
		{"invalid-alias", core.ErrSkillAliasInvalid, core.SkillPrepareFailureInvalidAlias},
		{"runtime-incompatible", core.ErrSkillRuntimeIncompatible, core.SkillPrepareFailureRuntimeIncompatible},
		{"replay-conflict", core.ErrSkillInvocationReplay, core.SkillPrepareFailureReplayConflict},
		{"id-collision", core.ErrSkillInvocationIDCollision, core.SkillPrepareFailureIDCollision},
		{"invalid-arguments", core.ErrSkillArgumentsInvalid, core.SkillPrepareFailureInvalidArguments},
		{"invalid-schema", core.ErrSkillArgumentsSchemaInvalid, core.SkillPrepareFailureInvalidArguments},
		{"other", errors.New("boom"), core.SkillPrepareFailureOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal, bus := newTestSkillJournalBus(t)
			wrapped := fmt.Errorf("prepare failed for https://user:s3cr3t@example.com/pkg: %w", tt.err)
			preparer := core.SkillPreparer{Port: stubSkillPort{err: wrapped}, Bus: bus}

			invocationID := "inv-" + tt.name
			_, delivery, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
				Name:         "mattpocock/code-review",
				InvocationID: invocationID,
			}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
			if !errors.Is(err, tt.err) {
				t.Fatalf("Prepare() error = %v, want wrapping %v", err, tt.err)
			}
			if len(delivery.Attempts) != 1 || delivery.Attempts[0].Outcome != "failed" {
				t.Fatalf("delivery.Attempts = %+v, want one failed attempt", delivery.Attempts)
			}

			// The durable Handler Attempt is what the acceptance criteria
			// require to be bounded and redacted; SagaJournal.AppendAttempt
			// enforces that on the copy it writes, matching every other
			// domain's Handler Attempts (see EventBus.Publish).
			attempts := readSagaAttempts(t, journal, invocationID)
			if len(attempts) != 1 || attempts[0].Outcome != "failed" {
				t.Fatalf("journaled attempts = %+v, want one failed attempt", attempts)
			}
			if strings.Contains(attempts[0].Error, "s3cr3t") {
				t.Fatalf("journaled handler attempt error was not redacted: %q", attempts[0].Error)
			}

			events := readSagaEvents(t, journal, invocationID)
			if len(events) != 2 {
				t.Fatalf("len(events) = %d, want 2 (requested, prepare-failed)", len(events))
			}
			failed := events[1]
			if failed.Type != core.SkillEventPrepareFailed {
				t.Fatalf("events[1].Type = %q, want %q", failed.Type, core.SkillEventPrepareFailed)
			}
			if failed.CausationID != events[0].EventID {
				t.Fatalf("failed.CausationID = %q, want %q", failed.CausationID, events[0].EventID)
			}

			var payload core.SkillPrepareFailedPayload
			if err := json.Unmarshal(failed.Payload, &payload); err != nil {
				t.Fatalf("unmarshal prepare-failed payload: %v", err)
			}
			if payload.FailureClass != tt.class {
				t.Fatalf("payload.FailureClass = %q, want %q", payload.FailureClass, tt.class)
			}
			if strings.Contains(payload.Diagnostic, "s3cr3t") {
				t.Fatalf("payload.Diagnostic leaked a credential: %q", payload.Diagnostic)
			}
		})
	}
}

func TestSkillPreparerPayloadsExcludeInstructionsMarkdown(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}

	_, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-no-instructions",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	entry := readySkillEntry()
	events := readSagaEvents(t, journal, "inv-no-instructions")
	for _, event := range events {
		if strings.Contains(string(event.Payload), entry.Package.Instructions) {
			t.Fatalf("event %q payload leaked instructions markdown: %s", event.Type, event.Payload)
		}
	}
}

func TestSkillPreparerNilBusDelegatesWithoutJournaling(t *testing.T) {
	port := newTestSkillPort(t)
	preparer := core.SkillPreparer{Port: port, Bus: nil}

	bundle, delivery, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-no-bus",
	}, core.SkillPrepareOptions{})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if bundle.InvocationID() != "inv-no-bus" {
		t.Fatalf("bundle.InvocationID() = %q", bundle.InvocationID())
	}
	if delivery.EventID != "" || len(delivery.Attempts) != 0 {
		t.Fatalf("delivery = %+v, want zero value", delivery)
	}
}

func TestSkillPreparerRequiresPort(t *testing.T) {
	preparer := core.SkillPreparer{}
	_, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review"}, core.SkillPrepareOptions{})
	if err == nil {
		t.Fatal("Prepare() with nil Port did not error")
	}
}

func TestSkillPreparerRespectsCanceledContext(t *testing.T) {
	_, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := preparer.Prepare(ctx, core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-canceled",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare() error = %v, want context.Canceled", err)
	}
}

func TestSkillPreparerNowOverrideIsUsedForRequestedEvent(t *testing.T) {
	journal, bus := newTestSkillJournalBus(t)
	preparer := core.SkillPreparer{Port: newTestSkillPort(t), Bus: bus}
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	_, _, err := preparer.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-fixed-clock",
	}, core.SkillPrepareOptions{RepositoryID: "repo-1", Now: fixed})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	events := readSagaEvents(t, journal, "inv-fixed-clock")
	if len(events) == 0 || !events[0].OccurredAt.Equal(fixed) {
		t.Fatalf("requested event OccurredAt = %v, want %v", events[0].OccurredAt, fixed)
	}
}
