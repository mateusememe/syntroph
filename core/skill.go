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
	"sort"
	"strings"
	"sync"
)

const SkillSchemaVersion = 1

var (
	ErrSkillNotFound              = errors.New("skill package not found")
	ErrUnsupportedSkill           = errors.New("unsupported skill package")
	ErrSkillInvocationReplay      = errors.New("skill invocation replay conflicts with the prepared bundle")
	ErrSkillInvocationIDCollision = errors.New("generated skill invocation identity already exists")
	// ErrSkillNameAmbiguous reports that an unqualified name matched more
	// than one catalog package. Resolution never guesses at precedence -- a
	// local package never silently overrides an external one -- so the
	// caller must qualify the request with source_id/name instead.
	ErrSkillNameAmbiguous = errors.New("skill package name is ambiguous; qualify it with source_id/name")
	// ErrSkillAliasInvalid reports that an explicit alias does not resolve
	// deterministically: its target is missing or itself ambiguous. Aliases
	// never fall back to guessing, so this fails before preparation.
	ErrSkillAliasInvalid = errors.New("skill alias does not resolve to exactly one known skill package")
	// ErrSkillRuntimeIncompatible reports that the requested runtime is not
	// declared compatible by the package. Runtime selection only validates
	// compatibility; it never changes the canonical instructions.
	ErrSkillRuntimeIncompatible = errors.New("requested runtime is not compatible with this skill package")
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
	License            string               `json:"license"`
	SourceURL          string               `json:"source_url"`
	SourceRevision     string               `json:"source_revision"`
	InstructionsPath   string               `json:"instructions_path"`
	Instructions       string               `json:"instructions"`
	Assets             []SkillAsset         `json:"assets,omitempty"`
	ArgumentsSchema    map[string]any       `json:"arguments_schema,omitempty"`
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

// skillBundleView is the complete immutable JSON Skill Bundle emitted by
// `syntroph skill prepare` and consumed by future RuntimePort adapters. It
// carries only safe relative asset references into the content-addressed
// store; it never contains a host filesystem path.
type skillBundleView struct {
	SchemaVersion      int                  `json:"schema_version"`
	InvocationID       string               `json:"invocation_id"`
	PackageIdentity    SkillPackageIdentity `json:"package_identity"`
	Description        string               `json:"description,omitempty"`
	Instructions       string               `json:"instructions"`
	Assets             []SkillAsset         `json:"assets,omitempty"`
	CompatibleRuntimes []string             `json:"compatible_runtimes,omitempty"`
	Runtime            string               `json:"runtime,omitempty"`
	Arguments          map[string]any       `json:"arguments,omitempty"`
	BundleHash         string               `json:"bundle_hash"`
}

// MarshalJSON renders the complete immutable JSON Skill Bundle. SkillBundle
// keeps its fields private everywhere else so a caller cannot mutate a
// prepared bundle; this is the one place its shape is public.
func (b SkillBundle) MarshalJSON() ([]byte, error) {
	return json.Marshal(skillBundleView{
		SchemaVersion:      b.schemaVersion,
		InvocationID:       b.invocationID,
		PackageIdentity:    b.identity,
		Description:        b.description,
		Instructions:       b.instructions,
		Assets:             b.assets,
		CompatibleRuntimes: b.runtimes,
		Runtime:            b.runtime,
		Arguments:          b.arguments,
		BundleHash:         b.bundleHash,
	})
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
	mu                sync.Mutex
	entries           map[string]SkillCatalogEntry
	byUnqualifiedName map[string][]string
	aliases           map[string]string
	byID              map[string]SkillBundle
	newID             InvocationIDFactory
}

// NewInMemorySkillPort builds a deterministic catalog from entries, keyed by
// their canonical source_id/name identity, plus an optional explicit alias
// table. aliases is never consulted to resolve entries -- an alias target
// must itself resolve through the canonical or unqualified path -- so
// aliases cannot chain into further aliases.
func NewInMemorySkillPort(entries []SkillCatalogEntry, aliases map[string]string, newID InvocationIDFactory) (*InMemorySkillPort, error) {
	if newID == nil {
		newID = randomInvocationID
	}
	p := &InMemorySkillPort{
		entries:           make(map[string]SkillCatalogEntry, len(entries)),
		byUnqualifiedName: make(map[string][]string),
		aliases:           cloneAliases(aliases),
		byID:              make(map[string]SkillBundle),
		newID:             newID,
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
		unqualified := entry.Package.Identity.Name
		p.byUnqualifiedName[unqualified] = append(p.byUnqualifiedName[unqualified], name)
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
	entry, err := p.resolveName(name)
	if err != nil {
		return SkillBundle{}, err
	}
	if entry.State != SkillReady {
		return SkillBundle{}, fmt.Errorf("%w: %s: %s", ErrUnsupportedSkill, entry.Package.Identity.QualifiedName(), entry.Diagnostic)
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

// resolveName finds the catalog entry a caller-supplied name identifies.
// Canonical source_id/name identities always take precedence. An explicit
// alias is consulted next, resolved through the canonical-or-unqualified
// path only (never through another alias). Anything else is treated as an
// unqualified package name and must match exactly one entry; a collision
// requires qualification rather than guessing, so a local package can never
// silently override an external one with the same name.
func (p *InMemorySkillPort) resolveName(name string) (SkillCatalogEntry, error) {
	if entry, ok := p.entries[name]; ok {
		return entry, nil
	}
	if target, ok := p.aliases[name]; ok {
		entry, err := p.resolvePackageName(target)
		if err != nil {
			return SkillCatalogEntry{}, fmt.Errorf("%w: alias %q targets %q: %v", ErrSkillAliasInvalid, name, target, err)
		}
		return entry, nil
	}
	return p.resolvePackageName(name)
}

func (p *InMemorySkillPort) resolvePackageName(name string) (SkillCatalogEntry, error) {
	if entry, ok := p.entries[name]; ok {
		return entry, nil
	}
	matches := p.byUnqualifiedName[name]
	switch len(matches) {
	case 0:
		return SkillCatalogEntry{}, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	case 1:
		return p.entries[matches[0]], nil
	default:
		sorted := append([]string(nil), matches...)
		sort.Strings(sorted)
		return SkillCatalogEntry{}, fmt.Errorf("%w: %q matches %s", ErrSkillNameAmbiguous, name, strings.Join(sorted, ", "))
	}
}

func newSkillBundle(invocationID string, pkg SkillPackage, runtime string, arguments map[string]any) (SkillBundle, error) {
	runtime = strings.TrimSpace(runtime)
	if err := validateSkillRuntimeCompatibility(pkg, runtime); err != nil {
		return SkillBundle{}, err
	}
	normalizedArguments, err := normalizeSkillArguments(arguments)
	if err != nil {
		return SkillBundle{}, err
	}
	schema, err := parseSkillArgumentsSchema(pkg.ArgumentsSchema)
	if err != nil {
		return SkillBundle{}, err
	}
	if err := validateSkillArguments(schema, normalizedArguments); err != nil {
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
		runtime:       runtime,
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
	entry.Package.ArgumentsSchema = cloneSkillArguments(entry.Package.ArgumentsSchema)
	entry.Package.CompatibleRuntimes = append([]string(nil), entry.Package.CompatibleRuntimes...)
	return entry
}

// validateSkillRuntimeCompatibility validates runtime selection without
// touching canonical instructions. An empty request runtime or a package
// that declares no compatible runtimes places no restriction; otherwise the
// requested runtime must be one of the package's declared runtimes.
func validateSkillRuntimeCompatibility(pkg SkillPackage, runtime string) error {
	if runtime == "" || len(pkg.CompatibleRuntimes) == 0 {
		return nil
	}
	for _, compatible := range pkg.CompatibleRuntimes {
		if compatible == runtime {
			return nil
		}
	}
	return fmt.Errorf("%w: %s does not support runtime %q (compatible: %s)", ErrSkillRuntimeIncompatible, pkg.Identity.QualifiedName(), runtime, strings.Join(pkg.CompatibleRuntimes, ", "))
}

func cloneAliases(aliases map[string]string) map[string]string {
	if len(aliases) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(aliases))
	for alias, target := range aliases {
		cloned[strings.TrimSpace(alias)] = strings.TrimSpace(target)
	}
	return cloned
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
