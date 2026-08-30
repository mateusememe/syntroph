# Installation

Syntroph is under active development and does not yet publish a stable binary release.

## Prerequisites

- Go (the version pinned by the project's future `go.mod`)
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

This creates `.syntroph/`, local memory and journal directories, templates, and baseline configuration. One installation manages one Git repository in the MVP.

## GitHub mirroring

```yaml
storage:
  backend: wiki
  github:
    provider: env-token # env-token | mcp
    repository: owner/name
```

Credentials are provided externally. Syntroph never stores or asks for GitHub tokens.
