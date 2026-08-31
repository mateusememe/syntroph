// Package storage contains the StoragePort and the remote mirror boundary.
// The local Session Diary remains the caller's canonical copy; implementations
// in this package only attempt to mirror it.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

var (
	ErrUnavailable         = errors.New("storage mirror unavailable")
	ErrConflict            = errors.New("storage mirror conflict")
	ErrPrerequisiteMissing = errors.New("storage prerequisite missing")
	ErrMirrorInProgress    = errors.New("storage mirror already in progress")
)

type Backend string

const (
	BackendWiki   Backend = "wiki"
	BackendIssues Backend = "issues"
)

type MirrorState string

const (
	Mirrored                   MirrorState = "mirrored"
	StorageSyncPending         MirrorState = "StorageSyncPending"
	StorageSyncConflict        MirrorState = "StorageSyncConflict"
	StoragePrerequisiteMissing MirrorState = "StoragePrerequisiteMissing"
)

type FailureClass string

const (
	FailureTransient         FailureClass = "transient"
	FailureConflict          FailureClass = "conflict"
	FailurePrerequisite      FailureClass = "prerequisite_missing"
	FailureAlreadyInProgress FailureClass = "already_in_progress"
)

// SessionDiary is the immutable local document handed to a mirror. Mirrors
// must never mutate it or use the remote copy as the canonical source.
type SessionDiary struct {
	SessionID    string
	RepositoryID string
	CommitSHA    string
	ArtifactHash string
	Content      string
}

func (d SessionDiary) Validate() error {
	if d.SessionID == "" || d.RepositoryID == "" || d.CommitSHA == "" || d.ArtifactHash == "" || d.Content == "" {
		return errors.New("session diary is missing required fields")
	}
	return nil
}

func (d SessionDiary) Key() string {
	sum := sha256.Sum256([]byte(d.RepositoryID + "\n" + d.CommitSHA + "\n" + d.ArtifactHash))
	return hex.EncodeToString(sum[:])
}

type MirrorResult struct {
	State               MirrorState
	Backend             Backend
	Provider            string
	Key                 string
	RemoteID            string
	RemoteURL           string
	RemoteRev           string
	ExpectedRev         string
	LocalHash           string
	EffectiveRemoteHash string
	RemoteContent       string
	FailureClass        FailureClass
	ConflictSnapshot    string
	AlreadyInProgress   bool
	// UnverifiedIdentity prevents recovery from reconstructing a binding when
	// a deterministic remote location does not carry the exact Syntroph marker.
	UnverifiedIdentity bool
	Cause              error
}

func (r MirrorResult) Pending() bool  { return r.State == StorageSyncPending }
func (r MirrorResult) Conflict() bool { return r.State == StorageSyncConflict }

// StoragePort mirrors a durable local Session Diary. Implementations must be
// idempotent for diary.Key and must report remote divergence instead of
// overwriting a revision they did not create.
type StoragePort interface {
	Mirror(context.Context, SessionDiary) MirrorResult
	Status(context.Context, SessionDiary) MirrorResult
}

// RemoteDocument is the provider-neutral representation returned by a GitHub
// Wiki page or Issue mirror.
type RemoteDocument struct {
	ID       string
	URL      string
	Revision string
	Content  string
	NotFound bool
}

// GitHubClient is the narrow seam for direct GitHub API or local MCP clients.
// A client is already authenticated; Syntroph never receives or stores a
// token itself.
type GitHubClient interface {
	Get(context.Context, Backend, string, string) (RemoteDocument, error)
	Put(context.Context, Backend, string, string, string, string) (RemoteDocument, error)
}

type Mirror struct {
	Backend    Backend
	Repository string
	Client     GitHubClient
}

func NewMirror(backend Backend, repository string, client GitHubClient) (*Mirror, error) {
	if backend != BackendWiki && backend != BackendIssues {
		return nil, errors.New("backend must be wiki or issues")
	}
	if repository == "" || client == nil {
		return nil, errors.New("repository and authenticated client are required")
	}
	return &Mirror{Backend: backend, Repository: repository, Client: client}, nil
}

