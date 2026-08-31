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

### GitHub Issues through an MCP stdio process

```yaml
storage:
  backend: issues
  provider: github-mcp
  mcp:
    command: ["your-github-mcp-server", "stdio"]
```

The configured MCP process owns its authentication. A session already open in Codex or Claude is not automatically reusable by the separate Syntroph CLI process. `syntroph doctor storage` verifies the configured executable without starting it.

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
