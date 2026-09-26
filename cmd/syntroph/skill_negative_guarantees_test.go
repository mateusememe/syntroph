package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/mateusememe/syntroph/adapters/skillfilesystem"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
)

// poisonedTransport fails any HTTP round trip it is asked to perform and
// counts how many were attempted, so a test can assert zero network access
// occurred rather than merely hoping no request happened to be observed.
type poisonedTransport struct{ calls int }

func (p *poisonedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.calls++
	return nil, fmt.Errorf("network access attempted: %s %s", req.Method, req.URL.String())
}

// TestSkillCatalogOfflineJourneyNeverTouchesNetwork proves that
// synchronizing the curated catalog and preparing a Skill Bundle never
// perform an HTTP round trip, by poisoning http.DefaultTransport for the
// duration of the journey and asserting it was never invoked.
func TestSkillCatalogOfflineJourneyNeverTouchesNetwork(t *testing.T) {
	fixtureRoot := newCuratedSkillFixture(t)

	poisoned := &poisonedTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = poisoned
	t.Cleanup(func() { http.DefaultTransport = previous })

	if _, _, err := runCLI(t, "skill", "sync", "--root", fixtureRoot); err != nil {
		t.Fatalf("skill sync: %v", err)
	}
	if _, _, err := runCLI(t, "skill", "prepare", "mattpocock/code-review",
		"--root", fixtureRoot,
		"--invocation-id", "inv-network-guard",
		"--arguments", `{"fixed_point":"origin/main"}`,
		"--repository", "network-guard-repo",
	); err != nil {
		t.Fatalf("skill prepare: %v", err)
	}
	if _, _, err := runCLI(t, "skill", "verify", "--root", fixtureRoot); err == nil {
		t.Fatal("skill verify unexpectedly succeeded (diagnosing-bugs should still fail verification)")
	}

	if poisoned.calls != 0 {
		t.Fatalf("catalog sync/prepare/verify performed %d HTTP round trip(s) through http.DefaultTransport; want 0", poisoned.calls)
	}
}

// TestSkillCatalogOfflineJourneyNeverRequiresCredentials proves that
// synchronizing and preparing the curated catalog succeeds even when the
// one credential this repository's storage layer reads
// (SYNTROPH_GITHUB_TOKEN) is entirely unset, demonstrating SkillPort's
// local-disk-only design never performs a credential lookup.
func TestSkillCatalogOfflineJourneyNeverRequiresCredentials(t *testing.T) {
	const credentialVar = "SYNTROPH_GITHUB_TOKEN"
	original, hadOriginal := os.LookupEnv(credentialVar)
	if err := os.Unsetenv(credentialVar); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadOriginal {
			os.Setenv(credentialVar, original)
		}
	})

	fixtureRoot := newCuratedSkillFixture(t)
	if _, _, err := runCLI(t, "skill", "sync", "--root", fixtureRoot); err != nil {
		t.Fatalf("skill sync without %s: %v", credentialVar, err)
	}
	if _, _, err := runCLI(t, "skill", "prepare", "mattpocock/code-review",
		"--root", fixtureRoot,
		"--invocation-id", "inv-credential-guard",
		"--arguments", `{"fixed_point":"origin/main"}`,
		"--repository", "credential-guard-repo",
	); err != nil {
		t.Fatalf("skill prepare without %s: %v", credentialVar, err)
	}
}

// TestSkillCatalogSyncNeverInvokesInstallersWritesRuntimeDirsOrExecutesScripts
// proves three related guarantees in one pass: sync and prepare never
// mutate the source .agents/skills tree (no installer invocation), they
// never execute the unsupported diagnosing-bugs package's shell script
// (its content hash is unchanged, and no stray output file appears next to
// it), and they never create a filesystem path outside the small,
// documented set Syntroph owns (no runtime-directory write).
func TestSkillCatalogSyncNeverInvokesInstallersWritesRuntimeDirsOrExecutesScripts(t *testing.T) {
	fixtureRoot := newCuratedSkillFixture(t)

	scriptPath := filepath.Join(fixtureRoot, ".agents", "skills", "diagnosing-bugs", "scripts", "hitl-loop.template.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("fixture is missing diagnosing-bugs' script: %v", err)
	}
	before := hashTree(t, filepath.Join(fixtureRoot, ".agents"))

	if _, _, err := runCLI(t, "skill", "sync", "--root", fixtureRoot); err != nil {
		t.Fatalf("skill sync: %v", err)
	}
	if _, _, err := runCLI(t, "skill", "prepare", "mattpocock/code-review",
		"--root", fixtureRoot,
		"--invocation-id", "inv-installer-guard",
		"--arguments", `{"fixed_point":"origin/main"}`,
		"--repository", "installer-guard-repo",
	); err != nil {
		t.Fatalf("skill prepare: %v", err)
	}

	after := hashTree(t, filepath.Join(fixtureRoot, ".agents"))
	if diff := treeDiff(before, after); diff != "" {
		t.Fatalf("sync/prepare mutated the source .agents/skills tree (no installer or script execution should ever touch it): %s", diff)
	}

	assertOnlyKnownTopLevelPaths(t, fixtureRoot, ".agents", ".syntroph")
	assertOnlyKnownTopLevelPaths(t, filepath.Join(fixtureRoot, ".syntroph"),
		"config.yaml", "skills.runtime.lock.yaml", "catalog", "journal")
}

