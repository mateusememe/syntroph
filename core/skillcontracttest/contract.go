// Package skillcontracttest contains reusable behavioral contracts for
// SkillPort and RuntimePort adapters. It is test support, not a product
// adapter.
package skillcontracttest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/mateusememe/syntroph/core"
)

type SkillPortFixture struct {
	Port            core.SkillPort
	ReadyName       string
	UnsupportedName string
	Runtime         string
}

type SkillPortFactory func(*testing.T) SkillPortFixture

// RunSkillPort verifies the Core behavior every catalog adapter must preserve.
// Later slices can extend this suite without changing adapter-specific tests.
func RunSkillPort(t *testing.T, name string, factory SkillPortFactory) {
	t.Helper()
	t.Run(name+"/prepare_replay_and_intentional_use", func(t *testing.T) {
		fixture := factory(t)
		validateSkillPortFixture(t, fixture)
		request := core.SkillPrepareRequest{
			Name:         fixture.ReadyName,
			InvocationID: "inv-contract-replay",
			Runtime:      fixture.Runtime,
			Arguments:    map[string]any{"fixed_point": "origin/main"},
		}

		first, err := fixture.Port.Prepare(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := fixture.Port.Prepare(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if first.Invocation() != replayed.Invocation() {
			t.Fatalf("technical replay changed invocation: first=%+v replay=%+v", first.Invocation(), replayed.Invocation())
		}
		if first.Runtime() != fixture.Runtime || !reflect.DeepEqual(first.Arguments(), request.Arguments) {
			t.Fatalf("prepared bundle changed runtime-neutral input: runtime=%q arguments=%+v", first.Runtime(), first.Arguments())
		}
		identity := first.PackageIdentity()
		if identity.SourceID == "" || identity.Name == "" || identity.PackageHash == "" || first.BundleHash() == "" {
			t.Fatalf("prepared bundle lacks deterministic identities: package=%+v bundle_hash=%q", identity, first.BundleHash())
		}

		changed := request
		changed.Arguments = map[string]any{"fixed_point": "HEAD~1"}
		if _, err := fixture.Port.Prepare(context.Background(), changed); !errors.Is(err, core.ErrSkillInvocationReplay) {
			t.Fatalf("changed replay error = %v, want ErrSkillInvocationReplay", err)
		}

		request.InvocationID = ""
		intentional, err := fixture.Port.Prepare(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if intentional.InvocationID() == first.InvocationID() {
			t.Fatalf("intentional use reused invocation ID %q", intentional.InvocationID())
		}
		if intentional.BundleHash() != first.BundleHash() {
			t.Fatalf("identical content changed bundle hash: first=%q intentional=%q", first.BundleHash(), intentional.BundleHash())
		}
		secondIntentional, err := fixture.Port.Prepare(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if secondIntentional.InvocationID() == intentional.InvocationID() {
			t.Fatalf("two intentional uses shared invocation ID %q", intentional.InvocationID())
		}
		if secondIntentional.BundleHash() != intentional.BundleHash() {
			t.Fatalf("second intentional use changed bundle hash: first=%q second=%q", intentional.BundleHash(), secondIntentional.BundleHash())
		}
	})

	t.Run(name+"/unsupported_package_is_isolated", func(t *testing.T) {
		fixture := factory(t)
		if fixture.UnsupportedName == "" {
			return
		}
		validateSkillPortFixture(t, fixture)
		if _, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.UnsupportedName}); !errors.Is(err, core.ErrUnsupportedSkill) {
			t.Fatalf("unsupported package error = %v, want ErrUnsupportedSkill", err)
		}
		if _, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.ReadyName, InvocationID: "inv-after-unsupported"}); err != nil {
			t.Fatalf("unsupported package invalidated the ready catalog: %v", err)
		}
	})
}

func validateSkillPortFixture(t *testing.T, fixture SkillPortFixture) {
	t.Helper()
	if fixture.Port == nil || fixture.ReadyName == "" || fixture.Runtime == "" {
		t.Fatal("skill port contract fixture is incomplete")
	}
}

type RuntimePortFixture struct {
	Port     core.RuntimePort
	Bundle   core.SkillBundle
	Received func() (core.SkillBundle, bool)
}

type RuntimePortFactory func(*testing.T) RuntimePortFixture

// RunRuntimePort verifies that a runtime consumes the prepared bundle without
// asking SkillPort to interpret or rewrite its instructions.
func RunRuntimePort(t *testing.T, name string, factory RuntimePortFactory) {
	t.Helper()
	t.Run(name+"/consumes_exact_bundle", func(t *testing.T) {
		fixture := factory(t)
		if fixture.Port == nil || fixture.Received == nil || fixture.Bundle.BundleHash() == "" {
			t.Fatal("runtime port contract fixture is incomplete")
		}
		receipt, err := fixture.Port.PrepareSkill(context.Background(), fixture.Bundle)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.InvocationID != fixture.Bundle.InvocationID() || receipt.BundleHash != fixture.Bundle.BundleHash() || receipt.Runtime != fixture.Bundle.Runtime() {
			t.Fatalf("runtime receipt changed bundle identity: receipt=%+v", receipt)
		}
		received, ok := fixture.Received()
		if !ok || !sameBundle(received, fixture.Bundle) {
			t.Fatalf("runtime did not consume the exact prepared bundle: received=%+v ok=%v", received.Invocation(), ok)
		}
	})
}

func sameBundle(left, right core.SkillBundle) bool {
	return left.Invocation() == right.Invocation() &&
		left.SchemaVersion() == right.SchemaVersion() &&
		left.Description() == right.Description() &&
		left.Instructions() == right.Instructions() &&
		reflect.DeepEqual(left.Assets(), right.Assets()) &&
		reflect.DeepEqual(left.CompatibleRuntimes(), right.CompatibleRuntimes()) &&
		left.Runtime() == right.Runtime() &&
		reflect.DeepEqual(left.Arguments(), right.Arguments())
}

// RecordingRuntime is a deterministic, effect-free RuntimePort fake shared by
// Core and future concrete adapter contract tests.
type RecordingRuntime struct {
	mu       sync.Mutex
	received core.SkillBundle
	hasValue bool
}

func (r *RecordingRuntime) PrepareSkill(ctx context.Context, bundle core.SkillBundle) (core.RuntimeSkillPreparation, error) {
	if err := ctx.Err(); err != nil {
		return core.RuntimeSkillPreparation{}, err
	}
	r.mu.Lock()
	r.received, r.hasValue = bundle, true
	r.mu.Unlock()
	return core.RuntimeSkillPreparation{
		InvocationID: bundle.InvocationID(),
		BundleHash:   bundle.BundleHash(),
		Runtime:      bundle.Runtime(),
	}, nil
}

func (r *RecordingRuntime) LastBundle() (core.SkillBundle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.received, r.hasValue
}

var _ core.RuntimePort = (*RecordingRuntime)(nil)
