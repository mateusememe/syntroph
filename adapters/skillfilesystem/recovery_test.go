package skillfilesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
)

func TestConcurrentSyncReturnsInProgressAndReadersKeepPreviousIndex(t *testing.T) {
	repositoryRoot, settings, sourcePath := recoveryCatalogFixture(t, "# First\n")
	baseline, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := baseline.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	previousHash := firstResult.Entries[0].Identity.PackageHash
	if err := os.WriteFile(sourcePath, []byte("# Second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	first, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	first.checkpoint = func(checkpoint SyncCheckpoint) error {
		if checkpoint == SyncCheckpointStagingCreated {
			close(entered)
			<-release
		}
		return nil
	}
	firstDone := make(chan error, 1)
	go func() {
		_, syncErr := first.Sync(context.Background())
		firstDone <- syncErr
	}()
	<-entered

	second, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	inProgress, err := second.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inProgress.State != SyncInProgress || inProgress.Owner == nil || inProgress.Owner.PID <= 0 {
		t.Fatalf("concurrent sync result = %+v, want live in-progress owner", inProgress)
	}
	listed, err := second.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Identity.PackageHash != previousHash {
		t.Fatalf("reader observed partial sync: entries=%+v previous_hash=%q", listed, previousHash)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestReadersObserveOnlyCompleteIndexesAtEverySyncCheckpoint(t *testing.T) {
	repositoryRoot, settings, sourcePath := recoveryCatalogFixture(t, "# First\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldHash := baseline.Entries[0].Identity.PackageHash
	if err := os.WriteFile(sourcePath, []byte("# Second\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var checkpoints []SyncCheckpoint
	var newHash string
	catalog.checkpoint = func(checkpoint SyncCheckpoint) error {
		checkpoints = append(checkpoints, checkpoint)
		listed, listErr := catalog.List(context.Background())
		if listErr != nil {
			return listErr
		}
		if len(listed) != 1 {
			return errors.New("checkpoint reader did not observe one complete entry")
		}
		if checkpoint == SyncCheckpointIndexPublished {
			newHash = listed[0].Identity.PackageHash
			if newHash == oldHash {
				return errors.New("published checkpoint still exposed the previous index")
			}
			return nil
		}
		if listed[0].Identity.PackageHash != oldHash {
			return errors.New("reader observed the next index before atomic publication")
		}
		return nil
	}
	result, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCheckpoints := []SyncCheckpoint{
		SyncCheckpointLockCandidateReady,
		SyncCheckpointLockPublished,
		SyncCheckpointStagingCreated,
		SyncCheckpointStoreObjectStaged,
		SyncCheckpointStoreReady,
		SyncCheckpointIndexStaged,
		SyncCheckpointIndexPublished,
	}
	if !reflect.DeepEqual(checkpoints, wantCheckpoints) {
		t.Fatalf("checkpoints = %v, want %v", checkpoints, wantCheckpoints)
	}
	if result.Entries[0].Identity.PackageHash != newHash {
		t.Fatalf("sync result hash = %q, published hash = %q", result.Entries[0].Identity.PackageHash, newHash)
	}
}

func TestLockOwnerIsCompleteBeforeExclusivePublication(t *testing.T) {
	repositoryRoot, settings, _ := recoveryCatalogFixture(t, "# Review\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("simulated crash before lock publication")
	catalog.checkpoint = func(checkpoint SyncCheckpoint) error {
		if checkpoint != SyncCheckpointLockCandidateReady {
			return nil
		}
		lockPath := filepath.Join(repositoryRoot, ".syntroph", "catalog", "sync.lock")
		if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
			return fmt.Errorf("incomplete lock became visible: %v", statErr)
		}
		candidates, globErr := filepath.Glob(filepath.Join(repositoryRoot, ".syntroph", "catalog", ".sync-lock-*.candidate"))
		if globErr != nil || len(candidates) != 1 {
			return fmt.Errorf("lock candidates = %v, %v", candidates, globErr)
		}
		if _, readErr := readSyncLockOwner(candidates[0]); readErr != nil {
			return fmt.Errorf("candidate owner is incomplete: %w", readErr)
		}
		return wantErr
	}
	if _, err := catalog.Sync(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("sync error = %v, want simulated crash", err)
	}
	if _, err := os.Stat(filepath.Join(repositoryRoot, ".syntroph", "catalog", "sync.lock")); !os.IsNotExist(err) {
		t.Fatalf("failed candidate published a lock: %v", err)
	}
}

func TestCandidateOnlyCrashIsExplicitlyRecoverable(t *testing.T) {
	if repositoryRoot := os.Getenv("SYNTROPH_TEST_CANDIDATE_CRASH_ROOT"); repositoryRoot != "" {
		settings := config.ResolvedSkills{
			Enabled:     true,
			RuntimeLock: filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml"),
			Sources: []config.ResolvedSkillSource{
				{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")},
				{ID: "source", Root: filepath.Join(repositoryRoot, "skills")},
			},
		}
		catalog, err := New(repositoryRoot, settings)
		if err != nil {
			os.Exit(71)
		}
		catalog.checkpoint = func(checkpoint SyncCheckpoint) error {
			if checkpoint == SyncCheckpointLockCandidateReady {
				os.Exit(73)
			}
			return nil
		}
		_, _ = catalog.Sync(context.Background())
		os.Exit(72)
	}

	repositoryRoot, settings, _ := recoveryCatalogFixture(t, "# Review\n")
	command := exec.Command(os.Args[0], "-test.run=^TestCandidateOnlyCrashIsExplicitlyRecoverable$")
	command.Env = append(os.Environ(), "SYNTROPH_TEST_CANDIDATE_CRASH_ROOT="+repositoryRoot)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 {
		t.Fatalf("candidate crash subprocess = %v, output=%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(repositoryRoot, ".syntroph", "catalog", "sync.lock")); !os.IsNotExist(err) {
		t.Fatalf("candidate-only crash published a lock: %v", err)
	}

	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := catalog.InspectSyncRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if recovery.State != SyncRecoveryOrphaned || recovery.Owner != nil || len(recovery.Candidates) != 1 {
		t.Fatalf("candidate-only recovery = %+v", recovery)
	}
	if err := catalog.RecoverOrphanedSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := catalog.InspectSyncRecovery(context.Background())
	if err != nil || after.State != SyncRecoveryNone {
		t.Fatalf("recovery after candidate cleanup = %+v, %v", after, err)
	}
}

func TestPackageStagingBelongsToSyncOwner(t *testing.T) {
	repositoryRoot, settings, _ := recoveryCatalogFixture(t, "# Review\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("simulated package materialization crash")
	catalog.checkpoint = func(checkpoint SyncCheckpoint) error {
		if checkpoint != SyncCheckpointStoreObjectStaged {
			return nil
		}
		owner, readErr := readSyncLockOwner(filepath.Join(repositoryRoot, ".syntroph", "catalog", "sync.lock"))
		if readErr != nil {
			return readErr
		}
		ownedPackages, globErr := filepath.Glob(filepath.Join(repositoryRoot, ".syntroph", "catalog", "staging", owner.OwnerID, "store", "*"))
		if globErr != nil || len(ownedPackages) != 1 {
			return fmt.Errorf("owned package staging = %v, %v", ownedPackages, globErr)
		}
		anonymousStoreEntries, globErr := filepath.Glob(filepath.Join(repositoryRoot, ".syntroph", "catalog", "store", ".package-*"))
		if globErr != nil || len(anonymousStoreEntries) != 0 {
			return fmt.Errorf("unowned package staging = %v, %v", anonymousStoreEntries, globErr)
		}
		return wantErr
	}
	if _, err := catalog.Sync(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("sync error = %v, want simulated crash", err)
	}
}

func TestFailedSyncPreservesActiveIndexAndCleansOwnedState(t *testing.T) {
	repositoryRoot, settings, sourcePath := recoveryCatalogFixture(t, "# First\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldHash := baseline.Entries[0].Identity.PackageHash
	if err := os.WriteFile(sourcePath, []byte("# Second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("simulated failure before publication")
	catalog.checkpoint = func(checkpoint SyncCheckpoint) error {
		if checkpoint == SyncCheckpointIndexStaged {
			return wantErr
		}
		return nil
	}
	if _, err := catalog.Sync(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("sync error = %v, want %v", err, wantErr)
	}
	listed, err := catalog.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Identity.PackageHash != oldHash {
		t.Fatalf("failed sync replaced active index: %+v", listed)
	}
	if _, err := os.Stat(filepath.Join(repositoryRoot, ".syntroph", "catalog", "sync.lock")); !os.IsNotExist(err) {
		t.Fatalf("owned lock remains after failure: %v", err)
	}
	staging, err := os.ReadDir(filepath.Join(repositoryRoot, ".syntroph", "catalog", "staging"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Fatalf("owned staging remains after failure: %v", staging)
	}
}

func TestFailedSyncBoundsDiagnosticWithoutLosingItsCause(t *testing.T) {
	repositoryRoot, settings, _ := recoveryCatalogFixture(t, "# Review\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New(strings.Repeat("diagnostic-", 300))
	catalog.checkpoint = func(SyncCheckpoint) error { return wantErr }
	_, err = catalog.Sync(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("bounded error lost original cause: %v", err)
	}
	if len(err.Error()) > maxSyncDiagnosticBytes {
		t.Fatalf("sync diagnostic has %d bytes, limit is %d", len(err.Error()), maxSyncDiagnosticBytes)
	}
}

func TestPrepareRefreshesCommittedIndexAfterPostPublicationFailure(t *testing.T) {
	repositoryRoot, settings, sourcePath := recoveryCatalogFixture(t, "# First\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "source/review", InvocationID: "before-publication"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("# Second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("simulated failure after index publication")
	catalog.checkpoint = func(checkpoint SyncCheckpoint) error {
		if checkpoint == SyncCheckpointIndexPublished {
			return wantErr
		}
		return nil
	}
	if _, err := catalog.Sync(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("sync error = %v, want %v", err, wantErr)
	}
	second, err := catalog.Prepare(context.Background(), core.SkillPrepareRequest{Name: "source/review", InvocationID: "after-publication"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Instructions() != "# Second\n" || second.PackageIdentity().PackageHash == first.PackageIdentity().PackageHash {
		t.Fatalf("prepare used stale catalog after committed publication: first=%+v second=%+v", first.PackageIdentity(), second.PackageIdentity())
	}
}

func TestExplicitRecoveryClearsOnlyVerifiedOrphanedSync(t *testing.T) {
	repositoryRoot, settings, _ := recoveryCatalogFixture(t, "# Review\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	catalog.pid = func() int { return 4242 }
	catalog.now = func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }
	catalog.ownerID = func() string { return "owner-4242" }
	lock, existing, err := catalog.acquireSyncLock(context.Background())
	if err != nil || existing != nil {
		t.Fatalf("acquire lock: lock=%v existing=%v err=%v", lock, existing, err)
	}
	stagingRoot := filepath.Join(repositoryRoot, ".syntroph", "catalog", "staging", lock.owner.OwnerID)
	writeRecoveryFile(t, filepath.Join(stagingRoot, "owner.json"), `{"schema_version":1,"pid":4242,"started_at":"2026-09-14T12:00:00Z","owner_id":"owner-4242"}`)

	catalog.processState = func(pid int) processState {
		if pid == 4242 {
			return processAlive
		}
		return processDead
	}
	live, err := catalog.InspectSyncRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live.State != SyncRecoveryInProgress || live.Owner == nil {
		t.Fatalf("live recovery state = %+v", live)
	}
	if err := catalog.RecoverOrphanedSync(context.Background()); !errors.Is(err, ErrSyncStillRunning) {
		t.Fatalf("live recovery error = %v, want ErrSyncStillRunning", err)
	}
	if _, err := os.Stat(stagingRoot); err != nil {
		t.Fatalf("live staging was mutated: %v", err)
	}

	catalog.processState = func(int) processState { return processUnknown }
	unverifiable, err := catalog.InspectSyncRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if unverifiable.State != SyncRecoveryUnverifiable {
		t.Fatalf("unverifiable recovery state = %+v", unverifiable)
	}
	if err := catalog.RecoverOrphanedSync(context.Background()); !errors.Is(err, ErrSyncLivenessUnverifiable) {
		t.Fatalf("unverifiable recovery error = %v", err)
	}
	if _, err := os.Stat(stagingRoot); err != nil {
		t.Fatalf("unverifiable staging was mutated: %v", err)
	}

	catalog.processState = func(int) processState { return processDead }
	orphaned, err := catalog.InspectSyncRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if orphaned.State != SyncRecoveryOrphaned || orphaned.StagingPath != stagingRoot {
		t.Fatalf("orphan recovery state = %+v", orphaned)
	}
	blocked, err := catalog.Sync(context.Background())
	if !errors.Is(err, ErrSyncRecoveryRequired) || blocked.State != SyncRecoveryRequired {
		t.Fatalf("sync with orphan = %+v, %v; want explicit recovery", blocked, err)
	}
	if _, err := os.Stat(stagingRoot); err != nil {
		t.Fatalf("blocked sync mutated orphan staging: %v", err)
	}
	if err := catalog.RecoverOrphanedSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stagingRoot); !os.IsNotExist(err) {
		t.Fatalf("orphan staging remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repositoryRoot, ".syntroph", "catalog", "sync.lock")); !os.IsNotExist(err) {
		t.Fatalf("orphan lock remains: %v", err)
	}
	if result, err := catalog.Sync(context.Background()); err != nil || result.State != SyncSucceeded {
		t.Fatalf("sync after explicit recovery = %+v, %v", result, err)
	}
}

func TestRecoveryRejectsUnconfinedLockOwnerWithoutMutation(t *testing.T) {
	repositoryRoot, settings, _ := recoveryCatalogFixture(t, "# Review\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(repositoryRoot, "outside")
	writeRecoveryFile(t, filepath.Join(outside, "sentinel"), "keep")
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "catalog", "sync.lock")
	writeRecoveryFile(t, lockPath, `{"schema_version":1,"pid":999999,"started_at":"2026-09-14T12:00:00Z","owner_id":"../../outside"}`)
	catalog.processState = func(int) processState { return processDead }
	if _, err := catalog.InspectSyncRecovery(context.Background()); err == nil {
		t.Fatal("unsafe lock owner was accepted")
	}
	if err := catalog.RecoverOrphanedSync(context.Background()); err == nil {
		t.Fatal("unsafe lock owner was recovered")
	}
	if _, err := os.Stat(filepath.Join(outside, "sentinel")); err != nil {
		t.Fatalf("recovery mutated path outside staging: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("unverifiable lock was removed: %v", err)
	}
}

func TestSyncReplayReusesStoreObjectsAndRetainsHistoricalBundles(t *testing.T) {
	repositoryRoot, settings, sourcePath := recoveryCatalogFixture(t, "# First\n")
	catalog, err := New(repositoryRoot, settings)
	if err != nil {
		t.Fatal(err)
	}
	first, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstHash := first.Entries[0].Identity.PackageHash
	second, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Entries[0].Identity.PackageHash != firstHash {
		t.Fatalf("replay changed package hash: first=%q second=%q", firstHash, second.Entries[0].Identity.PackageHash)
	}
	if err := os.WriteFile(sourcePath, []byte("# Second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := catalog.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	thirdHash := third.Entries[0].Identity.PackageHash
	if thirdHash == firstHash {
		t.Fatal("changed package reused the previous hash")
	}
	for _, hash := range []string{firstHash, thirdHash} {
		if _, err := os.Stat(filepath.Join(repositoryRoot, ".syntroph", "catalog", "store", hash, "package.json")); err != nil {
			t.Fatalf("historical store object %s is unavailable: %v", hash, err)
		}
	}
}

func recoveryCatalogFixture(t *testing.T, instructions string) (string, config.ResolvedSkills, string) {
	t.Helper()
	repositoryRoot := t.TempDir()
	lockPath := filepath.Join(repositoryRoot, ".syntroph", "skills.runtime.lock.yaml")
	writeRecoveryFile(t, lockPath, `schema_version: 1
packages:
  - source_id: source
    name: review
    directory: review
    description: Review changes
    license: MIT
    source_url: https://example.test/skills
    source_revision: revision-1
    instructions: SKILL.md
    files: [SKILL.md]
`)
	sourcePath := filepath.Join(repositoryRoot, "skills", "review", "SKILL.md")
	writeRecoveryFile(t, sourcePath, instructions)
	return repositoryRoot, config.ResolvedSkills{
		Enabled:     true,
		RuntimeLock: lockPath,
		Sources: []config.ResolvedSkillSource{
			{ID: "local", Root: filepath.Join(repositoryRoot, ".syntroph", "skills")},
			{ID: "source", Root: filepath.Join(repositoryRoot, "skills")},
		},
	}, sourcePath
}

func writeRecoveryFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
