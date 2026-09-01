// Package contracttest contains the shared behavioral contract for every
// Syntroph remote-storage provider. It is test support, not a production port.
package contracttest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/adapters/storageadapter"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

type Port interface {
	Mirror(context.Context, storage.SessionDiary) storage.MirrorResult
	Status(context.Context, storage.SessionDiary) storage.MirrorResult
	Resolve(context.Context, storage.SessionDiary, storage.Resolution, string) storage.MirrorResult
}

// Fixture connects the shared assertions to deterministic transport state.
// MutateRemote must change the existing mirror without going through Syntroph.
type Fixture struct {
	Port         Port
	Bindings     *storage.BindingStore
	Backend      storage.Backend
	Provider     string
	CreateCount  func() int
	MutateRemote func(*testing.T, storage.MirrorResult, string)
}

type Factory func(*testing.T, string) Fixture

// Run executes the same observable contract against a provider factory.
func Run(t *testing.T, name string, factory Factory) {
	t.Helper()
	t.Run(name+"/success_binding_replay_reconstruction_conflict_resolution", func(t *testing.T) {
		root := t.TempDir()
		fixture := factory(t, filepath.Join(root, "storage"))
		validateFixture(t, fixture)

		diary, journal := closeSession(t, root, fixture.Port)
		local, ok, err := (core.LocalMemoryStore{Root: filepath.Join(root, "memory")}).FindByIdempotencyKey(context.Background(), diary.IdempotencyKey)
		if err != nil || !ok || core.RenderSessionDiary(local) == "" {
			t.Fatalf("canonical local diary is not durable: ok=%v err=%v", ok, err)
		}
		if pending, err := core.InspectRecovery(context.Background(), journal); err != nil || len(pending) != 0 {
			t.Fatalf("successful mirror remains recoverable: items=%+v err=%v", pending, err)
		}

		providerDiary := toStorageDiary(diary)
		binding := assertBinding(t, fixture, providerDiary.Key())
		if binding.LocalHash != hash(providerDiary.Content) || binding.EffectiveRemoteHash == "" {
			t.Fatalf("binding hashes do not describe successful mirror: %+v", binding)
		}
		created := fixture.CreateCount()
		replayed := fixture.Port.Mirror(context.Background(), providerDiary)
		if replayed.State != storage.Mirrored || replayed.Key != providerDiary.Key() || fixture.CreateCount() != created {
			t.Fatalf("idempotent replay duplicated remote effect: result=%+v creates=%d want=%d", replayed, fixture.CreateCount(), created)
		}

		bindingPath, err := fixture.Bindings.Path(providerDiary.Key())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(bindingPath); err != nil {
			t.Fatal(err)
		}
		reconstructed := fixture.Port.Mirror(context.Background(), providerDiary)
		if reconstructed.State != storage.Mirrored || fixture.CreateCount() != created {
			t.Fatalf("lost binding reconstruction duplicated mirror: result=%+v creates=%d want=%d", reconstructed, fixture.CreateCount(), created)
		}
		assertBinding(t, fixture, providerDiary.Key())

		fixture.MutateRemote(t, reconstructed, "human remote edit\n")
		conflict := fixture.Port.Mirror(context.Background(), providerDiary)
		if conflict.State != storage.StorageSyncConflict || conflict.FailureClass != storage.FailureConflict || conflict.ConflictSnapshot == "" || !errors.Is(conflict.Cause, storage.ErrConflict) {
			t.Fatalf("remote divergence lacks conflict evidence: %+v", conflict)
		}
		info, err := os.Stat(conflict.ConflictSnapshot)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("conflict snapshot is not private: mode=%v err=%v", mode(info), err)
		}
		resolved := fixture.Port.Resolve(context.Background(), providerDiary, storage.KeepLocal, conflict.RemoteRev)
		if resolved.State != storage.Mirrored || resolved.RemoteRev == "" || resolved.RemoteRev == conflict.RemoteRev {
			t.Fatalf("explicit keep-local resolution failed: conflict=%+v resolved=%+v", conflict, resolved)
		}
		assertBinding(t, fixture, providerDiary.Key())
	})

	for _, tc := range []struct {
		name  string
		state storage.MirrorState
		class storage.FailureClass
		err   error
		next  string
	}{
		{name: "transient_pending_and_explicit_retry", state: storage.StorageSyncPending, class: storage.FailureTransient, err: storage.ErrUnavailable, next: "syntroph sync retry --storage"},
		{name: "prerequisite_evidence", state: storage.StoragePrerequisiteMissing, class: storage.FailurePrerequisite, err: storage.ErrPrerequisiteMissing, next: "syntroph doctor storage"},
	} {
		t.Run(name+"/"+tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixture := factory(t, filepath.Join(root, "storage"))
			validateFixture(t, fixture)
			fault := &faultPort{delegate: fixture.Port, backend: fixture.Backend, provider: fixture.Provider, state: tc.state, class: tc.class, cause: tc.err}
			diary, journal := closeSession(t, root, fault)
			if _, ok, err := (core.LocalMemoryStore{Root: filepath.Join(root, "memory")}).FindByIdempotencyKey(context.Background(), diary.IdempotencyKey); err != nil || !ok {
				t.Fatalf("remote failure lost canonical diary: ok=%v err=%v", ok, err)
			}
			items, err := core.InspectRecovery(context.Background(), journal)
			if err != nil || len(items) != 1 || items[0].State != string(tc.state) || items[0].Provider != fixture.Provider || !strings.Contains(items[0].NextAction, tc.next) {
				t.Fatalf("recovery evidence mismatch: items=%+v err=%v", items, err)
			}
			if _, ok, err := fixture.Bindings.Load(context.Background(), diary.IdempotencyKey); err != nil || ok {
				t.Fatalf("failed effect created binding: ok=%v err=%v", ok, err)
			}
			if tc.state == storage.StorageSyncPending {
				fault.clear()
				result := (storageadapter.Adapter{Provider: fault}).MirrorEvent(context.Background(), diary.SessionID+":storage-retry", diary)
				if result.State != string(storage.Mirrored) || result.Key != diary.IdempotencyKey || result.Provider != fixture.Provider {
					t.Fatalf("explicit retry changed mirror identity: %+v", result)
				}
				assertBinding(t, fixture, diary.IdempotencyKey)
			}
		})
	}
}