// countingSkillPort wraps a core.SkillPort and counts how many times
// Prepare is actually invoked on it, so a test can assert the caller's own
// call count matches the underlying port's call count exactly -- proving
// nothing between them retries on its own.
type countingSkillPort struct {
	port  core.SkillPort
	calls int
}

func (c *countingSkillPort) Prepare(ctx context.Context, request core.SkillPrepareRequest) (core.SkillBundle, error) {
	c.calls++
	return c.port.Prepare(ctx, request)
}

// TestSkillPrepareRejectedReplayIsNotAutomaticallyRetried proves that a
// single, explicit `skill prepare` call -- including one that ends up
// rejected as a same-invocation-ID, changed-input replay -- invokes the
// underlying SkillPort exactly once. Nothing in SkillPreparer or the
// catalog loops or retries a failed attempt on the caller's behalf.
func TestSkillPrepareRejectedReplayIsNotAutomaticallyRetried(t *testing.T) {
	fixtureRoot := newCuratedSkillFixture(t)
	if _, _, err := runCLI(t, "skill", "sync", "--root", fixtureRoot); err != nil {
		t.Fatalf("skill sync: %v", err)
	}

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
	journal, err := core.NewSagaJournal(filepath.Join(fixtureRoot, ".syntroph", "journal"))
	if err != nil {
		t.Fatalf("NewSagaJournal: %v", err)
	}
	bus, err := core.NewEventBus(journal)
	if err != nil {
		t.Fatalf("NewEventBus: %v", err)
	}
	counting := &countingSkillPort{port: catalog}
	preparer := core.SkillPreparer{Port: counting, Bus: bus}
	ctx := context.Background()
	invocationID := "inv-retry-guard"

	if _, _, err := preparer.Prepare(ctx, core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: invocationID,
		Arguments:    map[string]any{"fixed_point": "origin/main"},
	}, core.SkillPrepareOptions{RepositoryID: "retry-guard-repo"}); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("one explicit prepare call invoked the underlying SkillPort %d times; want exactly 1", counting.calls)
	}

	_, _, changedErr := preparer.Prepare(ctx, core.SkillPrepareRequest{
		Name:         "mattpocock/code-review",
		InvocationID: invocationID,
		Arguments:    map[string]any{"fixed_point": "HEAD~1"},
	}, core.SkillPrepareOptions{RepositoryID: "retry-guard-repo"})
	if !errors.Is(changedErr, core.ErrSkillInvocationReplay) {
		t.Fatalf("changed-input replay error = %v, want ErrSkillInvocationReplay", changedErr)
	}
	if counting.calls != 2 {
		t.Fatalf("the rejected replay attempt invoked the underlying SkillPort a total of %d times across both calls; want exactly 2 (one per explicit call, no automatic retry)", counting.calls)
	}

	// The rejection itself must be immediately visible in the journal as a
	// single prepare-failed event caused by this attempt's own
	// prepare-requested event, not left implicit or silently swallowed.
	records, err := journal.ReadSaga(ctx, invocationID)
	if err != nil {
		t.Fatalf("ReadSaga: %v", err)
	}
	sawPrepareFailed := false
	for _, record := range records {
		if record.Kind == "event" && record.Event != nil && record.Event.Type == core.SkillEventPrepareFailed {
			sawPrepareFailed = true
		}
	}
	if !sawPrepareFailed {
		t.Fatalf("saga %s has no skill.prepare-failed event recording the rejected replay", invocationID)
	}
}

// TestSkillCatalogSyncNeverDeletesPriorContent proves that re-synchronizing
// an already-synced, unchanged catalog never removes previously catalogued,
// content-addressed package files.
func TestSkillCatalogSyncNeverDeletesPriorContent(t *testing.T) {
	fixtureRoot := newCuratedSkillFixture(t)
	if _, _, err := runCLI(t, "skill", "sync", "--root", fixtureRoot); err != nil {
		t.Fatalf("first skill sync: %v", err)
	}
	storeRoot := filepath.Join(fixtureRoot, ".syntroph", "catalog", "store")
	before := hashTree(t, storeRoot)
	if len(before) == 0 {
		t.Fatal("expected the catalog store to contain synced package files after the first sync")
	}

	if _, _, err := runCLI(t, "skill", "sync", "--root", fixtureRoot); err != nil {
		t.Fatalf("second skill sync: %v", err)
	}
	after := hashTree(t, storeRoot)
	for path, sum := range before {
		got, ok := after[path]
		if !ok {
			t.Fatalf("re-sync deleted previously catalogued file %s", path)
		}
		if got != sum {
			t.Fatalf("re-sync changed previously catalogued file %s (want content-addressed store entries to be immutable)", path)
		}
	}
}

// hashTree returns a SHA-256 hex digest for every regular file under root,
// keyed by its path relative to root.
func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// treeDiff describes the first difference between two hashTree snapshots,
// or "" if they are identical.
func treeDiff(before, after map[string]string) string {
	for path, sum := range before {
		got, ok := after[path]
		if !ok {
			return fmt.Sprintf("file %s was removed", path)
		}
		if got != sum {
			return fmt.Sprintf("file %s changed content", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			return fmt.Sprintf("file %s was added", path)
		}
	}
	return ""
}

// assertOnlyKnownTopLevelPaths fails the test if root contains any
// top-level entry not named in allowed.
func assertOnlyKnownTopLevelPaths(t *testing.T, root string, allowed ...string) {
	t.Helper()
	allowedSet := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !allowedSet[entry.Name()] {
			t.Fatalf("unexpected path created under %s: %s (SkillPort must never write outside its documented set)", root, entry.Name())
		}
	}
}
