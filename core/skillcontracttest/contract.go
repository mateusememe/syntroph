// Package skillcontracttest contains reusable behavioral contracts for
// SkillPort and RuntimePort adapters. It is test support, not a product
// adapter.
package skillcontracttest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mateusememe/syntroph/core"
)

type SkillPortFixture struct {
	Port            core.SkillPort
	ReadyName       string
	UnsupportedName string
	Runtime         string
	// AmbiguousName is an unqualified name matched by two or more catalog
	// entries, proving Prepare rejects it instead of silently picking one.
	// Optional: the ambiguity subtest skips when this is empty.
	AmbiguousName string
	// AliasName resolves, through the fixture's configured aliases, to
	// ReadyName. Optional: the alias subtest skips when this is empty.
	AliasName string
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

	t.Run(name+"/deterministic_identities", func(t *testing.T) {
		left := factory(t)
		right := factory(t)
		validateSkillPortFixture(t, left)
		validateSkillPortFixture(t, right)
		leftBundle, err := left.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: left.ReadyName, InvocationID: "inv-contract-identity-left", Runtime: left.Runtime})
		if err != nil {
			t.Fatal(err)
		}
		rightBundle, err := right.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: right.ReadyName, InvocationID: "inv-contract-identity-right", Runtime: right.Runtime})
		if err != nil {
			t.Fatal(err)
		}
		if leftBundle.PackageIdentity() != rightBundle.PackageIdentity() {
			t.Fatalf("independently constructed catalogs disagree on package identity: left=%+v right=%+v", leftBundle.PackageIdentity(), rightBundle.PackageIdentity())
		}
		if leftBundle.BundleHash() != rightBundle.BundleHash() {
			t.Fatalf("independently constructed catalogs disagree on bundle hash: left=%q right=%q", leftBundle.BundleHash(), rightBundle.BundleHash())
		}
	})

	t.Run(name+"/unique_unqualified_name_resolves", func(t *testing.T) {
		fixture := factory(t)
		validateSkillPortFixture(t, fixture)
		segments := strings.SplitN(fixture.ReadyName, "/", 2)
		if len(segments) != 2 {
			t.Fatalf("ReadyName %q is not a qualified source_id/name identity", fixture.ReadyName)
		}
		unqualified := segments[1]
		qualified, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.ReadyName, InvocationID: "inv-contract-qualified", Runtime: fixture.Runtime})
		if err != nil {
			t.Fatal(err)
		}
		byUnqualified, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: unqualified, InvocationID: "inv-contract-unqualified", Runtime: fixture.Runtime})
		if err != nil {
			t.Fatalf("unique unqualified name %q did not resolve: %v", unqualified, err)
		}
		if qualified.PackageIdentity() != byUnqualified.PackageIdentity() {
			t.Fatalf("unqualified resolution disagreed with qualified: qualified=%+v unqualified=%+v", qualified.PackageIdentity(), byUnqualified.PackageIdentity())
		}
	})

	t.Run(name+"/ambiguous_unqualified_name_is_rejected", func(t *testing.T) {
		fixture := factory(t)
		if fixture.AmbiguousName == "" {
			return
		}
		validateSkillPortFixture(t, fixture)
		if _, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.AmbiguousName}); !errors.Is(err, core.ErrSkillNameAmbiguous) {
			t.Fatalf("ambiguous name error = %v, want ErrSkillNameAmbiguous", err)
		}
	})

	t.Run(name+"/alias_resolves_to_target", func(t *testing.T) {
		fixture := factory(t)
		if fixture.AliasName == "" {
			return
		}
		validateSkillPortFixture(t, fixture)
		target, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.ReadyName, InvocationID: "inv-contract-alias-target", Runtime: fixture.Runtime})
		if err != nil {
			t.Fatal(err)
		}
		byAlias, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.AliasName, InvocationID: "inv-contract-alias", Runtime: fixture.Runtime})
		if err != nil {
			t.Fatalf("alias %q did not resolve: %v", fixture.AliasName, err)
		}
		if target.PackageIdentity() != byAlias.PackageIdentity() {
			t.Fatalf("alias resolution disagreed with its target: target=%+v alias=%+v", target.PackageIdentity(), byAlias.PackageIdentity())
		}
	})

	t.Run(name+"/incompatible_runtime_is_rejected", func(t *testing.T) {
		fixture := factory(t)
		validateSkillPortFixture(t, fixture)
		baseline, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.ReadyName, InvocationID: "inv-contract-runtime-baseline", Runtime: fixture.Runtime})
		if err != nil {
			t.Fatal(err)
		}
		compatible := baseline.CompatibleRuntimes()
		if len(compatible) == 0 {
			// The Ready package accepts any runtime; incompatibility has
			// nothing to assert for this fixture.
			return
		}
		const incompatibleRuntime = "syntroph-contract-incompatible-runtime"
		for _, runtime := range compatible {
			if runtime == incompatibleRuntime {
				t.Fatalf("fixture's compatible runtimes unexpectedly include the contract's incompatible probe %q", incompatibleRuntime)
			}
		}
		if _, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.ReadyName, Runtime: incompatibleRuntime}); !errors.Is(err, core.ErrSkillRuntimeIncompatible) {
			t.Fatalf("incompatible runtime error = %v, want ErrSkillRuntimeIncompatible", err)
		}
	})

	t.Run(name+"/invalid_arguments_are_rejected", func(t *testing.T) {
		fixture := factory(t)
		validateSkillPortFixture(t, fixture)
		arguments := map[string]any{"__contract_test_undeclared_field__": true}
		if _, err := fixture.Port.Prepare(context.Background(), core.SkillPrepareRequest{Name: fixture.ReadyName, Runtime: fixture.Runtime, Arguments: arguments}); !errors.Is(err, core.ErrSkillArgumentsInvalid) {
			t.Fatalf("invalid arguments error = %v, want ErrSkillArgumentsInvalid", err)
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
