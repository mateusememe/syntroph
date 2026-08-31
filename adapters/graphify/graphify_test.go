package graphify

import (
	"context"
	"errors"
	"testing"

	"github.com/mateusememe/syntroph/core"
)

type fixtureRunner struct {
	output []byte
	err    error
	name   string
	args   []string
}

func (r *fixtureRunner) Run(_ context.Context, name string, args []string) ([]byte, error) {
	r.name, r.args = name, args
	return r.output, r.err
}

func TestAdapterResolvesFixtureNodes(t *testing.T) {
	runner := &fixtureRunner{output: []byte(`{"graph_snapshot_id":"snap-1","repository_id":"repo","commit_sha":"abc","nodes":[{"file":"core/session.go","name":"SessionCloser","kind":"type"}]}`)}
	a := New("graphify-fixture")
	a.Runner = runner
	got, err := a.Resolve(context.Background(), core.GraphResolveRequest{RepositoryID: "repo", CommitSHA: "abc", References: []core.CodeReference{{Path: "core/session.go", Symbol: "SessionCloser", Kind: "type"}, {Path: "missing.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.GraphSnapshotID != "snap-1" || got.References[0].Confidence != core.ConfidenceAuthoritative || got.References[1].Confidence != core.ConfidenceUnresolved {
		t.Fatalf("unexpected resolution: %#v", got)
	}
	if runner.name != "graphify-fixture" || len(runner.args) == 0 {
		t.Fatalf("graphify command was not invoked: %#v %#v", runner.name, runner.args)
	}
}

func TestAdapterClassifiesUnavailableAndIncompatibleOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *fixtureRunner
		want error
	}{
		{"unavailable", &fixtureRunner{err: errors.New("not found")}, ErrUnavailable},
		{"incompatible", &fixtureRunner{output: []byte(`{"unexpected":true}`)}, ErrIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New("graphify")
			a.Runner = tc.run
			_, err := a.Resolve(context.Background(), core.GraphResolveRequest{RepositoryID: "repo", CommitSHA: "abc"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
}
