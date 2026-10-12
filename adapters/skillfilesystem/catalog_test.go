package skillfilesystem_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/adapters/skillfilesystem"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
)

func TestSyncMaterializesOneLockedFilesystemPackage(t *testing.T) {
	repositoryRoot := t.TempDir()
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "config.yaml"), `skills:
  enabled: true
  runtime_lock: .syntroph/skills.runtime.lock.yaml
  sources:
    - id: mattpocock
      path: vendor/skills
`)
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	lockContents := `schema_version: 1
packages:
  - source_id: mattpocock
    name: code-review
    directory: code-review
    description: Review a fixed diff
    license: MIT
    source_url: https://github.com/mattpocock/skills
    source_revision: 8d66fba
    instructions: SKILL.md
    files:
      - references/checklist.md
      - SKILL.md
    compatible_runtimes: [codex, claude]
    arguments_schema:
      type: object
`
	writeFile(t, lockPath, lockContents)
	developmentLockPath := filepath.Join(repositoryRoot, "skills-lock.json")
	developmentLockContents := "{\"development_only\":true}\n"
	writeFile(t, developmentLockPath, developmentLockContents)
	packageRoot := filepath.Join(repositoryRoot, "vendor", "skills", "code-review")
	writeFile(t, filepath.Join(packageRoot, "SKILL.md"), "# Code Review\n\nReview the supplied diff.\n")
	writeFile(t, filepath.Join(packageRoot, "references", "checklist.md"), "# Checklist\n\n- Correctness\n")

	cfg, err := config.Load(filepath.Join(repositoryRoot, ".syntroph", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveSkills(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skillfilesystem.New(repositoryRoot, resolved)
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].State != core.SkillReady {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	summary := result.Entries[0]
	if summary.Identity.QualifiedName() != "mattpocock/code-review" || summary.Identity.PackageHash == "" {
		t.Fatalf("unexpected package identity: %+v", summary.Identity)
	}
	shown, err := catalog.Show(context.Background(), "mattpocock/code-review")
	if err != nil || shown.Package.Identity != summary.Identity {
		t.Fatalf("active catalog show mismatch: entry=%+v err=%v", shown, err)
	}
	pkg := shown.Package
	if pkg.License != "MIT" || pkg.SourceURL != "https://github.com/mattpocock/skills" || pkg.SourceRevision != "8d66fba" {
		t.Fatalf("normalized provenance missing: %+v", pkg)
	}
	if pkg.Instructions != "# Code Review\n\nReview the supplied diff.\n" || len(pkg.Assets) != 1 || pkg.Assets[0].Path != "references/checklist.md" {
		t.Fatalf("normalized package content mismatch: %+v", pkg)
	}
	if pkg.Assets[0].MediaType != "text/markdown" {
		t.Fatalf("asset media type must be host-independent: %q", pkg.Assets[0].MediaType)
	}
	storeManifest := filepath.Join(repositoryRoot, ".syntroph", "catalog", "store", summary.Identity.PackageHash, "package.json")
	if _, err := os.Stat(storeManifest); err != nil {
		t.Fatalf("immutable store object missing: %v", err)
	}
	for relative, want := range map[string]string{
		"SKILL.md":                "# Code Review\n\nReview the supplied diff.\n",
		"references/checklist.md": "# Checklist\n\n- Correctness\n",
	} {
		stored, err := os.ReadFile(filepath.Join(filepath.Dir(storeManifest), "files", filepath.FromSlash(relative)))
		if err != nil || string(stored) != want {
			t.Fatalf("stored file %q mismatch: contents=%q err=%v", relative, stored, err)
		}
	}

	listed, err := catalog.List(context.Background())
	if err != nil || len(listed) != 1 || listed[0].Identity != summary.Identity {
		t.Fatalf("active catalog list mismatch: entries=%+v err=%v", listed, err)
	}
	if got, err := os.ReadFile(lockPath); err != nil || string(got) != lockContents {
		t.Fatalf("synchronization mutated runtime lock: contents=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(developmentLockPath); err != nil || string(got) != developmentLockContents {
		t.Fatalf("synchronization mutated development-tool lock: contents=%q err=%v", got, err)
	}
}

func TestSyncRequiresStrictVersionedRuntimeLock(t *testing.T) {
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	catalog, err := skillfilesystem.New(repositoryRoot, config.ResolvedSkills{
		Enabled:     true,
		RuntimeLock: lockPath,
		Sources:     []config.ResolvedSkillSource{{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "runtime skill lock is missing") {
		t.Fatalf("missing lock error = %v", err)
	}
	writeFile(t, lockPath, "schema_version: 1\npackages: []\nunknown: true\n")
	if _, err := catalog.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("unknown lock field error = %v", err)
	}
}

func TestLocalManifestNormalizesToReadyPackageContract(t *testing.T) {
	repositoryRoot := t.TempDir()
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml"), "schema_version: 1\npackages: []\n")
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills", "repository-review", "skill.yaml"), `schema_version: 1
name: repository-review
description: Review repository conventions
license: MIT
source_url: https://github.com/example/repository
source_revision: local-revision
instructions: SKILL.md
files: [SKILL.md]
compatible_runtimes: [codex]
arguments_schema:
  type: object
`)
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills", "repository-review", "SKILL.md"), "# Repository Review\n")
	catalog, err := skillfilesystem.New(repositoryRoot, config.ResolvedSkills{
		Enabled:     true,
		RuntimeLock: filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml"),
		Sources:     []config.ResolvedSkillSource{{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("local sync entries = %d, want 1", len(result.Entries))
	}
	shown, err := catalog.Show(context.Background(), "local/repository-review")
	if err != nil {
		t.Fatal(err)
	}
	pkg := shown.Package
	if pkg.SchemaVersion != core.SkillSchemaVersion || pkg.Identity.SourceID != "local" || pkg.Identity.Name != "repository-review" || pkg.Identity.PackageHash == "" || pkg.License != "MIT" || pkg.SourceURL == "" || pkg.SourceRevision != "local-revision" || pkg.InstructionsPath != "SKILL.md" {
		t.Fatalf("local manifest did not normalize to the package contract: %+v", pkg)
	}
}

func TestPackageHashUsesCanonicalFileOrderAndExactBytes(t *testing.T) {
	first := syncExternalPackageHash(t, "      - references/checklist.md\n      - SKILL.md\n", "# Review\n")
	second := syncExternalPackageHash(t, "      - SKILL.md\n      - references/checklist.md\n", "# Review\n")
	changedBytes := syncExternalPackageHash(t, "      - SKILL.md\n      - references/checklist.md\n", "# Review\r\n")
	if first != second {
		t.Fatalf("declaration order changed package hash: first=%q second=%q", first, second)
	}
	if first == changedBytes {
		t.Fatalf("exact instruction bytes did not change package hash: %q", first)
	}
}

func TestSyncRetainsEarlierImmutableStoreObjectWhenSourceChanges(t *testing.T) {
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: review
    directory: review
    description: Review changes
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
`)
	sourcePath := filepath.Join(repositoryRoot, "skills", "review", "SKILL.md")
	writeFile(t, sourcePath, "# First\n")
	catalog, err := skillfilesystem.New(repositoryRoot, config.ResolvedSkills{
		Enabled:     true,
		RuntimeLock: lockPath,
		Sources: []config.ResolvedSkillSource{
			{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")},
			{ID: "source", Root: filepath.Join(repositoryRoot, "skills")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, sourcePath, "# Second\n")
	second, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstHash := first.Entries[0].Identity.PackageHash
	secondHash := second.Entries[0].Identity.PackageHash
	if firstHash == secondHash {
		t.Fatalf("source byte change reused package hash %q", firstHash)
	}
	for _, hash := range []string{firstHash, secondHash} {
		if _, err := os.Stat(filepath.Join(repositoryRoot, ".syntroph", "catalog", "store", hash, "package.json")); err != nil {
			t.Fatalf("immutable store object %s was not retained: %v", hash, err)
		}
	}
}

func syncExternalPackageHash(t *testing.T, filesYAML, instructions string) string {
	t.Helper()
	repositoryRoot := t.TempDir()
	lock := `schema_version: 1
packages:
  - source_id: source
    name: review
    directory: review
    description: Review changes
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files:
` + filesYAML
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, lockPath, lock)
	writeFile(t, filepath.Join(repositoryRoot, "skills", "review", "SKILL.md"), instructions)
	writeFile(t, filepath.Join(repositoryRoot, "skills", "review", "references", "checklist.md"), "# Checklist\n")
	catalog, err := skillfilesystem.New(repositoryRoot, config.ResolvedSkills{
		Enabled:     true,
		RuntimeLock: lockPath,
		Sources: []config.ResolvedSkillSource{
			{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")},
			{ID: "source", Root: filepath.Join(repositoryRoot, "skills")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return result.Entries[0].Identity.PackageHash
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
