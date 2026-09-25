package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// SkillInvocationRecordSchemaVersion is the schema version recorded on every
// portable Skill Invocation Record. Per ADR 0010, readers migrate the
// current and immediately previous version at read time; only version 1
// exists so far.
const SkillInvocationRecordSchemaVersion = 1

var (
	// ErrSkillInvocationRecordInvalid reports that a Skill Invocation
	// Record is structurally invalid -- a missing identity, an
	// unsupported schema version, or an outcome outside the approved
	// vocabulary -- before any journal evidence is consulted.
	ErrSkillInvocationRecordInvalid = errors.New("skill invocation record is invalid")
	// ErrSkillInvocationsSectionInvalid reports that a Markdown Session
	// Artifact's "Skill Invocations" section is missing its one required
	// strict JSON array block, contains more than one block, or contains
	// more than one such heading. Parsing never guesses at execution
	// evidence, per ADR 0028.
	ErrSkillInvocationsSectionInvalid = errors.New("skill invocations section is invalid")
	// ErrSkillInvocationContradiction reports that a runtime-declared
	// record's immutable fields -- name, package hash, runtime,
	// arguments, or result -- disagree with local Saga Journal evidence
	// for the same invocation ID. Divergent provenance is never silently
	// accepted.
	ErrSkillInvocationContradiction = errors.New("skill invocation record contradicts local journal evidence")
)

// SkillInvocationOutcome is the runtime-declared result of one Skill
// Invocation. Per ADR 0028, a failed outcome is historical evidence, never
// a synchronization obligation or an automatic retry.
type SkillInvocationOutcome string

const (
	SkillInvocationOutcomeSucceeded SkillInvocationOutcome = "succeeded"
	SkillInvocationOutcomeFailed    SkillInvocationOutcome = "failed"
)

// SkillInvocationProvenance classifies how a Skill Invocation Record's
// immutable fields were established. It is always computed by session
// close reconciliation against the local Saga Journal; a runtime cannot
// declare it directly.
type SkillInvocationProvenance string

const (
	// SkillInvocationJournalVerified means the record's immutable fields
	// matched local Saga Journal evidence for its invocation ID.
	SkillInvocationJournalVerified SkillInvocationProvenance = "journal-verified"
	// SkillInvocationRuntimeDeclared means no local Saga Journal evidence
	// was available for the record's invocation ID, so its fields are
	// trusted as reported by the runtime alone.
	SkillInvocationRuntimeDeclared SkillInvocationProvenance = "runtime-declared"
)

// SkillObservation nests a Skill Invocation's own decisions and lessons so
// Session Diary rendering never promotes them into top-level memory (ADR
// 0028; issue #26 acceptance criteria).
type SkillObservation struct {
	Decisions []string `json:"decisions,omitempty"`
	Lessons   []string `json:"lessons,omitempty"`
}

// SkillInvocationArtifact is repository-relative, runtime-declared metadata
// about a file a Skill Invocation produced. It is opaque pass-through data
// at this layer: verifying it against the filesystem (hash, media type,
// size, availability) is issue #27's responsibility.
type SkillInvocationArtifact struct {
	Path         string `json:"path"`
	SHA256       string `json:"sha256,omitempty"`
	MediaType    string `json:"media_type,omitempty"`
	SizeBytes    int64  `json:"size_bytes,omitempty"`
	Availability string `json:"availability,omitempty"`
}

// SkillInvocationRecord is the portable, runtime-neutral record of one
// Skill Invocation carried by a Session Artifact (ADR 0028). Session close
// reconciles it against available Saga Journal evidence and, once
// accepted, preserves it in a dedicated Session Diary section without
// flattening its nested observations into top-level decisions or lessons.
//
// Nested code references and artifact metadata are opaque pass-through
// data at this layer -- resolving code references through GraphPort and
// verifying artifacts against the filesystem is issue #27's
// responsibility, not this one's.
type SkillInvocationRecord struct {
	SchemaVersion       int                       `json:"schema_version"`
	InvocationID        string                    `json:"invocation_id"`
	RelatedInvocationID string                    `json:"related_invocation_id,omitempty"`
	PackageIdentity     SkillPackageIdentity      `json:"package_identity"`
	BundleHash          string                    `json:"bundle_hash,omitempty"`
	Runtime             string                    `json:"runtime,omitempty"`
	Arguments           map[string]any            `json:"arguments,omitempty"`
	Outcome             SkillInvocationOutcome    `json:"outcome"`
	Summary             string                    `json:"summary,omitempty"`
	Observations        SkillObservation          `json:"observations,omitempty"`
	CodeReferences      []CodeReference           `json:"code_references,omitempty"`
	Artifacts           []SkillInvocationArtifact `json:"artifacts,omitempty"`
	EventReferences     []string                  `json:"event_references,omitempty"`
	Provenance          SkillInvocationProvenance `json:"provenance,omitempty"`
}

