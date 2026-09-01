package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mateusememe/syntroph/adapters/githubissues"
	"github.com/mateusememe/syntroph/adapters/githubwiki"
	"github.com/mateusememe/syntroph/adapters/storageadapter"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

func doctorStorage(args []string, out, errOut interface{ Write([]byte) (int, error) }) error {
	fs := flag.NewFlagSet("doctor storage", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", ".syntroph", "Syntroph data root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("doctor storage accepts only --root")
	}
	port, report := loadConfiguredStorage(context.Background(), *root, filepath.Dir(*root), defaultStorageDependencies())
	if preflight, ok := port.(core.StoragePreflightPort); ok {
		report = preflight.Preflight(context.Background())
	}
	if !report.Enabled {
		fmt.Fprintln(out, "Storage mirroring is disabled; no remote checks or effects were performed.")
		return nil
	}
	fmt.Fprintf(out, "Storage: backend=%s provider=%s repository=%s\n", valueOrDash(report.Backend), valueOrDash(report.Provider), valueOrDash(report.Repository))
	for _, check := range report.Checks {
		fmt.Fprintf(out, "[%s] %s: %s", check.Status, check.Name, check.Detail)
		if check.Action != "" {
			fmt.Fprintf(out, " Next: %s", check.Action)
		}
		fmt.Fprintln(out)
	}
	if !report.Ready {
		return errors.New("storage prerequisites are missing")
	}
	fmt.Fprintln(out, "Storage preflight passed. No authentication or remote write was performed.")
	return nil
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

type storageDependencies struct {
	origin          func(context.Context, string) (string, error)
	getenv          func(string) string
	lookPath        func(string) (string, error)
	gitConfig       func(context.Context, string, string, string) (string, error)
	providerBuilder func(config.ResolvedStorage) storageadapter.Provider
	wikiBuilder     func(config.ResolvedStorage, string, string, string) storageadapter.Provider
}

var storageProviderBuilder = func(config.ResolvedStorage) storageadapter.Provider { return nil }
var wikiStorageProviderBuilder = buildWikiStorageProvider

func defaultStorageDependencies() storageDependencies {
	return storageDependencies{
		origin:          gitOrigin,
		getenv:          os.Getenv,
		lookPath:        exec.LookPath,
		gitConfig:       readGitConfig,
		providerBuilder: storageProviderBuilder,
		wikiBuilder:     wikiStorageProviderBuilder,
	}
}

func configuredGraphExecutable(root string) string {
	cfg, err := config.Load(filepath.Join(root, "config.yaml"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.GraphifyExecutable)
}

func configuredStorage(root, repositoryRoot string) core.StoragePort {
	port, _ := loadConfiguredStorage(context.Background(), root, repositoryRoot, defaultStorageDependencies())
	return port
}

func loadConfiguredStorage(ctx context.Context, root, repositoryRoot string, deps storageDependencies) (core.StoragePort, core.StoragePreflightResult) {
	cfg, err := config.Load(filepath.Join(root, "config.yaml"))
	if err != nil {
		report := failedPreflight("configuration", err, "Fix .syntroph/config.yaml, then run syntroph doctor storage again.")
		return storageadapter.Adapter{Check: staticCheck(report)}, report
	}
	if cfg.Storage == nil {
		report := core.StoragePreflightResult{Ready: true, Checks: []core.StoragePreflightCheck{{Name: "configuration", Status: "disabled", Detail: "Remote storage mirroring is not configured."}}}
		return nil, report
	}
	origin, err := deps.origin(ctx, repositoryRoot)
	if err != nil {
		report := failedPreflight("repository origin", err, "Configure remote.origin.url for this GitHub repository.")
		return storageadapter.Adapter{Check: staticCheck(report)}, report
	}
	resolved, err := cfg.ResolveStorage(origin)
	if err != nil {
		report := failedPreflight("configuration", err, "Fix .syntroph/config.yaml, then run syntroph doctor storage again.")
		return storageadapter.Adapter{Check: staticCheck(report)}, report
	}
	report := checkStoragePrerequisites(ctx, repositoryRoot, resolved, deps)
	var provider storageadapter.Provider
	if report.Ready && resolved.Provider == config.ProviderGitHubWikiGit && deps.wikiBuilder != nil {
		provider = deps.wikiBuilder(resolved, root, repositoryRoot, origin)
	} else if report.Ready && deps.providerBuilder != nil {
		provider = deps.providerBuilder(resolved)
	}
	if report.Ready && provider == nil {
		provider = concreteStorageProvider(resolved, root, deps.getenv)
	}
	adapter := storageadapter.Adapter{Provider: provider, Backend: resolved.Backend, ProviderID: resolved.Provider, Check: staticCheck(report)}
	return adapter, report
}

func buildWikiStorageProvider(resolved config.ResolvedStorage, root, repositoryRoot, origin string) storageadapter.Provider {
	gitExecutable := strings.TrimSpace(resolved.Wiki.GitExecutable)
	if gitExecutable == "" {
		gitExecutable = "git"
	}
	provider, err := githubwiki.NewManaged(filepath.Join(root, "storage"), githubwiki.Options{
		Repository: resolved.Repository, RepositoryRoot: repositoryRoot,
		RemoteURL: wikiRemoteURL(origin, resolved), WebURL: "https://github.com/" + resolved.Repository + "/wiki",
		GitExecutable: gitExecutable,
		Author:        githubwiki.Author{Name: resolved.Wiki.CommitAuthor.Name, Email: resolved.Wiki.CommitAuthor.Email},
	})
	if err != nil {
		return nil
	}
	return provider
}

func wikiRemoteURL(origin string, resolved config.ResolvedStorage) string {
	if resolved.CrossRepository {
		value := strings.TrimSpace(origin)
		if strings.HasPrefix(value, "git@github.com:") {
			return "git@github.com:" + resolved.Repository + ".wiki.git"
		}
		if strings.HasPrefix(value, "ssh://git@github.com/") {
			return "ssh://git@github.com/" + resolved.Repository + ".wiki.git"
		}
		return "https://github.com/" + resolved.Repository + ".wiki.git"
	}
	value := strings.TrimSpace(origin)
	if strings.HasSuffix(value, ".git") {
		return strings.TrimSuffix(value, ".git") + ".wiki.git"
	}
	if strings.Contains(value, "github.com") {
		return strings.TrimRight(value, "/") + ".wiki.git"
	}
	return "https://github.com/" + resolved.Repository + ".wiki.git"
}

func concreteStorageProvider(resolved config.ResolvedStorage, root string, getenv func(string) string) storageadapter.Provider {
	storageRoot := filepath.Join(root, "storage")
	var remote interface {
		Mirror(context.Context, storage.SessionDiary) storage.MirrorResult
		Status(context.Context, storage.SessionDiary) storage.MirrorResult
		Resolve(context.Context, storage.SessionDiary, storage.Resolution, string) storage.MirrorResult
	}
	var err error
	switch resolved.Provider {
	case config.ProviderGitHubREST:
		remote, err = githubissues.New(resolved.Repository, getenv(storage.GitHubTokenEnvironment), storageRoot, githubissues.Options{})
	case config.ProviderGitHubMCP:
		remote, err = githubissues.NewMCP(resolved.Repository, resolved.MCP.Command, storageRoot, githubissues.MCPOptions{})
	default:
		return nil
	}
	if err != nil {
		return failedStorageProvider{backend: storage.BackendIssues, provider: resolved.Provider, cause: err}
	}
	managed, err := storage.NewManagedMirror(storageRoot, resolved.Provider, remote)
	if err != nil {
		return failedStorageProvider{backend: storage.BackendIssues, provider: resolved.Provider, cause: err}
	}
	return managed
}

type failedStorageProvider struct {
	backend  storage.Backend
	provider string
	cause    error
}

func (p failedStorageProvider) Mirror(_ context.Context, diary storage.SessionDiary) storage.MirrorResult {
	state, class := storage.StorageSyncPending, storage.FailureTransient
	if errors.Is(p.cause, storage.ErrPrerequisiteMissing) {
		state, class = storage.StoragePrerequisiteMissing, storage.FailurePrerequisite
	}
	return storage.MirrorResult{State: state, Backend: p.backend, Provider: p.provider, Key: diary.Key(), FailureClass: class, Cause: p.cause}
}

func (p failedStorageProvider) Resolve(_ context.Context, diary storage.SessionDiary, _ storage.Resolution, _ string) storage.MirrorResult {
	return p.Mirror(context.Background(), diary)
}

func staticCheck(report core.StoragePreflightResult) func(context.Context) core.StoragePreflightResult {
	return func(context.Context) core.StoragePreflightResult { return report }
}

func failedPreflight(name string, cause error, action string) core.StoragePreflightResult {
	return core.StoragePreflightResult{
		Enabled: true, Cause: cause,
		Checks: []core.StoragePreflightCheck{{Name: name, Status: "missing", Detail: cause.Error(), Action: action}, recoveryCheck()},
	}
}

func checkStoragePrerequisites(ctx context.Context, repositoryRoot string, resolved config.ResolvedStorage, deps storageDependencies) core.StoragePreflightResult {
	report := core.StoragePreflightResult{
		Enabled: true, Ready: true, Backend: resolved.Backend, Provider: resolved.Provider, Repository: resolved.Repository,
		Checks: []core.StoragePreflightCheck{
			{Name: "configuration", Status: "ok", Detail: fmt.Sprintf("Exactly one supported provider is selected: %s with %s.", resolved.Backend, resolved.Provider)},
		},
	}
	if resolved.CrossRepository {
		report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: "repository safety", Status: "ok", Detail: fmt.Sprintf("Cross-repository destination %s is explicitly allowed; origin is %s.", resolved.Repository, resolved.OriginRepository)})
	} else {
		report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: "repository safety", Status: "ok", Detail: fmt.Sprintf("Destination %s matches the normalized origin.", resolved.Repository)})
	}

	switch resolved.Provider {
	case config.ProviderGitHubREST:
		if strings.TrimSpace(deps.getenv("SYNTROPH_GITHUB_TOKEN")) == "" {
			addMissing(&report, "credentials", "SYNTROPH_GITHUB_TOKEN is not set.", "Inject a GitHub token or GitHub App installation token through SYNTROPH_GITHUB_TOKEN; Syntroph never stores it.")
		} else {
			report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: "credentials", Status: "ok", Detail: "SYNTROPH_GITHUB_TOKEN is present; its value was not read into configuration or displayed."})
		}
	case config.ProviderGitHubMCP:
		if len(resolved.MCP.Command) == 0 || strings.TrimSpace(resolved.MCP.Command[0]) == "" {
			addMissing(&report, "MCP process", "storage.mcp.command is required for github-mcp.", "Configure the MCP stdio server command; that process owns its own authentication, and Syntroph does not reuse an agent host's MCP session.")
		} else if _, err := deps.lookPath(resolved.MCP.Command[0]); err != nil {
			addMissing(&report, "MCP process", fmt.Sprintf("MCP executable %q is unavailable.", resolved.MCP.Command[0]), "Install the configured executable or correct storage.mcp.command.")
		} else {
			report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: "MCP process", Status: "ok", Detail: fmt.Sprintf("MCP stdio executable %q is available; it was not started and owns its own authentication.", resolved.MCP.Command[0])})
		}
	case config.ProviderGitHubWikiGit:
		gitExecutable := strings.TrimSpace(resolved.Wiki.GitExecutable)
		if gitExecutable == "" {
			gitExecutable = "git"
		}
		if _, err := deps.lookPath(gitExecutable); err != nil {
			addMissing(&report, "Git process", fmt.Sprintf("Git executable %q is unavailable.", gitExecutable), "Install Git or correct storage.wiki.git_executable.")
		} else {
			report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: "Git process", Status: "ok", Detail: fmt.Sprintf("Git executable %q is available; no remote command was run.", gitExecutable)})
			checkWikiAuthor(ctx, repositoryRoot, gitExecutable, resolved, deps, &report)
		}
		report.Checks = append(report.Checks, core.StoragePreflightCheck{
			Name: "Wiki initialization", Status: "info",
			Detail: "GitHub requires the Wiki to be enabled and initialized with at least one page before its Git repository exists.",
			Action: "Enable Wiki in repository settings and create the first page on GitHub before running syntroph sync retry --storage.",
		})
	}
	report.Checks = append(report.Checks, recoveryCheck())
	return report
}

