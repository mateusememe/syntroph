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
	"sort"
	"strings"
	"time"
)

// CodeReference is supplied by a runtime. Resolution by GraphPort is a later
// step; preserving the declaration here keeps the diary useful offline.
type CodeReference struct {
	RepositoryID    string `json:"repository_id" yaml:"repository_id"`
	CommitSHA       string `json:"commit_sha" yaml:"commit_sha"`
	Path            string `json:"path" yaml:"path"`
	Symbol          string `json:"symbol,omitempty" yaml:"symbol,omitempty"`
	Kind            string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Confidence      string `json:"confidence" yaml:"confidence"`
	GraphSnapshotID string `json:"graph_snapshot_id,omitempty" yaml:"graph_snapshot_id,omitempty"`
}

// SessionArtifact is the runtime-neutral input to session close.
type SessionArtifact struct {
	Title          string          `json:"title"`
	Summary        string          `json:"summary"`
	Decisions      []string        `json:"decisions,omitempty"`
	Lessons        []string        `json:"lessons,omitempty"`
	CodeReferences []CodeReference `json:"code_references,omitempty"`
	Related        []string        `json:"related,omitempty"`
}

type ArtifactFormat string

const (
	ArtifactJSON     ArtifactFormat = "json"
	ArtifactMarkdown ArtifactFormat = "markdown"
)

func ParseSessionArtifact(data []byte, format ArtifactFormat) (SessionArtifact, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return SessionArtifact{}, errors.New("session artifact is empty")
	}
	if format == ArtifactJSON || (format == "" && strings.HasPrefix(strings.TrimSpace(string(data)), "{")) {
		var a SessionArtifact
		if err := json.Unmarshal(data, &a); err != nil {
			return SessionArtifact{}, fmt.Errorf("parse session artifact JSON: %w", err)
		}
		return validateArtifact(a)
	}
	return parseMarkdownArtifact(string(data))
}

func ManualSessionArtifact(summary string) (SessionArtifact, error) {
	return validateArtifact(SessionArtifact{Title: "Session diary", Summary: strings.TrimSpace(summary)})
}

func validateArtifact(a SessionArtifact) (SessionArtifact, error) {
	a.Title = strings.TrimSpace(a.Title)
	a.Summary = strings.TrimSpace(a.Summary)
	if a.Title == "" {
		a.Title = "Session diary"
	}
	if a.Summary == "" {
		return SessionArtifact{}, errors.New("session artifact summary is required")
	}
	for i := range a.CodeReferences {
		a.CodeReferences[i].Path = strings.TrimSpace(a.CodeReferences[i].Path)
		if a.CodeReferences[i].Confidence == "" {
			a.CodeReferences[i].Confidence = "authoritative"
		}
		if a.CodeReferences[i].Path == "" {
			return SessionArtifact{}, errors.New("code reference path is required")
		}
	}
	return a, nil
}

func parseMarkdownArtifact(markdown string) (SessionArtifact, error) {
	a := SessionArtifact{}
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	section := "summary"
	var body []string
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "---" {
			continue
		}
		if strings.HasPrefix(trim, "# ") {
			a.Title = strings.TrimSpace(strings.TrimPrefix(trim, "# "))
			continue
		}
		if strings.HasPrefix(trim, "## ") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trim, "## ")))
			continue
		}
		if strings.HasPrefix(trim, "- ") || strings.HasPrefix(trim, "* ") {
			value := strings.TrimSpace(trim[2:])
			switch section {
			case "decisions", "decision":
				a.Decisions = append(a.Decisions, value)
			case "lessons", "lesson":
				a.Lessons = append(a.Lessons, value)
			case "related", "related links":
				a.Related = append(a.Related, value)
			default:
				body = append(body, value)
			}
			continue
		}
		if trim != "" {
			body = append(body, trim)
		}
	}
	a.Summary = strings.TrimSpace(strings.Join(body, "\n"))
	return validateArtifact(a)
}

type SessionCloseRequest struct {
	RepositoryID  string
	CommitSHA     string
	Author        string
	Runtime       string
	Artifact      []byte
	Format        ArtifactFormat
	ManualSummary string
	Now           time.Time
}

