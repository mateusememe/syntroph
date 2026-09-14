package skillfilesystem_test

import (
	"context"
	"testing"

	"github.com/mateusememe/syntroph/adapters/skillfilesystem"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core/skillcontracttest"
)

func TestFilesystemCatalogUsesCoreSkillPortContract(t *testing.T) {
	skillcontracttest.RunSkillPort(t, "filesystem", func(t *testing.T) skillcontracttest.SkillPortFixture {
		t.Helper()
		repositoryRoot := t.TempDir()
		writeFile(t, repositoryRoot+"/.syntroph/skills.runtime.lock.yaml", `schema_version: 1
packages:
  - source_id: source
    name: review
    directory: review
    description: Review the supplied change
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
    compatible_runtimes: [codex]
`)
		writeFile(t, repositoryRoot+"/skills/review/SKILL.md", "# Review\n")
		settings := config.ResolvedSkills{
			Enabled:     true,
			RuntimeLock: repositoryRoot + "/.syntroph/skills.runtime.lock.yaml",
			Sources: []config.ResolvedSkillSource{
				{ID: "local", Root: repositoryRoot + "/.syntroph/skills"},
				{ID: "source", Root: repositoryRoot + "/skills"},
			},
		}
		catalog, err := skillfilesystem.New(repositoryRoot, settings)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		return skillcontracttest.SkillPortFixture{
			Port:      catalog,
			ReadyName: "source/review",
			Runtime:   "codex",
		}
	})
}
