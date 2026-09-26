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
	"path/filepath"
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
	Skills             *SkillsConfig  `yaml:"skills,omitempty"`
}

type SkillsConfig struct {
	Enabled     bool                `yaml:"enabled"`
	RuntimeLock string              `yaml:"runtime_lock,omitempty"`
	Sources     []SkillSourceConfig `yaml:"sources,omitempty"`
	Aliases     map[string]string   `yaml:"aliases,omitempty"`
}

type SkillSourceConfig struct {
	ID   string `yaml:"id"`
	Path string `yaml:"path"`
}

type ResolvedSkillSource struct {
	ID   string
	Root string
}

type ResolvedSkills struct {
	Enabled     bool
	RuntimeLock string
	Sources     []ResolvedSkillSource
	Aliases     map[string]string
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

// ResolveSkills confines every configured input to one Repository
// Installation and adds the implicit repository-authored local source.
func (c Config) ResolveSkills(repositoryRoot string) (ResolvedSkills, error) {
	if c.Skills == nil || !c.Skills.Enabled {
		return ResolvedSkills{}, nil
	}
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return ResolvedSkills{}, fmt.Errorf("resolve repository root: %w", err)
	}
	lock, err := resolveRepositoryPath(root, c.Skills.RuntimeLock, "skills.runtime_lock")
	if err != nil {
		return ResolvedSkills{}, err
	}
	localRoot, err := resolveRepositoryPath(root, filepath.Join(".syntroph", "skills"), "implicit local skill source")
	if err != nil {
		return ResolvedSkills{}, err
	}
	resolved := ResolvedSkills{
		Enabled:     true,
		RuntimeLock: lock,
		Sources:     []ResolvedSkillSource{{ID: "local", Root: localRoot}},
		Aliases:     make(map[string]string, len(c.Skills.Aliases)),
	}
	seen := map[string]struct{}{"local": {}}
	for _, source := range c.Skills.Sources {
		id := strings.TrimSpace(source.ID)
		if id == "" || id != source.ID {
			return ResolvedSkills{}, errors.New("skill source id is required and must not contain surrounding whitespace")
		}
		if _, exists := seen[id]; exists {
			return ResolvedSkills{}, fmt.Errorf("duplicate skill source id %q", id)
		}
		sourceRoot, err := resolveRepositoryPath(root, source.Path, "skill source "+id)
		if err != nil {
			return ResolvedSkills{}, err
		}
		seen[id] = struct{}{}
		resolved.Sources = append(resolved.Sources, ResolvedSkillSource{ID: id, Root: sourceRoot})
	}
	for alias, target := range c.Skills.Aliases {
		alias, target = strings.TrimSpace(alias), strings.TrimSpace(target)
		if alias == "" || target == "" {
			return ResolvedSkills{}, errors.New("skill aliases require non-empty names and targets")
		}
		resolved.Aliases[alias] = target
	}
	return resolved, nil
}

func resolveRepositoryPath(repositoryRoot, value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required when skills are enabled", field)
	}
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be relative to the Repository Installation", field)
	}
	resolved := filepath.Clean(filepath.Join(repositoryRoot, value))
	relative, err := filepath.Rel(repositoryRoot, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes the Repository Installation", field)
	}
	canonicalRoot, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Repository Installation symlinks: %w", err)
	}
	canonicalResolved, err := evalPathWithExistingParent(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve %s symlinks: %w", field, err)
	}
	canonicalRelative, err := filepath.Rel(canonicalRoot, canonicalResolved)
	if err != nil || canonicalRelative == ".." || strings.HasPrefix(canonicalRelative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes the Repository Installation through a symlink", field)
	}
	return resolved, nil
}

func evalPathWithExistingParent(path string) (string, error) {
	candidate := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", err
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
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
		if u.User != nil && !(strings.EqualFold(u.Scheme, "ssh") && u.User.Username() == "git" && !userinfoHasPassword(u)) {
			return "", errors.New("Git remote URL must not contain userinfo or embedded credentials")
		}
		path = u.Path
	} else if at := strings.Index(value, "@"); at >= 0 {
		if !strings.HasPrefix(value, "git@") {
			return "", errors.New("Git remote URL must not contain userinfo or embedded credentials")
		}
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

func userinfoHasPassword(u *url.URL) bool {
	_, ok := u.User.Password()
	return ok
}
