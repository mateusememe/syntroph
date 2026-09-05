# Concrete GitHub remote storage providers

## Problem Statement

Syntroph can persist Session Diaries locally and model remote synchronization, but its GitHub providers are currently only contract seams backed by fakes or a null adapter. A developer who enables Wiki or Issues mirroring cannot authenticate through an explicit external provider, create a real Remote Mirror, inspect durable conflicts, or complete storage recovery against GitHub.

## Solution

Implement concrete, backend-specific GitHub storage providers behind StoragePort. GitHub Issues uses direct REST as the reference provider and an explicit stdio MCP client as an alternative. GitHub Wiki uses its separate Git repository. Every provider preserves the same domain behavior: the local Session Diary remains canonical; one immutable diary maps to one Remote Mirror by Idempotency Key; failures and prerequisites remain observable and explicitly recoverable; credentials stay outside Syntroph.

Before remote transports, extend the Saga Journal, Recovery View, Remote Binding projection, conflict evidence, and per-diary locking. Replace the JSON configuration prototype with validated `.syntroph/config.yaml` and add `syntroph doctor storage`. A configured provider is attempted only after the local diary is durable, while remote failure never fails session close.

## User Stories

1. As a developer, I want to select one storage provider explicitly, so that Syntroph never replays an operation through an unexpected fallback.
2. As a developer, I want invalid backend/provider combinations rejected before external effects, so that configuration mistakes cannot create remote artifacts.
3. As a developer, I want Issues mirroring through direct GitHub REST, so that the reference provider works without an MCP process.
4. As a developer, I want Issues mirroring through a configured MCP stdio command, so that an authenticated MCP server can own credentials.
5. As a developer, I want Wiki mirroring through Git, so that Syntroph uses GitHub's actual Wiki transport.
6. As a developer, I want one Remote Mirror per Session Diary, so that remote identity and recovery remain unambiguous.
7. As a developer, I want every mirror keyed by the diary Idempotency Key, so that technical retries do not create duplicates.
8. As a developer, I want the remote body to include a deterministic invisible marker, so that a lost Remote Binding can be reconstructed.
9. As a developer, I want Remote Bindings persisted atomically, so that a crash cannot leave a partially written projection.
10. As a developer, I want a Remote Binding to retain backend, provider, remote ID, URL, typed revision, and hashes, so that recovery has sufficient provenance.
11. As a developer, I want the Saga Journal to remain the auditable history behind a binding, so that projections can be reconstructed.
12. As a developer, I want session close to attempt the configured mirror after local persistence, so that successful remote synchronization needs no extra command.
13. As a developer, I want remote failure to preserve a successful local session, so that GitHub availability never loses knowledge.
14. As a developer, I want failed mirrors recorded as Storage Sync Pending, so that I can retry them explicitly.
15. As a developer, I want remote divergence recorded as Storage Sync Conflict, so that conflicts are not mislabeled as outages.
16. As a developer, I want missing external setup recorded as Storage Prerequisite Missing, so that non-transient failures have actionable guidance.
17. As a developer, I want `syntroph doctor storage` to validate provider prerequisites, so that I can prepare a cloned repository before closing a session.
18. As a developer, I want session close to perform the same non-mutating preflight, so that a missing provider is explained without blocking the local diary.
19. As a developer, I want Syntroph never to initiate login or persist tokens, so that credential ownership stays explicit.
20. As a REST user, I want only `SYNTROPH_GITHUB_TOKEN` accepted, so that unrelated CI or `gh` credentials are never consumed silently.
21. As a GitHub App operator, I want to inject an installation token through the same variable, so that Syntroph does not need App credential logic.
22. As an MCP user, I want the MCP process to own OAuth or PAT authentication, so that Syntroph only speaks the protocol.
23. As an MCP user, I want provider capabilities negotiated before any write, so that a read-only or incomplete server fails safely.
24. As an MCP user, I want the configured process reused within one command and shut down afterward, so that no hidden daemon or long-lived credential state remains.
25. As an Issues user, I want missing reserved labels created idempotently, so that all mirrors remain discoverable.
26. As an Issues user, I want label permission failures reported as missing prerequisites, so that Syntroph does not create unclassified issues.
27. As an Issues user, I want each diary represented by its own completed issue, so that memory does not pollute the open work backlog.
28. As an Issues user, I want issue titles to identify Syntroph, date, and session, so that mirrors are human-readable.
29. As an Issues user, I want the complete Session Diary in the issue body, so that the remote copy remains useful without local tooling.
30. As an Issues user, I want corrections appended as comments, so that conflict resolution never overwrites remote history.
31. As an Issues user, I want `--keep-remote` to explicitly accept an edited remote version, so that human changes are preserved deliberately.
32. As an Issues user, I want REST retries limited to documented transient responses, so that authorization and conflicts are not retried blindly.
33. As an Issues user, I want rate-limit and Retry-After guidance honored, so that Syntroph behaves as a responsible GitHub client.
34. As a Wiki user, I want each diary stored at a deterministic date/session path, so that page identity can be reconstructed without search.
35. As a Wiki user, I want Wiki commits to use an explicit or existing Git author, so that Syntroph never rewrites Git configuration.
36. As a Wiki user, I want authentication delegated to SSH or the Git credential helper, so that Syntroph never reads Git credentials.
37. As a Wiki user, I want fast-forward-only pushes, so that concurrent remote edits become visible conflicts.
38. As a Wiki user, I want an uninitialized Wiki reported as a missing prerequisite, so that Syntroph does not attempt administrative bootstrap.
39. As a developer, I want provider-specific typed Remote Revisions, so that each transport validates the version it actually observed.
40. As a developer, I want conflicting remote content retained privately with mode 0600, so that recovery can display an offline diff safely.
41. As a developer, I want bindings to store hashes rather than full remote content, so that operational metadata stays compact.
42. As a developer, I want a Mirror Lock per Idempotency Key, so that concurrent processes cannot duplicate remote effects.
43. As a developer, I want concurrent attempts to report already-in-progress without another pending event, so that recovery remains clear.
44. As a developer, I want orphaned locks cleared only through verified explicit recovery, so that a live process is never preempted automatically.
45. As a developer, I want `sync status` and `sync recovery` to show provider, remote identity, revision, error, conflict, and next action, so that failures are diagnosable.
46. As a developer, I want `sync retry --storage` to reuse the configured provider and binding, so that recovery performs the original operation idempotently.
47. As a developer, I want conflict resolution to show the persisted local/remote diff before applying a choice, so that resolution is informed.
48. As a developer, I want a missing Issue binding reconstructed by exact marker verification, so that recovery does not trust approximate search.
49. As a Wiki user, I want a missing binding reconstructed by deterministic path and frontmatter verification, so that recovery is reliable.
50. As a repository owner, I want the destination derived from normalized `origin`, so that copied configuration cannot publish to the wrong repository.
51. As a repository owner, I want cross-repository mirroring to require an explicit destination and opt-in, so that exceptional routing is visible.
52. As a contributor, I want all providers to pass one shared StoragePort contract suite, so that transport differences do not change domain semantics.
53. As a contributor, I want deterministic HTTP, Git-command, and MCP fakes in CI, so that tests require no credentials or network.
54. As a maintainer, I want an opt-in live GitHub smoke test, so that integration can be verified without making credentials mandatory in CI.

