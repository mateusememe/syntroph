// Package graphify provides an optional GraphPort adapter for an installed
// Graphify CLI. The core does not import this package unless the adapter is
// explicitly configured.
package graphify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mateusememe/syntroph/core"
)

var (
	ErrUnavailable  = errors.New("graphify executable unavailable")
	ErrIncompatible = errors.New("graphify output incompatible")
)

// CommandRunner is the seam used by the adapter. It makes the integration
// deterministic in tests while ExecRunner exercises the real CLI.
type CommandRunner interface {
	Run(context.Context, string, []string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Adapter invokes Graphify's machine-readable export command. Graphify is
// deliberately discovered at runtime, so it remains an optional dependency.
// The command contract is documented in adapters/graphify/README.md.
type Adapter struct {
	Executable string
	Runner     CommandRunner
}

func New(executable string) *Adapter {
	if strings.TrimSpace(executable) == "" {
		executable = "graphify"
	}
	return &Adapter{Executable: executable, Runner: ExecRunner{}}
}

type response struct {
	GraphSnapshotID string               `json:"graph_snapshot_id"`
	RepositoryID    string               `json:"repository_id"`
	CommitSHA       string               `json:"commit_sha"`
	References      []core.CodeReference `json:"references"`
	Nodes           []node               `json:"nodes"`
}

type node struct {
	Path   string `json:"path"`
	File   string `json:"file"`
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

func (a *Adapter) Resolve(ctx context.Context, req core.GraphResolveRequest) (core.GraphResolution, error) {
	if a == nil || a.Runner == nil {
		return core.GraphResolution{}, fmt.Errorf("%w: adapter is not configured", ErrUnavailable)
	}
	if strings.TrimSpace(req.RepositoryID) == "" || strings.TrimSpace(req.CommitSHA) == "" {
		return core.GraphResolution{}, fmt.Errorf("%w: repository and commit are required", ErrIncompatible)
	}
	args := []string{"graph", "export", "--format", "json", "--repository", req.RepositoryID, "--commit", req.CommitSHA}
	b, err := a.Runner.Run(ctx, a.Executable, args)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return core.GraphResolution{}, ctx.Err()
		}
		return core.GraphResolution{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var out response
	if err := json.Unmarshal(b, &out); err != nil {
		return core.GraphResolution{}, fmt.Errorf("%w: invalid JSON: %v", ErrIncompatible, err)
	}
	if out.RepositoryID != "" && out.RepositoryID != req.RepositoryID || out.CommitSHA != "" && out.CommitSHA != req.CommitSHA {
		return core.GraphResolution{}, fmt.Errorf("%w: snapshot identity does not match request", ErrIncompatible)
	}
	if out.RepositoryID == "" {
		out.RepositoryID = req.RepositoryID
	}
	if out.CommitSHA == "" {
		out.CommitSHA = req.CommitSHA
	}
	if len(out.References) == 0 && len(out.Nodes) == 0 {
		return core.GraphResolution{}, fmt.Errorf("%w: response has no references or nodes", ErrIncompatible)
	}
	available := out.References
	for _, n := range out.Nodes {
		path := n.Path
		if path == "" {
			path = n.File
		}
		symbol := n.Symbol
		if symbol == "" {
			symbol = n.Name
		}
		available = append(available, core.CodeReference{Path: path, Symbol: symbol, Kind: n.Kind})
	}
	resolved := make([]core.CodeReference, len(req.References))
	copy(resolved, req.References)
	for i := range resolved {
		resolved[i].RepositoryID, resolved[i].CommitSHA = req.RepositoryID, req.CommitSHA
		resolved[i].Confidence = core.ConfidenceUnresolved
		for _, candidate := range available {
			if candidate.Path == resolved[i].Path && (resolved[i].Symbol == "" || candidate.Symbol == resolved[i].Symbol) && (resolved[i].Kind == "" || candidate.Kind == resolved[i].Kind) {
				resolved[i].Confidence = core.ConfidenceAuthoritative
				break
			}
		}
	}
	return core.GraphResolution{References: resolved, GraphSnapshotID: out.GraphSnapshotID}, nil
}