func (m *Mirror) Mirror(ctx context.Context, diary SessionDiary) MirrorResult {
	r := MirrorResult{Backend: m.Backend, Key: diary.Key(), LocalHash: contentHash(diary.Content)}
	if err := diary.Validate(); err != nil {
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, err
		return r
	}
	remote, err := m.Client.Get(ctx, m.Backend, m.Repository, r.Key)
	if err != nil {
		if errors.Is(err, ErrPrerequisiteMissing) {
			r.State, r.FailureClass, r.Cause = StoragePrerequisiteMissing, FailurePrerequisite, err
			return r
		}
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	if !remote.NotFound && remote.Content == diary.Content {
		r.State, r.RemoteID, r.RemoteURL, r.RemoteRev = Mirrored, remote.ID, remote.URL, remote.Revision
		r.EffectiveRemoteHash = contentHash(remote.Content)
		return r
	}
	if !remote.NotFound {
		r.State, r.FailureClass = StorageSyncConflict, FailureConflict
		r.RemoteID, r.RemoteURL, r.RemoteRev, r.ExpectedRev = remote.ID, remote.URL, remote.Revision, ""
		r.RemoteContent, r.EffectiveRemoteHash, r.Cause = remote.Content, contentHash(remote.Content), ErrConflict
		return r
	}
	created, err := m.Client.Put(ctx, m.Backend, m.Repository, r.Key, diary.Content, "")
	if err != nil {
		if errors.Is(err, ErrPrerequisiteMissing) {
			r.State, r.FailureClass, r.Cause = StoragePrerequisiteMissing, FailurePrerequisite, err
			return r
		}
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	r.State, r.RemoteID, r.RemoteURL, r.RemoteRev = Mirrored, created.ID, created.URL, created.Revision
	r.EffectiveRemoteHash = contentHash(created.Content)
	if r.EffectiveRemoteHash == contentHash("") {
		r.EffectiveRemoteHash = r.LocalHash
	}
	return r
}

func (m *Mirror) Status(ctx context.Context, diary SessionDiary) MirrorResult {
	r := MirrorResult{Backend: m.Backend, Key: diary.Key(), LocalHash: contentHash(diary.Content)}
	if err := diary.Validate(); err != nil {
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, err
		return r
	}
	remote, err := m.Client.Get(ctx, m.Backend, m.Repository, r.Key)
	if err != nil {
		if errors.Is(err, ErrPrerequisiteMissing) {
			r.State, r.FailureClass, r.Cause = StoragePrerequisiteMissing, FailurePrerequisite, err
			return r
		}
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	if remote.NotFound {
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, errors.New("remote document not found")
		return r
	}
	r.RemoteID, r.RemoteURL, r.RemoteRev = remote.ID, remote.URL, remote.Revision
	r.RemoteContent, r.EffectiveRemoteHash = remote.Content, contentHash(remote.Content)
	if remote.Content != diary.Content {
		r.State, r.FailureClass, r.Cause = StorageSyncConflict, FailureConflict, ErrConflict
		return r
	}
	r.State = Mirrored
	return r
}

// Resolve is intentionally explicit. KeepRemote only acknowledges the remote
// copy; KeepLocal writes only after the caller has displayed a diff and passes
// the revision it observed.
type Resolution string

const (
	KeepLocal  Resolution = "keep-local"
	KeepRemote Resolution = "keep-remote"
)

func (m *Mirror) Resolve(ctx context.Context, diary SessionDiary, choice Resolution, observedRevision string) MirrorResult {
	r := MirrorResult{Backend: m.Backend, Key: diary.Key(), LocalHash: contentHash(diary.Content)}
	if choice != KeepLocal && choice != KeepRemote {
		r.State, r.FailureClass, r.Cause = StorageSyncConflict, FailureConflict, errors.New("resolution must be keep-local or keep-remote")
		return r
	}
	remote, err := m.Client.Get(ctx, m.Backend, m.Repository, r.Key)
	if err != nil {
		if errors.Is(err, ErrPrerequisiteMissing) {
			r.State, r.FailureClass, r.Cause = StoragePrerequisiteMissing, FailurePrerequisite, err
			return r
		}
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	r.RemoteID, r.RemoteURL, r.RemoteRev, r.ExpectedRev = remote.ID, remote.URL, remote.Revision, observedRevision
	r.RemoteContent, r.EffectiveRemoteHash = remote.Content, contentHash(remote.Content)
	if observedRevision == "" || remote.Revision != observedRevision {
		r.State, r.FailureClass, r.Cause = StorageSyncConflict, FailureConflict, ErrConflict
		return r
	}
	if choice == KeepRemote {
		r.State = Mirrored
		return r
	}
	updated, err := m.Client.Put(ctx, m.Backend, m.Repository, r.Key, diary.Content, observedRevision)
	if err != nil {
		if errors.Is(err, ErrPrerequisiteMissing) {
			r.State, r.FailureClass, r.Cause = StoragePrerequisiteMissing, FailurePrerequisite, err
			return r
		}
		r.State, r.FailureClass, r.Cause = StorageSyncPending, FailureTransient, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	r.State, r.RemoteID, r.RemoteURL, r.RemoteRev = Mirrored, updated.ID, updated.URL, updated.Revision
	r.EffectiveRemoteHash = contentHash(updated.Content)
	if r.EffectiveRemoteHash == contentHash("") {
		r.EffectiveRemoteHash = r.LocalHash
	}
	return r
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