// validateSkillInvocationRecord checks structural validity only -- it never
// consults the Saga Journal. Provenance and EventReferences are always
// Core-computed during session close reconciliation, so any caller-supplied
// value is cleared here rather than trusted.
func validateSkillInvocationRecord(r SkillInvocationRecord) (SkillInvocationRecord, error) {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = SkillInvocationRecordSchemaVersion
	}
	if r.SchemaVersion != SkillInvocationRecordSchemaVersion {
		return SkillInvocationRecord{}, fmt.Errorf("%w: schema version must be %d", ErrSkillInvocationRecordInvalid, SkillInvocationRecordSchemaVersion)
	}
	r.InvocationID = strings.TrimSpace(r.InvocationID)
	if r.InvocationID == "" {
		return SkillInvocationRecord{}, fmt.Errorf("%w: invocation id is required", ErrSkillInvocationRecordInvalid)
	}
	r.RelatedInvocationID = strings.TrimSpace(r.RelatedInvocationID)
	r.PackageIdentity.SourceID = strings.TrimSpace(r.PackageIdentity.SourceID)
	r.PackageIdentity.Name = strings.TrimSpace(r.PackageIdentity.Name)
	if r.PackageIdentity.SourceID == "" || r.PackageIdentity.Name == "" {
		return SkillInvocationRecord{}, fmt.Errorf("%w: package identity source and name are required", ErrSkillInvocationRecordInvalid)
	}
	if len(r.PackageIdentity.PackageHash) != sha256.Size*2 {
		return SkillInvocationRecord{}, fmt.Errorf("%w: package hash must be a SHA-256 hex digest", ErrSkillInvocationRecordInvalid)
	}
	if _, err := hex.DecodeString(r.PackageIdentity.PackageHash); err != nil {
		return SkillInvocationRecord{}, fmt.Errorf("%w: package hash must be a SHA-256 hex digest", ErrSkillInvocationRecordInvalid)
	}
	r.BundleHash = strings.TrimSpace(r.BundleHash)
	r.Runtime = strings.TrimSpace(r.Runtime)
	r.Outcome = SkillInvocationOutcome(strings.ToLower(strings.TrimSpace(string(r.Outcome))))
	if r.Outcome != SkillInvocationOutcomeSucceeded && r.Outcome != SkillInvocationOutcomeFailed {
		return SkillInvocationRecord{}, fmt.Errorf("%w: outcome must be %q or %q", ErrSkillInvocationRecordInvalid, SkillInvocationOutcomeSucceeded, SkillInvocationOutcomeFailed)
	}
	r.Summary = strings.TrimSpace(r.Summary)
	for i := range r.CodeReferences {
		r.CodeReferences[i].Path = strings.TrimSpace(r.CodeReferences[i].Path)
		if r.CodeReferences[i].Confidence == "" {
			r.CodeReferences[i].Confidence = ConfidenceAuthoritative
		}
		if r.CodeReferences[i].Path == "" {
			return SkillInvocationRecord{}, fmt.Errorf("%w: nested code reference path is required", ErrSkillInvocationRecordInvalid)
		}
	}
	for i := range r.Artifacts {
		r.Artifacts[i].Path = strings.TrimSpace(r.Artifacts[i].Path)
		if r.Artifacts[i].Path == "" {
			return SkillInvocationRecord{}, fmt.Errorf("%w: artifact path is required", ErrSkillInvocationRecordInvalid)
		}
	}
	normalizedArguments, err := normalizeSkillArguments(r.Arguments)
	if err != nil {
		return SkillInvocationRecord{}, fmt.Errorf("%w: %v", ErrSkillInvocationRecordInvalid, err)
	}
	r.Arguments = normalizedArguments
	// Provenance and event references are Core-computed during
	// reconciliation; never trust an artifact-supplied value.
	r.Provenance = ""
	r.EventReferences = nil
	return r, nil
}

