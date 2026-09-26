package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/adapters/skillfilesystem"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
)

// preparedBundleView mirrors the JSON shape `syntroph skill prepare` writes
// for an immutable Skill Bundle (core.SkillBundle.MarshalJSON). It is
// re-declared here, rather than imported, because SkillBundle keeps its
// fields private everywhere except that one JSON encoding.
type preparedBundleView struct {
	SchemaVersion      int                       `json:"schema_version"`
	InvocationID       string                    `json:"invocation_id"`
	PackageIdentity    core.SkillPackageIdentity `json:"package_identity"`
	Description        string                    `json:"description,omitempty"`
	Instructions       string                    `json:"instructions"`
	Assets             []core.SkillAsset         `json:"assets,omitempty"`
	CompatibleRuntimes []string                  `json:"compatible_runtimes,omitempty"`
	Runtime            string                    `json:"runtime,omitempty"`
	Arguments          map[string]any            `json:"arguments,omitempty"`
	BundleHash         string                    `json:"bundle_hash"`
}

// runCLI drives the real `syntroph` command dispatch (the same entry point
// main() uses) so this acceptance journey exercises production code paths,
// not a parallel test-only shortcut.
func runCLI(t *testing.T, args ...string) (stdout string, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = run(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

// acceptanceRepoRoot locates the repository root from cmd/syntroph, where
// `go test` sets the working directory.
func acceptanceRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".syntroph", "skills.runtime.lock.yaml")); err != nil {
		t.Fatalf("could not locate the curated runtime lock relative to %s: %v", root, err)
	}
	return root
}

