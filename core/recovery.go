package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// RecoveryItem is the durable, read-only projection used by CLI and dashboard
// clients. Inspecting recovery never invokes a handler or external adapter.
type RecoveryItem struct {
	SagaID           string
	EventID          string
	RepositoryID     string
	IdempotencyKey   string
	State            string
	Backend          string
	Provider         string
	RemoteID         string
	RemoteURL        string
	RemoteRevision   string
	FailureClass     string
	ConflictSnapshot string
	Attempts         []HandlerAttempt
	LastError        string
	NextAction       string
}

// InspectRecovery derives pending obligations from the journal. A failed
// attempt, or a diary carrying a pending graph state, is surfaced as pending.
func InspectRecovery(ctx context.Context, journal *SagaJournal) ([]RecoveryItem, error) {
	if journal == nil {
		return nil, fmt.Errorf("journal is required")
	}
	sagas, err := journal.ListSagas(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]RecoveryItem, 0)
	for _, saga := range sagas {
		records, err := journal.ReadSaga(ctx, saga)
		if err != nil {
			return nil, err
		}
		item := RecoveryItem{SagaID: saga, State: "Succeeded", NextAction: "none"}
		// A handler may be delivered more than once. Only the latest attempt
		// for each handler represents its current obligation; the journal still
		// retains every attempt for audit and recovery explanations.
		graphState := ""
		var storageResult *StorageResult
		storageResolved := false
		for _, record := range records {
			if record.Attempt != nil {
				item.Attempts = append(item.Attempts, *record.Attempt)
				continue
			}
			if record.Event != nil {
				item.EventID, item.RepositoryID = record.Event.EventID, record.Event.RepositoryID
				var diary SessionDiary
				if record.Event.Type == "session.closed" && json.Unmarshal(record.Event.Payload, &diary) == nil {
					item.IdempotencyKey = diary.IdempotencyKey
					switch diary.GraphState {
					case GraphResolutionPending:
						graphState = GraphResolutionPending
					case GraphSyncPending:
						graphState = GraphSyncPending
					}
				}
				switch record.Event.Type {
				case "storage.sync.requested":
					var result StorageResult
					if json.Unmarshal(record.Event.Payload, &result) == nil {
						if result.State == "" {
							result.State = StorageSyncPending
						}
						storageResult, storageResolved = &result, false
					}
				case "storage.sync.succeeded", "storage.sync.resolved":
					storageResolved = true
					var result StorageResult
					if json.Unmarshal(record.Event.Payload, &result) == nil && result.Key != "" {
						storageResult = &result
					}
				case "storage.sync.pending", "storage.sync.conflict", "storage.prerequisite.missing":
					var result StorageResult
					if json.Unmarshal(record.Event.Payload, &result) == nil {
						storageResult, storageResolved = &result, false
					}
				}
			}
		}

		if storageResult != nil && !storageResolved {
			applyStorageRecovery(&item, *storageResult)
		} else {
			handlerPending := false
			seenHandlers := make(map[string]bool)
			for i := len(item.Attempts) - 1; i >= 0; i-- {
				attempt := item.Attempts[i]
				if seenHandlers[attempt.HandlerID] {
					continue
				}
				seenHandlers[attempt.HandlerID] = true
				if attempt.HandlerID != "storage-mirror" && attempt.Outcome == "failed" {
					item.State = "HandlerPending"
					item.LastError = attempt.Error
					item.NextAction = "syntroph sync retry"
					handlerPending = true
					break
				}
			}
			if !handlerPending && graphState != "" {
				item.State, item.NextAction = graphState, "syntroph sync retry --graph"
			}
		}
		if item.State != "Succeeded" {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SagaID < items[j].SagaID })
	return items, nil
}

func applyStorageRecovery(item *RecoveryItem, result StorageResult) {
	item.State = string(result.State)
	item.IdempotencyKey = firstNonEmpty(result.Key, item.IdempotencyKey)
	item.Backend, item.Provider = string(result.Backend), result.Provider
	item.RemoteID, item.RemoteURL, item.RemoteRevision = result.RemoteID, result.RemoteURL, result.RemoteRev
	item.FailureClass, item.ConflictSnapshot = string(result.FailureClass), result.ConflictSnapshot
	item.LastError = firstNonEmpty(result.Error, errorString(result.Cause))
	switch result.State {
	case "StorageSyncConflict":
		item.NextAction = fmt.Sprintf("syntroph sync resolve %s --keep-local|--keep-remote", item.SagaID)
	case "StoragePrerequisiteMissing":
		item.NextAction = "syntroph doctor storage"
	default:
		if result.AlreadyInProgress {
			item.NextAction = fmt.Sprintf("syntroph sync recovery --clear-lock %s", item.IdempotencyKey)
		} else {
			item.NextAction = "syntroph sync retry --storage"
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
