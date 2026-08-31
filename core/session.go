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
			case "code references", "code reference", "references":
				var ref CodeReference
				if json.Unmarshal([]byte(value), &ref) == nil {
					a.CodeReferences = append(a.CodeReferences, ref)
				} else {
					parts := strings.Split(value, "|")
					ref.Path = strings.TrimSpace(parts[0])
					if len(parts) > 1 {
						ref.Symbol = strings.TrimSpace(parts[1])
					}
					if len(parts) > 2 {
						ref.Kind = strings.TrimSpace(parts[2])
					}
					a.CodeReferences = append(a.CodeReferences, ref)
				}
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

// StoragePort mirrors the already durable diary to an external backend. The
// port deliberately uses core-owned types so adapters cannot mutate domain
// state or make the remote copy canonical.
type StoragePort interface {
	Mirror(context.Context, SessionDiary) StorageResult
}

// StoragePreflightPort exposes the same non-mutating prerequisite check used
// by `syntroph doctor storage`. Session close invokes it only after the local
// diary and session.closed event are durable.
type StoragePreflightPort interface {
	Preflight(context.Context) StoragePreflightResult
}

type StoragePreflightCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
}

type StoragePreflightResult struct {
	Enabled    bool                    `json:"enabled"`
	Ready      bool                    `json:"ready"`
	Backend    string                  `json:"backend,omitempty"`
	Provider   string                  `json:"provider,omitempty"`
	Repository string                  `json:"repository,omitempty"`
	Checks     []StoragePreflightCheck `json:"checks,omitempty"`
	Cause      error                   `json:"-"`
}

// EventAwareStoragePort lets external mirrors use the immutable event ID as
// their idempotency key while retaining the diary's artifact key.
type EventAwareStoragePort interface {
	MirrorEvent(context.Context, string, SessionDiary) StorageResult
}

type StorageResolver interface {
	Resolve(context.Context, SessionDiary, string, string) StorageResult
}

