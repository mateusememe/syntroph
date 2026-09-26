package skillfilesystem_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/adapters/skillfilesystem"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
)

func TestSyncKeepsReadyPackageWhenAnotherPackageUsesProhibitedContent(t *testing.T) {
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: code-review
    directory: code-review
    description: Review changes
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
  - source_id: source
    name: diagnosing-bugs
    directory: diagnosing-bugs
    description: Diagnose bugs
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md, scripts/hitl-loop.template.sh]
`)
	writeFile(t, filepath.Join(repositoryRoot, "skills", "code-review", "SKILL.md"), "# Review\n")
	writeFile(t, filepath.Join(repositoryRoot, "skills", "diagnosing-bugs", "SKILL.md"), "# Diagnose\n")
	writeFile(t, filepath.Join(repositoryRoot, "skills", "diagnosing-bugs", "scripts", "hitl-loop.template.sh"), "#!/bin/sh\nexit 0\n")

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
	if len(result.Entries) != 2 {
		t.Fatalf("sync entries = %d, want 2", len(result.Entries))
	}
	ready, unsupported := result.Entries[0], result.Entries[1]
	if ready.Identity.QualifiedName() != "source/code-review" || ready.State != core.SkillReady {
		t.Fatalf("ready entry = %+v", ready)
	}
	if unsupported.Identity.QualifiedName() != "source/diagnosing-bugs" || unsupported.State != core.UnsupportedSkillPackage {
		t.Fatalf("unsupported entry = %+v", unsupported)
	}
	if !strings.Contains(unsupported.Diagnostic, `scripts/hitl-loop.template.sh`) || !strings.Contains(unsupported.Diagnostic, "prohibited") {
		t.Fatalf("unsupported diagnostic is not actionable: %q", unsupported.Diagnostic)
	}
	if strings.Contains(unsupported.Diagnostic, repositoryRoot) {
		t.Fatalf("unsupported diagnostic exposed an absolute host path: %q", unsupported.Diagnostic)
	}

	listed, err := catalog.List(context.Background())
	if err != nil || len(listed) != 2 || listed[1] != unsupported {
		t.Fatalf("mixed catalog list = %+v, err = %v", listed, err)
	}
	shown, err := catalog.Show(context.Background(), "source/diagnosing-bugs")
	if err != nil || shown.State != core.UnsupportedSkillPackage || shown.Diagnostic != unsupported.Diagnostic {
		t.Fatalf("unsupported catalog show = %+v, err = %v", shown, err)
	}
	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "source/diagnosing-bugs"}); !errors.Is(err, core.ErrUnsupportedSkill) {
		t.Fatalf("unsupported prepare error = %v, want ErrUnsupportedSkill", err)
	}
	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "source/code-review"}); err != nil {
		t.Fatalf("ready package became unusable: %v", err)
	}
}

func TestSyncRejectsUnsafeAndOversizedDeclaredContent(t *testing.T) {
	tests := []struct {
		name       string
		files      []string
		setup      func(*testing.T, string)
		diagnostic string
	}{
		{
			name:       "missing declared file",
			files:      []string{"SKILL.md", "missing.txt"},
			setup:      func(*testing.T, string) {},
			diagnostic: "is missing",
		},
		{
			name:  "symlink",
			files: []string{"SKILL.md", "linked.txt"},
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "target.txt"), "target\n")
				if err := os.Symlink("target.txt", filepath.Join(root, "linked.txt")); err != nil {
					t.Fatal(err)
				}
			},
			diagnostic: "not a regular file",
		},
		{
			name:  "directory declared as file",
			files: []string{"SKILL.md", "notes.txt"},
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "notes.txt"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			diagnostic: "not a regular file",
		},
		{
			name:  "path traversal is not normalized away",
			files: []string{"SKILL.md", "docs/../notes.txt"},
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "notes.txt"), "notes\n")
			},
			diagnostic: "package-relative path",
		},
		{
			name:  "executable mode",
			files: []string{"SKILL.md", "notes.txt"},
			setup: func(t *testing.T, root string) {
				path := filepath.Join(root, "notes.txt")
				writeFile(t, path, "notes\n")
				if err := os.Chmod(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			diagnostic: "executable content",
		},
		{
			name:  "invalid UTF-8 text",
			files: []string{"SKILL.md", "notes.txt"},
			setup: func(t *testing.T, root string) {
				path := filepath.Join(root, "notes.txt")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte{0xff, 0xfe}, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			diagnostic: "valid UTF-8",
		},
		{
			name:  "oversized instructions",
			files: []string{"SKILL.md"},
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "SKILL.md"), strings.Repeat("x", 256*1024+1))
			},
			diagnostic: "256 KiB",
		},
		{
			name:  "oversized asset",
			files: []string{"SKILL.md", "large.txt"},
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "large.txt"), strings.Repeat("x", 1024*1024+1))
			},
			diagnostic: "1 MiB",
		},
		{
			name:  "excessive directory depth",
			files: []string{"SKILL.md", "1/2/3/4/5/6/7/8/9/notes.txt"},
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "1/2/3/4/5/6/7/8/9/notes.txt"), "notes\n")
			},
			diagnostic: "eight directory levels",
		},
	}
	manyFiles := []string{"SKILL.md"}
	for index := 0; index < 128; index++ {
		manyFiles = append(manyFiles, fmt.Sprintf("assets/%03d.txt", index))
	}
	tests = append(tests, struct {
		name       string
		files      []string
		setup      func(*testing.T, string)
		diagnostic string
	}{
		name:  "too many declared files",
		files: manyFiles,
		setup: func(t *testing.T, root string) {
			for _, file := range manyFiles[1:] {
				writeFile(t, filepath.Join(root, file), "x")
			}
		},
		diagnostic: "at most 128 files",
	})
	bundleFiles := []string{"SKILL.md"}
	for index := 0; index < 6; index++ {
		bundleFiles = append(bundleFiles, fmt.Sprintf("assets/%d.txt", index))
	}
	tests = append(tests, struct {
		name       string
		files      []string
		setup      func(*testing.T, string)
		diagnostic string
	}{
		name:  "oversized bundle",
		files: bundleFiles,
		setup: func(t *testing.T, root string) {
			for _, file := range bundleFiles[1:] {
				writeFile(t, filepath.Join(root, file), strings.Repeat("x", 900*1024))
			}
		},
		diagnostic: "5 MiB",
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositoryRoot := t.TempDir()
			packageRoot := filepath.Join(repositoryRoot, "skills", "unsafe")
			writeFile(t, filepath.Join(packageRoot, "SKILL.md"), "# Safe default\n")
			test.setup(t, packageRoot)
			entry := syncOnePackage(t, repositoryRoot, test.files)
			if entry.State != core.UnsupportedSkillPackage || !strings.Contains(entry.Diagnostic, test.diagnostic) {
				t.Fatalf("entry = %+v, want UnsupportedSkillPackage diagnostic containing %q", entry, test.diagnostic)
			}
		})
	}
}

func syncOnePackage(t *testing.T, repositoryRoot string, files []string) skillfilesystem.EntrySummary {
	t.Helper()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	var declared strings.Builder
	for _, file := range files {
		declared.WriteString("      - ")
		declared.WriteString(file)
		declared.WriteByte('\n')
	}
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: unsafe
    directory: unsafe
    description: Unsafe fixture
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files:
`+declared.String())
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
	if len(result.Entries) != 1 {
		t.Fatalf("sync entries = %d, want 1", len(result.Entries))
	}
	return result.Entries[0]
}

