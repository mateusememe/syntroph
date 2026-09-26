package core_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/core"
)

// testPackageHash and testOtherPackageHash are stand-in SHA-256 hex digests
// (their exact content is never checked, only their length and hex
// alphabet) so tests never depend on a real SkillPort's computed hash.
var (
	testPackageHash      = strings.Repeat("a", 64)
	testOtherPackageHash = strings.Repeat("b", 64)
)

func validSkillInvocationRecord() core.SkillInvocationRecord {
	return core.SkillInvocationRecord{
		InvocationID: "inv-1",
		PackageIdentity: core.SkillPackageIdentity{
			SourceID:    "mattpocock",
			Name:        "code-review",
			PackageHash: testPackageHash,
		},
		Runtime: "codex",
		Outcome: core.SkillInvocationOutcomeSucceeded,
		Summary: "reviewed the diff",
	}
}

func jsonArtifactWithSkillInvocations(t *testing.T, records []core.SkillInvocationRecord) []byte {
	t.Helper()
	artifact := struct {
		Title            string                       `json:"title"`
		Summary          string                       `json:"summary"`
		SkillInvocations []core.SkillInvocationRecord `json:"skill_invocations"`
	}{Title: "Session", Summary: "did things", SkillInvocations: records}
	b, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return b
}

func TestJSONSessionArtifactAcceptsSkillInvocationsDirectly(t *testing.T) {
	data := jsonArtifactWithSkillInvocations(t, []core.SkillInvocationRecord{validSkillInvocationRecord()})
	a, err := core.ParseSessionArtifact(data, core.ArtifactJSON)
	if err != nil {
		t.Fatalf("ParseSessionArtifact() error = %v", err)
	}
	if len(a.SkillInvocations) != 1 {
		t.Fatalf("len(a.SkillInvocations) = %d, want 1", len(a.SkillInvocations))
	}
	got := a.SkillInvocations[0]
	if got.InvocationID != "inv-1" || got.PackageIdentity.QualifiedName() != "mattpocock/code-review" {
		t.Fatalf("unexpected record: %+v", got)
	}
	if got.SchemaVersion != core.SkillInvocationRecordSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want default %d", got.SchemaVersion, core.SkillInvocationRecordSchemaVersion)
	}
}

// A runtime cannot pre-declare its own provenance verdict or forge event
// references; both are cleared during structural validation and only ever
// set by session close reconciliation.
func TestParseSessionArtifactClearsRuntimeSuppliedProvenanceAndEventReferences(t *testing.T) {
	record := validSkillInvocationRecord()
	record.Provenance = core.SkillInvocationJournalVerified
	record.EventReferences = []string{"forged-event"}
	data := jsonArtifactWithSkillInvocations(t, []core.SkillInvocationRecord{record})
	a, err := core.ParseSessionArtifact(data, core.ArtifactJSON)
	if err != nil {
		t.Fatalf("ParseSessionArtifact() error = %v", err)
	}
	got := a.SkillInvocations[0]
	if got.Provenance != "" {
		t.Fatalf("Provenance = %q, want cleared", got.Provenance)
	}
	if got.EventReferences != nil {
		t.Fatalf("EventReferences = %v, want cleared", got.EventReferences)
	}
}

func TestValidateSkillInvocationRecordStructuralRules(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(core.SkillInvocationRecord) core.SkillInvocationRecord
		wantErr error
	}{
		{
			name:    "missing invocation id",
			mutate:  func(r core.SkillInvocationRecord) core.SkillInvocationRecord { r.InvocationID = "  "; return r },
			wantErr: core.ErrSkillInvocationRecordInvalid,
		},
		{
			name: "missing package identity",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.PackageIdentity.Name = ""
				return r
			},
			wantErr: core.ErrSkillInvocationRecordInvalid,
		},
		{
			name: "malformed package hash",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.PackageIdentity.PackageHash = "not-a-hash"
				return r
			},
			wantErr: core.ErrSkillInvocationRecordInvalid,
		},
		{
			name:    "unsupported schema version",
			mutate:  func(r core.SkillInvocationRecord) core.SkillInvocationRecord { r.SchemaVersion = 99; return r },
			wantErr: core.ErrSkillInvocationRecordInvalid,
		},
		{
			name:    "unsupported outcome",
			mutate:  func(r core.SkillInvocationRecord) core.SkillInvocationRecord { r.Outcome = "maybe"; return r },
			wantErr: core.ErrSkillInvocationRecordInvalid,
		},
		{
			name: "missing nested code reference path",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.CodeReferences = []core.CodeReference{{}}
				return r
			},
			wantErr: core.ErrSkillInvocationRecordInvalid,
		},
		{
			name: "missing artifact path",
			mutate: func(r core.SkillInvocationRecord) core.SkillInvocationRecord {
				r.Artifacts = []core.SkillInvocationArtifact{{}}
				return r
			},
			wantErr: core.ErrSkillInvocationRecordInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := jsonArtifactWithSkillInvocations(t, []core.SkillInvocationRecord{tt.mutate(validSkillInvocationRecord())})
			_, err := core.ParseSessionArtifact(data, core.ArtifactJSON)
			if err == nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseSessionArtifact() error = %v, want wrapping %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateSkillInvocationRecordAcceptsOutcomeUppercaseAndTrimsWhitespace(t *testing.T) {
	record := validSkillInvocationRecord()
	record.Outcome = "  Succeeded  "
	data := jsonArtifactWithSkillInvocations(t, []core.SkillInvocationRecord{record})
	a, err := core.ParseSessionArtifact(data, core.ArtifactJSON)
	if err != nil {
		t.Fatalf("ParseSessionArtifact() error = %v", err)
	}
	if a.SkillInvocations[0].Outcome != core.SkillInvocationOutcomeSucceeded {
		t.Fatalf("Outcome = %q, want normalized %q", a.SkillInvocations[0].Outcome, core.SkillInvocationOutcomeSucceeded)
	}
}

func skillInvocationsMarkdownBlock(t *testing.T, records []core.SkillInvocationRecord) string {
	t.Helper()
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent() error = %v", err)
	}
	return string(b)
}