type StorageResult struct {
	EventID             string `json:"event_id,omitempty"`
	State               string `json:"state"`
	Backend             string `json:"backend,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Key                 string `json:"idempotency_key,omitempty"`
	RemoteID            string `json:"remote_id,omitempty"`
	RemoteURL           string `json:"remote_url,omitempty"`
	RemoteRev           string `json:"remote_revision,omitempty"`
	ExpectedRev         string `json:"expected_revision,omitempty"`
	LocalHash           string `json:"local_hash,omitempty"`
	EffectiveRemoteHash string `json:"effective_remote_hash,omitempty"`
	FailureClass        string `json:"failure_class,omitempty"`
	ConflictSnapshot    string `json:"conflict_snapshot,omitempty"`
	AlreadyInProgress   bool   `json:"already_in_progress,omitempty"`
	Error               string `json:"error,omitempty"`
	Cause               error  `json:"-"`
}

type SessionCloser struct {
	Memory  MemoryPort
	Bus     *EventBus
	Graph   GraphPort
	Storage StoragePort
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
	graphState := GraphResolutionPending
	var graphSnapshotID string
	if c.Graph != nil {
		resolution, graphErr := c.Graph.Resolve(ctx, GraphResolveRequest{RepositoryID: req.RepositoryID, CommitSHA: req.CommitSHA, References: refs})
		if graphErr != nil {
			graphState = GraphResolutionPending
			refs = unresolvedReferences(refs, req.RepositoryID, req.CommitSHA)
		} else {
			refs = resolution.References
			graphSnapshotID = resolution.GraphSnapshotID
			graphState = GraphReady
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
	payload, _ := json.Marshal(diary)
	e := Event{EventID: diary.SessionID, Type: "session.closed", OccurredAt: now, RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, SchemaVersion: 1, Payload: payload}
	if c.Bus == nil {
		if c.Storage != nil {
			ready := true
			if preflight, ok := c.Storage.(StoragePreflightPort); ok {
				ready = preflight.Preflight(ctx).Ready
			}
			if ready {
				_ = c.Storage.Mirror(ctx, diary)
			}
		}
		return diary, Delivery{EventID: diary.SessionID}, nil
	}
	delivery, err := c.Bus.Publish(ctx, e)
	if err != nil || c.Storage == nil {
		return diary, delivery, err
	}

	// The session.closed event is durable before the first remote effect. A
	// restart therefore observes the canonical diary and never infers that it
	// should replay storage automatically.
	storageEventID := diary.SessionID + ":storage"
	var storageResult StorageResult
	preflightReady := true
	if preflight, ok := c.Storage.(StoragePreflightPort); ok {
		check := preflight.Preflight(ctx)
		preflightReady = check.Ready
		if !check.Ready {
			cause := check.Cause
			if cause == nil {
				cause = errors.New("storage prerequisites are missing")
			}
			storageResult = StorageResult{
				EventID: storageEventID, State: "StoragePrerequisiteMissing",
				Backend: check.Backend, Provider: check.Provider,
				FailureClass: "prerequisite_missing", Error: cause.Error(), Cause: cause,
			}
		}
	}
	if preflightReady {
		if mirror, ok := c.Storage.(EventAwareStoragePort); ok {
			storageResult = mirror.MirrorEvent(ctx, storageEventID, diary)
		} else {
			storageResult = c.Storage.Mirror(ctx, diary)
			storageResult.EventID = storageEventID
		}
	}
	if storageResult.Cause != nil && storageResult.Error == "" {
		storageResult.Error = storageResult.Cause.Error()
	}
	if journal, ok := c.Bus.journal.(*SagaJournal); ok {
		attempt := HandlerAttempt{EventID: storageEventID, SagaID: diary.SessionID, HandlerID: "storage-mirror", AttemptedAt: time.Now().UTC(), Outcome: "succeeded"}
		if storageResult.State != "mirrored" {
			attempt.Outcome, attempt.Error = "failed", storageResult.Error
		}
		if storageResult.AlreadyInProgress {
			attempt.Outcome = "skipped"
		}
		if appendErr := journal.AppendAttempt(ctx, attempt); appendErr != nil {
			return diary, delivery, appendErr
		}
		delivery.Attempts = append(delivery.Attempts, attempt)
		// A concurrent attempt is diagnostic, not another pending obligation.
		if !storageResult.AlreadyInProgress {
			outcomePayload, _ := json.Marshal(storageResult)
			storageEvent := Event{EventID: storageEventID, Type: storageEventType(storageResult.State), OccurredAt: time.Now().UTC(), RepositoryID: diary.RepositoryID, SagaID: diary.SessionID, CorrelationID: diary.SessionID, CausationID: e.EventID, SchemaVersion: 1, Payload: outcomePayload}
			if appendErr := journal.AppendEvent(ctx, storageEvent); appendErr != nil {
				return diary, delivery, appendErr
			}
		}
	}
	return diary, delivery, err
}

func storageEventType(state string) string {
	switch state {
	case "mirrored":
		return "storage.sync.succeeded"
	case "StorageSyncConflict":
		return "storage.sync.conflict"
	case "StoragePrerequisiteMissing":
		return "storage.prerequisite.missing"
	default:
		return "storage.sync.pending"
	}
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

func RenderSessionDiary(d SessionDiary) string {
	refs, _ := json.Marshal(d.CodeReferences)
	related := append([]string(nil), d.Related...)
	sort.Strings(related)
	for i := range related {
		related[i] = wikilink(related[i])
	}
	metadata, _ := json.Marshal(d)
	var sections strings.Builder
	sections.WriteString(d.Summary + "\n")
	if len(d.Decisions) > 0 {
		sections.WriteString("\n## Decisions\n")
		for _, v := range d.Decisions {
			sections.WriteString("- " + v + "\n")
		}
	}
	if len(d.Lessons) > 0 {
		sections.WriteString("\n## Lessons\n")
		for _, v := range d.Lessons {
			sections.WriteString("- " + v + "\n")
		}
	}
	if len(d.CodeReferences) > 0 {
		sections.WriteString("\n## Code References\n")
		for _, v := range d.CodeReferences {
			sections.WriteString("- " + v.Path + " | " + v.Symbol + " | " + v.Kind + "\n")
		}
	}
	return fmt.Sprintf("---\nsession_id: %s\nidempotency_key: %s\nrepository_id: %s\ncommit_sha: %s\nartifact_hash: %s\nauthor: %s\nruntime: %s\ncreated_at: %s\ntitle: %s\ncode_references: %s\nrelated: %s\n---\n\n<!-- syntroph-metadata\n%s\n-->\n\n# %s\n\n%s", d.SessionID, d.IdempotencyKey, d.RepositoryID, d.CommitSHA, d.ArtifactHash, d.Author, d.Runtime, d.CreatedAt.Format(time.RFC3339Nano), d.Title, refs, strings.Join(related, ","), metadata, d.Title, sections.String())
}

func wikilink(value string) string {
	v := strings.TrimSpace(value)
	if strings.HasPrefix(v, "[[") && strings.HasSuffix(v, "]]") {
		return v
	}
	return "[[" + strings.Trim(v, "[]") + "]]"
}

func renderDiary(d SessionDiary) string { return RenderSessionDiary(d) }

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