func TestSyncRejectsPackageDirectoryEscapingItsSource(t *testing.T) {
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, filepath.Join(repositoryRoot, "secret", "SKILL.md"), "# Secret\n")
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: escaping
    directory: ../secret
    description: Attempts to escape its declared source
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
`)
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
	if len(result.Entries) != 1 || result.Entries[0].State != core.UnsupportedSkillPackage {
		t.Fatalf("escaping package directory result = %+v", result)
	}
	if !strings.Contains(result.Entries[0].Diagnostic, "escapes") {
		t.Fatalf("escaping diagnostic = %q", result.Entries[0].Diagnostic)
	}
	if strings.Contains(result.Entries[0].Diagnostic, repositoryRoot) {
		t.Fatalf("escaping diagnostic exposed an absolute host path: %q", result.Entries[0].Diagnostic)
	}
}

func TestSyncKeepsValidLocalPackageWhenAnotherManifestIsInvalid(t *testing.T) {
	repositoryRoot := t.TempDir()
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml"), "schema_version: 1\npackages: []\n")
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills", "ready", "skill.yaml"), `schema_version: 1
name: ready
description: Ready package
license: MIT
source_url: https://example.test/ready
source_revision: revision-1
instructions: SKILL.md
files: [SKILL.md]
`)
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills", "ready", "SKILL.md"), "# Ready\n")
	writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills", "broken", "skill.yaml"), "schema_version: [not-an-integer]\n")

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
	if len(result.Entries) != 2 || result.Entries[0].Identity.QualifiedName() != "local/broken" || result.Entries[0].State != core.UnsupportedSkillPackage || result.Entries[1].State != core.SkillReady {
		t.Fatalf("mixed local catalog = %+v", result.Entries)
	}
	if strings.Contains(result.Entries[0].Diagnostic, repositoryRoot) || !strings.Contains(result.Entries[0].Diagnostic, "manifest") {
		t.Fatalf("invalid manifest diagnostic is unsafe or vague: %q", result.Entries[0].Diagnostic)
	}
}

func TestSyncEnforcesManifestAndRuntimeLockSizeLimit(t *testing.T) {
	t.Run("local manifest becomes unsupported", func(t *testing.T) {
		repositoryRoot := t.TempDir()
		writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml"), "schema_version: 1\npackages: []\n")
		manifest := strings.Repeat("# padding\n", 30_000) + `schema_version: 1
