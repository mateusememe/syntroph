package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/core"
)

func TestReadySkillPackagePreparesRuntimeNeutralBundle(t *testing.T) {
	identity := core.SkillPackageIdentity{
		SourceID:    "mattpocock",
		Name:        "code-review",
		PackageHash: strings.Repeat("a", 64),
	}
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{{
		State: core.SkillReady,
		Package: core.SkillPackage{
			SchemaVersion:      1,
			Identity:           identity,
			Description:        "Review a fixed diff",
			Instructions:       "# Code Review\n\nReview the supplied diff.",
			CompatibleRuntimes: []string{"codex", "claude"},
			ArgumentsSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"fixed_point": map[string]any{"type": "string"}},
			},
		},
	}}, nil, func() (string, error) { return "inv-001", nil })
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := port.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:      "mattpocock/code-review",
		Runtime:   "codex",
		Arguments: map[string]any{"fixed_point": "origin/main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.InvocationID() != "inv-001" || bundle.PackageIdentity() != identity {
		t.Fatalf("unexpected bundle identity: invocation=%q package=%+v", bundle.InvocationID(), bundle.PackageIdentity())
	}
	if bundle.Runtime() != "codex" || bundle.Instructions() != "# Code Review\n\nReview the supplied diff." {
		t.Fatalf("unexpected prepared content: runtime=%q instructions=%q", bundle.Runtime(), bundle.Instructions())
	}
	if bundle.Arguments()["fixed_point"] != "origin/main" || bundle.BundleHash() == "" {
		t.Fatalf("unexpected prepared arguments or hash: args=%+v hash=%q", bundle.Arguments(), bundle.BundleHash())
	}
}

func TestInvocationIdentitySeparatesReplayFromIntentionalReuse(t *testing.T) {
	generated := 0
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, nil, func() (string, error) {
		generated++
		return "inv-generated", nil
	})
	if err != nil {
		t.Fatal(err)
	}

	request := core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-explicit",
		Runtime:      "codex",
		Arguments:    map[string]any{"fixed_point": "origin/main"},
	}
	first, err := port.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := port.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Invocation() != replayed.Invocation() {
		t.Fatalf("replay changed invocation: first=%+v replay=%+v", first.Invocation(), replayed.Invocation())
	}
	if generated != 0 {
		t.Fatalf("explicit invocation IDs must not call the ID factory: calls=%d", generated)
	}

	request.InvocationID = ""
	intentional, err := port.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if intentional.Invocation().ID == first.Invocation().ID {
		t.Fatalf("intentional use reused invocation ID %q", intentional.Invocation().ID)
	}
	if intentional.BundleHash() != first.BundleHash() {
		t.Fatalf("identical package, runtime, and arguments changed bundle hash: first=%q intentional=%q", first.BundleHash(), intentional.BundleHash())
	}

	request.InvocationID = "inv-explicit"
	request.Arguments = map[string]any{"fixed_point": "HEAD~1"}
	if _, err := port.Prepare(context.Background(), request); !errors.Is(err, core.ErrSkillInvocationReplay) {
		t.Fatalf("changed replay error = %v, want ErrSkillInvocationReplay", err)
	}
}

func TestUnsupportedPackageRemainsAddressableWithItsDiagnostic(t *testing.T) {
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{{
		State: core.UnsupportedSkillPackage,
		Package: core.SkillPackage{Identity: core.SkillPackageIdentity{
			SourceID: "mattpocock",
			Name:     "diagnosing-bugs",
		}},
		Diagnostic: "scripts are not supported before SandboxPort integration",
	}}, nil, nil)
	if err != nil {
		t.Fatalf("catalog rejected an isolated unsupported package: %v", err)
	}

	_, err = port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/diagnosing-bugs"})
	if !errors.Is(err, core.ErrUnsupportedSkill) {
		t.Fatalf("prepare error = %v, want ErrUnsupportedSkill", err)
	}
	if !strings.Contains(err.Error(), "scripts are not supported") {
		t.Fatalf("prepare error omitted diagnostic: %v", err)
	}
}