// newCuratedSkillFixture copies the repository's real curated runtime lock,
// its config, and the .agents/skills packages it declares into an isolated
// temporary repository. The acceptance journey runs entirely inside that
// copy so it never mutates the real repository's working tree and stays
// safe to run concurrently with other tests.
func newCuratedSkillFixture(t *testing.T) string {
	t.Helper()
	repoRoot := acceptanceRepoRoot(t)
	fixtureRoot := t.TempDir()

	copyTree(t, filepath.Join(repoRoot, ".agents", "skills"), filepath.Join(fixtureRoot, ".agents", "skills"))
	copyFile(t,
		filepath.Join(repoRoot, ".syntroph", "config.yaml"),
		filepath.Join(fixtureRoot, ".syntroph", "config.yaml"))
	copyFile(t,
		filepath.Join(repoRoot, ".syntroph", "skills.runtime.lock.yaml"),
		filepath.Join(fixtureRoot, ".syntroph", "skills.runtime.lock.yaml"))
	return fixtureRoot
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSkillCatalogOfflineAcceptanceJourney is SkillPort's one executable,
// offline, end-to-end acceptance journey (issue #28). It runs the real CLI
// against the repository's own curated runtime lock and config, entirely
// from local disk, and demonstrates: catalog synchronization producing the
// expected Ready/UnsupportedSkillPackage split; a Ready package's bundle
// preparation; technical replay by explicit invocation ID; rejection of a
// same-ID, changed-input replay; an intentional new use that reuses bundle
// content under a fresh invocation ID; Session Diary provenance reconciled
// against Saga Journal evidence (journal-verified); nested code reference
// resolution through a deterministic, locally published GraphPort snapshot;
// and repository-relative artifact evidence verified against the
// filesystem without ever copying its contents into the diary.
func TestSkillCatalogOfflineAcceptanceJourney(t *testing.T) {
	fixtureRoot := newCuratedSkillFixture(t)

	// 1. Sync the curated catalog from local disk only.
	syncOut, _, err := runCLI(t, "skill", "sync", "--root", fixtureRoot)
	if err != nil {
		t.Fatalf("skill sync: %v", err)
	}
	var syncResult skillfilesystem.SyncResult
	if err := json.Unmarshal([]byte(syncOut), &syncResult); err != nil {
		t.Fatalf("decode sync result: %v\n%s", err, syncOut)
	}
	if syncResult.State != skillfilesystem.SyncSucceeded {
		t.Fatalf("sync state = %q, want %q", syncResult.State, skillfilesystem.SyncSucceeded)
	}
	ready, unsupported := 0, 0
	diagnosingBugsDiagnostic := ""
	for _, entry := range syncResult.Entries {
		switch entry.State {
		case core.SkillReady:
			ready++
		case core.UnsupportedSkillPackage:
			unsupported++
			if entry.Identity.Name == "diagnosing-bugs" {
				diagnosingBugsDiagnostic = entry.Diagnostic
			}
		default:
			t.Fatalf("unexpected catalog state %q for %s", entry.State, entry.Identity.QualifiedName())
		}
	}
	if ready != 16 {
		t.Fatalf("ready packages = %d, want 16", ready)
	}
	if unsupported != 1 {
		t.Fatalf("unsupported packages = %d, want 1", unsupported)
	}
	if diagnosingBugsDiagnostic == "" {
		t.Fatal("diagnosing-bugs did not sync as an UnsupportedSkillPackage with an explanatory diagnostic")
	}

	// 2. list mirrors the same classification.
	listOut, _, err := runCLI(t, "skill", "list", "--root", fixtureRoot)
	if err != nil {
		t.Fatalf("skill list: %v", err)
	}
	var listEntries []skillfilesystem.EntrySummary
	if err := json.Unmarshal([]byte(listOut), &listEntries); err != nil {
		t.Fatalf("decode list: %v\n%s", err, listOut)
	}
	if len(listEntries) != 17 {
		t.Fatalf("len(listEntries) = %d, want 17 (16 ready + diagnosing-bugs)", len(listEntries))
	}

	// 3. show a Ready package: full metadata, including its arguments schema.
	showOut, _, err := runCLI(t, "skill", "show", "mattpocock/code-review", "--root", fixtureRoot)
	if err != nil {
		t.Fatalf("skill show mattpocock/code-review: %v", err)
	}
	var codeReviewShow skillShowView
	if err := json.Unmarshal([]byte(showOut), &codeReviewShow); err != nil {
		t.Fatalf("decode show: %v\n%s", err, showOut)
	}
	if codeReviewShow.State != core.SkillReady {
		t.Fatalf("code-review state = %q, want %q", codeReviewShow.State, core.SkillReady)
	}
	if codeReviewShow.Package.Identity.PackageHash == "" {
		t.Fatal("code-review package hash is empty")
	}
	if codeReviewShow.Package.ArgumentsSchema == nil {
		t.Fatal("code-review did not declare an arguments schema")
	}

	// 4. show the intentionally unsupported package: it remains visible in
	// the catalog, correctly classified, never silently dropped.
	diagOut, _, err := runCLI(t, "skill", "show", "mattpocock/diagnosing-bugs", "--root", fixtureRoot)
	if err != nil {
		t.Fatalf("skill show mattpocock/diagnosing-bugs: %v", err)
	}
	var diagnosingBugsShow skillShowView
	if err := json.Unmarshal([]byte(diagOut), &diagnosingBugsShow); err != nil {
		t.Fatalf("decode show diagnosing-bugs: %v\n%s", err, diagOut)
	}
	if diagnosingBugsShow.State != core.UnsupportedSkillPackage {
		t.Fatalf("diagnosing-bugs state = %q, want %q", diagnosingBugsShow.State, core.UnsupportedSkillPackage)
	}
	if diagnosingBugsShow.Diagnostic == "" {
		t.Fatal("diagnosing-bugs show did not explain why it is unsupported")
	}

	// 5. verify: the unsupported package fails verification; every Ready
	// package passes.
	verifyOut, _, verifyErr := runCLI(t, "skill", "verify", "--root", fixtureRoot)
	if verifyErr == nil {
		t.Fatal("skill verify succeeded despite an unsupported package remaining in the catalog")
	}
	var verifyEntries []skillfilesystem.EntrySummary
	if err := json.Unmarshal([]byte(verifyOut), &verifyEntries); err != nil {
		t.Fatalf("decode verify: %v\n%s", err, verifyOut)
	}
	failedVerification := 0
	for _, entry := range verifyEntries {
		if entry.State != core.SkillReady {
			failedVerification++
		}
	}
	if failedVerification != 1 {
		t.Fatalf("verify failures = %d, want 1 (diagnosing-bugs only)", failedVerification)
	}

	// 6. prepare a Ready package's bundle under an explicit invocation ID.
	invocationID := "inv-acceptance-journey-1"
	prepareArgs := []string{
		"skill", "prepare", "mattpocock/code-review",
		"--root", fixtureRoot,
		"--invocation-id", invocationID,
		"--arguments", `{"fixed_point":"origin/main"}`,
		"--repository", "acceptance-repo",
		"--session", "acceptance-session",
	}
	firstOut, _, err := runCLI(t, prepareArgs...)
	if err != nil {
		t.Fatalf("skill prepare: %v", err)
	}
	var firstBundle preparedBundleView
	if err := json.Unmarshal([]byte(firstOut), &firstBundle); err != nil {
		t.Fatalf("decode prepared bundle: %v\n%s", err, firstOut)
	}
	if firstBundle.InvocationID != invocationID {
		t.Fatalf("invocation id = %q, want %q", firstBundle.InvocationID, invocationID)
	}
	if firstBundle.BundleHash == "" {
		t.Fatal("prepared bundle hash is empty")
	}

	// 7. replay by the same explicit invocation ID with identical inputs:
	// the same bundle, the same invocation identity.
	replayOut, _, err := runCLI(t, prepareArgs...)
	if err != nil {
		t.Fatalf("skill prepare (technical replay): %v", err)
	}
	var replayedBundle preparedBundleView
	if err := json.Unmarshal([]byte(replayOut), &replayedBundle); err != nil {
		t.Fatalf("decode replayed bundle: %v\n%s", err, replayOut)
	}
	if replayedBundle.InvocationID != firstBundle.InvocationID || replayedBundle.BundleHash != firstBundle.BundleHash {
		t.Fatalf("technical replay changed bundle identity: first=%+v replay=%+v", firstBundle, replayedBundle)
	}

	// 8. the same invocation ID with changed inputs is rejected outright,
	// never silently retried or reconciled against the first use.
	//
	// Replay-contradiction detection lives in the SkillPort's in-memory
	// invocation ledger (core.InMemorySkillPort), scoped to one Port
	// instance's lifetime -- the same scope the shared
	// core/skillcontracttest.RunSkillPort contract suite exercises. Each
	// separate `syntroph skill prepare` process constructs its own Catalog,
	// so this step drives the same production adapter (skillfilesystem.New,
	// still reading only from fixtureRoot's synced catalog on disk) through
	// two Prepare calls on one Catalog value, rather than two separate CLI
	// invocations, to observe that ledger within its real lifetime.
	cfg, err := config.Load(filepath.Join(fixtureRoot, ".syntroph", "config.yaml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	resolvedSkills, err := cfg.ResolveSkills(fixtureRoot)
	if err != nil {
		t.Fatalf("ResolveSkills: %v", err)
	}
	catalog, err := skillfilesystem.New(fixtureRoot, resolvedSkills)
	if err != nil {
		t.Fatalf("skillfilesystem.New: %v", err)
	}
	ctx := context.Background()
	if _, err := catalog.Prepare(ctx, core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: invocationID,
		Arguments:    map[string]any{"fixed_point": "origin/main"},
	}); err != nil {
		t.Fatalf("re-prepare with unchanged inputs on a live catalog: %v", err)
	}
	_, changedErr := catalog.Prepare(ctx, core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: invocationID,
		Arguments:    map[string]any{"fixed_point": "HEAD~1"},
	})
	if !errors.Is(changedErr, core.ErrSkillInvocationReplay) {
		t.Fatalf("changed-input replay error = %v, want ErrSkillInvocationReplay", changedErr)
	}

	// 9. an intentional new use (empty invocation ID) gets a fresh
	// invocation ID but an identical bundle hash, because the content is
	// unchanged.
	intentionalArgs := []string{
		"skill", "prepare", "mattpocock/code-review",
		"--root", fixtureRoot,
		"--arguments", `{"fixed_point":"origin/main"}`,
		"--repository", "acceptance-repo",
		"--session", "acceptance-session",
	}
	intentionalOut, _, err := runCLI(t, intentionalArgs...)
	if err != nil {
		t.Fatalf("skill prepare (intentional new use): %v", err)
	}
	var intentionalBundle preparedBundleView
	if err := json.Unmarshal([]byte(intentionalOut), &intentionalBundle); err != nil {
		t.Fatalf("decode intentional-use bundle: %v\n%s", err, intentionalOut)
	}
	if intentionalBundle.InvocationID == firstBundle.InvocationID {
		t.Fatal("intentional new use reused the earlier explicit invocation id")
	}
	if intentionalBundle.BundleHash != firstBundle.BundleHash {
		t.Fatalf("identical content changed bundle hash: first=%q intentional=%q", firstBundle.BundleHash, intentionalBundle.BundleHash)
	}

	// 10. publish a deterministic, offline graph snapshot so nested code
	// reference resolution in session close has real evidence to match
	// against. This is the repository's own LocalGraphPort adapter: no
	// network access and no external graph service are involved.
	repositoryID, commitSHA := "acceptance-repo", "deadbeef00000000000000000000000000000001"
	graphPort, err := core.NewLocalGraphPort(filepath.Join(fixtureRoot, ".syntroph", "graph"))
	if err != nil {
		t.Fatalf("NewLocalGraphPort: %v", err)
	}
	topLevelRef := core.CodeReference{Path: ".agents/skills/code-review/SKILL.md", Symbol: "code-review"}
	nestedRef := core.CodeReference{Path: ".agents/skills/code-review/agents/openai.yaml", Symbol: "code-review-agent"}
	snapshot, err := graphPort.Publish(context.Background(), core.GraphSnapshot{
		RepositoryID: repositoryID,
		CommitSHA:    commitSHA,
		References:   []core.CodeReference{topLevelRef, nestedRef},
	})
	if err != nil {
		t.Fatalf("Publish graph snapshot: %v", err)
	}

	// 11. produce a real, repository-relative artifact file the invocation
	// is declared to have produced, so session close can verify it exists
	// on disk without ever copying its bytes into the diary.
	reportContent := []byte("# Code Review\n\nDiff since origin/main looks good; ship it.\n")
	reportPath := filepath.Join(fixtureRoot, "reports", "code-review.md")
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, reportContent, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(reportContent)
	wantSHA256 := hex.EncodeToString(wantSum[:])

	// 12. build the Session Artifact declaring the journaled invocation,
	// its nested code reference, and the artifact it produced.
	artifact := map[string]any{
		"title":           "Acceptance journey",
		"summary":         "Exercised the curated Skill Catalog end to end, offline.",
		"decisions":       []string{"Ship the reviewed diff."},
		"lessons":         []string{"The offline acceptance journey covers sync through session close."},
		"code_references": []core.CodeReference{topLevelRef},
		"skill_invocations": []map[string]any{
			{
				"invocation_id": invocationID,
				"package_identity": map[string]any{
					"source_id":    firstBundle.PackageIdentity.SourceID,
					"name":         firstBundle.PackageIdentity.Name,
					"package_hash": firstBundle.PackageIdentity.PackageHash,
				},
				"bundle_hash":     firstBundle.BundleHash,
				"runtime":         firstBundle.Runtime,
				"arguments":       firstBundle.Arguments,
				"outcome":         "succeeded",
				"code_references": []core.CodeReference{nestedRef},
				"artifacts": []map[string]any{
					{"path": "reports/code-review.md"},
				},
			},
		},
	}
	artifactBytes, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(fixtureRoot, "session-artifact.json")
	if err := os.WriteFile(artifactPath, artifactBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// 13. close the session through the same CLI surface a runtime would
	// use, journal-verifying the invocation and resolving both the
	// top-level and nested code references in one graph batch.
	closeOut, _, err := runCLI(t, "session", "close",
		"--root", filepath.Join(fixtureRoot, ".syntroph"),
		"--repository", repositoryID,
		"--commit", commitSHA,
		"--author", "acceptance-suite",
		"--artifact", artifactPath,
		"--format", "json",
	)
	if err != nil {
		t.Fatalf("session close: %v", err)
	}
	var closed struct {
		Diary core.SessionDiary `json:"diary"`
	}
	if err := json.Unmarshal([]byte(closeOut), &closed); err != nil {
		t.Fatalf("decode session close output: %v\n%s", err, closeOut)
	}
	diary := closed.Diary

	if len(diary.SkillInvocations) != 1 {
		t.Fatalf("len(diary.SkillInvocations) = %d, want 1", len(diary.SkillInvocations))
	}
	invocation := diary.SkillInvocations[0]
	if invocation.InvocationID != invocationID {
		t.Fatalf("diary invocation id = %q, want %q", invocation.InvocationID, invocationID)
	}
	if invocation.Provenance != core.SkillInvocationJournalVerified {
		t.Fatalf("provenance = %q, want %q (Saga Journal evidence was available)", invocation.Provenance, core.SkillInvocationJournalVerified)
	}
	if len(invocation.EventReferences) == 0 {
		t.Fatal("journal-verified invocation carries no event references")
	}

	if diary.GraphState != core.GraphReady || diary.GraphSnapshotID != snapshot.ID {
		t.Fatalf("top-level graph resolution: state=%q snapshot=%q, want ready/%q", diary.GraphState, diary.GraphSnapshotID, snapshot.ID)
	}
	if len(diary.CodeReferences) != 1 || diary.CodeReferences[0].Confidence != core.ConfidenceAuthoritative {
		t.Fatalf("top-level code reference not authoritatively resolved: %+v", diary.CodeReferences)
	}
	if len(invocation.CodeReferences) != 1 || invocation.CodeReferences[0].Confidence != core.ConfidenceAuthoritative {
		t.Fatalf("nested code reference not authoritatively resolved: %+v", invocation.CodeReferences)
	}

	if len(invocation.Artifacts) != 1 {
		t.Fatalf("len(invocation.Artifacts) = %d, want 1", len(invocation.Artifacts))
	}
	evidence := invocation.Artifacts[0]
	if evidence.Availability != core.ArtifactAvailabilityVerified || evidence.SHA256 != wantSHA256 || evidence.SizeBytes != int64(len(reportContent)) {
		t.Fatalf("artifact evidence was not verified against disk: %+v", evidence)
	}

	rendered := core.RenderSessionDiary(diary)
	if strings.Contains(rendered, "Diff since origin/main looks good") {
		t.Fatal("rendered Session Diary leaked the produced artifact's body; it must carry metadata only")
	}
}