func checkWikiAuthor(ctx context.Context, repositoryRoot, gitExecutable string, resolved config.ResolvedStorage, deps storageDependencies, report *core.StoragePreflightResult) {
	name := strings.TrimSpace(resolved.Wiki.CommitAuthor.Name)
	email := strings.TrimSpace(resolved.Wiki.CommitAuthor.Email)
	if (name == "") != (email == "") {
		addMissing(report, "Wiki commit author", "storage.wiki.commit_author requires both name and email.", "Configure both author fields or remove both to use existing Git user.name and user.email.")
		return
	}
	if name != "" {
		report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: "Wiki commit author", Status: "ok", Detail: fmt.Sprintf("Wiki commits will use configured author %s <%s>; Git configuration was not changed.", name, email)})
		return
	}
	name, nameErr := deps.gitConfig(ctx, gitExecutable, repositoryRoot, "user.name")
	email, emailErr := deps.gitConfig(ctx, gitExecutable, repositoryRoot, "user.email")
	if nameErr != nil || emailErr != nil || strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		addMissing(report, "Wiki commit author", "No complete storage.wiki.commit_author or existing Git user.name/user.email is available.", "Configure storage.wiki.commit_author or Git identity; Syntroph never changes Git configuration.")
		return
	}
	report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: "Wiki commit author", Status: "ok", Detail: fmt.Sprintf("Wiki commits will use existing Git author %s <%s>; configuration was not changed.", strings.TrimSpace(name), strings.TrimSpace(email))})
}

func addMissing(report *core.StoragePreflightResult, name, detail, action string) {
	report.Ready = false
	report.Checks = append(report.Checks, core.StoragePreflightCheck{Name: name, Status: "missing", Detail: detail, Action: action})
	if report.Cause == nil {
		report.Cause = errors.New(detail)
	}
}

func recoveryCheck() core.StoragePreflightCheck {
	return core.StoragePreflightCheck{
		Name: "recovery", Status: "info",
		Detail: "Diagnostics perform no remote writes, authentication, configuration changes, or automatic recovery.",
		Action: "Inspect with syntroph sync recovery; after fixing prerequisites, confirm explicitly with syntroph sync retry --storage.",
	}
}

func gitOrigin(ctx context.Context, repositoryRoot string) (string, error) {
	return readGitConfig(ctx, "git", repositoryRoot, "remote.origin.url")
}

func readGitConfig(ctx context.Context, executable, repositoryRoot, key string) (string, error) {
	cmd := exec.CommandContext(ctx, executable, "-C", repositoryRoot, "config", "--get", key)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read Git %s: %w", key, err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("Git %s is empty", key)
	}
	return value, nil
}