func TestPreparedBundleIsDetachedFromMutableInputsAndAccessorResults(t *testing.T) {
	entry := readySkillEntry()
	entry.Package.Assets = []core.SkillAsset{{Path: "references/checklist.md", SHA256: strings.Repeat("b", 64)}}
	arguments := map[string]any{
		"targets": []string{"core/skill.go"},
		"options": map[string]string{"mode": "strict"},
	}
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := port.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: "inv-immutable",
		Arguments:    arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHash := bundle.BundleHash()

	entry.Package.Assets[0].Path = "mutated-at-source"
	arguments["targets"].([]string)[0] = "mutated-at-request"
	assets := bundle.Assets()
	assets[0].Path = "mutated-accessor"
	returnedArguments := bundle.Arguments()
	returnedArguments["options"].(map[string]any)["mode"] = "relaxed"

	if got := bundle.Assets()[0].Path; got != "references/checklist.md" {
		t.Fatalf("bundle asset was mutable: %q", got)
	}
	if got := bundle.Arguments()["targets"].([]any)[0]; got != "core/skill.go" {
		t.Fatalf("bundle argument was mutable: %q", got)
	}
	if got := bundle.Arguments()["options"].(map[string]any)["mode"]; got != "strict" {
		t.Fatalf("nested bundle argument was mutable: %v", got)
	}
	if bundle.BundleHash() != wantHash {
		t.Fatalf("immutable bundle hash changed: got=%q want=%q", bundle.BundleHash(), wantHash)
	}
}

func TestCatalogRejectsNonCanonicalPackageIdentity(t *testing.T) {
	entry := readySkillEntry()
	entry.Package.Identity.SourceID = " mattpocock"
	if _, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil, nil); err == nil {
		t.Fatal("catalog accepted a package identity that cannot be looked up canonically")
	}
}

func TestGeneratedInvocationIDCollisionNeverBecomesReplay(t *testing.T) {
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, nil, func() (string, error) {
		return "inv-collision", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := core.SkillPrepareRequest{Name: "mattpocock/code-review"}
	if _, err := port.Prepare(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), request); !errors.Is(err, core.ErrSkillInvocationIDCollision) {
		t.Fatalf("generated collision error = %v, want ErrSkillInvocationIDCollision", err)
	}
}

func TestUnqualifiedNameResolvesOnlyWhenUnique(t *testing.T) {
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "code-review", InvocationID: "inv-unqualified"})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.PackageIdentity().QualifiedName() != "mattpocock/code-review" {
		t.Fatalf("unqualified resolution returned %+v", bundle.PackageIdentity())
	}
}

func TestUnqualifiedNameCollisionRequiresQualificationAndNeverFavorsLocal(t *testing.T) {
	local := readySkillEntry()
	local.Package.Identity.SourceID = "local"
	external := readySkillEntry()
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{local, external}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A colliding unqualified name must fail even though one of the two
	// candidates is the local source; local packages never silently
	// override an external package with the same name.
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "code-review"}); !errors.Is(err, core.ErrSkillNameAmbiguous) {
		t.Fatalf("collision error = %v, want ErrSkillNameAmbiguous", err)
	}
	// Both qualified identities remain independently addressable.
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "local/code-review", InvocationID: "inv-local"}); err != nil {
		t.Fatalf("qualified local name failed: %v", err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", InvocationID: "inv-external"}); err != nil {
		t.Fatalf("qualified external name failed: %v", err)
	}
}

func TestExplicitAliasResolvesDeterministically(t *testing.T) {
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, map[string]string{"review": "mattpocock/code-review"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "review", InvocationID: "inv-alias"})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.PackageIdentity().QualifiedName() != "mattpocock/code-review" {
		t.Fatalf("alias resolution returned %+v", bundle.PackageIdentity())
	}
}

func TestInvalidAliasFailsBeforePreparation(t *testing.T) {
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, map[string]string{"missing": "nobody/nothing"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "missing"}); !errors.Is(err, core.ErrSkillAliasInvalid) {
		t.Fatalf("invalid alias error = %v, want ErrSkillAliasInvalid", err)
	}
}

func TestAmbiguousAliasTargetFailsBeforePreparation(t *testing.T) {
	local := readySkillEntry()
	local.Package.Identity.SourceID = "local"
	external := readySkillEntry()
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{local, external}, map[string]string{"review": "code-review"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "review"}); !errors.Is(err, core.ErrSkillAliasInvalid) {
		t.Fatalf("ambiguous alias target error = %v, want ErrSkillAliasInvalid", err)
	}
}

func TestQualifiedNameTakesPrecedenceOverAliasSharingTheSameKey(t *testing.T) {
	decoy := core.SkillCatalogEntry{
		State: core.SkillReady,
		Package: core.SkillPackage{
			SchemaVersion:      core.SkillSchemaVersion,
			Identity:           core.SkillPackageIdentity{SourceID: "other", Name: "decoy", PackageHash: strings.Repeat("c", 64)},
			Description:        "Decoy that must never be reached through the canonical identity",
			Instructions:       "# Decoy\n",
			CompatibleRuntimes: []string{"codex"},
		},
	}
	canonical := readySkillEntry()
	// The alias key intentionally collides with the canonical qualified
	// name of another package. A canonical qualified name must always be
	// resolved directly, never redirected through an alias sharing the
	// same literal key.
	aliases := map[string]string{"mattpocock/code-review": "other/decoy"}
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{canonical, decoy}, aliases, nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", InvocationID: "inv-canonical"})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Instructions() != canonical.Package.Instructions {
		t.Fatalf("canonical qualified name resolved to the wrong package: %q", bundle.Instructions())
	}
}

