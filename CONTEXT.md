# Syntroph Core

Syntroph coordinates development tools so that artifacts produced by one capability become useful context for another. This glossary defines the language of the first flow: session memory and graph synchronization within one repository.

## Memory

**Session Diary**: An immutable memory document created when a session closes, recording that session's evolution, context, decisions, and lessons.
_Avoid_: memory page, handoff page, mutable session note

**Compiled Memory**: An updateable projection derived from one or more Session Diaries. It never alters its source diaries.
_Avoid_: session diary, source memory

**Related Link**: An explicit link between memories that preserves provenance across diaries and projections.
_Avoid_: implicit context, orphan reference

## Synchronization

**Graph Sync Pending**: A state in which the diary is persisted but graph enrichment or push has not completed and can be safely resumed.
_Avoid_: failed memory, rolled-back session

**Storage Sync Pending**: A state in which the local canonical diary has not successfully mirrored to the configured remote backend, retaining destination and cause for retry.
_Avoid_: unsaved memory, lost remote page

**Storage Sync Conflict**: A mirror whose remote copy diverged from the Syntroph-known revision. In the MVP, the user resolves it explicitly through the CLI; the adapter never silently overwrites it.
_Avoid_: automatic overwrite, silent merge

**Remote Mirror**: The single remote object corresponding to one immutable Session Diary. It is derived from the diary's Idempotency Key and never aggregates multiple diaries.
_Avoid_: compiled memory, remote source of truth, shared diary page

**Storage Provider**: The explicitly selected remote integration responsible for one mirror attempt. Changing providers never happens as an automatic fallback.
_Avoid_: transparent fallback, credential owner

**Remote Binding**: The durable local association between a Session Diary and its Remote Mirror, including backend, provider, remote identity, URL, and observed revision. It is the normal lookup path; marker-based remote search is recovery only.
_Avoid_: remote cache, source of truth, search result

**Storage Prerequisite Missing**: A non-transient state in which an enabled Storage Provider cannot operate until the user completes an external repository or authentication prerequisite.
_Avoid_: storage unavailable, automatic setup

**Remote Correction**: An append-only record that makes the local Session Diary the effective version of a divergent Issue mirror without overwriting its existing body.
_Avoid_: force overwrite, silent merge

**Remote Revision**: A provider-specific immutable reference to the remote version observed by Syntroph and recorded in a Remote Binding.
_Avoid_: local timestamp, best-effort version

**Provider Capability**: A remote operation that a configured Storage Provider proves it can perform before the first mirror effect.
_Avoid_: assumed tool, optional permission

**Conflict Snapshot**: A private local copy of divergent remote content retained so recovery can present an offline diff without placing that content in the Remote Binding.
_Avoid_: remote binding, canonical diary, cached mirror

**Mirror Lock**: The local ownership record that prevents concurrent remote effects for the same Idempotency Key and can only be cleared through verified recovery when orphaned.
_Avoid_: remote lock, automatic stale lock

**Graph Resolution Pending**: A persisted diary whose code references could not yet be resolved by GraphPort and remain available for later processing.
_Avoid_: invalid diary, graph failure

**Repository Installation**: A `.syntroph/` instance owned by one Git repository, identified by normalized remote and the SHA associated with a session.
_Avoid_: workspace-wide installation, multi-repository installation

## Orchestration

**Saga Journal**: A local append-only record of saga states and transitions used for recovery and dashboard projection after process exit or crash.
_Avoid_: event broker, transient command log

**Event ID**: A stable Core event identifier used by handlers and external adapters to recognize redelivery of the same operation.
_Avoid_: request ID, invocation ID

**Idempotency Key**: A deterministic key formed from `repository_id`, commit SHA, and Session Artifact hash, distinguishing technical redelivery from another artifact at the same commit.
_Avoid_: session name, event ID

**Handler Attempt**: One execution of a handler for an event, recorded with `attempted_at`, `handler_id`, outcome, and error. The event remains immutable between attempts.
_Avoid_: event execution timestamp, event mutation

**Recovery View**: The CLI view explaining pending states, attempts, errors, next steps, and safe commands for resuming a saga or synchronization.
_Avoid_: raw journal dump, automatic repair

**Confidence**: The provenance level of a code reference: `authoritative` when declared by a runtime, `heuristic` when inferred, or `unresolved` when not yet resolved.
_Avoid_: accuracy score, trust score

**Session Artifact**: A structured Markdown or JSON document supplied by a runtime or user as raw material for a Session Diary.
_Avoid_: transcript, raw chat log
