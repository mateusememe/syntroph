package skillfilesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const SyncLockSchemaVersion = 1

type processState uint8

const (
	processUnknown processState = iota
	processAlive
	processDead
)

type SyncState string

const (
	SyncSucceeded        SyncState = "succeeded"
	SyncInProgress       SyncState = "in_progress"
	SyncRecoveryRequired SyncState = "recovery_required"
)

type SyncCheckpoint string

const (
	SyncCheckpointLockCandidateReady SyncCheckpoint = "lock_candidate_ready"
	SyncCheckpointLockPublished      SyncCheckpoint = "lock_published"
	SyncCheckpointStagingCreated     SyncCheckpoint = "staging_created"
	SyncCheckpointStoreObjectStaged  SyncCheckpoint = "store_object_staged"
	SyncCheckpointStoreReady         SyncCheckpoint = "store_ready"
	SyncCheckpointIndexStaged        SyncCheckpoint = "index_staged"
	SyncCheckpointIndexPublished     SyncCheckpoint = "index_published"
)

type SyncRecoveryState string

const (
	SyncRecoveryNone         SyncRecoveryState = "none"
	SyncRecoveryInProgress   SyncRecoveryState = "in_progress"
	SyncRecoveryOrphaned     SyncRecoveryState = "orphaned"
	SyncRecoveryUnverifiable SyncRecoveryState = "unverifiable"
)

// ErrSyncStillRunning prevents explicit recovery from preempting a live owner.
var ErrSyncStillRunning = errors.New("skill catalog synchronization is still running")

// ErrSyncRecoveryRequired requires explicit recovery instead of automatic lock removal.
var ErrSyncRecoveryRequired = errors.New("orphaned skill catalog synchronization requires explicit recovery with syntroph skill recovery --clear-orphan")

// ErrSyncLivenessUnverifiable prevents cleanup when process state is uncertain.
var ErrSyncLivenessUnverifiable = errors.New("skill catalog synchronization owner liveness cannot be verified")

type SyncRecovery struct {
	State       SyncRecoveryState   `json:"state"`
	Owner       *SyncLockOwner      `json:"owner,omitempty"`
	StagingPath string              `json:"staging_path,omitempty"`
	Candidates  []SyncLockCandidate `json:"candidates,omitempty"`
}

type SyncLockCandidate struct {
	Path  string        `json:"path"`
	Owner SyncLockOwner `json:"owner"`
}

type SyncLockOwner struct {
	SchemaVersion int       `json:"schema_version"`
	PID           int       `json:"pid"`
	StartedAt     time.Time `json:"started_at"`
	OwnerID       string    `json:"owner_id"`
}

type syncLock struct {
	path          string
	owner         SyncLockOwner
	syncDirectory func(string) error
}

func (c *Catalog) acquireSyncLock(ctx context.Context) (*syncLock, *SyncLockOwner, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(c.catalogRoot, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create skill catalog root: %w", err)
	}
	path := filepath.Join(c.catalogRoot, "sync.lock")
	owner := SyncLockOwner{
		SchemaVersion: SyncLockSchemaVersion,
		PID:           c.pid(),
		StartedAt:     c.now().UTC(),
		OwnerID:       c.ownerID(),
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return nil, nil, err
	}
	candidate := filepath.Join(c.catalogRoot, ".sync-lock-"+owner.OwnerID+".candidate")
	temporary, err := os.CreateTemp(c.catalogRoot, ".sync-owner-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create skill catalog sync owner temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	temporaryPresent := true
	defer func() {
		if temporaryPresent {
			_ = os.Remove(temporaryPath)
		}
	}()
	candidatePresent := false
	defer func() {
		if candidatePresent {
			_ = os.Remove(candidate)
		}
	}()
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, nil, fmt.Errorf("persist skill catalog sync owner temporary file: %w", err)
	}
	published, publishErr := publishLockCandidate(temporaryPath, candidate)
	if published {
		candidatePresent = true
		if publishErr == nil {
			temporaryPresent = false
		}
	}
	if publishErr != nil {
		return nil, nil, fmt.Errorf("publish skill catalog sync lock candidate: %w", publishErr)
	}
	if err := syncDirectory(c.catalogRoot); err != nil {
		return nil, nil, fmt.Errorf("persist skill catalog sync lock candidate directory: %w", err)
	}
	if err := c.reachCheckpoint(SyncCheckpointLockCandidateReady); err != nil {
		return nil, nil, err
	}
	lock := &syncLock{path: path, owner: owner, syncDirectory: syncDirectory}
	published, publishErr = publishLockCandidate(candidate, path)
	if errors.Is(publishErr, os.ErrExist) {
		existing, readErr := readSyncLockOwner(path)
		if readErr != nil {
			return nil, nil, fmt.Errorf("read existing skill catalog sync lock: %w", readErr)
		}
		return nil, &existing, nil
	} else if publishErr != nil {
		if published {
			publishErr = errors.Join(publishErr, lock.Release())
		}
		return nil, nil, fmt.Errorf("publish skill catalog sync lock: %w", publishErr)
	}
	candidatePresent = false
	if err := syncDirectory(c.catalogRoot); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("persist skill catalog sync lock directory: %w", err), lock.Release())
	}
	if err := c.reachCheckpoint(SyncCheckpointLockPublished); err != nil {
		return nil, nil, errors.Join(err, lock.Release())
	}
	return lock, nil, nil
}

