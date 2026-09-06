package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

func TestStorageContractsAreCoreCanonicalAliases(t *testing.T) {
	var coreResult StorageResult
	var adapterResult storage.MirrorResult
	coreResult = adapterResult
	adapterResult = coreResult
	_ = adapterResult
}

func TestSagaJournalRedactsAndBoundsProviderDiagnostics(t *testing.T) {
	journal, err := NewSagaJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := "never-journal-this"
	message := "git https://alice:" + secret + "@github.com/acme/repo token=" + secret + " " + strings.Repeat("x", 5000)
	attempt := HandlerAttempt{EventID: "event", SagaID: "saga", HandlerID: "storage", AttemptedAt: time.Now().UTC(), Outcome: "failed", Error: message}
	if err := journal.AppendAttempt(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	records, err := journal.ReadSaga(context.Background(), "saga")
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	got := records[0].Attempt.Error
	if strings.Contains(got, secret) || len(got) > 2070 || !strings.Contains(got, "[redacted]") || !strings.Contains(got, "[truncated]") {
		t.Fatalf("unsafe diagnostic persisted: length=%d value=%q", len(got), got)
	}
}

func TestStorageResultForJournalRedactsCauseAndStorageEventTypeIsCanonical(t *testing.T) {
	secret := "journal-secret"
	result := StorageResultForJournal(StorageResult{State: StorageSyncPending, Cause: errors.New("token=" + secret)})
	if result.Error == "" || strings.Contains(result.Error, secret) || !strings.Contains(result.Error, "[redacted]") {
		t.Fatalf("journal result error = %q", result.Error)
	}
	for state, want := range map[StorageState]string{
		StorageMirrored: "storage.sync.succeeded", StorageSyncConflict: "storage.sync.conflict",
		StoragePrerequisiteMissing: "storage.prerequisite.missing", StorageSyncPending: "storage.sync.pending",
	} {
		if got := StorageEventType(state); got != want {
			t.Errorf("StorageEventType(%q) = %q, want %q", state, got, want)
		}
	}
}
