package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// RemoteRevision is opaque to Core. Storage providers own the shape of their
// revisions and must use one of the documented provider-typed forms.
type RemoteRevision string

func (r RemoteRevision) Validate() error {
	value := string(r)
	if strings.HasPrefix(value, "git:") {
		if isHex(strings.TrimPrefix(value, "git:"), 7) {
			return nil
		}
		return errors.New("git remote revision requires a commit SHA")
	}
	for _, prefix := range []string{"issue:", "comment:"} {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		body := strings.TrimPrefix(value, prefix)
		first, last := strings.IndexByte(body, ':'), strings.LastIndexByte(body, ':')
		if first <= 0 || last <= first+1 || last == len(body)-1 || !isSHA256(body[last+1:]) {
			return fmt.Errorf("%s remote revision requires id, observed time, and body hash", strings.TrimSuffix(prefix, ":"))
		}
		return nil
	}
	return fmt.Errorf("unsupported remote revision %q", value)
}

// RemoteBinding is the current operational projection of one immutable diary.
// It intentionally stores hashes rather than remote content.
type RemoteBinding struct {
	IdempotencyKey      string         `json:"idempotency_key"`
	Backend             Backend        `json:"backend"`
	Provider            string         `json:"provider"`
	RemoteID            string         `json:"remote_id"`
	URL                 string         `json:"url"`
	RemoteRevision      RemoteRevision `json:"remote_revision"`
	LocalHash           string         `json:"local_hash"`
	EffectiveRemoteHash string         `json:"effective_remote_hash"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

func (b RemoteBinding) Validate() error {
	if !isSHA256(b.IdempotencyKey) {
		return errors.New("remote binding requires a SHA-256 idempotency key")
	}
	if b.Backend != BackendWiki && b.Backend != BackendIssues {
		return errors.New("remote binding requires a supported backend")
	}
	if b.Provider == "" || b.RemoteID == "" || b.URL == "" {
		return errors.New("remote binding requires provider, remote ID, and URL")
	}
	if err := b.RemoteRevision.Validate(); err != nil {
		return err
	}
	if !isSHA256(b.LocalHash) || !isSHA256(b.EffectiveRemoteHash) {
		return errors.New("remote binding requires SHA-256 local and effective remote hashes")
	}
	if b.UpdatedAt.IsZero() {
		return errors.New("remote binding requires updated_at")
	}
	return nil
}

// BindingStore atomically maintains the current Remote Binding projection.
type BindingStore struct{ root string }

func NewBindingStore(root string) (*BindingStore, error) {
	if root == "" {
		return nil, errors.New("storage root is required")
	}
	dir := filepath.Join(root, "bindings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &BindingStore{root: dir}, nil
}

func (s *BindingStore) Path(key string) (string, error) {
	if !isSHA256(key) {
		return "", errors.New("binding key must be a SHA-256 digest")
	}
	return filepath.Join(s.root, key+".json"), nil
}

func (s *BindingStore) Save(ctx context.Context, binding RemoteBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, _ := s.Path(binding.IdempotencyKey)
	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWrite(path, data, 0o600)
}

func (s *BindingStore) Load(ctx context.Context, key string) (RemoteBinding, bool, error) {
	if err := ctx.Err(); err != nil {
		return RemoteBinding{}, false, err
	}
	path, err := s.Path(key)
	if err != nil {
		return RemoteBinding{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RemoteBinding{}, false, nil
	}
	if err != nil {
		return RemoteBinding{}, false, err
	}
	var binding RemoteBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return RemoteBinding{}, false, fmt.Errorf("decode remote binding: %w", err)
	}
	if err := binding.Validate(); err != nil {
		return RemoteBinding{}, false, fmt.Errorf("validate remote binding: %w", err)
	}
	return binding, true, nil
}

// ConflictStore keeps divergent remote content out of Remote Bindings while
// retaining private evidence for an offline recovery diff.
type ConflictStore struct{ root string }

func NewConflictStore(root string) (*ConflictStore, error) {
	if root == "" {
		return nil, errors.New("storage root is required")
	}
	dir := filepath.Join(root, "conflicts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &ConflictStore{root: dir}, nil
}

func (s *ConflictStore) Write(ctx context.Context, key, revision, content string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !isSHA256(key) || revision == "" {
		return "", errors.New("conflict snapshot requires idempotency key and revision")
	}
	dir := filepath.Join(s.root, key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, safeComponent(revision)+".remote.md")
	if err := atomicWrite(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

type MirrorLockOwner struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	OwnerID   string    `json:"owner_id"`
}

type MirrorInProgressError struct {
	Owner MirrorLockOwner
}

func (e *MirrorInProgressError) Error() string {
	return fmt.Sprintf("%v (pid %d since %s)", ErrMirrorInProgress, e.Owner.PID, e.Owner.StartedAt.Format(time.RFC3339Nano))
}

func (e *MirrorInProgressError) Unwrap() error { return ErrMirrorInProgress }

type ProcessAliveFunc func(int) bool

// MirrorLocks owns per-idempotency-key lock files. Existing locks are never
// cleared as a side effect of acquisition.
type MirrorLocks struct {
	root         string
	processAlive ProcessAliveFunc
	now          func() time.Time
	pid          int
}

func NewMirrorLocks(root string) (*MirrorLocks, error) {
	if root == "" {
		return nil, errors.New("storage root is required")
	}
	dir := filepath.Join(root, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &MirrorLocks{root: dir, processAlive: defaultProcessAlive, now: func() time.Time { return time.Now().UTC() }, pid: os.Getpid()}, nil
}

type MirrorLock struct {
	path  string
	owner MirrorLockOwner
	once  bool
}

func (l *MirrorLock) Release() error {
	if l == nil || l.once {
		return nil
	}
	l.once = true
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var owner MirrorLockOwner
	if json.Unmarshal(data, &owner) != nil || owner.OwnerID != l.owner.OwnerID {
		return errors.New("mirror lock ownership changed before release")
	}
	return os.Remove(l.path)
}

func (s *MirrorLocks) Acquire(ctx context.Context, key string) (*MirrorLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	owner := MirrorLockOwner{PID: s.pid, StartedAt: s.now(), OwnerID: randomID()}
	data, _ := json.Marshal(owner)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := readLockOwner(path)
		if readErr != nil {
			return nil, fmt.Errorf("read existing mirror lock: %w", readErr)
		}
		return nil, &MirrorInProgressError{Owner: existing}
	}
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return &MirrorLock{path: path, owner: owner}, nil
}

func (s *MirrorLocks) Owner(ctx context.Context, key string) (MirrorLockOwner, bool, error) {
	if err := ctx.Err(); err != nil {
		return MirrorLockOwner{}, false, err
	}
	path, err := s.path(key)
	if err != nil {
		return MirrorLockOwner{}, false, err
	}
	owner, err := readLockOwner(path)
	if errors.Is(err, os.ErrNotExist) {
		return MirrorLockOwner{}, false, nil
	}
	return owner, err == nil, err
}

// ClearOrphan is deliberately explicit. It verifies process liveness and
// refuses to preempt a live or unverifiable owner.
func (s *MirrorLocks) ClearOrphan(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	owner, err := readLockOwner(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.processAlive(owner.PID) {
		return fmt.Errorf("cannot clear mirror lock: process %d is still alive", owner.PID)
	}
	return os.Remove(path)
}

func (s *MirrorLocks) path(key string) (string, error) {
	if !isSHA256(key) {
		return "", errors.New("mirror lock key must be a SHA-256 digest")
	}
	return filepath.Join(s.root, key+".lock"), nil
}

// ManagedMirror layers durable bindings, conflicts, and local concurrency on
// top of any provider-neutral mirror implementation.
type ManagedMirror struct {
	Remote interface {
		Mirror(context.Context, SessionDiary) MirrorResult
		Status(context.Context, SessionDiary) MirrorResult
		Resolve(context.Context, SessionDiary, Resolution, string) MirrorResult
	}
	Provider  string
	Bindings  *BindingStore
	Conflicts *ConflictStore
	Locks     *MirrorLocks
}

func NewManagedMirror(root, provider string, remote interface {
	Mirror(context.Context, SessionDiary) MirrorResult
	Status(context.Context, SessionDiary) MirrorResult
	Resolve(context.Context, SessionDiary, Resolution, string) MirrorResult
}) (*ManagedMirror, error) {
	if provider == "" || remote == nil {
		return nil, errors.New("managed mirror requires provider and remote")
	}
	bindings, err := NewBindingStore(root)
	if err != nil {
		return nil, err
	}
	conflicts, err := NewConflictStore(root)
	if err != nil {
		return nil, err
	}
	locks, err := NewMirrorLocks(root)
	if err != nil {
		return nil, err
	}
	return &ManagedMirror{Remote: remote, Provider: provider, Bindings: bindings, Conflicts: conflicts, Locks: locks}, nil
}

func (m *ManagedMirror) Mirror(ctx context.Context, diary SessionDiary) MirrorResult {
	return m.run(ctx, diary, func() MirrorResult { return m.Remote.Mirror(ctx, diary) })
}

func (m *ManagedMirror) Status(ctx context.Context, diary SessionDiary) MirrorResult {
	result := m.Remote.Status(ctx, diary)
	result.Provider = m.Provider
	return result
}

func (m *ManagedMirror) Resolve(ctx context.Context, diary SessionDiary, choice Resolution, observedRevision string) MirrorResult {
	return m.run(ctx, diary, func() MirrorResult { return m.Remote.Resolve(ctx, diary, choice, observedRevision) })
}

func (m *ManagedMirror) run(ctx context.Context, diary SessionDiary, effect func() MirrorResult) MirrorResult {
	key := diary.Key()
	lock, err := m.Locks.Acquire(ctx, key)
	if err != nil {
		result := MirrorResult{Key: key, Provider: m.Provider, State: StorageSyncPending, FailureClass: FailureTransient, Cause: err}
		if errors.Is(err, ErrMirrorInProgress) {
			result.FailureClass, result.AlreadyInProgress = FailureAlreadyInProgress, true
		}
		return result
	}
	defer lock.Release()

	result := effect()
	result.Provider = m.Provider
	if result.State != Mirrored && result.State != StorageSyncConflict {
		return result
	}
	if result.State == StorageSyncConflict {
		path, snapshotErr := m.Conflicts.Write(ctx, key, result.RemoteRev, result.RemoteContent)
		if snapshotErr != nil {
			result.State, result.FailureClass, result.Cause = StorageSyncPending, FailureTransient, fmt.Errorf("persist conflict snapshot: %w", snapshotErr)
			return result
		}
		result.ConflictSnapshot = path
		if result.UnverifiedIdentity {
			return result
		}
	}
	binding := RemoteBinding{
		IdempotencyKey: key, Backend: result.Backend, Provider: m.Provider,
		RemoteID: result.RemoteID, URL: result.RemoteURL, RemoteRevision: RemoteRevision(result.RemoteRev),
		LocalHash: result.LocalHash, EffectiveRemoteHash: result.EffectiveRemoteHash, UpdatedAt: time.Now().UTC(),
	}
	if err := m.Bindings.Save(ctx, binding); err != nil {
		result.State, result.FailureClass, result.Cause = StorageSyncPending, FailureTransient, fmt.Errorf("persist remote binding: %w", err)
	}
	return result
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".syntroph-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readLockOwner(path string) (MirrorLockOwner, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MirrorLockOwner{}, err
	}
	var owner MirrorLockOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return MirrorLockOwner{}, err
	}
	if owner.PID <= 0 || owner.StartedAt.IsZero() || owner.OwnerID == "" {
		return MirrorLockOwner{}, errors.New("invalid mirror lock owner")
	}
	return owner, nil
}

func randomID() string {
	sequence := lockSequence.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), sequence)))
	return hex.EncodeToString(sum[:16])
}

var lockSequence atomic.Uint64

func safeComponent(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "revision"
	}
	return b.String()
}

func isHex(value string, minimum int) bool {
	if len(value) < minimum {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func isSHA256(value string) bool { return len(value) == 64 && isHex(value, 64) }
