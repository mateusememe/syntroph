# Pluggable GitHub credential providers (superseded)

This initial credential-only naming was superseded by ADRs 0015 and 0016. Configuration now selects one complete storage provider: `github-rest` or `github-mcp` for Issues, and `github-wiki-git` for Wiki. Direct REST accepts only externally injected `SYNTROPH_GITHUB_TOKEN`; MCP and Git own their authentication outside Syntroph. There is no automatic provider fallback.