type SessionDiary struct {
	SessionID       string          `json:"session_id"`
	IdempotencyKey  string          `json:"idempotency_key"`
	RepositoryID    string          `json:"repository_id"`
	CommitSHA       string          `json:"commit_sha"`
	ArtifactHash    string          `json:"artifact_hash"`
	Author          string          `json:"author"`
	Runtime         string          `json:"runtime,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	Title           string          `json:"title"`
	Summary         string          `json:"summary"`
	Decisions       []string        `json:"decisions,omitempty"`
	Lessons         []string        `json:"lessons,omitempty"`
	CodeReferences  []CodeReference `json:"code_references,omitempty"`
	Related         []string        `json:"related,omitempty"`
	GraphSnapshotID string          `json:"graph_snapshot_id,omitempty"`
	GraphState      string          `json:"graph_state,omitempty"`
}

type MemoryPort interface {
	FindByIdempotencyKey(context.Context, string) (SessionDiary, bool, error)
	SaveDiary(context.Context, SessionDiary) error
}

type SessionCloser struct {
	Memory MemoryPort
	Bus    *EventBus
	Graph  GraphPort
}

func (c SessionCloser) Close(ctx context.Context, req SessionCloseRequest) (SessionDiary, Delivery, error) {
	if c.Memory == nil {
		return SessionDiary{}, Delivery{}, errors.New("memory port is required")
	}
	if req.RepositoryID == "" || req.CommitSHA == "" {
		return SessionDiary{}, Delivery{}, errors.New("repository id and commit SHA are required")
	}
	var a SessionArtifact
	var err error
	if len(req.Artifact) > 0 {
		a, err = ParseSessionArtifact(req.Artifact, req.Format)
	} else {
		a, err = ManualSessionArtifact(req.ManualSummary)
	}
	if err != nil {
		return SessionDiary{}, Delivery{}, err
	}
	canonical, _ := json.Marshal(a)
	artifactSum := sha256.Sum256(canonical)
	artifactHash := hex.EncodeToString(artifactSum[:])
	keySum := sha256.Sum256([]byte(req.RepositoryID + "\n" + req.CommitSHA + "\n" + artifactHash))
	key := hex.EncodeToString(keySum[:])
	if existing, ok, err := c.Memory.FindByIdempotencyKey(ctx, key); err != nil {
		return SessionDiary{}, Delivery{}, err
	} else if ok {
		return existing, Delivery{EventID: existing.SessionID}, nil
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	refs := a.CodeReferences
	graphState := GraphReady
	var graphSnapshotID string
	if c.Graph != nil {
		resolution, graphErr := c.Graph.Resolve(ctx, GraphResolveRequest{RepositoryID: req.RepositoryID, CommitSHA: req.CommitSHA, References: refs})
		if graphErr != nil {
			graphState = GraphResolutionPending
			refs = unresolvedReferences(refs, req.RepositoryID, req.CommitSHA)
		} else {
			refs = resolution.References
			graphSnapshotID = resolution.GraphSnapshotID
			if publisher, ok := c.Graph.(GraphPublisher); ok {
				_, publishErr := publisher.Publish(ctx, GraphSnapshot{RepositoryID: req.RepositoryID, CommitSHA: req.CommitSHA, References: refs})
				if publishErr != nil {
					graphState = GraphSyncPending
				}
			}
		}
	}
	diary := SessionDiary{SessionID: key[:24], IdempotencyKey: key, RepositoryID: req.RepositoryID, CommitSHA: req.CommitSHA, ArtifactHash: artifactHash, Author: req.Author, Runtime: req.Runtime, CreatedAt: now, Title: a.Title, Summary: a.Summary, Decisions: a.Decisions, Lessons: a.Lessons, CodeReferences: refs, Related: a.Related, GraphSnapshotID: graphSnapshotID, GraphState: graphState}
	if err := c.Memory.SaveDiary(ctx, diary); err != nil {
		return SessionDiary{}, Delivery{}, err
	}
	if c.Bus == nil {
		return diary, Delivery{EventID: diary.SessionID}, nil
	}
	payload, _ := json.Marshal(diary)
	e := Event{EventID: diary.SessionID, Type: "session.closed", OccurredAt: now, RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, SchemaVersion: 1, Payload: payload}
	delivery, err := c.Bus.Publish(ctx, e)
	return diary, delivery, err
}

func unresolvedReferences(refs []CodeReference, repositoryID, commitSHA string) []CodeReference {
	out := make([]CodeReference, len(refs))
	copy(out, refs)
	for i := range out {
		out[i].RepositoryID = repositoryID
		out[i].CommitSHA = commitSHA
		out[i].Confidence = ConfidenceUnresolved
		out[i].GraphSnapshotID = ""
	}
	return out
}

// LocalMemoryStore is the canonical immutable .syntroph/memory store.
type LocalMemoryStore struct{ Root string }

func (s LocalMemoryStore) FindByIdempotencyKey(ctx context.Context, key string) (SessionDiary, bool, error) {
	var found SessionDiary
	err := filepath.Walk(s.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var d SessionDiary
		if e = json.Unmarshal([]byte(extractMetadata(string(b))), &d); e != nil {
			return nil
		}
		if d.IdempotencyKey == key {
			found = d
		}
		return nil
	})
	return found, found.IdempotencyKey != "", err
}

func (s LocalMemoryStore) SaveDiary(ctx context.Context, d SessionDiary) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	path := filepath.Join(s.Root, d.CreatedAt.Format("2006"), d.CreatedAt.Format("01"), d.SessionID+"-"+d.ArtifactHash[:12]+".md")
	if existing, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(existing), "idempotency_key: "+d.IdempotencyKey) {
			return nil
		}
		return errors.New("immutable diary path already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := renderDiary(d)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".diary-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.WriteString(content); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func renderDiary(d SessionDiary) string {
	refs, _ := json.Marshal(d.CodeReferences)
	related := append([]string(nil), d.Related...)
	sort.Strings(related)
	metadata, _ := json.Marshal(d)
	return fmt.Sprintf("---\nsession_id: %s\nidempotency_key: %s\nrepository_id: %s\ncommit_sha: %s\nartifact_hash: %s\nauthor: %s\nruntime: %s\ncreated_at: %s\ntitle: %s\ncode_references: %s\nrelated: %s\n---\n\n<!-- syntroph-metadata\n%s\n-->\n\n# %s\n\n%s\n", d.SessionID, d.IdempotencyKey, d.RepositoryID, d.CommitSHA, d.ArtifactHash, d.Author, d.Runtime, d.CreatedAt.Format(time.RFC3339Nano), d.Title, refs, strings.Join(related, ","), metadata, d.Title, d.Summary)
}

func extractMetadata(content string) string {
	const marker = "<!-- syntroph-metadata\n"
	start := strings.Index(content, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	end := strings.Index(content[start:], "\n-->")
	if end < 0 {
		return ""
	}
	return content[start : start+end]
}
