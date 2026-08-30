package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Event is immutable once published. Payload is kept opaque to the Core.
type Event struct {
	EventID       string          `json:"event_id"`
	Type          string          `json:"type"`
	OccurredAt    time.Time       `json:"occurred_at"`
	RepositoryID  string          `json:"repository_id"`
	SagaID        string          `json:"saga_id"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id"`
	SchemaVersion int             `json:"schema_version"`
	Payload       json.RawMessage `json:"payload"`
}

func (e Event) Validate() error {
	if e.EventID == "" || e.Type == "" || e.RepositoryID == "" || e.SagaID == "" || e.CorrelationID == "" || e.SchemaVersion < 1 || len(e.Payload) == 0 {
		return errors.New("event is missing required fields")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("event occurred_at is required")
	}
	return nil
}

type HandlerAttempt struct {
	EventID     string    `json:"event_id"`
	SagaID      string    `json:"saga_id"`
	HandlerID   string    `json:"handler_id"`
	AttemptedAt time.Time `json:"attempted_at"`
	Outcome     string    `json:"outcome"`
	Error       string    `json:"error,omitempty"`
}

func (a HandlerAttempt) Validate() error {
	if a.EventID == "" || a.SagaID == "" || a.HandlerID == "" || a.Outcome == "" || a.AttemptedAt.IsZero() {
		return errors.New("handler attempt is missing required fields")
	}
	return nil
}

func (a HandlerAttempt) Err() error {
	if a.Error == "" {
		return nil
	}
	return fmt.Errorf("handler %s: %s", a.HandlerID, a.Error)
}