func TestRuntimeSelectionValidatesCompatibilityWithoutChangingInstructions(t *testing.T) {
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	compatible, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", Runtime: "codex", InvocationID: "inv-compatible"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", Runtime: "antigravity"}); !errors.Is(err, core.ErrSkillRuntimeIncompatible) {
		t.Fatalf("incompatible runtime error = %v, want ErrSkillRuntimeIncompatible", err)
	}
	unspecified, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", InvocationID: "inv-unspecified"})
	if err != nil {
		t.Fatal(err)
	}
	if compatible.Instructions() != unspecified.Instructions() {
		t.Fatalf("runtime selection changed canonical instructions: compatible=%q unspecified=%q", compatible.Instructions(), unspecified.Instructions())
	}
}

func TestArgumentsSchemaRejectsUndeclaredFieldsAndEnforcesRequiredAndSizeLimits(t *testing.T) {
	entry := readySkillEntry()
	entry.Package.ArgumentsSchema = map[string]any{
		"type":     "object",
		"required": []any{"note"},
		"properties": map[string]any{
			"note": map[string]any{"type": "string", "maxLength": 5},
		},
	}
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", Arguments: map[string]any{}}); !errors.Is(err, core.ErrSkillArgumentsInvalid) {
		t.Fatalf("missing required field error = %v, want ErrSkillArgumentsInvalid", err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", Arguments: map[string]any{"note": "ok", "token": "undeclared"}}); !errors.Is(err, core.ErrSkillArgumentsInvalid) {
		t.Fatalf("undeclared field error = %v, want ErrSkillArgumentsInvalid", err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", Arguments: map[string]any{"note": "too long"}}); !errors.Is(err, core.ErrSkillArgumentsInvalid) {
		t.Fatalf("oversized field error = %v, want ErrSkillArgumentsInvalid", err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", InvocationID: "inv-valid", Arguments: map[string]any{"note": "ok"}}); err != nil {
		t.Fatalf("valid arguments were rejected: %v", err)
	}
}

func TestArgumentsSchemaEnforcesEnumConstraints(t *testing.T) {
	entry := readySkillEntry()
	entry.Package.ArgumentsSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"strict", "relaxed"}},
		},
	}
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", Arguments: map[string]any{"mode": "invalid"}}); !errors.Is(err, core.ErrSkillArgumentsInvalid) {
		t.Fatalf("enum violation error = %v, want ErrSkillArgumentsInvalid", err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", InvocationID: "inv-enum", Arguments: map[string]any{"mode": "strict"}}); err != nil {
		t.Fatalf("valid enum value was rejected: %v", err)
	}
}

func TestArgumentsSchemaRejectsAnUnsupportedTypeIncludingSecret(t *testing.T) {
	entry := readySkillEntry()
	entry.Package.ArgumentsSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"token": map[string]any{"type": "secret"},
		},
	}
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review"}); !errors.Is(err, core.ErrSkillArgumentsSchemaInvalid) {
		t.Fatalf("secret-typed schema error = %v, want ErrSkillArgumentsSchemaInvalid", err)
	}
}

func TestNoDeclaredSchemaPermitsOnlyAnEmptyObject(t *testing.T) {
	entry := readySkillEntry()
	entry.Package.ArgumentsSchema = nil
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", Arguments: map[string]any{"anything": "here"}}); !errors.Is(err, core.ErrSkillArgumentsInvalid) {
		t.Fatalf("no-schema non-empty arguments error = %v, want ErrSkillArgumentsInvalid", err)
	}
	if _, err := port.Prepare(context.Background(), core.SkillPrepareRequest{Name: "mattpocock/code-review", InvocationID: "inv-empty"}); err != nil {
		t.Fatalf("no-schema empty arguments were rejected: %v", err)
	}
}

func readySkillEntry() core.SkillCatalogEntry {
	return core.SkillCatalogEntry{
		State: core.SkillReady,
		Package: core.SkillPackage{
			SchemaVersion:      core.SkillSchemaVersion,
			Identity:           core.SkillPackageIdentity{SourceID: "mattpocock", Name: "code-review", PackageHash: strings.Repeat("a", 64)},
			Description:        "Review a fixed diff",
			Instructions:       "# Code Review\n\nReview the supplied diff.",
			CompatibleRuntimes: []string{"codex", "claude"},
			ArgumentsSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fixed_point": map[string]any{"type": "string"},
					"targets": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
					"options": map[string]any{"type": "object"},
				},
			},
		},
	}
}
