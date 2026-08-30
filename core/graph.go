package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrGraphUnavailable = errors.New("graph port unavailable")
	ErrGraphStale       = errors.New("graph snapshot is stale")
)

const (
	GraphReady              = "ready"
	GraphResolutionPending  = "GraphResolutionPending"
	GraphSyncPending        = "GraphSyncPending"
	ConfidenceAuthoritative = "authoritative"
	ConfidenceHeuristic     = "heuristic"
	ConfidenceUnresolved    = "unresolved"
)

// GraphResolveRequest is deliberately runtime-neutral. Explicit references
// are supplied by the Session Artifact and remain authoritative when found.
type GraphResolveRequest struct {
	RepositoryID string
	CommitSHA    string
	References   []CodeReference
}

type GraphResolution struct {
	References      []CodeReference
	GraphSnapshotID string
}

// GraphPort resolves code references against the active graph snapshot. A
// missing or stale implementation returns an error; SessionCloser converts it
// into Graph Resolution Pending without losing the diary.
type GraphPort interface {
	Resolve(context.Context, GraphResolveRequest) (GraphResolution, error)
}

// GraphPublisher is optional so a read-only GraphPort can still be used. Its
// operation is idempotent by content-addressed snapshot ID.
type GraphPublisher interface {
	Publish(context.Context, GraphSnapshot) (GraphSnapshot, error)
}

type GraphSnapshot struct {
	ID           string          `json:"graph_snapshot_id"`
	RepositoryID string          `json:"repository_id"`
	CommitSHA    string          `json:"commit_sha"`
	CreatedAt    time.Time       `json:"created_at"`
	References   []CodeReference `json:"references"`
}

func (s GraphSnapshot) contentID() (string, error) {
	clone := s
	clone.ID, clone.CreatedAt = "", time.Time{}
	b, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// LocalGraphPort is a deterministic GraphPort adapter. Snapshots are
// immutable files addressed by their content hash; active contains only the
// current pointer and may safely be replaced on a later graph push.
type LocalGraphPort struct{ Root string }

func NewLocalGraphPort(root string) (*LocalGraphPort, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("graph root is required")
	}
	if err := os.MkdirAll(filepath.Join(root, "snapshots"), 0o755); err != nil {
		return nil, err
	}
	return &LocalGraphPort{Root: root}, nil
}

func (p *LocalGraphPort) snapshotPath(id string) string {
	return filepath.Join(p.Root, "snapshots", id+".json")
}

func (p *LocalGraphPort) activePath() string { return filepath.Join(p.Root, "active") }

func (p *LocalGraphPort) Publish(ctx context.Context, snapshot GraphSnapshot) (GraphSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return GraphSnapshot{}, err
	}
	if snapshot.RepositoryID == "" || snapshot.CommitSHA == "" {
		return GraphSnapshot{}, errors.New("graph snapshot repository and commit are required")
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	id, err := snapshot.contentID()
	if err != nil {
		return GraphSnapshot{}, err
	}
	snapshot.ID = id
	b, err := json.Marshal(snapshot)
	if err != nil {
		return GraphSnapshot{}, err
	}
	path := p.snapshotPath(id)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		tmp, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
		if err != nil {
			return GraphSnapshot{}, err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err = tmp.Write(append(b, '\n')); err == nil {
			err = tmp.Sync()
		}
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(name, path)
		}
		if err != nil {
			return GraphSnapshot{}, err
		}
	} else if err != nil {
		return GraphSnapshot{}, err
	}
	tmp, err := os.CreateTemp(p.Root, ".active-*")
	if err != nil {
		return GraphSnapshot{}, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.WriteString(id + "\n"); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, p.activePath())
	}
	return snapshot, err
}

func (p *LocalGraphPort) activeSnapshot() (GraphSnapshot, error) {
	b, err := os.ReadFile(p.activePath())
	if err != nil {
		if os.IsNotExist(err) {
			return GraphSnapshot{}, fmt.Errorf("%w: no active snapshot", ErrGraphUnavailable)
		}
		return GraphSnapshot{}, err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return GraphSnapshot{}, fmt.Errorf("%w: active pointer is empty", ErrGraphUnavailable)
	}
	b, err = os.ReadFile(p.snapshotPath(id))
	if err != nil {
		return GraphSnapshot{}, fmt.Errorf("%w: snapshot %s: %v", ErrGraphUnavailable, id, err)
	}
	var snapshot GraphSnapshot
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return GraphSnapshot{}, err
	}
	return snapshot, nil
}

func (p *LocalGraphPort) Resolve(ctx context.Context, req GraphResolveRequest) (GraphResolution, error) {
	if err := ctx.Err(); err != nil {
		return GraphResolution{}, err
	}
	snapshot, err := p.activeSnapshot()
	if err != nil {
		return GraphResolution{}, err
	}
	if snapshot.RepositoryID != req.RepositoryID || snapshot.CommitSHA != req.CommitSHA {
		return GraphResolution{}, fmt.Errorf("%w: snapshot %s is for %s@%s", ErrGraphStale, snapshot.ID, snapshot.RepositoryID, snapshot.CommitSHA)
	}
	resolved := make([]CodeReference, len(req.References))
	copy(resolved, req.References)
	for i := range resolved {
		if resolved[i].Confidence == "" {
			resolved[i].Confidence = ConfidenceAuthoritative
		}
		found := false
		for _, candidate := range snapshot.References {
			if candidate.Path == resolved[i].Path && (resolved[i].Symbol == "" || candidate.Symbol == resolved[i].Symbol) && (resolved[i].Kind == "" || candidate.Kind == resolved[i].Kind) {
				found = true
				break
			}
		}
		if !found {
			resolved[i].Confidence = ConfidenceUnresolved
		}
		resolved[i].RepositoryID = req.RepositoryID
		resolved[i].CommitSHA = req.CommitSHA
		resolved[i].GraphSnapshotID = snapshot.ID
	}
	return GraphResolution{References: resolved, GraphSnapshotID: snapshot.ID}, nil
}