name: oversized
description: Oversized manifest
license: MIT
source_url: https://example.test/oversized
source_revision: revision-1
instructions: SKILL.md
files: [SKILL.md]
`
		writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills", "oversized", "skill.yaml"), manifest)
		writeFile(t, filepath.Join(repositoryRoot, ".syntroph", "skills", "oversized", "SKILL.md"), "# Oversized\n")
		catalog, err := skillfilesystem.New(repositoryRoot, config.ResolvedSkills{
			Enabled: true, RuntimeLock: filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml"),
			Sources: []config.ResolvedSkillSource{{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := catalog.Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Entries) != 1 || result.Entries[0].State != core.UnsupportedSkillPackage || !strings.Contains(result.Entries[0].Diagnostic, "256 KiB") {
			t.Fatalf("oversized local manifest result = %+v", result)
		}
	})

	t.Run("runtime lock rejects publication", func(t *testing.T) {
		repositoryRoot := t.TempDir()
		lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
		writeFile(t, lockPath, strings.Repeat("# padding\n", 30_000)+"schema_version: 1\npackages: []\n")
		catalog, err := skillfilesystem.New(repositoryRoot, config.ResolvedSkills{
			Enabled: true, RuntimeLock: lockPath,
			Sources: []config.ResolvedSkillSource{{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "256 KiB") {
			t.Fatalf("oversized runtime lock error = %v", err)
		}
	})
}

func TestVerifySucceedsForConsistentMixedCatalogAndFailsOnUnsupportedPackage(t *testing.T) {
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: code-review
    directory: code-review
    description: Review changes
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
  - source_id: source
    name: diagnosing-bugs
    directory: diagnosing-bugs
    description: Diagnose bugs
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md, scripts/hitl-loop.template.sh]
`)
	writeFile(t, filepath.Join(repositoryRoot, "skills", "code-review", "SKILL.md"), "# Review\n")
	writeFile(t, filepath.Join(repositoryRoot, "skills", "diagnosing-bugs", "SKILL.md"), "# Diagnose\n")
	writeFile(t, filepath.Join(repositoryRoot, "skills", "diagnosing-bugs", "scripts", "hitl-loop.template.sh"), "#!/bin/sh\nexit 0\n")

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
	if _, err := catalog.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Verifying the Ready package alone succeeds.
	entries, err := catalog.Verify(context.Background(), "source/code-review")
	if err != nil || len(entries) != 1 || entries[0].State != core.SkillReady {
		t.Fatalf("verify ready package = %+v, err = %v", entries, err)
	}

	// Verifying the unsupported package alone fails with a non-zero-worthy error.
	entries, err = catalog.Verify(context.Background(), "source/diagnosing-bugs")
	if err == nil || !errors.Is(err, core.ErrUnsupportedSkill) {
		t.Fatalf("verify unsupported package error = %v, want ErrUnsupportedSkill", err)
	}
	if len(entries) != 1 || entries[0].State != core.UnsupportedSkillPackage {
		t.Fatalf("verify unsupported package entries = %+v", entries)
	}
	if strings.Contains(entries[0].Diagnostic, repositoryRoot) {
		t.Fatalf("verify diagnostic exposed an absolute host path: %q", entries[0].Diagnostic)
	}

	// Verifying the whole catalog reports every package but still fails
	// because one package in the mixed catalog is unsupported.
	entries, err = catalog.Verify(context.Background(), "")
	if err == nil || !errors.Is(err, core.ErrUnsupportedSkill) {
		t.Fatalf("verify whole catalog error = %v, want ErrUnsupportedSkill", err)
	}
	if len(entries) != 2 {
		t.Fatalf("verify whole catalog entries = %+v", entries)
	}

	// An unknown package name fails distinctly.
	if _, err := catalog.Verify(context.Background(), "source/does-not-exist"); !errors.Is(err, core.ErrSkillNotFound) {
		t.Fatalf("verify unknown package error = %v, want ErrSkillNotFound", err)
	}
}

