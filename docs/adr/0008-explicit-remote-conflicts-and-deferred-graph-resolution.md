# Explicit remote conflicts and deferred graph resolution

An unexpected remote revision produces Storage Sync Conflict and awaits explicit CLI action rather than a silent overwrite. A missing, failed, or stale GraphPort produces Graph Resolution Pending while the Session Diary remains durable, so code enrichment can be retried without blocking or invalidating session history.
