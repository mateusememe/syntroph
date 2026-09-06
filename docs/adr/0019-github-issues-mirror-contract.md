# GitHub Issues mirror contract

The REST provider accepts credentials only from `SYNTROPH_GITHUB_TOKEN`, including externally minted GitHub App installation tokens. It never falls back to `GITHUB_TOKEN`. Before creating a mirror, REST or MCP ensures the reserved `syntroph-memory` and `syntroph-session` labels exist with fixed metadata; inability to manage them records Storage Prerequisite Missing and creates no issue.

Each mirror is titled `[Syntroph] YYYY-MM-DD — <session_id>`, contains the complete Session Diary plus an invisible Idempotency Key marker, receives both labels, and is immediately closed with the completed state reason. Remote Corrections are versioned comments, never body overwrites. REST retries at most three times for 429, explicit secondary limits, 502, 503, and 504, honoring Retry-After or exponential backoff with jitter. Authentication, authorization, validation, and conflicts are not retried automatically.
