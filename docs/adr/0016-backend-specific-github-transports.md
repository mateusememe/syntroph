# Backend-specific GitHub transports

StoragePort preserves common mirror semantics without pretending that GitHub backends share a transport. Issues supports `github-rest` or `github-mcp`; Wiki supports `github-wiki-git`, because GitHub exposes Wiki as a separate Git repository and the official MCP catalog has no Wiki tools. Invalid backend/provider combinations fail configuration validation before any external effect.

Each Issue mirror is its own immediately closed issue with the reserved labels `syntroph-memory` and `syntroph-session`. Each Wiki mirror is its own page. Both are keyed by the diary Idempotency Key and persist a Remote Binding locally; a deterministic invisible marker enables recovery search when the binding is lost.
