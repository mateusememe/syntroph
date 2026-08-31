# Pluggable GitHub credential providers

GitHub mirror adapters declare `storage.github.provider` as `env-token` or `mcp` and receive an already-authenticated client. Syntroph never stores or prompts for tokens; the provider mechanism remains behind the adapter boundary so either direct GitHub API or a local MCP can be selected without changing the core.
