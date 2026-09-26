package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SkillEventSchemaVersion is the schema version recorded on every skill
// preparation and invocation event. Per ADR 0010, schema versions increase
// per event type; SagaJournal.ReadSaga already defaults a zero envelope
// SchemaVersion to 1 at read time (migrateEvent), which is also the only
// schema version skill events have ever used, so no additional migration
// code is needed until a second skill event schema version exists.
const SkillEventSchemaVersion = 1

// Skill event types recorded in the Saga Journal. Preparation events
// (prepare-requested, prepared, prepare-failed) are emitted by
// SkillPreparer in this slice. Invocation events are defined here so a
// Session Diary and a future RuntimePort adapter share one vocabulary, but
// no adapter in this slice emits them; that begins once runtime reporting
// is implemented (see issues #26 and #27).
const (
	SkillEventPrepareRequested = "skill.prepare-requested"
	SkillEventPrepared         = "skill.prepared"
	SkillEventPrepareFailed    = "skill.prepare-failed"

	SkillEventInvocationStarted   = "skill.invocation-started"
	SkillEventInvocationCompleted = "skill.invocation-completed"
	SkillEventInvocationFailed    = "skill.invocation-failed"
	SkillEventInvocationLinked    = "skill.invocation-linked"
)

// SkillPrepareFailureClass buckets a prepare failure into a stable,
// dashboard-safe category. It lets a reader classify what happened without
// parsing the bounded, redacted diagnostic text.
type SkillPrepareFailureClass string

const (
	SkillPrepareFailureUnsupportedPackage  SkillPrepareFailureClass = "UnsupportedSkillPackage"
	SkillPrepareFailureNotFound            SkillPrepareFailureClass = "SkillNotFound"
	SkillPrepareFailureAmbiguousName       SkillPrepareFailureClass = "AmbiguousName"
	SkillPrepareFailureInvalidAlias        SkillPrepareFailureClass = "InvalidAlias"
	SkillPrepareFailureRuntimeIncompatible SkillPrepareFailureClass = "RuntimeIncompatible"
	SkillPrepareFailureInvalidArguments    SkillPrepareFailureClass = "InvalidArguments"
	SkillPrepareFailureReplayConflict      SkillPrepareFailureClass = "ReplayConflict"
	SkillPrepareFailureIDCollision         SkillPrepareFailureClass = "InvocationIDCollision"
	SkillPrepareFailureOther               SkillPrepareFailureClass = "PrepareFailed"
)

// classifySkillPrepareFailure maps a Prepare error to a stable failure
// class using errors.Is, so wrapped errors from an adapter still classify
// correctly.
func classifySkillPrepareFailure(err error) SkillPrepareFailureClass {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnsupportedSkill):
		return SkillPrepareFailureUnsupportedPackage
	case errors.Is(err, ErrSkillNotFound):
		return SkillPrepareFailureNotFound
	case errors.Is(err, ErrSkillNameAmbiguous):
		return SkillPrepareFailureAmbiguousName
	case errors.Is(err, ErrSkillAliasInvalid):
		return SkillPrepareFailureInvalidAlias
	case errors.Is(err, ErrSkillRuntimeIncompatible):
		return SkillPrepareFailureRuntimeIncompatible
	case errors.Is(err, ErrSkillInvocationReplay):
		return SkillPrepareFailureReplayConflict
	case errors.Is(err, ErrSkillInvocationIDCollision):
		return SkillPrepareFailureIDCollision
	case errors.Is(err, ErrSkillArgumentsInvalid), errors.Is(err, ErrSkillArgumentsSchemaInvalid):
		return SkillPrepareFailureInvalidArguments
	default:
		return SkillPrepareFailureOther
	}
}

