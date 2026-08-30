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
	SagaID       string
	EventID      string
	RepositoryID string
	State        string
	Attempts     []HandlerAttempt
	LastError    string
	NextAction   string
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
		latest := make(map[string]HandlerAttempt)
		for _, record := range records {
			if record.Attempt != nil {
				item.Attempts = append(item.Attempts, *record.Attempt)
				latest[record.Attempt.HandlerID] = *record.Attempt
			}
		}
		for _, record := range records {
			if record.Event != nil {
				item.EventID, item.RepositoryID = record.Event.EventID, record.Event.RepositoryID
				var diary SessionDiary
				if json.Unmarshal(record.Event.Payload, &diary) == nil {
					switch diary.GraphState {
					case GraphResolutionPending:
						item.State, item.NextAction = GraphResolutionPending, "syntroph sync retry --graph"
					case GraphSyncPending:
						item.State, item.NextAction = GraphSyncPending, "syntroph sync retry --graph"
					}
				}
			}
		}
		for _, attempt := range latest {
			if attempt.Outcome == "failed" {
				item.State = "HandlerPending"
				item.LastError = attempt.Error
				item.NextAction = "syntroph sync retry"
			}
		}
		if item.State != "Succeeded" {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SagaID < items[j].SagaID })
	return items, nil
}