func validateFixture(t *testing.T, fixture Fixture) {
	t.Helper()
	if fixture.Port == nil || fixture.Bindings == nil || fixture.Backend == "" || fixture.Provider == "" || fixture.CreateCount == nil || fixture.MutateRemote == nil {
		t.Fatal("storage contract fixture is incomplete")
	}
}

func assertBinding(t *testing.T, fixture Fixture, key string) storage.RemoteBinding {
	t.Helper()
	binding, ok, err := fixture.Bindings.Load(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("remote binding missing: ok=%v err=%v", ok, err)
	}
	if binding.Backend != fixture.Backend || binding.Provider != fixture.Provider || binding.RemoteID == "" || binding.URL == "" || binding.IdempotencyKey != key {
		t.Fatalf("remote binding identity mismatch: %+v", binding)
	}
	if err := binding.RemoteRevision.Validate(); err != nil {
		t.Fatalf("remote binding revision is not provider-typed: %q: %v", binding.RemoteRevision, err)
	}
	return binding
}

type readyGraph struct{}

func (readyGraph) Resolve(_ context.Context, request core.GraphResolveRequest) (core.GraphResolution, error) {
	return core.GraphResolution{References: request.References, GraphSnapshotID: "contract-snapshot"}, nil
}

func closeSession(t *testing.T, root string, provider Port) (core.SessionDiary, *core.SagaJournal) {
	t.Helper()
	journal, err := core.NewSagaJournal(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	bus, err := core.NewEventBus(journal)
	if err != nil {
		t.Fatal(err)
	}
	closer := core.SessionCloser{
		Memory: core.LocalMemoryStore{Root: filepath.Join(root, "memory")}, Graph: readyGraph{}, Bus: bus,
		Storage: storageadapter.Adapter{Provider: provider, Backend: "issues", ProviderID: "contract"},
	}
	diary, _, err := closer.Close(context.Background(), core.SessionCloseRequest{
		RepositoryID: "github.com/mateusememe/syntroph", CommitSHA: "contract-sha", Author: "contract-test",
		Now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Format: core.ArtifactJSON,
		Artifact: []byte(`{"title":"Storage contract","summary":"provider behavioral contract"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return diary, journal
}

func toStorageDiary(d core.SessionDiary) storage.SessionDiary {
	return storage.SessionDiary{SessionID: d.SessionID, RepositoryID: d.RepositoryID, CommitSHA: d.CommitSHA, ArtifactHash: d.ArtifactHash, Content: core.RenderSessionDiary(d)}
}

type faultPort struct {
	delegate Port
	backend  storage.Backend
	provider string
	state    storage.MirrorState
	class    storage.FailureClass
	cause    error
}

func (p *faultPort) clear() { p.state, p.class, p.cause = "", "", nil }

func (p *faultPort) Mirror(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	if p.cause != nil {
		return storage.MirrorResult{State: p.state, Backend: p.backend, Provider: p.provider, Key: diary.Key(), FailureClass: p.class, Cause: p.cause}
	}
	return p.delegate.Mirror(ctx, diary)
}

func (p *faultPort) Status(ctx context.Context, diary storage.SessionDiary) storage.MirrorResult {
	return p.delegate.Status(ctx, diary)
}

func (p *faultPort) Resolve(ctx context.Context, diary storage.SessionDiary, choice storage.Resolution, observed string) storage.MirrorResult {
	return p.delegate.Resolve(ctx, diary, choice, observed)
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func mode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