// skillJournalEvidence is the local Saga Journal's evidence for one
// invocation ID, gathered from whichever skill events already exist for
// it. identityFrom/outcomeFrom record which event contributed each part of
// the evidence so reconciliation can report EventReferences precisely.
type skillJournalEvidence struct {
	found bool

	identity     SkillPackageIdentity
	bundleHash   string
	runtime      string
	arguments    map[string]any
	identityFrom string

	hasOutcome  bool
	outcome     SkillInvocationOutcome
	outcomeFrom string
}

// gatherSkillJournalEvidence reads one invocation's saga segment and
// extracts whatever identity and outcome evidence its own skill events
// already carry. It never resolves nested code references or artifacts --
// those remain opaque pass-through data at this layer (issue #27).
func gatherSkillJournalEvidence(ctx context.Context, journal Journal, invocationID string) (skillJournalEvidence, error) {
	var ev skillJournalEvidence
	if journal == nil {
		return ev, nil
	}
	records, err := journal.ReadSaga(ctx, invocationID)
	if err != nil {
		return skillJournalEvidence{}, err
	}
	for _, record := range records {
		if record.Kind != "event" || record.Event == nil {
			continue
		}
		e := record.Event
		switch e.Type {
		case SkillEventPrepared:
			var payload SkillPreparedPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue
			}
			ev.found = true
			ev.identity = payload.PackageIdentity
			ev.bundleHash = payload.BundleHash
			ev.runtime = payload.Runtime
			ev.arguments = payload.Arguments
			ev.identityFrom = e.EventID
		case SkillEventInvocationStarted:
			var payload SkillInvocationStartedPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue
			}
			ev.found = true
			if ev.identityFrom == "" {
				ev.identity = payload.PackageIdentity
				ev.bundleHash = payload.BundleHash
				ev.runtime = payload.Runtime
				ev.identityFrom = e.EventID
			}
		case SkillEventInvocationCompleted:
			var payload SkillInvocationCompletedPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue
			}
			ev.found = true
			ev.hasOutcome = true
			ev.outcome = SkillInvocationOutcomeSucceeded
			ev.outcomeFrom = e.EventID
			if ev.identityFrom == "" {
				ev.identity = payload.PackageIdentity
				ev.bundleHash = payload.BundleHash
				ev.runtime = payload.Runtime
				ev.identityFrom = e.EventID
			}
		case SkillEventInvocationFailed:
			var payload SkillInvocationFailedPayload
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				continue
			}
			ev.found = true
			ev.hasOutcome = true
			ev.outcome = SkillInvocationOutcomeFailed
			ev.outcomeFrom = e.EventID
			if ev.identityFrom == "" {
				ev.identity = payload.PackageIdentity
				ev.bundleHash = payload.BundleHash
				ev.runtime = payload.Runtime
				ev.identityFrom = e.EventID
			}
		case SkillEventPrepareFailed:
			// Preparation never produced a bundle, so no identity
			// evidence is available -- but it is still proof the
			// invocation could not have succeeded.
			ev.found = true
			ev.hasOutcome = true
			ev.outcome = SkillInvocationOutcomeFailed
			ev.outcomeFrom = e.EventID
		}
	}
	return ev, nil
}

