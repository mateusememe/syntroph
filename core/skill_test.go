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
		},
	}}, func() (string, error) { return "inv-001", nil })
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
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, func() (string, error) {
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
	}}, nil)
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
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil)
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
	if _, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{entry}, nil); err == nil {
		t.Fatal("catalog accepted a package identity that cannot be looked up canonically")
	}
}

func TestGeneratedInvocationIDCollisionNeverBecomesReplay(t *testing.T) {
	port, err := core.NewInMemorySkillPort([]core.SkillCatalogEntry{readySkillEntry()}, func() (string, error) {
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

func readySkillEntry() core.SkillCatalogEntry {
	return core.SkillCatalogEntry{
		State: core.SkillReady,
		Package: core.SkillPackage{
			SchemaVersion:      core.SkillSchemaVersion,
			Identity:           core.SkillPackageIdentity{SourceID: "mattpocock", Name: "code-review", PackageHash: strings.Repeat("a", 64)},
			Description:        "Review a fixed diff",
			Instructions:       "# Code Review\n\nReview the supplied diff.",
			CompatibleRuntimes: []string{"codex", "claude"},
		},
	}
}
