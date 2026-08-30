# MVP session close, memory, and graph synchronization

## Problem Statement

Developers and AI runtimes produce useful session decisions, lessons, and code context, but that knowledge is difficult to preserve, relate, and reuse across tools. Syntroph needs a repository-scoped flow that records session evolution durably, enriches it with code-graph references, and makes incomplete remote synchronization recoverable without losing history.

## Solution

Implement the first Syntroph vertical slice in the Go hexagonal Core. A user or runtime submits a structured Markdown or JSON Session Artifact, or a manual summary fallback. On session close, Syntroph resolves explicit code references through GraphPort when available, writes an immutable Session Diary to local canonical storage, journals the saga and handler attempts, updates a content-addressed graph snapshot, and mirrors the diary to the configured GitHub Wiki or Issues adapter. Graph and storage failures remain visible pending obligations that can be inspected and retried explicitly.

## User Stories

1. As a developer, I want to close a coding session with a structured Markdown artifact, so that my decisions and lessons become durable knowledge.
2. As a developer, I want to close a coding session with a structured JSON artifact, so that any supported runtime can feed Syntroph.
3. As a developer, I want to provide a manual summary when no structured artifact exists, so that I can still record a session.
4. As a developer, I want each closed session to create an immutable Session Diary, so that its history cannot be silently rewritten.
5. As a developer, I want each diary to retain session, repository, commit, artifact, author, runtime, and creation identity, so that I can trace its provenance.
6. As a developer, I want diaries to link to related memories, so that knowledge can be navigated across sessions.
7. As a developer, I want duplicate submission of the same repository, commit, and artifact to be idempotent, so that retries do not create duplicate diaries.
8. As a developer, I want a different artifact at the same commit to remain distinct, so that separate sessions are not conflated.
9. As a developer, I want explicit code references to include paths, optional symbols, kinds, commit, and graph snapshot identity, so that references remain historically interpretable.
10. As a developer, I want GraphPort to resolve references before diary persistence, so that the diary includes useful code context when the graph is available.
11. As a developer, I want a missing or stale GraphPort to produce Graph Resolution Pending, so that graph availability never blocks knowledge capture.
12. As a developer, I want heuristic references marked separately from authoritative references, so that guessed context is never presented as fact.
13. As a developer, I want graph snapshots addressed by content, so that a diary can point to the exact snapshot used for resolution.
14. As a developer, I want the active graph snapshot tracked separately from historical snapshots, so that remote branch rewrites do not invalidate diary provenance.
15. As a developer, I want the local diary to be canonical, so that missing credentials or network access cannot lose my work.
16. As a developer, I want GitHub Wiki and Issues to be selectable mirrors, so that storage can match repository needs without changing Core behavior.
17. As a developer, I want mirror failures recorded as Storage Sync Pending, so that I can see what has not reached GitHub.
18. As a developer, I want unexpected remote revisions to produce Storage Sync Conflict, so that human edits are not silently overwritten.
19. As a developer, I want conflict resolution to require an explicit keep-local or keep-remote choice, so that synchronization is deliberate.
20. As a developer, I want a diff before resolving a storage conflict, so that I understand the effect of my choice.
21. As a developer, I want GitHub credentials supplied by an external environment-token or local MCP provider, so that Syntroph never stores or prompts for secrets.
22. As a developer, I want every event to carry stable identity and causal metadata, so that I can trace a session end to every downstream effect.
23. As a developer, I want event envelopes versioned per event type, so that readers can evolve without rewriting immutable history.
24. As a developer, I want handler attempts recorded separately from immutable events, so that retries have complete timing, outcome, and error history.
25. As a developer, I want independent handlers isolated from one another, so that one failed adapter does not block unrelated consumers.
26. As a developer, I want causal saga transitions ordered, so that session close remains deterministic.
27. As a developer, I want the Saga Journal append-only and local, so that state survives process crashes without requiring a broker.
28. As a developer, I want crash recovery to be explanatory rather than automatic, so that external effects are never repeated without my confirmation.
29. As a developer, I want `sync status` to summarize pending obligations, so that I know whether a session is fully mirrored.
30. As a developer, I want `sync recovery` to explain attempts, errors, and next safe actions, so that I can diagnose incomplete work.
31. As a developer, I want `sync retry` scoped to storage or graph, so that I can retry only the failed obligation.
32. As a developer, I want a successful local session to remain successful even with remote obligations pending, so that availability is not confused with eventual synchronization.
33. As a developer, I want one Syntroph installation to own one Git repository, so that identity and synchronization boundaries remain unambiguous.
34. As a contributor, I want port contracts independent from adapters, so that fake adapters can test Core behavior deterministically.
35. As a contributor, I want contract tests for MemoryPort, GraphPort, and StoragePort, so that adapter replacements preserve behavior.
36. As an operator, I want the dashboard to consume the event projection without writing adapters directly, so that read concerns do not bypass the Core.

