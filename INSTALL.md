# Installation

Syntroph is under active development and does not yet publish a stable binary release.

## Prerequisites

- Go 1.23 or newer
- Git
- Optional Graphify installation for the real GraphPort spike
- Optional GitHub access through an existing token or configured local MCP client

## Install from source

```sh
git clone https://github.com/mateusememe/syntroph.git
cd syntroph
go install ./cmd/syntroph
```

When the CLI implementation and tagged releases are available:

```sh
go install github.com/mateusememe/syntroph/cmd/syntroph@latest
```

Verify with `syntroph --version`.

## Initialize a repository

```sh
cd path/to/your/repository
syntroph init
```

The `init` wizard is planned but not yet exposed by the current CLI. The current session-close command creates the required local state on first use. One installation manages one Git repository in the MVP.

## GitHub mirroring

`.syntroph/config.yaml` is the only supported configuration file. JSON configuration is not accepted. Select exactly one backend/provider pair; Syntroph never falls back to another provider automatically.

### GitHub Issues through REST

```yaml
storage:
  backend: issues
  provider: github-rest
```

The destination defaults to the normalized GitHub `origin`. Inject only the dedicated environment variable before running a mirror command:

```sh
export SYNTROPH_GITHUB_TOKEN="<existing token or GitHub App installation token>"
syntroph doctor storage
```

Syntroph does not use `GITHUB_TOKEN`, run `gh auth login`, inspect private `gh` credential files, or persist the token.

The token must be able to read and write Issues and repository labels for the selected repository. Syntroph creates or reconciles the reserved `syntroph-memory` and `syntroph-session` labels before creating a mirror, then creates one completed Issue per Session Diary. If label administration or Issues access is unavailable, the local diary remains successful and recovery reports `StoragePrerequisiteMissing`; no unlabeled Issue is created.

Remote failures are retried only when explicitly transient and remain visible through `syntroph sync recovery`. After fixing a prerequisite, run `syntroph sync retry --storage`. If a human edits the remote Issue body, `syntroph sync resolve <session-id> --keep-local` displays the persisted diff and appends a correction comment; `--keep-remote` accepts the edit. Neither choice overwrites the existing Issue body.

### GitHub Issues through an MCP stdio process

```yaml
storage:
  backend: issues
  provider: github-mcp
  mcp:
    command: ["github-mcp-server", "stdio", "--toolsets=issues,labels"]
```

The configured MCP process owns its authentication. Configure it for non-interactive authentication through its own environment, GitHub App, or authenticated wrapper; never put a token in `.syntroph/config.yaml`. A session already open in Codex or Claude is not automatically reusable by the separate Syntroph CLI process.

The `issues,labels` toolsets are both required. Before the first remote effect, Syntroph starts the configured executable directly (never through a shell), negotiates MCP, paginates `tools/list`, and validates schemas for label read/write, Issue read/create/update, comments, and labeled Issue listing. Missing tools or read-only mode become `StoragePrerequisiteMissing` and create no Issue. `syntroph doctor storage` verifies the executable without starting it; the capability check occurs when a confirmed mirror or retry starts, so doctor never opens an interactive login flow.

Syntroph starts one stdio process for the complete mirror operation, reuses it for every MCP tool call, and closes its stdin when the operation finishes. HTTP MCP endpoints and hidden daemons are not supported.

### GitHub Wiki through Git

```yaml
storage:
  backend: wiki
  provider: github-wiki-git
  wiki:
    commit_author:
      name: Syntroph
      email: syntroph@example.com
```

Git authentication is delegated to the existing credential helper or SSH configuration. Syntroph never reads credentials or modifies Git configuration. If `commit_author` is omitted, both existing `user.name` and `user.email` must be available. GitHub Wiki must already be enabled and initialized with its first page; Syntroph does not change repository settings or create that first page.

### Explicit cross-repository destination

A different destination is rejected unless both the destination and opt-in are present:

```yaml
storage:
  backend: issues
  provider: github-rest
  repository: owner/central-memory
  allow_cross_repository: true
```

After every clone, run:

```sh
syntroph doctor storage
```