func (lock *syncLock) Release() error {
	if lock == nil {
		return nil
	}
	owner, err := readSyncLockOwner(lock.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner.OwnerID != lock.owner.OwnerID {
		return errors.New("skill catalog sync lock ownership changed before release")
	}
	if err := os.Remove(lock.path); err != nil {
		return err
	}
	return lock.syncDirectory(filepath.Dir(lock.path))
}

// InspectSyncRecovery reports local recovery state without mutating it.
func (c *Catalog) InspectSyncRecovery(ctx context.Context) (SyncRecovery, error) {
	if err := ctx.Err(); err != nil {
		return SyncRecovery{}, err
	}
	candidates, err := c.discoverSyncLockCandidates()
	if err != nil {
		return SyncRecovery{}, err
	}
	owner, err := readSyncLockOwner(filepath.Join(c.catalogRoot, "sync.lock"))
	if errors.Is(err, os.ErrNotExist) {
		if len(candidates) == 0 {
			return SyncRecovery{State: SyncRecoveryNone}, nil
		}
		return SyncRecovery{State: c.recoveryState(nil, candidates), Candidates: candidates}, nil
	}
	if err != nil {
		return SyncRecovery{}, fmt.Errorf("inspect skill catalog sync recovery: %w", err)
	}
	return SyncRecovery{
		State:       c.recoveryState(&owner, candidates),
		Owner:       &owner,
		StagingPath: filepath.Join(c.catalogRoot, "staging", owner.OwnerID),
		Candidates:  candidates,
	}, nil
}

func (c *Catalog) discoverSyncLockCandidates() ([]SyncLockCandidate, error) {
	paths, err := filepath.Glob(filepath.Join(c.catalogRoot, ".sync-lock-*.candidate"))
	if err != nil {
		return nil, fmt.Errorf("discover skill catalog sync lock candidates: %w", err)
	}
	sort.Strings(paths)
	candidates := make([]SyncLockCandidate, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect skill catalog sync lock candidate: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("skill catalog sync lock candidate %s is not a regular file", filepath.Base(path))
		}
		owner, err := readSyncLockOwner(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read skill catalog sync lock candidate: %w", err)
		}
		if filepath.Base(path) != ".sync-lock-"+owner.OwnerID+".candidate" {
			return nil, errors.New("skill catalog sync lock candidate name does not match its owner")
		}
		candidates = append(candidates, SyncLockCandidate{Path: path, Owner: owner})
	}
	return candidates, nil
}

func (c *Catalog) recoveryState(owner *SyncLockOwner, candidates []SyncLockCandidate) SyncRecoveryState {
	state := SyncRecoveryOrphaned
	probe := func(pid int) {
		switch c.processState(pid) {
		case processUnknown:
			state = SyncRecoveryUnverifiable
		case processAlive:
			if state != SyncRecoveryUnverifiable {
				state = SyncRecoveryInProgress
			}
		}
	}
	if owner != nil {
		probe(owner.PID)
	}
	for _, candidate := range candidates {
		probe(candidate.Owner.PID)
	}
	return state
}

// RecoverOrphanedSync removes only state whose recorded process is confirmed dead.
func (c *Catalog) RecoverOrphanedSync(ctx context.Context) error {
	recovery, err := c.InspectSyncRecovery(ctx)
	if err != nil {
		return err
	}
	if recovery.State == SyncRecoveryNone {
		return nil
	}
	probe := func(pid int) error {
		switch c.processState(pid) {
		case processAlive:
			return ErrSyncStillRunning
		case processUnknown:
			return ErrSyncLivenessUnverifiable
		default:
			return nil
		}
	}
	if recovery.Owner != nil {
		if err := probe(recovery.Owner.PID); err != nil {
			return err
		}
	}
	for _, candidate := range recovery.Candidates {
		if err := probe(candidate.Owner.PID); err != nil {
			return err
		}
	}
	if recovery.Owner != nil {
		stagingOwnerPath := filepath.Join(recovery.StagingPath, "owner.json")
		stagingOwner, err := readSyncLockOwner(stagingOwnerPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("verify orphaned skill catalog staging owner: %w", err)
		}
		if err == nil && stagingOwner != *recovery.Owner {
			return errors.New("orphaned skill catalog staging ownership does not match the installation lock")
		}
		if errors.Is(err, os.ErrNotExist) {
			if _, statErr := os.Stat(recovery.StagingPath); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("inspect orphaned skill catalog staging: %w", statErr)
			}
		}
	}
	if recovery.Owner != nil {
		if err := os.RemoveAll(recovery.StagingPath); err != nil {
			return fmt.Errorf("remove orphaned skill catalog staging: %w", err)
		}
	}
	for _, candidate := range recovery.Candidates {
		if err := os.Remove(candidate.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove orphaned skill catalog lock candidate: %w", err)
		}
	}
	if recovery.Owner != nil {
		lock := &syncLock{path: filepath.Join(c.catalogRoot, "sync.lock"), owner: *recovery.Owner, syncDirectory: syncDirectory}
		if err := lock.Release(); err != nil {
			return fmt.Errorf("clear orphaned skill catalog sync lock: %w", err)
		}
	} else if len(recovery.Candidates) > 0 {
		if err := syncDirectory(c.catalogRoot); err != nil {
			return fmt.Errorf("persist orphaned skill catalog candidate cleanup: %w", err)
		}
	}
	return nil
}

func readSyncLockOwner(path string) (SyncLockOwner, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SyncLockOwner{}, err
	}
	var owner SyncLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return SyncLockOwner{}, err
	}
	if owner.SchemaVersion != SyncLockSchemaVersion || owner.PID <= 0 || owner.StartedAt.IsZero() || !validSyncOwnerID(owner.OwnerID) {
		return SyncLockOwner{}, errors.New("invalid skill catalog sync lock owner")
	}
	return owner, nil
}

func validSyncOwnerID(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func randomSyncOwnerID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}
