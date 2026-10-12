# Local skill catalog and runtime-neutral bundles

## Problem Statement

Syntroph tracks a curated set of engineering skills, but those files are currently development tooling rather than a product capability. The Core cannot discover, validate, prepare, audit, or associate a skill with the Session Diary that benefited from it. Directly adopting one runtime's skill directory or asking SkillPort to execute Markdown would couple the architecture to unstable Claude, Codex, or Antigravity conventions. Automatically downloading or executing package content would also make an otherwise local operation non-reproducible and unsafe.

## Solution

Add a repository-scoped Skill Catalog behind SkillPort. An explicit synchronization command imports declared local Skill Sources, validates each Skill Package against a versioned lock and safety policy, and materializes immutable Skill Bundles in a content-addressed store. SkillPort prepares bundles but never interprets their instructions or writes into runtime-owned directories. CLI commands expose catalog state, integrity, provenance, and runtime-neutral JSON bundles.

Skill preparation and later runtime outcomes use immutable events and portable Skill Invocation Records. Session close validates those records, links journal evidence when available, resolves nested code references through GraphPort, and embeds the results in a dedicated Session Diary section without flattening their observations into top-level memory.

## User Stories

1. As a repository maintainer, I want a repository-scoped runtime skill lock, so that every contributor prepares the same package content.
2. As a repository maintainer, I want the runtime skill lock separated from Syntroph's development-tool lock, so that product state and contributor tooling do not acquire conflicting meanings.
3. As a repository maintainer, I want skill sources declared in the strict YAML configuration, so that the catalog never searches implicit directories.
4. As a repository maintainer, I want the repository's local skill source to be available without extra configuration, so that project-authored skills remain easy to version.
5. As a repository maintainer, I want source identifiers to be unique, so that a package identity cannot silently change origin.
6. As a repository maintainer, I want filesystem sources confined to the Repository Installation, so that synchronization cannot read arbitrary host paths.
7. As a repository maintainer, I want synchronization to consume only local sources, so that it never downloads content or requires credentials.
8. As a repository maintainer, I want synchronization to leave the lock unchanged, so that materialization cannot masquerade as a dependency update.
9. As a repository maintainer, I want an absent lock to produce a precise instruction, so that no catalog is invented implicitly.
10. As a repository maintainer, I want the initial Syntroph installation to include a curated runtime lock, so that supported engineering skills can be synchronized immediately.
11. As a developer, I want each external package's allowed files declared in the runtime lock, so that referenced or neighboring files are not trusted heuristically.
12. As a skill author, I want local packages to declare their files and metadata in a manifest, so that local and external packages normalize to the same contract.
13. As a developer, I want package name, description, license, source URL, source revision, arguments schema, runtime compatibility, and allowed files validated, so that prepared bundles carry complete provenance.
14. As a developer, I want package identity to include source, name, and package hash, so that identical names from different sources remain distinguishable.
15. As a developer, I want unqualified names accepted only when uniquely resolvable, so that a local package cannot silently override an external one.
16. As a repository maintainer, I want explicit aliases in configuration, so that short names remain stable and reviewable.
17. As a developer, I want package hashes computed from a canonical ordered file sequence and normalized manifest, so that synchronization is deterministic.
18. As a developer, I want file hashes computed from exact bytes without silent line-ending changes, so that source drift is observable.
19. As a developer, I want the store addressed by package hash, so that an existing immutable bundle is never overwritten.
20. As a developer, I want the active index replaced atomically only after complete validation, so that a crash cannot expose a partial catalog.
21. As a developer, I want read commands to use the last valid index during synchronization, so that inspection and preparation remain available.
22. As a developer, I want synchronization protected by one installation-scoped lock, so that concurrent writers cannot publish competing indexes.
23. As a developer, I want orphaned local synchronization state removable after verifying its process is dead, so that a local crash is recoverable without treating it as an external effect.
24. As a developer, I want old content-addressed bundles retained, so that synchronization never performs surprising deletion.
25. As a developer, I want valid packages classified as Ready, so that catalog state is explicit.
26. As a developer, I want invalid packages classified as UnsupportedSkillPackage without invalidating other packages, so that one unsafe package does not hide the usable catalog.
27. As a developer, I want an Unsupported Skill Package to retain precise validation diagnostics, so that list, show, and verify explain what must change.
28. As a security-conscious user, I want symlinks, non-regular files, path traversal, escaping paths, and executable content rejected, so that synchronization cannot smuggle host access or code execution into a bundle.
29. As a security-conscious user, I want only declared Markdown and static UTF-8 assets accepted, so that package contents remain inspectable data.
30. As a security-conscious user, I want undeclared files ignored and declared missing files rejected, so that the trusted package surface is explicit.
31. As a developer, I want per-file, manifest, depth, count, and bundle size limits, so that accidental or hostile packages cannot consume unbounded resources.
32. As a developer, I want license, source URL, and source revision to be mandatory declarations, so that attribution is never inferred or lost.
33. As a user, I want to list catalog entries and their state, source, identity, and short diagnostic, so that I can understand the installation without opening internal files.
34. As a user, I want to show one package's full metadata, provenance, allowed assets, hashes, compatibility, and validation result, so that I can audit it before preparation.
35. As a user, I want to verify one package or the entire catalog, so that integrity drift produces a non-zero command result suitable for CI.
36. As a user, I want synchronization to report Ready and unsupported packages while succeeding when the index is internally consistent, so that partial package rejection is observable rather than catastrophic.
37. As a runtime adapter, I want SkillPort to prepare a small immutable Skill Bundle, so that runtime-specific translation stays outside the Core.
38. As a runtime adapter, I want a prepared bundle to include canonical instructions, metadata, package identity, bundle hash, compatible runtimes, validated arguments, and safe relative asset references, so that I need no knowledge of source layouts.
39. As a runtime adapter, I want preparation to reject an incompatible runtime, unsupported package, ambiguous name, invalid alias, drifted store, or invalid arguments before returning instructions, so that failures happen before interpretation.
40. As a runtime adapter, I want SkillPort to avoid prompt interpolation, so that argument handling remains structured and auditable.
41. As a security-conscious user, I want skill arguments to reject secrets and remain fully journalable, so that tokens never enter bundles, events, or Session Diaries.
42. As a user, I want preparation to emit JSON to standard output by default, so that local tools and future runtime adapters can consume it without filesystem conventions.
43. As a user, I want preparation optionally written atomically to a caller-selected file with private permissions, so that I can hand off a bundle without exposing it to other local users.
44. As a user, I want asset references to remain inside the immutable store, so that preparation never writes into Claude, Codex, or Antigravity directories.
45. As a user, I want each intentional use to receive a new invocation identity, so that two uses of the same bundle are not mistaken for redelivery.
46. As a recovery mechanism, I want technical preparation replay to reuse an explicitly supplied invocation identity, so that the same bundle and event identities are returned idempotently.
47. As an operator, I want preparation requested before the local effect and followed by prepared or prepare-failed events, so that the Saga Journal explains every outcome.
48. As an operator, I want handler attempts separated from immutable events, so that repeated delivery never mutates event execution time.
49. As a future RuntimePort adapter, I want invocation-started, completed, and failed events defined by the Core, so that runtimes report outcomes consistently.
50. As a user, I want a failed skill invocation recorded as historical evidence rather than a pending synchronization, so that failure does not trigger an automatic retry.
51. As a user, I want an intentional retry to create a new invocation related to the failed one, so that audit history distinguishes attempts from redelivery.
52. As a runtime, I want to provide a portable Skill Invocation Record in a Session Artifact, so that session memory does not depend on access to one local journal.
53. As a user, I want an invocation record marked journal-verified when its immutable fields match local evidence, so that its provenance is explicit.
54. As a user, I want a record without matching local evidence accepted as runtime-declared, so that external runtimes can still contribute knowledge.
55. As a user, I want a record that contradicts available journal evidence rejected, so that divergent provenance is not silently accepted.
56. As a user, I want skill results to retain status, summary, observations, code references, related invocation, and artifact metadata, so that the diary captures useful structured evidence without a transcript.
57. As a user, I want skill observations nested under their invocation, so that decisions and lessons are not silently promoted into top-level memory.
58. As a user, I want skill-generated code references resolved in the same GraphPort batch as top-level references and written back to their originating invocation, so that code context retains provenance.
59. As a user, I want GraphPort failure to preserve nested references and mark Graph Resolution Pending, so that enrichment never blocks the local diary.
60. As a user, I want artifact paths restricted to repository-relative locations, so that session close cannot read arbitrary host files.
61. As a user, I want existing artifacts verified by hash, media type, and size, so that the diary records what was actually observed.
62. As a user, I want missing artifacts retained with missing availability, so that a vanished output does not prevent local knowledge capture.
63. As a user, I want artifact contents excluded from the Session Diary, so that memory records evidence without duplicating potentially large or sensitive files.
64. As a user, I want JSON Session Artifacts to carry skill_invocations directly, so that structured runtimes have a natural interchange format.
65. As a user, I want Markdown Session Artifacts to carry one strict JSON block under Skill Invocations, so that nested records remain unambiguous and human-visible.
66. As a user, I want malformed or duplicate Skill Invocations blocks rejected explicitly, so that parsing never guesses at execution evidence.
67. As a user, I want session close to emit an immutable link from the diary saga to each accepted invocation, so that independently created sagas become traceable without rewriting history.
68. As a dashboard reader, I want skill event payloads to contain descriptors, hashes, state, and provenance rather than full instructions, so that operational views remain useful without copying prompts into the journal.
69. As a maintainer, I want schema versions on locks, manifests, bundles, invocation records, and skill events, so that the current and immediately previous versions can be migrated at read time.
70. As a maintainer, I want the selected engineering skill containing a shell template to remain available for repository development but unsupported by the runtime catalog, so that current workflows remain intact without weakening the product policy.

