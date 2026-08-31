# Git-backed Wiki mirrors

GitHub Wiki mirrors use the configured `git` executable and delegate authentication to existing SSH or credential-helper configuration. Each operation fetches or clones a temporary Wiki repository, verifies the observed remote HEAD, writes `Sessions/YYYY/MM/<session_id>-<artifact_hash_short>.md`, commits as `syntroph: mirror session <session_id>`, and pushes fast-forward only. Non-fast-forward records Storage Sync Conflict; credential, repository, Wiki bootstrap, or author configuration failures record Storage Prerequisite Missing.

Commit authorship comes from `storage.wiki.commit_author` with fallback to repository Git `user.name` and `user.email`. Syntroph never edits Git configuration. Page frontmatter carries the Idempotency Key and binding marker.
