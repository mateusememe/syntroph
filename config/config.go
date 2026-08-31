// Package config loads and validates the repository-scoped Syntroph
// configuration. Before the first stable release, config.yaml is deliberately
// the only accepted format.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	BackendIssues = "issues"
	BackendWiki   = "wiki"

	ProviderGitHubREST    = "github-rest"
	ProviderGitHubMCP     = "github-mcp"
	ProviderGitHubWikiGit = "github-wiki-git"
)

type Config struct {
	GraphifyExecutable string         `yaml:"graphify_executable,omitempty"`
	Storage            *StorageConfig `yaml:"storage,omitempty"`
}

type StorageConfig struct {
	Backend              string     `yaml:"backend"`
	Provider             string     `yaml:"provider"`
	Repository           string     `yaml:"repository,omitempty"`
	AllowCrossRepository bool       `yaml:"allow_cross_repository,omitempty"`
	MCP                  MCPConfig  `yaml:"mcp,omitempty"`
	Wiki                 WikiConfig `yaml:"wiki,omitempty"`
}

type MCPConfig struct {
	Command []string `yaml:"command,omitempty"`
}

type WikiConfig struct {
	GitExecutable string       `yaml:"git_executable,omitempty"`
	CommitAuthor  CommitAuthor `yaml:"commit_author,omitempty"`
}

type CommitAuthor struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// ResolvedStorage contains the one provider and destination that may be used
// for this installation. No fallback provider is represented by this type.
type ResolvedStorage struct {
	Enabled              bool
	Backend              string
	Provider             string
	Repository           string
	OriginRepository     string
	CrossRepository      bool
	AllowCrossRepository bool
	MCP                  MCPConfig
	Wiki                 WikiConfig
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read Syntroph configuration: %w", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Config{}, nil
	}
	// YAML is a superset of JSON. Reject JSON syntax explicitly so the removed
	// prototype cannot remain an undocumented compatibility format.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return Config{}, errors.New("Syntroph configuration must use YAML syntax in .syntroph/config.yaml; JSON is not supported")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse Syntroph YAML configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("Syntroph configuration must contain exactly one YAML document")
		}
		return Config{}, fmt.Errorf("parse Syntroph YAML configuration: %w", err)
	}
	return cfg, nil
}

func (c Config) ResolveStorage(origin string) (ResolvedStorage, error) {
	if c.Storage == nil {
		return ResolvedStorage{}, nil
	}
	s := *c.Storage
	s.Backend = strings.TrimSpace(s.Backend)
	s.Provider = strings.TrimSpace(s.Provider)
	s.Repository = strings.TrimSpace(s.Repository)
	if s.Backend == "" {
		return ResolvedStorage{}, errors.New("storage backend is required when storage is configured")
	}
	if s.Provider == "" {
		return ResolvedStorage{}, errors.New("storage provider is required when storage is configured")
	}
	switch s.Backend {
	case BackendIssues:
		if s.Provider != ProviderGitHubREST && s.Provider != ProviderGitHubMCP {
			return ResolvedStorage{}, fmt.Errorf("issues requires provider %s or %s", ProviderGitHubREST, ProviderGitHubMCP)
		}
	case BackendWiki:
		if s.Provider != ProviderGitHubWikiGit {
			return ResolvedStorage{}, fmt.Errorf("wiki requires provider %s", ProviderGitHubWikiGit)
		}
	default:
		return ResolvedStorage{}, fmt.Errorf("unsupported storage backend %q", s.Backend)
	}
	if s.Provider != ProviderGitHubMCP && len(s.MCP.Command) != 0 {
		return ResolvedStorage{}, fmt.Errorf("storage.mcp is only valid with provider %s", ProviderGitHubMCP)
	}
	if s.Provider != ProviderGitHubWikiGit && (s.Wiki.GitExecutable != "" || s.Wiki.CommitAuthor.Name != "" || s.Wiki.CommitAuthor.Email != "") {
		return ResolvedStorage{}, fmt.Errorf("storage.wiki is only valid with provider %s", ProviderGitHubWikiGit)
	}

	originRepository, err := NormalizeGitHubRepository(origin)
	if err != nil {
		return ResolvedStorage{}, fmt.Errorf("derive storage repository from origin: %w", err)
	}
	destination := originRepository
	if s.Repository != "" {
		destination, err = NormalizeGitHubRepository(s.Repository)
		if err != nil {
			return ResolvedStorage{}, fmt.Errorf("invalid storage.repository: %w", err)
		}
	}
	cross := destination != originRepository
	if cross && (!s.AllowCrossRepository || s.Repository == "") {
		return ResolvedStorage{}, fmt.Errorf("storage.repository %q differs from origin %q; set the explicit destination and allow_cross_repository: true", destination, originRepository)
	}
	return ResolvedStorage{
		Enabled: true, Backend: s.Backend, Provider: s.Provider,
		Repository: destination, OriginRepository: originRepository,
		CrossRepository: cross, AllowCrossRepository: s.AllowCrossRepository,
		MCP: s.MCP, Wiki: s.Wiki,
	}, nil
}

// NormalizeGitHubRepository converts GitHub HTTPS, SSH, SCP-style, and
// owner/name inputs into the canonical owner/name storage destination.
func NormalizeGitHubRepository(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("Git origin is missing; configure remote.origin.url before enabling storage")
	}
	path := value
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return "", errors.New("repository must be hosted on github.com")
		}
		path = u.Path
	} else if at := strings.Index(value, "@"); at >= 0 {
		colon := strings.Index(value[at:], ":")
		if colon < 0 || !strings.EqualFold(value[at+1:at+colon], "github.com") {
			return "", errors.New("repository must be hosted on github.com")
		}
		path = value[at+colon+1:]
	} else if strings.HasPrefix(strings.ToLower(value), "github.com/") {
		path = value[len("github.com/"):]
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" || strings.ContainsAny(path, "?#") {
		return "", fmt.Errorf("repository %q must be owner/name or a GitHub remote URL", value)
	}
	return parts[0] + "/" + parts[1], nil
}