// reconcileSkillInvocationRecord classifies one already-structurally-valid
// record against local Saga Journal evidence for its invocation ID. A
// record whose invocation ID has no local evidence remains
// runtime-declared; a record whose immutable fields agree with local
// evidence becomes journal-verified; a record that disagrees is rejected
// explicitly, per ADR 0028.
func reconcileSkillInvocationRecord(ctx context.Context, journal Journal, record SkillInvocationRecord) (SkillInvocationRecord, error) {
	evidence, err := gatherSkillJournalEvidence(ctx, journal, record.InvocationID)
	if err != nil {
		return SkillInvocationRecord{}, err
	}
	if !evidence.found {
		record.Provenance = SkillInvocationRuntimeDeclared
		record.EventReferences = nil
		return record, nil
	}

	var contradictions []string
	if evidence.identityFrom != "" {
		if evidence.identity.QualifiedName() != record.PackageIdentity.QualifiedName() {
			contradictions = append(contradictions, fmt.Sprintf("name (record %q, journal %q)", record.PackageIdentity.QualifiedName(), evidence.identity.QualifiedName()))
		}
		if evidence.identity.PackageHash != record.PackageIdentity.PackageHash {
			contradictions = append(contradictions, "package hash")
		}
		if evidence.runtime != record.Runtime {
			contradictions = append(contradictions, fmt.Sprintf("runtime (record %q, journal %q)", record.Runtime, evidence.runtime))
		}
		if evidence.bundleHash != "" && record.BundleHash != "" && evidence.bundleHash != record.BundleHash {
			contradictions = append(contradictions, "bundle hash")
		}
		if !reflect.DeepEqual(evidence.arguments, record.Arguments) {
			contradictions = append(contradictions, "arguments")
		}
	}
	if evidence.hasOutcome && evidence.outcome != record.Outcome {
		contradictions = append(contradictions, fmt.Sprintf("result (record %q, journal %q)", record.Outcome, evidence.outcome))
	}
	if len(contradictions) > 0 {
		return SkillInvocationRecord{}, fmt.Errorf("%w: invocation %s: %s", ErrSkillInvocationContradiction, record.InvocationID, strings.Join(contradictions, "; "))
	}

	var refs []string
	if evidence.identityFrom != "" {
		refs = append(refs, evidence.identityFrom)
	}
	if evidence.outcomeFrom != "" && evidence.outcomeFrom != evidence.identityFrom {
		refs = append(refs, evidence.outcomeFrom)
	}
	record.Provenance = SkillInvocationJournalVerified
	record.EventReferences = refs
	return record, nil
}

// skillInvocationsHeading is the exact (case-insensitive) Markdown heading
// text a Session Artifact's Skill Invocations section must use.
const skillInvocationsHeading = "skill invocations"

// extractSkillInvocationsSection scans normalized (LF-only) Markdown for a
// "## Skill Invocations" heading, requires exactly one fenced code block
// containing a strict JSON array under it, and returns the decoded records
// plus the remaining Markdown with that section removed so the rest of the
// artifact parser never sees its fenced JSON as bullets. A missing block, a
// malformed block, more than one block, or more than one such heading is
// rejected explicitly rather than guessed at.
func extractSkillInvocationsSection(markdown string) ([]SkillInvocationRecord, string, error) {
	lines := strings.Split(markdown, "\n")
	var remainder []string
	var records []SkillInvocationRecord
	found := false
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "## ") && strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")), skillInvocationsHeading) {
			if found {
				return nil, "", fmt.Errorf("%w: duplicate %q section", ErrSkillInvocationsSectionInvalid, "Skill Invocations")
			}
			found = true

			j := i + 1
			inFence := false
			blockCount := 0
			var fenceLines []string
			var block string
			for ; j < len(lines); j++ {
				bodyTrim := strings.TrimSpace(lines[j])
				if !inFence && strings.HasPrefix(bodyTrim, "## ") {
					break
				}
				if strings.HasPrefix(bodyTrim, "```") {
					if !inFence {
						inFence = true
						fenceLines = nil
						continue
					}
					inFence = false
					blockCount++
					if blockCount > 1 {
						return nil, "", fmt.Errorf("%w: more than one JSON block under %q", ErrSkillInvocationsSectionInvalid, "Skill Invocations")
					}
					block = strings.Join(fenceLines, "\n")
					continue
				}
				if inFence {
					fenceLines = append(fenceLines, lines[j])
				}
			}
			if inFence {
				return nil, "", fmt.Errorf("%w: unterminated JSON code block under %q", ErrSkillInvocationsSectionInvalid, "Skill Invocations")
			}
			if blockCount == 0 {
				return nil, "", fmt.Errorf("%w: exactly one strict JSON array block is required under %q", ErrSkillInvocationsSectionInvalid, "Skill Invocations")
			}
			decoder := json.NewDecoder(strings.NewReader(block))
			var decoded []SkillInvocationRecord
			if err := decoder.Decode(&decoded); err != nil {
				return nil, "", fmt.Errorf("%w: malformed JSON array under %q: %v", ErrSkillInvocationsSectionInvalid, "Skill Invocations", err)
			}
			if decoder.More() {
				return nil, "", fmt.Errorf("%w: trailing content after JSON array under %q", ErrSkillInvocationsSectionInvalid, "Skill Invocations")
			}
			records = decoded
			// Consume the heading and its entire body; the outer loop
			// resumes at the next heading (or EOF) already found by j.
			i = j - 1
			continue
		}
		remainder = append(remainder, lines[i])
	}
	return records, strings.Join(remainder, "\n"), nil
}
