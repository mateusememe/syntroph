package core

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Handler func(context.Context, Event) error
type Subscription struct {
	ID      string
	Type    string
	Handler Handler
}
type Delivery struct {
	EventID  string
	Attempts []HandlerAttempt
}

// EventBus journals an event before synchronously delivering it to subscribers.
// A handler failure is recorded and isolated; other handlers still run.
type EventBus struct {
	journal  Journal
	mu       sync.RWMutex
	handlers map[string][]Subscription
}

func NewEventBus(journal Journal) (*EventBus, error) {
	if journal == nil {
		return nil, fmt.Errorf("journal is required")
	}
	return &EventBus{journal: journal, handlers: make(map[string][]Subscription)}, nil
}
func (b *EventBus) Subscribe(eventType, id string, handler Handler) error {
	if eventType == "" || id == "" || handler == nil {
		return fmt.Errorf("event type, handler id, and handler are required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], Subscription{ID: id, Type: eventType, Handler: handler})
	return nil
}

func (b *EventBus) Publish(ctx context.Context, event Event) (Delivery, error) {
	if err := event.Validate(); err != nil {
		return Delivery{}, err
	}
	if err := b.journal.AppendEvent(ctx, event); err != nil {
		return Delivery{}, err
	}
	b.mu.RLock()
	subs := append([]Subscription(nil), b.handlers[event.Type]...)
	b.mu.RUnlock()
	d := Delivery{EventID: event.EventID}
	for _, sub := range subs {
		a := HandlerAttempt{EventID: event.EventID, SagaID: event.SagaID, HandlerID: sub.ID, AttemptedAt: time.Now().UTC()}
		err := sub.Handler(ctx, event)
		if err != nil {
			a.Outcome = "failed"
			a.Error = err.Error()
		} else {
			a.Outcome = "succeeded"
		}
		if journalErr := b.journal.AppendAttempt(ctx, a); journalErr != nil {
			return d, journalErr
		}
		d.Attempts = append(d.Attempts, a)
	}
	return d, nil
}
