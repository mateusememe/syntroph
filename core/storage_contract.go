package core

import "github.com/mateusememe/syntroph/core/storagecontract"

func SafeStorageDiagnostic(value string) string { return storagecontract.SafeDiagnostic(value) }

// StorageBackend identifies the remote representation while remaining
// independent from any concrete transport.
type StorageBackend = storagecontract.Backend

const (
	StorageBackendWiki   = storagecontract.BackendWiki
	StorageBackendIssues = storagecontract.BackendIssues
)

// StorageState is the provider-neutral outcome of a remote mirror operation.
type StorageState = storagecontract.State

const (
	StorageMirrored            = storagecontract.Mirrored
	StorageSyncPending         = storagecontract.SyncPending
	StorageSyncConflict        = storagecontract.SyncConflict
	StoragePrerequisiteMissing = storagecontract.PrerequisiteMissing
)

type StorageFailureClass = storagecontract.FailureClass

const (
	StorageFailureTransient         = storagecontract.FailureTransient
	StorageFailureConflict          = storagecontract.FailureConflict
	StorageFailurePrerequisite      = storagecontract.FailurePrerequisite
	StorageFailureAlreadyInProgress = storagecontract.FailureAlreadyInProgress
)

// RemoteRevision is opaque to Core. Providers validate their own typed form.
type RemoteRevision = storagecontract.RemoteRevision

// RemoteDiary is the immutable, provider-neutral document passed across the
// StoragePort boundary. It is deliberately separate from transport payloads.
type RemoteDiary = storagecontract.Diary

// StorageResult is the canonical provider-neutral result. RemoteContent is
// transient evidence: adapters persist it privately before journaling.
type StorageResult = storagecontract.Result

// RemoteBinding is the current operational projection. The journal remains
// the durable history from which it can be reconstructed.
type RemoteBinding = storagecontract.Binding
