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

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