## Implementation Decisions

- SkillPort is a deep Core seam with operations to inspect the catalog, verify integrity, synchronize local declarations, and prepare one immutable bundle. It does not execute prompts.
- RuntimePort owns interpretation of Skill Bundles. This slice supplies the minimal contract and a deterministic fake only; concrete runtime adapters are deferred.
- The normalized Skill Package manifest includes schema version, name, description, license, source identity, source URL, source revision, allowed files, arguments schema, and optional compatible runtimes.
- External filesystem packages declare allowed files in the repository-scoped runtime lock. Repository-authored packages declare them in their local manifest. The adapter converts both into the same normalized manifest.
- The repository-scoped runtime lock is distinct from the root development-tool lock and is never mutated by synchronization.
- Configuration is strict YAML. It declares whether skills are enabled, the runtime lock, local filesystem sources, and explicit aliases. The repository-local source is implicit.
- Filesystem roots must resolve inside the Repository Installation. Absolute and escaping source paths are unsupported.
- The active catalog distinguishes Ready and UnsupportedSkillPackage entries. Unsupported entries retain safe diagnostics but have no preparable bundle.
- The store is content-addressed by SHA-256 and immutable. The active index, staging directories, and synchronization lock are operational projections; the lock and repository-authored packages are versioned inputs.
- Synchronization uses an exclusive local lock, builds a complete staging index, and replaces the active index atomically. Readers continue using the last valid index. Orphaned local state may be removed after process-liveness verification because no external effect exists.
- Synchronization never downloads content, invokes an installer, updates the runtime lock, deletes old bundles, or writes to runtime-owned directories.
- Package hashing uses the normalized manifest and an ordered sequence of POSIX relative path, exact byte length, and exact file bytes. Line endings are not normalized.
- Accepted content is limited to regular UTF-8 Markdown and explicitly declared static assets. Executable content, symlinks, non-regular files, traversal, escaping paths, undeclared dependencies, and missing declared files are unsupported.
- Limits are 256 KiB for primary instructions, 1 MiB per asset, 5 MiB per bundle, 128 files, 256 KiB for manifests and locks, and eight directory levels.
- The arguments schema supports object, string, boolean, integer, arrays of those types, required fields, enum constraints, and size limits. An absent schema permits only an empty object. Secrets are not a supported argument type.
- Canonical package identity is source ID, package name, and package hash. Unqualified names resolve only when unique; aliases are explicit and never change precedence silently.
- Preparation produces an immutable JSON Skill Bundle on standard output or atomically writes it to a requested private file. Runtime selection validates compatibility and contributes hints without modifying canonical instructions.
- Each intentional invocation receives a new invocation ID. An explicitly repeated invocation ID is technical replay and must reproduce the same bundle identity and phase event IDs.
- A Skill Invocation owns its saga. Correlation uses a supplied runtime session identity or the invocation identity when no session is known. Session close later emits links without modifying prior events.
- Skill events are prepare requested, prepared, prepare failed, invocation started, completed, failed, and linked. Handler execution data remains in Handler Attempts. Journal payloads exclude full instruction content.
- A Skill Invocation Record contains its schema version, invocation and related-invocation identities, package identity and hash, runtime, auditable arguments, outcome, summary, nested observations, nested code references, artifact metadata, event references, and provenance.
- Session Artifacts carry complete portable invocation records. Matching journal evidence promotes provenance to journal-verified; absent evidence remains runtime-declared; contradictory evidence fails validation.
- JSON artifacts use a skill_invocations field. Markdown artifacts use exactly one strict JSON array block under the Skill Invocations heading.
- Session Diaries preserve invocation records in a dedicated section. They never silently flatten skill observations into top-level decisions or lessons.
- GraphPort resolves top-level and nested code references in one deduplicated batch and maps results back to the original locations. Failure preserves every reference and produces Graph Resolution Pending.
- Runtime artifact evidence is repository-relative metadata: path, SHA-256, media type, size, and verified or missing availability. Session close does not copy artifact contents.
- Failed runtime invocations are historical records, not pending obligations. An intentional retry creates a related new invocation.
- Locks, manifests, bundles, invocation records, and skill events use integer schema versions. Readers support the current and immediately previous version through read-time migration without rewriting immutable sources.
- The initial curated runtime lock includes only packages that satisfy the static-content policy. Development tooling remains independently available even when a package is unsupported by SkillPort.

