package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// ListSagas returns the saga identifiers represented by journal segments.
// Segment names are content hashes, so the identifier is recovered from the
// immutable records rather than inferred from a filename.
func (j *SagaJournal) ListSagas(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(j.dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range bytes.Split(b, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var r JournalRecord
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("journal record: %w", err)
			}
			if r.Event != nil {
				seen[r.Event.SagaID] = true
			}
			if r.Attempt != nil {
				seen[r.Attempt.SagaID] = true
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
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
	a.Error = SafeStorageDiagnostic(a.Error)
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
		// Read-time migration keeps the append-only source untouched. Version
		// zero is the immediately previous event schema used by early MVP
		// journals; derive causal defaults from the immutable envelope.
		if r.Event != nil {
			migrateEvent(r.Event)
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

func migrateEvent(e *Event) {
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
		if e.CorrelationID == "" {
			e.CorrelationID = e.SagaID
		}
		if e.CausationID == "" {
			e.CausationID = ""
		}
	}
}

func safeSegmentName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:]) + ".jsonl"
}
