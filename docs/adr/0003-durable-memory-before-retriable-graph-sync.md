# Durable memory before retriable graph sync

When graph synchronization fails after a session closes, Syntroph keeps the persisted Session Diary and records Graph Sync Pending for idempotent retry by the user, a session skill, or a local MCP integration. It does not compensate by deleting or reverting already persisted knowledge, because that would misrepresent the session history.
