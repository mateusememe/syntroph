package core

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testEvent() Event {
	return Event{EventID: "evt-1", Type: "session.closed", OccurredAt: time.Now().UTC(), RepositoryID: "repo", SagaID: "saga-1", CorrelationID: "corr-1", SchemaVersion: 1, Payload: json.RawMessage(`{"summary":"done"}`)}
}

func TestEventBusJournalsEventAndIsolatesHandlerFailures(t *testing.T) {
	j, err := NewSagaJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewEventBus(j)
	if err != nil {
		t.Fatal(err)
	}
	var called bool
	if err := b.Subscribe("session.closed", "failing", func(context.Context, Event) error { return errors.New("unavailable") }); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe("session.closed", "healthy", func(context.Context, Event) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	d, err := b.Publish(context.Background(), testEvent())
	if err != nil {
		t.Fatal(err)
	}
	if !called || len(d.Attempts) != 2 || d.Attempts[0].Outcome != "failed" || d.Attempts[1].Outcome != "succeeded" {
		t.Fatalf("unexpected delivery: %+v called=%v", d, called)
	}
	records, err := j.ReadSaga(context.Background(), "saga-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0].Kind != "event" {
		t.Fatalf("unexpected journal records: %+v", records)
	}
}

func TestSagaJournalSurvivesReopenAndKeepsAttemptsSeparate(t *testing.T) {
	dir := t.TempDir()
	e := testEvent()
	j, err := NewSagaJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AppendEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	a := HandlerAttempt{EventID: e.EventID, SagaID: e.SagaID, HandlerID: "h", AttemptedAt: time.Now().UTC(), Outcome: "failed", Error: "timeout"}
	if err := j.AppendAttempt(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	j2, err := NewSagaJournal(filepath.Clean(dir))
	if err != nil {
		t.Fatal(err)
	}
	records, err := j2.ReadSaga(context.Background(), e.SagaID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Attempt.Error != "timeout" {
		t.Fatalf("records not recovered: %+v", records)
	}
}

func TestEventBusRequiresCausalEventBeforeHandler(t *testing.T) {
	j, _ := NewSagaJournal(t.TempDir())
	b, _ := NewEventBus(j)
	seen := false
	_ = b.Subscribe("x", "h", func(context.Context, Event) error {
		records, err := j.ReadSaga(context.Background(), "saga-1")
		if err != nil {
			t.Fatal(err)
		}
		seen = len(records) == 1 && records[0].Kind == "event"
		return nil
	})
	e := testEvent()
	e.Type = "x"
	if _, err := b.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("handler ran before event was journaled")
	}
}

func TestEventAndAttemptValidation(t *testing.T) {
	if err := (Event{}).Validate(); err == nil {
		t.Fatal("expected invalid event")
	}
	if err := (HandlerAttempt{}).Validate(); err == nil {
		t.Fatal("expected invalid attempt")
	}
}
