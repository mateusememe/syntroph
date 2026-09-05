// Package storageadapter bridges the core-owned diary to the provider package.
package storageadapter

import (
	"context"
	"errors"

	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

type Provider interface {
	Mirror(context.Context, storage.SessionDiary) storage.MirrorResult
	Resolve(context.Context, storage.SessionDiary, storage.Resolution, string) storage.MirrorResult
}

type RecoveryProvider interface {
	Recover(context.Context, storage.SessionDiary) storage.MirrorResult
}

type Adapter struct {
	Provider   Provider
	Backend    string
	ProviderID string
	Check      func(context.Context) core.StoragePreflightResult
}

func (a Adapter) Preflight(ctx context.Context) core.StoragePreflightResult {
	if a.Check != nil {
		return a.Check(ctx)
	}
	if a.Provider != nil {
		return core.StoragePreflightResult{Enabled: true, Ready: true}
	}
	return core.StoragePreflightResult{Enabled: true, Cause: storage.ErrPrerequisiteMissing}
}

func (a Adapter) Mirror(ctx context.Context, d core.SessionDiary) core.StorageResult {
	return a.MirrorEvent(ctx, d.SessionID, d)
}
func (a Adapter) MirrorEvent(ctx context.Context, eventID string, d core.SessionDiary) core.StorageResult {
	if a.Provider == nil {
		return core.StorageResult{EventID: eventID, State: storage.StorageSyncPending, Backend: core.StorageBackend(a.Backend), Provider: a.ProviderID, Cause: storage.ErrUnavailable}
	}
	r := a.Provider.Mirror(ctx, storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)})
	r.EventID = eventID
	return r
}

func (a Adapter) Resolve(ctx context.Context, d core.SessionDiary, choice, observedRevision string) core.StorageResult {
	if a.Provider == nil {
		return core.StorageResult{State: storage.StorageSyncPending, Backend: core.StorageBackend(a.Backend), Provider: a.ProviderID, Cause: storage.ErrUnavailable}
	}
	r := a.Provider.Resolve(ctx, storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)}, storage.Resolution(choice), observedRevision)
	return r
}

func (a Adapter) RecoverEvent(ctx context.Context, eventID string, d core.SessionDiary) core.StorageResult {
	provider, ok := a.Provider.(RecoveryProvider)
	if !ok || provider == nil {
		return core.StorageResult{EventID: eventID, State: storage.StorageSyncPending, Backend: core.StorageBackend(a.Backend), Provider: a.ProviderID, Key: d.IdempotencyKey, FailureClass: storage.FailureTransient, Cause: errors.New("configured storage provider does not support explicit recovery")}
	}
	r := provider.Recover(ctx, storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)})
	r.EventID = eventID
	return r
}

func (a Adapter) BeginCommand() {
	if lifecycle, ok := a.Provider.(interface{ BeginCommand() }); ok {
		lifecycle.BeginCommand()
	}
}

func (a Adapter) EndCommand() error {
	if lifecycle, ok := a.Provider.(interface{ EndCommand() error }); ok {
		return lifecycle.EndCommand()
	}
	return nil
}

var _ core.StoragePort = Adapter{}
var _ core.EventAwareStoragePort = Adapter{}
var _ core.StorageRecoveryPort = Adapter{}
var _ core.StoragePreflightPort = Adapter{}
