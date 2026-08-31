// Package storageadapter bridges the core-owned diary to the provider package.
package storageadapter

import (
	"context"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

type Adapter struct{ Provider *storage.Mirror }

func (a Adapter) Mirror(ctx context.Context, d core.SessionDiary) core.StorageResult {
	return a.MirrorEvent(ctx, d.SessionID, d)
}
func (a Adapter) MirrorEvent(ctx context.Context, eventID string, d core.SessionDiary) core.StorageResult {
	if a.Provider == nil {
		return core.StorageResult{EventID: eventID, State: string(storage.StorageSyncPending), Cause: storage.ErrUnavailable}
	}
	r := a.Provider.Mirror(ctx, storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)})
	return core.StorageResult{EventID: eventID, State: string(r.State), Backend: string(r.Backend), Key: r.Key, RemoteID: r.RemoteID, RemoteRev: r.RemoteRev, ExpectedRev: r.ExpectedRev, Cause: r.Cause}
}

func (a Adapter) Resolve(ctx context.Context, d core.SessionDiary, choice, observedRevision string) core.StorageResult {
	if a.Provider == nil {
		return core.StorageResult{State: string(storage.StorageSyncPending), Cause: storage.ErrUnavailable}
	}
	r := a.Provider.Resolve(ctx, storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)}, storage.Resolution(choice), observedRevision)
	return core.StorageResult{State: string(r.State), Backend: string(r.Backend), Key: r.Key, RemoteID: r.RemoteID, RemoteRev: r.RemoteRev, ExpectedRev: r.ExpectedRev, Cause: r.Cause}
}

var _ core.StoragePort = Adapter{}
var _ core.EventAwareStoragePort = Adapter{}
