package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/config"
)

func TestLoadAndResolveSupportedStorageProviders(t *testing.T) {
	tests := []struct {
		name, backend, provider string
	}{
		{name: "issues REST", backend: "issues", provider: "github-rest"},
		{name: "issues MCP", backend: "issues", provider: "github-mcp"},
		{name: "Wiki Git", backend: "wiki", provider: "github-wiki-git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, "storage:\n  backend: "+tt.backend+"\n  provider: "+tt.provider+"\n")
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := cfg.ResolveStorage("git@github.com:mateusememe/syntroph.git")
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.Enabled || resolved.Backend != tt.backend || resolved.Provider != tt.provider || resolved.Repository != "mateusememe/syntroph" || resolved.CrossRepository {
				t.Fatalf("unexpected resolution: %+v", resolved)
			}
		})
	}
}

func TestResolveStorageRejectsUnsafeOrAmbiguousConfiguration(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{name: "invalid pair", yaml: "storage:\n  backend: wiki\n  provider: github-rest\n", want: "wiki requires provider github-wiki-git"},
		{name: "missing provider", yaml: "storage:\n  backend: issues\n", want: "provider is required"},
		{name: "unknown fallback", yaml: "storage:\n  backend: issues\n  provider: github-rest\n  fallback_provider: github-mcp\n", want: "field fallback_provider not found"},
		{name: "inactive MCP settings", yaml: "storage:\n  backend: issues\n  provider: github-rest\n  mcp:\n    command: [server]\n", want: "storage.mcp is only valid"},
		{name: "unsafe destination", yaml: "storage:\n  backend: issues\n  provider: github-rest\n  repository: another/project\n", want: "allow_cross_repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tt.yaml))
			if err == nil {
				_, err = cfg.ResolveStorage("https://github.com/mateusememe/syntroph.git")
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestResolveStorageAllowsExplicitCrossRepositoryOptIn(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, "storage:\n  backend: issues\n  provider: github-rest\n  repository: another/project\n  allow_cross_repository: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveStorage("https://github.com/mateusememe/syntroph")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Repository != "another/project" || !resolved.CrossRepository {
		t.Fatalf("unexpected cross-repository resolution: %+v", resolved)
	}
}

func TestNormalizeGitHubRepositoryRejectsEmbeddedCredentials(t *testing.T) {
	for _, remote := range []string{
		"https://alice:super-secret@github.com/mateusememe/syntroph.git",
		"alice:super-secret@github.com:mateusememe/syntroph.git",
	} {
		_, err := config.NormalizeGitHubRepository(remote)
		if err == nil || strings.Contains(err.Error(), "super-secret") {
			t.Fatalf("remote %q was not rejected safely: %v", remote, err)
		}
	}
	if got, err := config.NormalizeGitHubRepository("ssh://git@github.com/mateusememe/syntroph.git"); err != nil || got != "mateusememe/syntroph" {
		t.Fatalf("credential-free SSH remote rejected: got=%q err=%v", got, err)
	}
}

func TestLoadRejectsJSONAndUnknownYAMLFields(t *testing.T) {
	for _, contents := range []string{
		`{"storage":{"backend":"issues","provider":"github-rest"}}`,
		"storage:\n  backend: issues\n  provider: github-rest\nunknown: true\n",
	} {
		_, err := config.Load(writeConfig(t, contents))
		if err == nil {
			t.Fatalf("expected strict YAML-only rejection for %q", contents)
		}
	}
}

func TestLoadAndResolveRepositoryScopedSkills(t *testing.T) {
	repositoryRoot := t.TempDir()
	path := filepath.Join(repositoryRoot, ".syntroph", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`skills:
  enabled: true
  runtime_lock: .syntroph/skills.runtime.lock.yaml
  sources:
    - id: mattpocock
      path: .agents/skills
  aliases:
    review: mattpocock/code-review
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveSkills(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Enabled || resolved.RuntimeLock != filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml") {
		t.Fatalf("unexpected skills resolution: %+v", resolved)
	}
	if len(resolved.Sources) != 2 || resolved.Sources[0].ID != "local" || resolved.Sources[0].Root != filepath.Join(repositoryRoot, ".syntroph", "skills") || resolved.Sources[1].ID != "mattpocock" || resolved.Sources[1].Root != filepath.Join(repositoryRoot, ".agents", "skills") {
		t.Fatalf("unexpected skill sources: %+v", resolved.Sources)
	}
	if resolved.Aliases["review"] != "mattpocock/code-review" {
		t.Fatalf("unexpected aliases: %+v", resolved.Aliases)
	}
}

func TestResolveSkillsRejectsSourceSymlinkEscapingRepository(t *testing.T) {
	repositoryRoot := t.TempDir()
	external := t.TempDir()
	link := filepath.Join(repositoryRoot, "linked-skills")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := config.Config{Skills: &config.SkillsConfig{
		Enabled:     true,
		RuntimeLock: ".syntroph/skills.runtime.lock.yaml",
		Sources:     []config.SkillSourceConfig{{ID: "external", Path: "linked-skills"}},
	}}
	if _, err := cfg.ResolveSkills(repositoryRoot); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping source resolution error = %v, want repository escape rejection", err)
	}
}

func TestResolveSkillsRejectsAmbiguousOrEscapingConfiguration(t *testing.T) {
	repositoryRoot := t.TempDir()
	tests := []struct {
		name string
		cfg  config.SkillsConfig
		want string
	}{
		{
			name: "missing runtime lock",
			cfg:  config.SkillsConfig{Enabled: true},
			want: "skills.runtime_lock is required",
		},
		{
			name: "duplicate source identity",
			cfg: config.SkillsConfig{Enabled: true, RuntimeLock: ".syntroph/lock.yaml", Sources: []config.SkillSourceConfig{
				{ID: "source", Path: "skills/a"}, {ID: "source", Path: "skills/b"},
			}},
			want: "duplicate skill source id",
		},
		{
			name: "reserved local identity",
			cfg: config.SkillsConfig{Enabled: true, RuntimeLock: ".syntroph/lock.yaml", Sources: []config.SkillSourceConfig{
				{ID: "local", Path: "skills"},
			}},
			want: "duplicate skill source id",
		},
		{
			name: "escaping source",
			cfg: config.SkillsConfig{Enabled: true, RuntimeLock: ".syntroph/lock.yaml", Sources: []config.SkillSourceConfig{
				{ID: "source", Path: "../outside"},
			}},
			want: "escapes the Repository Installation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (config.Config{Skills: &tt.cfg}).ResolveSkills(repositoryRoot)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsUnknownSkillConfigurationFields(t *testing.T) {
	_, err := config.Load(writeConfig(t, `skills:
  enabled: true
  runtime_lock: .syntroph/lock.yaml
  automatic_download: true
`))
	if err == nil || !strings.Contains(err.Error(), "automatic_download") {
		t.Fatalf("unknown skill field error = %v", err)
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
