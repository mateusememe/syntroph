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
	ErrUnavailable = errors.New("storage mirror unavailable")
	ErrConflict    = errors.New("storage mirror conflict")
)

type Backend string

const (
	BackendWiki   Backend = "wiki"
	BackendIssues Backend = "issues"
)

type MirrorState string

const (
	Mirrored            MirrorState = "mirrored"
	StorageSyncPending  MirrorState = "StorageSyncPending"
	StorageSyncConflict MirrorState = "StorageSyncConflict"
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
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", d.RepositoryID, d.CommitSHA, d.ArtifactHash)
	return hex.EncodeToString(h.Sum(nil))
}

type MirrorResult struct {
	State       MirrorState
	Backend     Backend
	Key         string
	RemoteID    string
	RemoteRev   string
	ExpectedRev string
	Cause       error
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
	r := MirrorResult{Backend: m.Backend, Key: diary.Key()}
	if err := diary.Validate(); err != nil {
		r.State, r.Cause = StorageSyncPending, err
		return r
	}
	remote, err := m.Client.Get(ctx, m.Backend, m.Repository, r.Key)
	if err != nil {
		r.State, r.Cause = StorageSyncPending, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	if !remote.NotFound && remote.Content == diary.Content {
		r.State, r.RemoteID, r.RemoteRev = Mirrored, remote.ID, remote.Revision
		return r
	}
	if !remote.NotFound {
		r.State, r.RemoteID, r.RemoteRev, r.ExpectedRev = StorageSyncConflict, remote.ID, remote.Revision, ""
		r.Cause = ErrConflict
		return r
	}
	created, err := m.Client.Put(ctx, m.Backend, m.Repository, r.Key, diary.Content, "")
	if err != nil {
		r.State, r.Cause = StorageSyncPending, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	r.State, r.RemoteID, r.RemoteRev = Mirrored, created.ID, created.Revision
	return r
}

func (m *Mirror) Status(ctx context.Context, diary SessionDiary) MirrorResult {
	r := MirrorResult{Backend: m.Backend, Key: diary.Key()}
	if err := diary.Validate(); err != nil {
		r.State, r.Cause = StorageSyncPending, err
		return r
	}
	remote, err := m.Client.Get(ctx, m.Backend, m.Repository, r.Key)
	if err != nil {
		r.State, r.Cause = StorageSyncPending, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	if remote.NotFound {
		r.State, r.Cause = StorageSyncPending, errors.New("remote document not found")
		return r
	}
	r.RemoteID, r.RemoteRev = remote.ID, remote.Revision
	if remote.Content != diary.Content {
		r.State, r.Cause = StorageSyncConflict, ErrConflict
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
	r := MirrorResult{Backend: m.Backend, Key: diary.Key()}
	if choice != KeepLocal && choice != KeepRemote {
		r.State, r.Cause = StorageSyncConflict, errors.New("resolution must be keep-local or keep-remote")
		return r
	}
	remote, err := m.Client.Get(ctx, m.Backend, m.Repository, r.Key)
	if err != nil {
		r.State, r.Cause = StorageSyncPending, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	r.RemoteID, r.RemoteRev, r.ExpectedRev = remote.ID, remote.Revision, observedRevision
	if observedRevision == "" || remote.Revision != observedRevision {
		r.State, r.Cause = StorageSyncConflict, ErrConflict
		return r
	}
	if choice == KeepRemote {
		r.State = Mirrored
		return r
	}
	updated, err := m.Client.Put(ctx, m.Backend, m.Repository, r.Key, diary.Content, observedRevision)
	if err != nil {
		r.State, r.Cause = StorageSyncPending, fmt.Errorf("%w: %v", ErrUnavailable, err)
		return r
	}
	r.State, r.RemoteID, r.RemoteRev = Mirrored, updated.ID, updated.Revision
	return r
}
