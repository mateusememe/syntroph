# Contributing to Syntroph

Contributions must preserve the hexagonal boundary: domain contracts and event semantics belong in the Core; integrations belong behind ports and adapters.

## Before opening a pull request

1. Read [CONTEXT.md](CONTEXT.md) and relevant [ADRs](docs/adr/).
2. Update the domain model and add an ADR for difficult-to-reverse architectural decisions.
3. Add contract tests for every port or adapter change.
4. Keep external effects idempotent by `event_id` and preserve the append-only Saga Journal.
5. Never commit credentials, transcripts, generated graph caches, or real `.syntroph/memory/` data.

## Development workflow

```sh
git checkout -b feat/short-description
go test ./...
go vet ./...
git diff --check
```

Use focused Conventional Commit subjects, such as `feat(core): add event envelope validation`.

## Pull requests

Describe the problem, chosen boundary, tests run, and manual verification still open. If a remote adapter cannot be exercised locally, include a deterministic fake or contract fixture and state the limitation clearly.
