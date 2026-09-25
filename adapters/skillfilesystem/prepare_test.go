package skillfilesystem_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/adapters/skillfilesystem"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
)

// syncedTwoSourceCatalog configures two skill sources that both publish an
// unqualified package named "review", plus an alias configured through
// config.yaml, and returns the synchronized catalog for prepare-path tests.
func syncedTwoSourceCatalog(t *testing.T, aliases map[string]string) *skillfilesystem.Catalog {
	t.Helper()
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: first
    name: review
    directory: review
    description: First source review skill
    license: MIT
    source_url: https://example.test/first
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
    compatible_runtimes: [codex]
  - source_id: second
    name: review
    directory: review
    description: Second source review skill
    license: MIT
    source_url: https://example.test/second
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
    compatible_runtimes: [claude]
`)
	writeFile(t, filepath.Join(repositoryRoot, "first", "review", "SKILL.md"), "# First Review\n")
	writeFile(t, filepath.Join(repositoryRoot, "second", "review", "SKILL.md"), "# Second Review\n")
	settings := config.ResolvedSkills{
		Enabled:     true,
		RuntimeLock: lockPath,
		Sources: []config.ResolvedSkillSource{
			{ID: "first", Root: filepath.Join(repositoryRoot, "first")},
			{ID: "second", Root: filepath.Join(repositoryRoot, "second")},
		},
		Aliases: aliases,
	}
	catalog, err := skillfilesystem.New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestPrepareResolvesAliasesConfiguredThroughSync(t *testing.T) {
	catalog := syncedTwoSourceCatalog(t, map[string]string{"review": "second/review"})
	bundle, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "review",
		InvocationID: "inv-alias-wiring",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.PackageIdentity().SourceID != "second" {
		t.Fatalf("alias configured via config.yaml resolved to %+v, want source_id=second", bundle.PackageIdentity())
	}
	if !strings.Contains(bundle.Instructions(), "Second Review") {
		t.Fatalf("alias resolution returned unexpected instructions: %q", bundle.Instructions())
	}
}

func TestPrepareRejectsAmbiguousUnqualifiedNameAcrossTwoSources(t *testing.T) {
	catalog := syncedTwoSourceCatalog(t, nil)
	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "review"}); !errors.Is(err, core.ErrSkillNameAmbiguous) {
		t.Fatalf("ambiguous unqualified prepare error = %v, want ErrSkillNameAmbiguous", err)
	}
	// Both sources remain reachable once qualified, proving neither one
	// silently wins the collision.
	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "first/review", InvocationID: "inv-first"}); err != nil {
		t.Fatalf("qualified first source failed: %v", err)
	}
	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "second/review", InvocationID: "inv-second"}); err != nil {
		t.Fatalf("qualified second source failed: %v", err)
	}
}

func TestPrepareRejectsIncompatibleRuntimeAgainstASyncedPackage(t *testing.T) {
	catalog := syncedTwoSourceCatalog(t, nil)
	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:    "first/review",
		Runtime: "claude",
	}); !errors.Is(err, core.ErrSkillRuntimeIncompatible) {
		t.Fatalf("incompatible runtime error = %v, want ErrSkillRuntimeIncompatible", err)
	}
	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "first/review",
		Runtime:      "codex",
		InvocationID: "inv-runtime-ok",
	}); err != nil {
		t.Fatalf("compatible runtime was rejected: %v", err)
	}
}

func TestPreparedBundleNeverLeaksAbsoluteHostPaths(t *testing.T) {
	catalog := syncedTwoSourceCatalog(t, nil)
	bundle, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{
		Name:         "first/review",
		InvocationID: "inv-no-host-paths",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range bundle.Assets() {
		if filepath.IsAbs(asset.Path) || strings.Contains(asset.Path, "..") {
			t.Fatalf("bundle asset leaked an unsafe path: %+v", asset)
		}
	}
}