## Testing Decisions

- Test observable behavior at the highest useful seam by invoking the CLI against temporary Repository Installations and inspecting command output, file permissions, catalog state, journal records, and Session Diaries.
- Share a contract suite between the Core SkillPort fake and the filesystem adapter. The contract covers deterministic identities, partial package rejection, unique and qualified resolution, aliases, integrity verification, preparation replay, runtime compatibility, and argument validation.
- Share a minimal RuntimePort contract suite with a deterministic fake. Concrete Codex, Claude Code, and Antigravity adapters will reuse it in later slices.
- Exercise synchronization interruption at each staging boundary and prove that readers observe either the old complete index or the new complete index, never a partial one.
- Exercise concurrent synchronization, live and orphaned lock ownership, immutable store reuse, and retention of unreferenced bundles.
- Use adversarial fixtures for symlinks, traversal, absolute paths, executable modes and extensions, non-regular files, invalid UTF-8, missing and undeclared files, unsupported schemas, hash drift, collisions, excessive depth, file counts, and byte limits.
- Invoke list, show, verify, sync, and prepare through the CLI and assert stable exit semantics, bounded diagnostics, JSON output, private output permissions, and zero network or installer calls.
- Test immutable skill event envelopes, Handler Attempts, causal identifiers, redelivery with the same invocation ID, and the absence of full instruction Markdown in journal records.
- Parse both JSON and Markdown Session Artifacts with valid, missing, malformed, duplicate, journal-verified, runtime-declared, and contradictory invocation records.
- Resolve mixed top-level and nested code references through one GraphPort fake, assert deduplication, re-association, confidence, snapshot identity, and Graph Resolution Pending behavior.
- Verify artifact paths, hashes, sizes, media types, missing availability, host-path rejection, and the absence of copied artifact bodies in memory.
- Keep all default tests and CI offline with temporary files, deterministic clocks and IDs, fake ports, and no runtime credentials.
- Require the complete Go test suite, race detector, vet, formatting, diff validation, and existing StoragePort/GraphPort contracts to remain green.

## Out of Scope

- Prompt interpretation or execution by SkillPort.
- Concrete Codex, Claude Code, or Antigravity CLI RuntimePort adapters.
- Executing scripts, binaries, hooks, or package installers.
- SandboxPort integration or consent policy for executable skill assets.
- Direct downloads, GitHub clients, registry clients, mutable version resolution, or skill add, update, remove, and garbage-collection commands.
- Automatic writes to runtime-owned directories such as Claude, Codex, or Antigravity configuration folders.
- Secret-valued skill arguments or prompt interpolation.
- Compiled Memory aggregation or automatic promotion of skill observations.
- Dashboard implementation.
- Automatic retry of failed runtime invocations.
- Global catalogs, absolute filesystem sources, or cross-repository source roots.
- Network access, authentication, or credential persistence.

## Further Notes

- The current selected development skills include one shell template under the diagnosing-bugs package. That package must remain usable by repository contributors but appear as UnsupportedSkillPackage in the runtime catalog until executable assets are governed by SandboxPort.
- The first concrete RuntimePort slice after this specification will implement Codex, followed by Claude Code and Antigravity CLI against the same contract suite.
- The existing Session Diary remains the immutable local source of memory. Skill Invocation Records enrich it without changing StoragePort mirroring semantics.
