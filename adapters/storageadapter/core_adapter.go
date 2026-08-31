// Package storageadapter bridges the core-owned diary to the provider package.
package storageadapter

import (
	"context"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

type Provider interface {
	Mirror(context.Context, storage.SessionDiary) storage.MirrorResult
	Resolve(context.Context, storage.SessionDiary, storage.Resolution, string) storage.MirrorResult
}

type Adapter struct{ Provider Provider }

func (a Adapter) Mirror(ctx context.Context, d core.SessionDiary) core.StorageResult {
	return a.MirrorEvent(ctx, d.SessionID, d)
}
func (a Adapter) MirrorEvent(ctx context.Context, eventID string, d core.SessionDiary) core.StorageResult {
	if a.Provider == nil {
		return core.StorageResult{EventID: eventID, State: string(storage.StorageSyncPending), Cause: storage.ErrUnavailable}
	}
	r := a.Provider.Mirror(ctx, storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)})
	return core.StorageResult{
		EventID: eventID, State: string(r.State), Backend: string(r.Backend), Provider: r.Provider,
		Key: r.Key, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, RemoteRev: r.RemoteRev,
		ExpectedRev: r.ExpectedRev, LocalHash: r.LocalHash, EffectiveRemoteHash: r.EffectiveRemoteHash,
		FailureClass: string(r.FailureClass), ConflictSnapshot: r.ConflictSnapshot,
		AlreadyInProgress: r.AlreadyInProgress, Cause: r.Cause,
	}
}

func (a Adapter) Resolve(ctx context.Context, d core.SessionDiary, choice, observedRevision string) core.StorageResult {
	if a.Provider == nil {
		return core.StorageResult{State: string(storage.StorageSyncPending), Cause: storage.ErrUnavailable}
	}
	r := a.Provider.Resolve(ctx, storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)}, storage.Resolution(choice), observedRevision)
	return core.StorageResult{
		State: string(r.State), Backend: string(r.Backend), Provider: r.Provider,
		Key: r.Key, RemoteID: r.RemoteID, RemoteURL: r.RemoteURL, RemoteRev: r.RemoteRev,
		ExpectedRev: r.ExpectedRev, LocalHash: r.LocalHash, EffectiveRemoteHash: r.EffectiveRemoteHash,
		FailureClass: string(r.FailureClass), ConflictSnapshot: r.ConflictSnapshot,
		AlreadyInProgress: r.AlreadyInProgress, Cause: r.Cause,
	}
}

var _ core.StoragePort = Adapter{}
var _ core.EventAwareStoragePort = Adapter{}
