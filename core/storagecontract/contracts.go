// Package storagecontract owns the provider-neutral contracts exposed by the
// Core. It is a leaf package so adapters can share the exact domain types
// without importing the orchestration package or creating import cycles.
package storagecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Backend string

const (
	BackendWiki   Backend = "wiki"
	BackendIssues Backend = "issues"
)

type State string

const (
	Mirrored            State = "mirrored"
	SyncPending         State = "StorageSyncPending"
	SyncConflict        State = "StorageSyncConflict"
	PrerequisiteMissing State = "StoragePrerequisiteMissing"
)

type FailureClass string

const (
	FailureTransient         FailureClass = "transient"
	FailureConflict          FailureClass = "conflict"
	FailurePrerequisite      FailureClass = "prerequisite_missing"
	FailureAlreadyInProgress FailureClass = "already_in_progress"
)

type RemoteRevision string

func (r RemoteRevision) Validate() error {
	v := string(r)
	if strings.HasPrefix(v, "git:") {
		if isHex(strings.TrimPrefix(v, "git:"), 7) {
			return nil
		}
		return errors.New("git remote revision requires a commit SHA")
	}
	for _, prefix := range []string{"issue:", "comment:"} {
		if !strings.HasPrefix(v, prefix) {
			continue
		}
		body := strings.TrimPrefix(v, prefix)
		first, last := strings.IndexByte(body, ':'), strings.LastIndexByte(body, ':')
		if first <= 0 || last <= first+1 || last == len(body)-1 || !isSHA256(body[last+1:]) {
			return fmt.Errorf("%s remote revision requires id, observed time, and body hash", strings.TrimSuffix(prefix, ":"))
		}
		return nil
	}
	return fmt.Errorf("unsupported remote revision %q", v)
}

type Diary struct {
	SessionID    string
	RepositoryID string
	CommitSHA    string
	ArtifactHash string
	Content      string
}

func (d Diary) Validate() error {
	if d.SessionID == "" || d.RepositoryID == "" || d.CommitSHA == "" || d.ArtifactHash == "" || d.Content == "" {
		return errors.New("session diary is missing required fields")
	}
	return nil
}

func (d Diary) Key() string {
	sum := sha256.Sum256([]byte(d.RepositoryID + "\n" + d.CommitSHA + "\n" + d.ArtifactHash))
	return hex.EncodeToString(sum[:])
}

type Result struct {
	EventID             string       `json:"event_id,omitempty"`
	State               State        `json:"state"`
	Backend             Backend      `json:"backend,omitempty"`
	Provider            string       `json:"provider,omitempty"`
	Key                 string       `json:"idempotency_key,omitempty"`
	RemoteID            string       `json:"remote_id,omitempty"`
	RemoteURL           string       `json:"remote_url,omitempty"`
	RemoteRev           string       `json:"remote_revision,omitempty"`
	ExpectedRev         string       `json:"expected_revision,omitempty"`
	LocalHash           string       `json:"local_hash,omitempty"`
	EffectiveRemoteHash string       `json:"effective_remote_hash,omitempty"`
	RemoteContent       string       `json:"-"`
	FailureClass        FailureClass `json:"failure_class,omitempty"`
	ConflictSnapshot    string       `json:"conflict_snapshot,omitempty"`
	AlreadyInProgress   bool         `json:"already_in_progress,omitempty"`
	UnverifiedIdentity  bool         `json:"-"`
	Error               string       `json:"error,omitempty"`
	Cause               error        `json:"-"`
}

func (r Result) Pending() bool  { return r.State == SyncPending }
func (r Result) Conflict() bool { return r.State == SyncConflict }

type Binding struct {
	IdempotencyKey      string         `json:"idempotency_key"`
	Backend             Backend        `json:"backend"`
	Provider            string         `json:"provider"`
	RemoteID            string         `json:"remote_id"`
	URL                 string         `json:"url"`
	RemoteRevision      RemoteRevision `json:"remote_revision"`
	LocalHash           string         `json:"local_hash"`
	EffectiveRemoteHash string         `json:"effective_remote_hash"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

func (b Binding) Validate() error {
	if !isSHA256(b.IdempotencyKey) {
		return errors.New("remote binding requires a SHA-256 idempotency key")
	}
	if b.Backend != BackendWiki && b.Backend != BackendIssues {
		return errors.New("remote binding requires a supported backend")
	}
	if b.Provider == "" || b.RemoteID == "" || b.URL == "" {
		return errors.New("remote binding requires provider, remote ID, and URL")
	}
	if err := b.RemoteRevision.Validate(); err != nil {
		return err
	}
	if !isSHA256(b.LocalHash) || !isSHA256(b.EffectiveRemoteHash) {
		return errors.New("remote binding requires SHA-256 local and effective remote hashes")
	}
	if b.UpdatedAt.IsZero() {
		return errors.New("remote binding requires updated_at")
	}
	return nil
}

func isHex(v string, min int) bool {
	if len(v) < min {
		return false
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
func isSHA256(v string) bool { return len(v) == 64 && isHex(v, 64) }

const maxDiagnosticBytes = 2048

var credentialLike = regexp.MustCompile(`(?i)(authorization|token|password|secret|pat)(\s*[:=]\s*)([^\s,;)]+)`)

// SafeDiagnostic removes URL userinfo and credential-shaped values, then
// bounds provider diagnostics before they can enter the journal.
func SafeDiagnostic(value string) string {
	for _, field := range strings.Fields(value) {
		if !strings.Contains(field, "://") {
			continue
		}
		trimmed := strings.Trim(field, "()[]{}<>,;\"'")
		if parsed, err := url.Parse(trimmed); err == nil && parsed.User != nil {
			parsed.User = nil
			value = strings.ReplaceAll(value, trimmed, parsed.String())
		}
	}
	value = credentialLike.ReplaceAllString(value, "$1$2[redacted]")
	if len(value) > maxDiagnosticBytes {
		value = value[:maxDiagnosticBytes] + " [truncated]"
	}
	return strings.TrimSpace(value)
}