// SkillPrepareRequestedPayload is durable evidence that a Skill Invocation
// saga began. It is journaled before SkillPort.Prepare runs, so a crash
// during preparation still leaves an explanation behind. It carries only
// the caller's descriptor of intent and its auditable arguments -- never
// package instructions, which are not resolved until Prepare runs.
type SkillPrepareRequestedPayload struct {
	InvocationID  string         `json:"invocation_id"`
	RequestedName string         `json:"requested_name"`
	Runtime       string         `json:"runtime,omitempty"`
	Arguments     map[string]any `json:"arguments,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
}

// SkillPreparedPayload is durable evidence that preparation succeeded. It
// carries the resolved package's provenance and hashes, never its
// instructions Markdown.
type SkillPreparedPayload struct {
	InvocationID    string               `json:"invocation_id"`
	PackageIdentity SkillPackageIdentity `json:"package_identity"`
	BundleHash      string               `json:"bundle_hash"`
	Runtime         string               `json:"runtime,omitempty"`
	Arguments       map[string]any       `json:"arguments,omitempty"`
	SessionID       string               `json:"session_id,omitempty"`
}

// SkillPrepareFailedPayload is durable evidence that preparation failed. It
// carries a stable failure class plus a bounded, redacted diagnostic --
// never the caller's raw error text, and never package instructions.
type SkillPrepareFailedPayload struct {
	InvocationID  string                   `json:"invocation_id"`
	RequestedName string                   `json:"requested_name"`
	Runtime       string                   `json:"runtime,omitempty"`
	Arguments     map[string]any           `json:"arguments,omitempty"`
	FailureClass  SkillPrepareFailureClass `json:"failure_class"`
	Diagnostic    string                   `json:"diagnostic,omitempty"`
	SessionID     string                   `json:"session_id,omitempty"`
}

// SkillInvocationStartedPayload records that a prepared bundle began
// runtime execution. Core defines this payload now so a future RuntimePort
// adapter and Session Diaries share one event vocabulary, but no adapter in
// this slice emits it.
type SkillInvocationStartedPayload struct {
	InvocationID    string               `json:"invocation_id"`
	PackageIdentity SkillPackageIdentity `json:"package_identity"`
	BundleHash      string               `json:"bundle_hash"`
	Runtime         string               `json:"runtime,omitempty"`
	SessionID       string               `json:"session_id,omitempty"`
}

// SkillInvocationCompletedPayload records that a started invocation
// finished successfully. Nested observations, code references, and
// artifact metadata belong to the portable Skill Invocation Record (see
// ADR 0028 and issues #26/#27), not this bounded journal event.
type SkillInvocationCompletedPayload struct {
	InvocationID    string               `json:"invocation_id"`
	PackageIdentity SkillPackageIdentity `json:"package_identity"`
	BundleHash      string               `json:"bundle_hash"`
	Runtime         string               `json:"runtime,omitempty"`
	Summary         string               `json:"summary,omitempty"`
	SessionID       string               `json:"session_id,omitempty"`
}

// SkillInvocationFailedPayload records that a started invocation failed at
// runtime. Per ADR 0028, this is historical evidence, never a
// synchronization obligation or an automatic retry; an intentional retry
// creates a new Skill Invocation and links back with
// SkillInvocationLinkedPayload.
type SkillInvocationFailedPayload struct {
	InvocationID    string               `json:"invocation_id"`
	PackageIdentity SkillPackageIdentity `json:"package_identity"`
	BundleHash      string               `json:"bundle_hash"`
	Runtime         string               `json:"runtime,omitempty"`
	Diagnostic      string               `json:"diagnostic,omitempty"`
	SessionID       string               `json:"session_id,omitempty"`
}

// SkillInvocationLinkedPayload lets an independently created saga -- for
// example, session close -- point back at a Skill Invocation saga without
// rewriting any of its prior events.
type SkillInvocationLinkedPayload struct {
	InvocationID string `json:"invocation_id"`
	LinkedFrom   string `json:"linked_from"`
}

// SkillPrepareOptions carries journaling-only context for SkillPreparer.
// None of these fields participate in the prepared bundle's identity or
// hash; they only shape the Saga Journal envelope around an otherwise
// unchanged Prepare call.
type SkillPrepareOptions struct {
	// RepositoryID identifies the Repository Installation recorded on
	// every journaled skill event.
	RepositoryID string
	// SessionID optionally correlates this Skill Invocation with a
	// runtime session. When empty, correlation falls back to the
	// invocation's own identity, per ADR 0028.
	SessionID string
	// Now overrides the clock used for journaled events; it defaults to
	// time.Now().UTC() when zero.
	Now time.Time
}

// SkillPreparer journals the preparation lifecycle of one Skill Invocation
// around an otherwise unmodified SkillPort. One Skill Invocation owns its
// saga: SagaID is the invocation ID, and phase event IDs are derived from
// it so a technical replay with the same invocation ID reproduces the same
// event identities instead of appending new ones.
//
// When Bus is nil, or its journal is not a *SagaJournal, SkillPreparer
// delegates directly to Port without journaling -- matching
// SessionCloser's rule that a caller without a durable journal only gets
// the effects that are safe without one. SkillPort.Prepare performs no
// external effect (ADR 0026), so that delegation is safe here.
type SkillPreparer struct {
	Port SkillPort
	Bus  *EventBus
}

// Prepare journals a prepare-requested event before invoking Port.Prepare,
// then journals a Handler Attempt and a prepared or prepare-failed outcome
// event. The prepare-requested event is durable before preparation runs,
// so a crash mid-preparation still leaves an explanation in the journal.
func (p SkillPreparer) Prepare(ctx context.Context, request SkillPrepareRequest, opts SkillPrepareOptions) (SkillBundle, Delivery, error) {
	if p.Port == nil {
		return SkillBundle{}, Delivery{}, errors.New("skill port is required")
	}
	if p.Bus == nil {
		bundle, err := p.Port.Prepare(ctx, request)
		return bundle, Delivery{}, err
	}
	journal, ok := p.Bus.journal.(*SagaJournal)
	if !ok {
		bundle, err := p.Port.Prepare(ctx, request)
		return bundle, Delivery{}, err
	}
	if err := ctx.Err(); err != nil {
		return SkillBundle{}, Delivery{}, err
	}

	invocationID := strings.TrimSpace(request.InvocationID)
	if invocationID == "" {
		generated, err := randomInvocationID()
		if err != nil {
			return SkillBundle{}, Delivery{}, fmt.Errorf("create skill invocation identity: %w", err)
		}
		invocationID = generated
	}
	// Once assigned, the invocation ID is always passed to Port explicitly.
	// This keeps journaled event IDs and the prepared bundle's identity in
	// lockstep, whether the caller supplied the ID (a technical replay) or
	// this method generated it (an intentional new use).
	request.InvocationID = invocationID

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	correlationID := strings.TrimSpace(opts.SessionID)
	if correlationID == "" {
		correlationID = invocationID
	}
	requestedEventID := invocationID + ":prepare-requested"

	requestedPayload, err := json.Marshal(SkillPrepareRequestedPayload{
		InvocationID:  invocationID,
		RequestedName: strings.TrimSpace(request.Name),
		Runtime:       strings.TrimSpace(request.Runtime),
		Arguments:     request.Arguments,
		SessionID:     strings.TrimSpace(opts.SessionID),
	})
	if err != nil {
		return SkillBundle{}, Delivery{}, fmt.Errorf("encode skill prepare-requested payload: %w", err)
	}
	requestedEvent := Event{
		EventID:       requestedEventID,
		Type:          SkillEventPrepareRequested,
		OccurredAt:    now,
		RepositoryID:  opts.RepositoryID,
		SagaID:        invocationID,
		CorrelationID: correlationID,
		SchemaVersion: SkillEventSchemaVersion,
		Payload:       requestedPayload,
	}
	if err := journal.AppendEvent(ctx, requestedEvent); err != nil {
		return SkillBundle{}, Delivery{}, err
	}

	bundle, prepareErr := p.Port.Prepare(ctx, request)

	attempt := HandlerAttempt{
		EventID:     requestedEventID,
		SagaID:      invocationID,
		HandlerID:   "skill-prepare",
		AttemptedAt: time.Now().UTC(),
	}
	if prepareErr != nil {
		attempt.Outcome = "failed"
		attempt.Error = prepareErr.Error()
	} else {
		attempt.Outcome = "succeeded"
	}
	// AppendAttempt bounds and redacts attempt.Error before it crosses the
	// journal boundary, matching every other Handler Attempt in the repo.
	if err := journal.AppendAttempt(ctx, attempt); err != nil {
		return bundle, Delivery{EventID: requestedEventID, Attempts: []HandlerAttempt{attempt}}, err
	}
	delivery := Delivery{EventID: requestedEventID, Attempts: []HandlerAttempt{attempt}}

	if prepareErr != nil {
		failedPayload, encodeErr := json.Marshal(SkillPrepareFailedPayload{
			InvocationID:  invocationID,
			RequestedName: strings.TrimSpace(request.Name),
			Runtime:       strings.TrimSpace(request.Runtime),
			Arguments:     request.Arguments,
			FailureClass:  classifySkillPrepareFailure(prepareErr),
			Diagnostic:    SafeStorageDiagnostic(prepareErr.Error()),
			SessionID:     strings.TrimSpace(opts.SessionID),
		})
		if encodeErr != nil {
			return bundle, delivery, fmt.Errorf("encode skill prepare-failed payload: %w", encodeErr)
		}
		failedEvent := Event{
			EventID:       invocationID + ":prepare-failed",
			Type:          SkillEventPrepareFailed,
			OccurredAt:    time.Now().UTC(),
			RepositoryID:  opts.RepositoryID,
			SagaID:        invocationID,
			CorrelationID: correlationID,
			CausationID:   requestedEventID,
			SchemaVersion: SkillEventSchemaVersion,
			Payload:       failedPayload,
		}
		if err := journal.AppendEvent(ctx, failedEvent); err != nil {
			return bundle, delivery, err
		}
		return bundle, delivery, prepareErr
	}

	preparedPayload, encodeErr := json.Marshal(SkillPreparedPayload{
		InvocationID:    invocationID,
		PackageIdentity: bundle.PackageIdentity(),
		BundleHash:      bundle.BundleHash(),
		Runtime:         bundle.Runtime(),
		Arguments:       bundle.Arguments(),
		SessionID:       strings.TrimSpace(opts.SessionID),
	})
	if encodeErr != nil {
		return bundle, delivery, fmt.Errorf("encode skill prepared payload: %w", encodeErr)
	}
	preparedEvent := Event{
		EventID:       invocationID + ":prepared",
		Type:          SkillEventPrepared,
		OccurredAt:    time.Now().UTC(),
		RepositoryID:  opts.RepositoryID,
		SagaID:        invocationID,
		CorrelationID: correlationID,
		CausationID:   requestedEventID,
		SchemaVersion: SkillEventSchemaVersion,
		Payload:       preparedPayload,
	}
	if err := journal.AppendEvent(ctx, preparedEvent); err != nil {
		return bundle, delivery, err
	}
	return bundle, delivery, nil
}