## Implementation Decisions

- Extend recovery and persistence first: Remote Binding projection, typed Remote Revision, Storage Sync Conflict, Storage Prerequisite Missing, private Conflict Snapshots, and Mirror Locks.
- Store current bindings under the local Syntroph state and write them atomically. Keep Saga Journal as the authoritative transition history.
- Treat Remote Revision as opaque in Core. Issues bodies, correction comments, and Wiki commits use provider-validated typed revisions.
- Replace the JSON prototype with `.syntroph/config.yaml` only; no compatibility path is required before the first stable release.
- Derive the destination from normalized `origin`. Cross-repository destinations require both an explicit repository and explicit opt-in.
- Permit only `github-rest` and `github-mcp` for Issues, and only `github-wiki-git` for Wiki.
- Select exactly one provider per installation. Never fall back automatically between transports.
- Run a non-mutating storage preflight from both `doctor storage` and session close.
- Make the direct REST provider the reference implementation. It accepts only `SYNTROPH_GITHUB_TOKEN` and uses the current GitHub API version headers.
- Serialize REST mutations and retry at most three times only for 429, explicit secondary limits, 502, 503, and 504, honoring Retry-After or bounded exponential backoff with jitter.
- Ensure the reserved labels exist before issue creation. Do not create a mirror when label management is unavailable.
- Create one issue per diary, include the complete diary and deterministic marker, apply both reserved labels, and immediately close it as completed.
- Never overwrite an Issue body during conflict resolution. A keep-local choice creates a versioned Remote Correction comment; keep-remote advances the binding to the accepted remote revision.
- Implement MCP over a configured stdio command first. Negotiate required issue read, create/update, comment, label, and search capabilities before writes. Defer HTTP MCP.
- Implement Wiki through the configured Git executable, using temporary clones/fetches, deterministic page paths, explicit/fallback author resolution, and fast-forward-only pushes.
- Never initialize a GitHub Wiki administratively. Report actionable setup when its repository does not yet exist.
- Acquire a per-key local lock before every external effect. Orphan cleanup is an explicit recovery operation that verifies process liveness.
- Persist divergent remote content separately with restrictive permissions and reference it from the journal for offline diffs.
- Reconstruct missing bindings only in recovery: paginate labeled closed Issues and verify markers; calculate Wiki paths and verify frontmatter.
- Implement in this order: recovery/bindings/locks; YAML and doctor; REST; Wiki Git; MCP stdio; shared contract suite and opt-in live smoke.