## Implementation Decisions

- Build the vertical slice in the Go hexagonal Core around MemoryPort, GraphPort, and StoragePort.
- Use a synchronous in-process Event Bus for delivery and an append-only Saga Journal for durable transitions.
- Use causal ordering for Core saga transitions; independent handlers may run in parallel after predecessor journalization.
- Declare at-least-once delivery for external operations; handlers and adapters are idempotent by event ID.
- Require event envelope fields: event ID, type, occurred time, repository ID, saga ID, correlation ID, causation ID, schema version, and payload. Handler attempts carry attempted time, handler ID, outcome, and error separately.
- Support the current and immediately previous schema version through read-time migration without rewriting events or diaries.
- Accept structured Markdown or JSON Session Artifacts and a manual summary fallback; exclude raw transcript capture from this slice.
- Derive idempotency from repository ID, commit SHA, and artifact hash.
- Keep an immutable local Session Diary as canonical under the repository's Syntroph state; Compiled Memory remains a future derived projection.
- Resolve explicit code references before persistence where GraphPort is available; retain unresolved references and pending state otherwise.
- Represent code-reference confidence as authoritative, heuristic, or unresolved.
- Store content-addressed graph snapshots and an active snapshot pointer; preserve the referenced snapshot identity in diaries.
- Implement GitHub Wiki and Issues as real StoragePort mirrors with an external env-token or MCP credential provider.
- Record remote divergence as Storage Sync Conflict and require CLI diff plus explicit resolution.
- Expose status, recovery, scoped retry, and explicit conflict-resolution CLI operations.
- Keep one repository per installation in the MVP; defer cross-repository aggregation to a dashboard projection.
- Preserve the existing Adapter Registry, Saga, Strategy, Repository, and CQRS boundaries described by the accepted ADRs.

## Testing Decisions

- Test observable behavior at the highest seam: invoke the session-close use case with fake MemoryPort, GraphPort, StoragePort, and Event Bus collaborators, then inspect diaries, journal entries, emitted events, and returned recovery states.
- Use contract tests shared by local fakes and real adapter implementations for MemoryPort, GraphPort, and StoragePort.
- Verify duplicate artifacts are idempotent, distinct artifacts at one commit remain distinct, and event redelivery does not duplicate external effects.
- Verify causal ordering, isolated handler failure, durable journal recovery, and explicit post-crash retry behavior.
- Verify graph-unavailable, graph-stale, remote-unavailable, and remote-conflict paths preserve the local diary and expose the correct pending state.
- Verify schema migration from the immediately previous version without rewriting source records.
- Verify confidence and graph snapshot provenance in persisted references.
- Verify CLI recovery output and diff-before-resolution behavior as user-facing contract tests.
- No implementation-specific assertions about concrete adapter internals; no real credentials or live remote state in deterministic tests.
- There is no existing production code or test prior art in this repository yet; the initial suite establishes these port-contract and vertical-slice seams.

## Out of Scope

- Raw transcript capture or automatic transcript redaction.
- A general-purpose external message broker.
- SQLite journal storage.
- Automatic retry or external effects immediately after a crash.
- Automatic conflict merging or silent remote overwrite.
- Cross-repository installations and aggregation.
- Compiled Memory as a mutable source record.
- SandboxPort, CompressionPort, dashboard implementation, release automation, and full runtime adapters beyond the contract boundary.
- A custom GitHub login flow or credential storage.
- Heuristic code-reference extraction as authoritative data.

## Further Notes

- The repository currently contains design documents and no Go implementation or `go.mod`; implementation work must establish the module and command surface before source installation commands become executable.
- The selected engineering skills from `mattpocock/skills` are tracked in the repository lockfile, but that repository is methodology/content, not a Syntroph runtime dependency.
- The canonical project vocabulary is maintained in `CONTEXT.md`; accepted architectural rationale is maintained in `docs/adr/`.