Doctor is non-mutating: it does not authenticate, edit configuration, initialize Wiki, or write remotely. Session close runs the same preflight only after its local Session Diary is durable. Missing prerequisites become `StoragePrerequisiteMissing`; fix them, inspect `syntroph sync recovery`, then confirm an external retry explicitly with `syntroph sync retry --storage`.

## Storage recovery

The local Session Diary under `.syntroph/memory/` is canonical and remains successful even when a mirror fails. Recovery is never executed automatically after a crash.

```sh
syntroph sync status
syntroph sync recovery
syntroph sync retry --storage
syntroph sync resolve <session-id> --keep-local
syntroph sync resolve <session-id> --keep-remote
```

`sync recovery` shows the selected provider, remote identity and typed revision when known, the failure class, private conflict snapshot, and the next explicit action. Retry preserves the original Idempotency Key and provider. Conflict resolution displays the local/remote diff first: Issues append a correction comment for `--keep-local`, while Wiki writes an auditable commit; `--keep-remote` accepts the observed remote revision. Never change provider while recovering one operation.

## Opt-in live GitHub smoke

Normal `go test ./...` and pull-request CI are offline. The live smoke is behind the `livegithub` build tag and a manual `workflow_dispatch`; the workflow has no `pull_request` trigger and should use a protected `live-storage-smoke` environment. Run it only against a dedicated disposable repository, never a production tracker or Wiki.

Configure these repository environment values:

- Variable `SYNTROPH_LIVE_GITHUB_REPOSITORY`: dedicated `owner/name` target.
- Optional variable `SYNTROPH_LIVE_RUNNER`: a runner label with the required Git/MCP authentication; it defaults to `ubuntu-latest`.
- Optional variable `SYNTROPH_LIVE_WIKI_REMOTE_URL`: authenticated Wiki remote when the normalized HTTPS URL is unsuitable.
- Secret `SYNTROPH_LIVE_GITHUB_TOKEN`: used only by the `github-rest` job.
- Secret `SYNTROPH_LIVE_MCP_COMMAND_JSON`: a JSON string array such as `["github-mcp-server","stdio","--toolsets=issues,labels"]`; the command must not contain a token. The MCP process owns authentication.

The Wiki job receives no REST token. Its runner must already have a credential helper or SSH identity that can push to an enabled, initialized Wiki. The MCP runner must already provide the configured executable and non-interactive provider-owned authentication with Issue read/create/update, comment, labels, and Issue search capabilities.

Start **Live storage smoke** from GitHub Actions, select one provider, and provide a new opaque `run_id`, for example `2026-09-01-rest-01`. The identity is included in the deterministic Session ID and Idempotency Key. The test mirrors once, replays the same operation, checks status and its typed Remote Binding, and prints `LIVE_SMOKE_RESOURCE` with the exact remote URL.

For a local maintainer run, set only the variables required by the selected provider plus the explicit confirmation:

```sh
export SYNTROPH_LIVE_GITHUB_CONFIRM=I_UNDERSTAND_THIS_CREATES_PERSISTENT_GITHUB_RESOURCES
export SYNTROPH_LIVE_STORAGE_PROVIDER=github-rest
export SYNTROPH_LIVE_GITHUB_REPOSITORY=owner/disposable-syntroph-smoke
export SYNTROPH_LIVE_RUN_ID=2026-09-01-rest-01
export SYNTROPH_GITHUB_TOKEN='<dedicated token>'
go test -tags=livegithub ./integration/livegithub -run '^TestConfiguredGitHubStorageProvider$' -count=1 -v
```

Smoke mirrors are intentionally persistent because deleting them would weaken the production immutability contract. Issues and MCP runs leave one closed, labeled Issue; Wiki runs leave one deterministic page/commit. Clean up only through the dedicated repository's normal administrative lifecycle (or delete the Wiki page with an explicit external Git commit). If a run fails after a remote effect, retain its logs and `run_id`, fix the prerequisite, and rerun the same provider and `run_id`; the exact marker reconstructs a missing local binding without creating a second mirror. Do not retry through another provider.