## Testing Decisions

- Use one primary high-level seam: close a session through the application service with a configured provider, then inspect the local diary, provider-visible Remote Mirror, Remote Binding, Saga Journal, and Recovery View. Exercise retry and resolution through the same public use-case surface.
- Run the same behavioral StoragePort contract suite against REST, Wiki Git, MCP, and deterministic fakes.
- Verify provider validation performs zero writes for invalid combinations, missing credentials, missing capabilities, missing labels permission, uninitialized Wiki, and unsafe cross-repository targets.
- Verify immediate mirror success, transient failure, retry exhaustion, idempotent replay, lost-binding reconstruction, and process restart.
- Verify REST request methods, headers, API version, token source, serialized mutation order, label lifecycle, issue closure, correction comments, pagination, rate-limit behavior, and error classification with a deterministic HTTP server.
- Verify Wiki clone/fetch, deterministic path, author selection, commit message, fast-forward push, non-fast-forward conflict, missing Wiki, missing credentials, and cleanup using isolated Git repositories and an injected command runner.
- Verify MCP initialize, capability discovery, tool mapping, write sequence, read-only rejection, malformed responses, process exit, cancellation, and graceful shutdown using a fake stdio server.
- Verify Remote Bindings are atomic and reconstructible, Conflict Snapshots use restrictive permissions, typed revisions reject the wrong provider format, and Mirror Locks prevent duplicate effects.
- Verify recovery output contains provider, remote identity, revision, error, diff source, and exact next action.
- Keep default CI completely offline. Gate live GitHub smoke tests behind explicit environment configuration and never run them for untrusted pull requests.
- Existing Session Diary, Event Bus, Saga Journal, GraphPort, and recovery tests are prior art and must remain green.

## Out of Scope

- HTTP/remote MCP transport in the first provider release.
- Automatic authentication, OAuth UI, `gh auth login`, credential-file discovery, or token persistence.
- Reading `GITHUB_TOKEN` implicitly.
- GitHub Wiki through REST or MCP.
- Automatic provider fallback.
- Automatic creation or administrative enabling of a GitHub Wiki.
- Silent Issue body overwrite or automatic conflict merge.
- Automatic clearing of orphaned locks.
- Cross-repository mirroring without explicit opt-in.
- Compiled Memory aggregation or multiple Session Diaries in one remote object.
- Dashboard implementation and remote-provider UI.
- Mandatory live GitHub tests in CI.

## Further Notes

- GitHub Issues and GitHub Wiki deliberately use different transports while preserving one domain contract.
- Direct REST behavior is authoritative when REST and MCP expose slightly different response shapes.
- The app must document post-clone prerequisites clearly so choosing a remote provider does not imply that Syntroph itself owns authentication.
- Public documentation and examples must use YAML and remove the existing JSON configuration prototype.
