package core

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type JournalRecord struct {
	Kind    string          `json:"kind"`
	Event   *Event          `json:"event,omitempty"`
	Attempt *HandlerAttempt `json:"attempt,omitempty"`
}

type Journal interface {
	AppendEvent(context.Context, Event) error
	AppendAttempt(context.Context, HandlerAttempt) error
	ReadSaga(context.Context, string) ([]JournalRecord, error)
}

// SagaJournal stores one append-only JSONL segment per saga.
type SagaJournal struct {
	dir string
	mu  sync.Mutex
}

func NewSagaJournal(dir string) (*SagaJournal, error) {
	if dir == "" {
		return nil, errors.New("journal directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &SagaJournal{dir: dir}, nil
}

func (j *SagaJournal) AppendEvent(ctx context.Context, e Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	return j.append(ctx, e.SagaID, JournalRecord{Kind: "event", Event: &e})
}
func (j *SagaJournal) AppendAttempt(ctx context.Context, a HandlerAttempt) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return j.append(ctx, a.SagaID, JournalRecord{Kind: "attempt", Attempt: &a})
}

func (j *SagaJournal) append(ctx context.Context, sagaID string, record JournalRecord) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(j.dir, safeSegmentName(sagaID)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

func (j *SagaJournal) ReadSaga(ctx context.Context, sagaID string) ([]JournalRecord, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	f, err := os.Open(filepath.Join(j.dir, safeSegmentName(sagaID)))
	if errors.Is(err, os.ErrNotExist) {
		return []JournalRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []JournalRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var r JournalRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("journal record: %w", err)
		}
		if r.Kind != "event" && r.Kind != "attempt" {
			return nil, fmt.Errorf("journal record: unknown kind %q", r.Kind)
		}
		out = append(out, r)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func safeSegmentName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:]) + ".jsonl"
}
