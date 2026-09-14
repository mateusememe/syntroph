package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const SkillSchemaVersion = 1

var (
	ErrSkillNotFound              = errors.New("skill package not found")
	ErrUnsupportedSkill           = errors.New("unsupported skill package")
	ErrSkillInvocationReplay      = errors.New("skill invocation replay conflicts with the prepared bundle")
	ErrSkillInvocationIDCollision = errors.New("generated skill invocation identity already exists")
)

// SkillPackageIdentity identifies immutable source content independently from
// any intentional use of it.
type SkillPackageIdentity struct {
	SourceID    string `json:"source_id"`
	Name        string `json:"name"`
	PackageHash string `json:"package_hash"`
}

func (i SkillPackageIdentity) QualifiedName() string { return i.SourceID + "/" + i.Name }

type SkillAsset struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type,omitempty"`
}

// SkillPackage is the normalized source material held by a catalog adapter.
// SkillPort converts it into an opaque SkillBundle before callers may use it.
type SkillPackage struct {
	SchemaVersion      int                  `json:"schema_version"`
	Identity           SkillPackageIdentity `json:"identity"`
	Description        string               `json:"description"`
	Instructions       string               `json:"instructions"`
	Assets             []SkillAsset         `json:"assets,omitempty"`
	CompatibleRuntimes []string             `json:"compatible_runtimes,omitempty"`
}

type SkillCatalogState string

const (
	SkillReady              SkillCatalogState = "Ready"
	UnsupportedSkillPackage SkillCatalogState = "UnsupportedSkillPackage"
)

type SkillCatalogEntry struct {
	State      SkillCatalogState `json:"state"`
	Package    SkillPackage      `json:"package"`
	Diagnostic string            `json:"diagnostic,omitempty"`
}

type SkillPrepareRequest struct {
	Name         string         `json:"name"`
	InvocationID string         `json:"invocation_id,omitempty"`
	Runtime      string         `json:"runtime,omitempty"`
	Arguments    map[string]any `json:"arguments,omitempty"`
}

// SkillInvocation identifies one intentional use of an immutable package and
// the exact bundle prepared for that use.
type SkillInvocation struct {
	ID              string               `json:"id"`
	PackageIdentity SkillPackageIdentity `json:"package_identity"`
	BundleHash      string               `json:"bundle_hash"`
}

// SkillBundle exposes copies of mutable data so a prepared bundle cannot be
// changed by a caller or RuntimePort adapter after its identity is calculated.
type SkillBundle struct {
	schemaVersion int
	invocationID  string
	identity      SkillPackageIdentity
	description   string
	instructions  string
	assets        []SkillAsset
	runtimes      []string
	runtime       string
	arguments     map[string]any
	bundleHash    string
}

func (b SkillBundle) SchemaVersion() int                    { return b.schemaVersion }
func (b SkillBundle) InvocationID() string                  { return b.invocationID }
func (b SkillBundle) PackageIdentity() SkillPackageIdentity { return b.identity }
func (b SkillBundle) Description() string                   { return b.description }
func (b SkillBundle) Instructions() string                  { return b.instructions }
func (b SkillBundle) Assets() []SkillAsset                  { return append([]SkillAsset(nil), b.assets...) }
func (b SkillBundle) CompatibleRuntimes() []string          { return append([]string(nil), b.runtimes...) }
func (b SkillBundle) Runtime() string                       { return b.runtime }
func (b SkillBundle) Arguments() map[string]any             { return cloneSkillArguments(b.arguments) }
func (b SkillBundle) BundleHash() string                    { return b.bundleHash }
func (b SkillBundle) Invocation() SkillInvocation {
	return SkillInvocation{
		ID:              b.invocationID,
		PackageIdentity: b.identity,
		BundleHash:      b.bundleHash,
	}
}

type SkillPort interface {
	Prepare(context.Context, SkillPrepareRequest) (SkillBundle, error)
}

// RuntimeSkillPreparation is the minimal receipt shared by future runtime
// adapters after they translate a SkillBundle into their native mechanism.
type RuntimeSkillPreparation struct {
	InvocationID string `json:"invocation_id"`
	BundleHash   string `json:"bundle_hash"`
	Runtime      string `json:"runtime,omitempty"`
}

type RuntimePort interface {
	PrepareSkill(context.Context, SkillBundle) (RuntimeSkillPreparation, error)
}

type InvocationIDFactory func() (string, error)

// InMemorySkillPort is the deterministic Core adapter and contract reference
// for catalog implementations. It performs no filesystem or runtime effects.
type InMemorySkillPort struct {
	mu      sync.Mutex
	entries map[string]SkillCatalogEntry
	byID    map[string]SkillBundle
	newID   InvocationIDFactory
}

func NewInMemorySkillPort(entries []SkillCatalogEntry, newID InvocationIDFactory) (*InMemorySkillPort, error) {
	if newID == nil {
		newID = randomInvocationID
	}
	p := &InMemorySkillPort{
		entries: make(map[string]SkillCatalogEntry, len(entries)),
		byID:    make(map[string]SkillBundle),
		newID:   newID,
	}
	for _, entry := range entries {
		if err := validateSkillCatalogEntry(entry); err != nil {
			return nil, err
		}
		name := entry.Package.Identity.QualifiedName()
		if _, exists := p.entries[name]; exists {
			return nil, fmt.Errorf("duplicate skill package %q", name)
		}
		p.entries[name] = cloneSkillCatalogEntry(entry)
	}
	return p, nil
}

