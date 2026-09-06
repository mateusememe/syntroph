package core

import "github.com/mateusememe/syntroph/core/storagecontract"

func SafeStorageDiagnostic(value string) string { return storagecontract.SafeDiagnostic(value) }

// StorageResultForJournal copies the provider result into its durable form.
// Provider errors remain available through Cause while only bounded, redacted
// diagnostics cross the append-only journal boundary.
func StorageResultForJournal(result StorageResult) StorageResult {
	if result.Error == "" && result.Cause != nil {
		result.Error = result.Cause.Error()
	}
	result.Error = SafeStorageDiagnostic(result.Error)
	return result
}

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

// StorageEventType is the Core-owned mapping from storage state to immutable
// event type. CLI recovery paths use the same mapping as session close.
func StorageEventType(state StorageState) string {
	switch state {
	case StorageMirrored:
		return "storage.sync.succeeded"
	case StorageSyncConflict:
		return "storage.sync.conflict"
	case StoragePrerequisiteMissing:
		return "storage.prerequisite.missing"
	default:
		return "storage.sync.pending"
	}
}