func TestVerifyDetectsStoreDriftAfterPublication(t *testing.T) {
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: code-review
    directory: code-review
    description: Review changes
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md, references/checklist.md]
`)
	writeFile(t, filepath.Join(repositoryRoot, "skills", "code-review", "SKILL.md"), "# Review\n")
	writeFile(t, filepath.Join(repositoryRoot, "skills", "code-review", "references", "checklist.md"), "# Checklist\n")

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
	if len(result.Entries) != 1 || result.Entries[0].State != core.SkillReady {
		t.Fatalf("initial sync result = %+v", result)
	}

	if entries, err := catalog.Verify(context.Background(), ""); err != nil || len(entries) != 1 || entries[0].State != core.SkillReady {
		t.Fatalf("verify before drift = %+v, err = %v", entries, err)
	}

	// Tamper with the immutable store directly, simulating drift that a
	// consistent Skill Source cannot itself produce.
	packageHash := result.Entries[0].Identity.PackageHash
	tamperedAsset := filepath.Join(repositoryRoot, ".syntroph", "catalog", "store", packageHash, "files", "references", "checklist.md")
	if err := os.WriteFile(tamperedAsset, []byte("# Tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := catalog.Verify(context.Background(), "")
	if err == nil || !errors.Is(err, core.ErrUnsupportedSkill) {
		t.Fatalf("verify after drift error = %v, want ErrUnsupportedSkill", err)
	}
	if len(entries) != 1 || entries[0].State != core.UnsupportedSkillPackage || !strings.Contains(entries[0].Diagnostic, "drifted") {
		t.Fatalf("verify after drift entries = %+v", entries)
	}
	if strings.Contains(entries[0].Diagnostic, repositoryRoot) {
		t.Fatalf("drift diagnostic exposed an absolute host path: %q", entries[0].Diagnostic)
	}
}

func TestPrepareRejectsDriftedStoreBeforeReturningInstructions(t *testing.T) {
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: code-review
    directory: code-review
    description: Review changes
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md, references/checklist.md]
`)
	writeFile(t, filepath.Join(repositoryRoot, "skills", "code-review", "SKILL.md"), "# Review\n")
	writeFile(t, filepath.Join(repositoryRoot, "skills", "code-review", "references", "checklist.md"), "# Checklist\n")

	settings := config.ResolvedSkills{
		Enabled:     true,
		RuntimeLock: lockPath,
		Sources: []config.ResolvedSkillSource{
			{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")},
			{ID: "source", Root: filepath.Join(repositoryRoot, "skills")},
		},
	}
	catalog, err := skillfilesystem.New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].State != core.SkillReady {
		t.Fatalf("initial sync result = %+v", result)
	}

	if _, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "source/code-review"}); err != nil {
		t.Fatalf("prepare before drift = %v", err)
	}

	// Tamper with the immutable store directly, simulating drift that a
	// consistent Skill Source cannot itself produce.
	packageHash := result.Entries[0].Identity.PackageHash
	tamperedAsset := filepath.Join(repositoryRoot, ".syntroph", "catalog", "store", packageHash, "files", "references", "checklist.md")
	if err := os.WriteFile(tamperedAsset, []byte("# Tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A fresh Catalog, matching a new `syntroph skill prepare` process, must
	// observe the drift: Prepare's in-process cache only spans repeated
	// calls on one Catalog instance sharing an index hash, never a new one.
	freshCatalog, err := skillfilesystem.New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := freshCatalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "source/code-review"}); !errors.Is(err, core.ErrUnsupportedSkill) {
		t.Fatalf("prepare after drift error = %v, want ErrUnsupportedSkill", err)
	}
}