func (p *InMemorySkillPort) Prepare(ctx context.Context, request SkillPrepareRequest) (SkillBundle, error) {
	if err := ctx.Err(); err != nil {
		return SkillBundle{}, err
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return SkillBundle{}, errors.New("skill package name is required")
	}
	entry, ok := p.entries[name]
	if !ok {
		return SkillBundle{}, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	if entry.State != SkillReady {
		return SkillBundle{}, fmt.Errorf("%w: %s: %s", ErrUnsupportedSkill, name, entry.Diagnostic)
	}
	invocationID := strings.TrimSpace(request.InvocationID)
	explicitInvocationID := invocationID != ""
	if !explicitInvocationID {
		var err error
		invocationID, err = p.newID()
		if err != nil {
			return SkillBundle{}, fmt.Errorf("create skill invocation identity: %w", err)
		}
		invocationID = strings.TrimSpace(invocationID)
	}
	if invocationID == "" {
		return SkillBundle{}, errors.New("skill invocation identity is required")
	}
	bundle, err := newSkillBundle(invocationID, entry.Package, request.Runtime, request.Arguments)
	if err != nil {
		return SkillBundle{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, replay := p.byID[invocationID]; replay {
		if !explicitInvocationID {
			return SkillBundle{}, fmt.Errorf("%w: %s", ErrSkillInvocationIDCollision, invocationID)
		}
		if existing.bundleHash != bundle.bundleHash {
			return SkillBundle{}, fmt.Errorf("%w: %s", ErrSkillInvocationReplay, invocationID)
		}
		return existing, nil
	}
	p.byID[invocationID] = bundle
	return bundle, nil
}

func newSkillBundle(invocationID string, pkg SkillPackage, runtime string, arguments map[string]any) (SkillBundle, error) {
	normalizedArguments, err := normalizeSkillArguments(arguments)
	if err != nil {
		return SkillBundle{}, err
	}
	bundle := SkillBundle{
		schemaVersion: SkillSchemaVersion,
		invocationID:  invocationID,
		identity:      pkg.Identity,
		description:   pkg.Description,
		instructions:  pkg.Instructions,
		assets:        append([]SkillAsset(nil), pkg.Assets...),
		runtimes:      append([]string(nil), pkg.CompatibleRuntimes...),
		runtime:       strings.TrimSpace(runtime),
		arguments:     normalizedArguments,
	}
	canonical := struct {
		SchemaVersion int                  `json:"schema_version"`
		Identity      SkillPackageIdentity `json:"identity"`
		Description   string               `json:"description"`
		Instructions  string               `json:"instructions"`
		Assets        []SkillAsset         `json:"assets,omitempty"`
		Runtimes      []string             `json:"compatible_runtimes,omitempty"`
		Runtime       string               `json:"runtime,omitempty"`
		Arguments     map[string]any       `json:"arguments,omitempty"`
	}{bundle.schemaVersion, bundle.identity, bundle.description, bundle.instructions, bundle.assets, bundle.runtimes, bundle.runtime, bundle.arguments}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return SkillBundle{}, fmt.Errorf("encode skill bundle identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	bundle.bundleHash = hex.EncodeToString(sum[:])
	return bundle, nil
}

func normalizeSkillArguments(arguments map[string]any) (map[string]any, error) {
	if arguments == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("encode skill arguments: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized map[string]any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, fmt.Errorf("normalize skill arguments: %w", err)
	}
	return normalized, nil
}

func validateSkillCatalogEntry(entry SkillCatalogEntry) error {
	pkg := entry.Package
	sourceID := strings.TrimSpace(pkg.Identity.SourceID)
	name := strings.TrimSpace(pkg.Identity.Name)
	if sourceID == "" || name == "" {
		return errors.New("skill package source and name are required")
	}
	if sourceID != pkg.Identity.SourceID || name != pkg.Identity.Name {
		return errors.New("skill package source and name must not contain surrounding whitespace")
	}
	if entry.State == UnsupportedSkillPackage {
		if strings.TrimSpace(entry.Diagnostic) == "" {
			return errors.New("unsupported skill package diagnostic is required")
		}
		return nil
	}
	if entry.State != SkillReady {
		return errors.New("skill catalog entry state is invalid")
	}
	if pkg.SchemaVersion != SkillSchemaVersion {
		return fmt.Errorf("skill package schema version must be %d", SkillSchemaVersion)
	}
	if len(pkg.Identity.PackageHash) != sha256.Size*2 {
		return errors.New("skill package hash must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(pkg.Identity.PackageHash); err != nil {
		return errors.New("skill package hash must be a SHA-256 hex digest")
	}
	if strings.TrimSpace(pkg.Instructions) == "" {
		return errors.New("ready skill package instructions are required")
	}
	return nil
}

func cloneSkillCatalogEntry(entry SkillCatalogEntry) SkillCatalogEntry {
	entry.Package.Assets = append([]SkillAsset(nil), entry.Package.Assets...)
	entry.Package.CompatibleRuntimes = append([]string(nil), entry.Package.CompatibleRuntimes...)
	return entry
}

func cloneSkillArguments(arguments map[string]any) map[string]any {
	if arguments == nil {
		return nil
	}
	clone := make(map[string]any, len(arguments))
	for key, value := range arguments {
		clone[key] = cloneSkillArgument(value)
	}
	return clone
}

func cloneSkillArgument(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneSkillArguments(typed)
	case []any:
		clone := make([]any, len(typed))
		for i := range typed {
			clone[i] = cloneSkillArgument(typed[i])
		}
		return clone
	default:
		return typed
	}
}

func randomInvocationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "inv_" + hex.EncodeToString(value[:]), nil
}