func TestMarkdownSessionArtifactAcceptsExactlyOneStrictSkillInvocationsBlock(t *testing.T) {
	block := skillInvocationsMarkdownBlock(t, []core.SkillInvocationRecord{validSkillInvocationRecord()})
	markdown := fmt.Sprintf("# Session\n\n%s\n\n## Skill Invocations\n\n```json\n%s\n```\n\n## Decisions\n\n- top-level decision\n", "Did some work", block)
	a, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err != nil {
		t.Fatalf("ParseSessionArtifact() error = %v", err)
	}
	if len(a.SkillInvocations) != 1 || a.SkillInvocations[0].InvocationID != "inv-1" {
		t.Fatalf("SkillInvocations = %+v, want one parsed record", a.SkillInvocations)
	}
	if len(a.Decisions) != 1 || a.Decisions[0] != "top-level decision" {
		t.Fatalf("Decisions = %v, want the section after Skill Invocations to still parse", a.Decisions)
	}
}

func TestMarkdownSessionArtifactWithoutSkillInvocationsHeadingIsUnaffected(t *testing.T) {
	markdown := "# Session\n\nDid some work\n\n## Decisions\n\n- a decision\n"
	a, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err != nil {
		t.Fatalf("ParseSessionArtifact() error = %v", err)
	}
	if len(a.SkillInvocations) != 0 {
		t.Fatalf("SkillInvocations = %+v, want none", a.SkillInvocations)
	}
}

func TestMarkdownSessionArtifactRejectsMissingSkillInvocationsBlock(t *testing.T) {
	markdown := "# Session\n\nDid some work\n\n## Skill Invocations\n\n## Decisions\n\n- a decision\n"
	_, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err == nil || !errors.Is(err, core.ErrSkillInvocationsSectionInvalid) {
		t.Fatalf("ParseSessionArtifact() error = %v, want ErrSkillInvocationsSectionInvalid", err)
	}
}

func TestMarkdownSessionArtifactRejectsDuplicateSkillInvocationsBlockInOneSection(t *testing.T) {
	block := skillInvocationsMarkdownBlock(t, []core.SkillInvocationRecord{validSkillInvocationRecord()})
	markdown := fmt.Sprintf("# Session\n\nDid some work\n\n## Skill Invocations\n\n```json\n%s\n```\n\n```json\n%s\n```\n", block, block)
	_, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err == nil || !errors.Is(err, core.ErrSkillInvocationsSectionInvalid) {
		t.Fatalf("ParseSessionArtifact() error = %v, want ErrSkillInvocationsSectionInvalid", err)
	}
}

func TestMarkdownSessionArtifactRejectsDuplicateSkillInvocationsSections(t *testing.T) {
	block := skillInvocationsMarkdownBlock(t, []core.SkillInvocationRecord{validSkillInvocationRecord()})
	markdown := fmt.Sprintf("# Session\n\nDid some work\n\n## Skill Invocations\n\n```json\n%s\n```\n\n## Skill Invocations\n\n```json\n%s\n```\n", block, block)
	_, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err == nil || !errors.Is(err, core.ErrSkillInvocationsSectionInvalid) {
		t.Fatalf("ParseSessionArtifact() error = %v, want ErrSkillInvocationsSectionInvalid", err)
	}
}

func TestMarkdownSessionArtifactRejectsMalformedSkillInvocationsJSON(t *testing.T) {
	markdown := "# Session\n\nDid some work\n\n## Skill Invocations\n\n```json\n{not valid json\n```\n"
	_, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err == nil || !errors.Is(err, core.ErrSkillInvocationsSectionInvalid) {
		t.Fatalf("ParseSessionArtifact() error = %v, want ErrSkillInvocationsSectionInvalid", err)
	}
}

func TestMarkdownSessionArtifactRejectsNonArraySkillInvocationsJSON(t *testing.T) {
	markdown := "# Session\n\nDid some work\n\n## Skill Invocations\n\n```json\n{\"invocation_id\":\"inv-1\"}\n```\n"
	_, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err == nil || !errors.Is(err, core.ErrSkillInvocationsSectionInvalid) {
		t.Fatalf("ParseSessionArtifact() error = %v, want ErrSkillInvocationsSectionInvalid", err)
	}
}

func TestMarkdownSessionArtifactRejectsTrailingContentAfterSkillInvocationsArray(t *testing.T) {
	block := skillInvocationsMarkdownBlock(t, []core.SkillInvocationRecord{validSkillInvocationRecord()})
	markdown := fmt.Sprintf("# Session\n\nDid some work\n\n## Skill Invocations\n\n```json\n%s\ntrailing garbage\n```\n", block)
	_, err := core.ParseSessionArtifact([]byte(markdown), core.ArtifactMarkdown)
	if err == nil || !errors.Is(err, core.ErrSkillInvocationsSectionInvalid) {
		t.Fatalf("ParseSessionArtifact() error = %v, want ErrSkillInvocationsSectionInvalid", err)
	}
}
