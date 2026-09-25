// Package skillfilesystem synchronizes declared local Skill Packages into the
// repository-scoped, content-addressed Skill Catalog.
package skillfilesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
	"gopkg.in/yaml.v3"
)

const (
	RuntimeLockSchemaVersion = 1
	LocalManifestName        = "skill.yaml"
	maxSkillDiagnosticBytes  = 1024
	maxInstructionsBytes     = 256 * 1024
	maxAssetBytes            = 1024 * 1024
	maxBundleBytes           = 5 * 1024 * 1024
	maxPackageFiles          = 128
	maxDirectoryDepth        = 8
	maxManifestBytes         = 256 * 1024
)

type packageValidationError struct{ message string }

func (e *packageValidationError) Error() string { return e.message }

func invalidPackage(format string, args ...any) error {
	return &packageValidationError{message: fmt.Sprintf(format, args...)}
}

type packageDeclaration struct {
	SchemaVersion      int            `yaml:"schema_version" json:"schema_version"`
	SourceID           string         `yaml:"source_id,omitempty" json:"source_id"`
	Name               string         `yaml:"name" json:"name"`
	Directory          string         `yaml:"directory,omitempty" json:"-"`
	Description        string         `yaml:"description" json:"description"`
	License            string         `yaml:"license" json:"license"`
	SourceURL          string         `yaml:"source_url" json:"source_url"`
	SourceRevision     string         `yaml:"source_revision" json:"source_revision"`
	Instructions       string         `yaml:"instructions" json:"instructions"`
	Files              []string       `yaml:"files" json:"files"`
	ArgumentsSchema    map[string]any `yaml:"arguments_schema,omitempty" json:"arguments_schema,omitempty"`
	CompatibleRuntimes []string       `yaml:"compatible_runtimes,omitempty" json:"compatible_runtimes,omitempty"`
}

type runtimeLock struct {
	SchemaVersion int                  `yaml:"schema_version"`
	Packages      []packageDeclaration `yaml:"packages"`
}

type catalogIndex struct {
	SchemaVersion int               `json:"schema_version"`
	Aliases       map[string]string `json:"aliases,omitempty"`
	Entries       []EntrySummary    `json:"entries"`
}

type EntrySummary struct {
	State      core.SkillCatalogState    `json:"state"`
	Identity   core.SkillPackageIdentity `json:"identity"`
	Diagnostic string                    `json:"diagnostic,omitempty"`
}

type sourceFile struct {
	path string
	data []byte
}

type SyncResult struct {
	State   SyncState      `json:"state"`
	Owner   *SyncLockOwner `json:"owner,omitempty"`
	Entries []EntrySummary `json:"entries"`
}

type Catalog struct {
	mu                sync.Mutex
	catalogRoot       string
	settings          config.ResolvedSkills
	preparer          core.SkillPort
	preparerIndexHash string
	checkpoint        func(SyncCheckpoint) error
	processState      func(int) processState
	now               func() time.Time
	pid               func() int
	ownerID           func() string
}

func New(repositoryRoot string, settings config.ResolvedSkills) (*Catalog, error) {
	if !settings.Enabled {
		return nil, errors.New("skills are not enabled in .syntroph/config.yaml")
	}
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	if settings.RuntimeLock == "" || len(settings.Sources) == 0 {
		return nil, errors.New("resolved skill configuration is incomplete")
	}
	return &Catalog{
		catalogRoot:  filepath.Join(root, ".syntroph", "catalog"),
		settings:     settings,
		processState: defaultProcessState,
		now:          func() time.Time { return time.Now().UTC() },
		pid:          os.Getpid,
		ownerID:      randomSyncOwnerID,
	}, nil
}

func (c *Catalog) Sync(ctx context.Context) (result SyncResult, resultErr error) {
	defer func() { resultErr = boundSyncDiagnostic(resultErr) }()
	if err := ctx.Err(); err != nil {
		return SyncResult{}, err
	}
	ownership, existingOwner, err := c.acquireSyncLock(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if existingOwner != nil {
		switch c.processState(existingOwner.PID) {
		case processAlive:
			return SyncResult{State: SyncInProgress, Owner: existingOwner}, nil
		case processDead:
			return SyncResult{State: SyncRecoveryRequired, Owner: existingOwner}, ErrSyncRecoveryRequired
		default:
			return SyncResult{State: SyncRecoveryRequired, Owner: existingOwner}, ErrSyncLivenessUnverifiable
		}
	}
	stagingRoot := filepath.Join(c.catalogRoot, "staging", ownership.owner.OwnerID)
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return SyncResult{}, errors.Join(fmt.Errorf("create skill catalog staging directory: %w", err), ownership.Release())
	}
	defer func() {
		cleanupErr := os.RemoveAll(stagingRoot)
		resultErr = errors.Join(resultErr, cleanupErr)
		if cleanupErr == nil {
			resultErr = errors.Join(resultErr, ownership.Release())
		}
	}()
	ownerData, err := json.Marshal(ownership.owner)
	if err != nil {
		return SyncResult{}, fmt.Errorf("encode skill catalog staging owner: %w", err)
	}
	if err := writeAtomicMode(filepath.Join(stagingRoot, "owner.json"), ownerData, 0o600); err != nil {
		return SyncResult{}, fmt.Errorf("write skill catalog staging owner: %w", err)
	}
	if err := c.reachCheckpoint(SyncCheckpointStagingCreated); err != nil {
		return SyncResult{}, err
	}
	var runtime runtimeLock
	if err := readStrictYAML(c.settings.RuntimeLock, &runtime); err != nil {
		if os.IsNotExist(err) {
			return SyncResult{}, fmt.Errorf("runtime skill lock is missing at %s; create or restore the configured lock before running syntroph skill sync", c.settings.RuntimeLock)
		}
		return SyncResult{}, fmt.Errorf("read runtime skill lock: %w", err)
	}
	if runtime.SchemaVersion != RuntimeLockSchemaVersion {
		return SyncResult{}, fmt.Errorf("runtime skill lock schema_version must be %d", RuntimeLockSchemaVersion)
	}
	sources := make(map[string]config.ResolvedSkillSource, len(c.settings.Sources))
	for _, source := range c.settings.Sources {
		sources[source.ID] = source
	}

	entries := make([]core.SkillCatalogEntry, 0, len(runtime.Packages))
	seen := make(map[string]struct{})
	for _, declaration := range runtime.Packages {
		if err := ctx.Err(); err != nil {
			return SyncResult{}, err
		}
		if declaration.SourceID == "local" {
			return SyncResult{}, errors.New("local Skill Packages must use skill.yaml manifests, not runtime lock declarations")
		}
		if declaration.SchemaVersion == 0 {
			declaration.SchemaVersion = runtime.SchemaVersion
		}
		source, ok := sources[declaration.SourceID]
		if !ok || declaration.SourceID == "" {
			return SyncResult{}, fmt.Errorf("runtime lock package %q references undeclared source %q", declaration.Name, declaration.SourceID)
		}
		entry, err := c.loadPackage(source, declaration.Directory, declaration, stagingRoot)
		if err != nil {
			return SyncResult{}, err
		}
		if err := addUniqueEntry(&entries, seen, entry); err != nil {
			return SyncResult{}, err
		}
	}
	if local, ok := sources["local"]; ok {
		localEntries, err := c.loadLocalPackages(ctx, local, stagingRoot)
		if err != nil {
			return SyncResult{}, err
		}
		for _, entry := range localEntries {
			if err := addUniqueEntry(&entries, seen, entry); err != nil {
				return SyncResult{}, err
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Package.Identity.QualifiedName() < entries[j].Package.Identity.QualifiedName()
	})
	summaries := summarizeEntries(entries)
	if _, err := core.NewInMemorySkillPort(entries, c.settings.Aliases, nil); err != nil {
		return SyncResult{}, fmt.Errorf("validate skill catalog preparer: %w", err)
	}
	if err := c.reachCheckpoint(SyncCheckpointStoreReady); err != nil {
		return SyncResult{}, err
	}
	stagedIndex, err := c.stageIndex(stagingRoot, summaries)
	if err != nil {
		return SyncResult{}, err
	}
	if err := c.verifyStagedIndex(stagedIndex); err != nil {
		return SyncResult{}, err
	}
	if err := c.reachCheckpoint(SyncCheckpointIndexStaged); err != nil {
		return SyncResult{}, err
	}
	if err := c.publishStagedIndex(stagedIndex); err != nil {
		return SyncResult{}, err
	}
	if err := c.reachCheckpoint(SyncCheckpointIndexPublished); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{State: SyncSucceeded, Entries: summaries}, nil
}

func (c *Catalog) List(ctx context.Context) ([]EntrySummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index, err := c.readIndex()
	if err != nil {
		return nil, err
	}
	return append([]EntrySummary(nil), index.Entries...), nil
}

func (c *Catalog) Show(ctx context.Context, name string) (core.SkillCatalogEntry, error) {
	if err := ctx.Err(); err != nil {
		return core.SkillCatalogEntry{}, err
	}
	index, err := c.readIndex()
	if err != nil {
		return core.SkillCatalogEntry{}, err
	}
	name = strings.TrimSpace(name)
	for _, summary := range index.Entries {
		if summary.Identity.QualifiedName() != name {
			continue
		}
		entry := core.SkillCatalogEntry{State: summary.State, Diagnostic: summary.Diagnostic}
		if summary.State == core.SkillReady {
			pkg, err := c.readStoredPackage(summary.Identity.PackageHash)
			if err != nil {
				return core.SkillCatalogEntry{}, err
			}
			entry.Package = pkg
		} else {
			entry.Package.Identity = summary.Identity
		}
		return entry, nil
	}
	return core.SkillCatalogEntry{}, fmt.Errorf("%w: %s", core.ErrSkillNotFound, name)
}

// Verify inspects the active catalog index — optionally narrowed to one
// qualified package name — and confirms that every Ready entry's immutable
// store content still matches what was recorded when it was synchronized.
// It never re-reads Skill Sources or the runtime lock: drift observed here
// means the local content-addressed store itself changed after publication.
// The returned error wraps core.ErrUnsupportedSkill whenever the requested
// package, or any package in a whole-catalog verification, is not Ready or
// has drifted, so CLI callers can surface a non-zero result.
func (c *Catalog) Verify(ctx context.Context, name string) ([]EntrySummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	index, err := c.readIndex()
	if err != nil {
		return nil, err
	}
	entries := append([]EntrySummary(nil), index.Entries...)
	name = strings.TrimSpace(name)
	if name != "" {
		found := false
		for _, entry := range entries {
			if entry.Identity.QualifiedName() == name {
				entries = []EntrySummary{entry}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: %s", core.ErrSkillNotFound, name)
		}
	}
	invalid := 0
	for i := range entries {
		if entries[i].State != core.SkillReady {
			invalid++
			continue
		}
		if driftErr := c.verifyStoredPackageIntegrity(entries[i].Identity.PackageHash); driftErr != nil {
			diagnostic := strings.TrimSpace(driftErr.Error())
			if len(diagnostic) > maxSkillDiagnosticBytes {
				diagnostic = diagnostic[:maxSkillDiagnosticBytes]
			}
			entries[i] = EntrySummary{State: core.UnsupportedSkillPackage, Identity: entries[i].Identity, Diagnostic: diagnostic}
			invalid++
		}
	}
	if invalid > 0 {
		return entries, fmt.Errorf("%w: %d of %d skill package(s) failed verification", core.ErrUnsupportedSkill, invalid, len(entries))
	}
	return entries, nil
}

// verifyStoredPackageIntegrity recomputes the recorded package's declared
// files from the immutable store and compares them against the metadata
// captured at materialization time. Diagnostics stay store-relative so they
// never leak the host's absolute repository path.
func (c *Catalog) verifyStoredPackageIntegrity(packageHash string) error {
	pkg, err := c.readStoredPackage(packageHash)
	if err != nil {
		return errors.New("stored skill package is missing or unreadable")
	}
	filesRoot := filepath.Join(c.catalogRoot, "store", packageHash, "files")
	want := make(map[string]struct{}, len(pkg.Assets)+1)
	if pkg.InstructionsPath != "" {
		want[pkg.InstructionsPath] = struct{}{}
		data, readErr := os.ReadFile(filepath.Join(filesRoot, filepath.FromSlash(pkg.InstructionsPath)))
		if readErr != nil {
			return fmt.Errorf("stored instructions file %q is missing or unreadable", pkg.InstructionsPath)
		}
		if string(data) != pkg.Instructions {
			return fmt.Errorf("stored instructions file %q has drifted from the recorded package", pkg.InstructionsPath)
		}
	}
	for _, asset := range pkg.Assets {
		want[asset.Path] = struct{}{}
		data, readErr := os.ReadFile(filepath.Join(filesRoot, filepath.FromSlash(asset.Path)))
		if readErr != nil {
			return fmt.Errorf("stored asset %q is missing or unreadable", asset.Path)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != asset.SHA256 {
			return fmt.Errorf("stored asset %q has drifted from its recorded hash", asset.Path)
		}
	}
	var extra string
	walkErr := filepath.WalkDir(filesRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(filesRoot, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if _, ok := want[rel]; !ok && extra == "" {
			extra = rel
		}
		return nil
	})
	if walkErr != nil {
		return errors.New("stored skill package files are unreadable")
	}
	if extra != "" {
		return fmt.Errorf("stored package contains undeclared file %q", extra)
	}
	return nil
}

func (c *Catalog) Prepare(ctx context.Context, request core.SkillPrepareRequest) (core.SkillBundle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index, encoded, err := c.readIndexData()
	if err != nil {
		return core.SkillBundle{}, err
	}
	sum := sha256.Sum256(encoded)
	indexHash := hex.EncodeToString(sum[:])
	if c.preparer != nil && c.preparerIndexHash == indexHash {
		return c.preparer.Prepare(ctx, request)
	}
	entries, err := c.loadEntriesFromIndex(ctx, index)
	if err != nil {
		return core.SkillBundle{}, err
	}
	prepared, err := core.NewInMemorySkillPort(entries, index.Aliases, nil)
	if err != nil {
		return core.SkillBundle{}, err
	}
	c.preparer = prepared
	c.preparerIndexHash = indexHash
	return c.preparer.Prepare(ctx, request)
}

func (c *Catalog) loadEntriesFromIndex(ctx context.Context, index catalogIndex) ([]core.SkillCatalogEntry, error) {
	entries := make([]core.SkillCatalogEntry, 0, len(index.Entries))
	for _, summary := range index.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := core.SkillCatalogEntry{State: summary.State, Diagnostic: summary.Diagnostic}
		if summary.State == core.SkillReady {
			pkg, err := c.readStoredPackage(summary.Identity.PackageHash)
			if err != nil {
				return nil, err
			}
			entry.Package = pkg
		} else {
			entry.Package.Identity = summary.Identity
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (c *Catalog) loadLocalPackages(ctx context.Context, source config.ResolvedSkillSource, stagingRoot string) ([]core.SkillCatalogEntry, error) {
	directories, err := os.ReadDir(source.Root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read local skill source: %w", err)
	}
	var entries []core.SkillCatalogEntry
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !directory.IsDir() {
			continue
		}
		manifestPath := filepath.Join(source.Root, directory.Name(), LocalManifestName)
		var declaration packageDeclaration
		if err := readStrictYAML(manifestPath, &declaration); err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) && !os.IsNotExist(err) {
				return nil, fmt.Errorf("read local skill manifest: %w", err)
			}
			entries = append(entries, unsupportedEntry("local", directory.Name(), directory.Name(), invalidPackage("manifest is invalid: %v", safeManifestDiagnostic(err))))
			continue
		}
		if declaration.SourceID != "" || declaration.Directory != "" {
			entries = append(entries, unsupportedEntry("local", declaration.Name, directory.Name(), invalidPackage("manifest must not declare source_id or directory")))
			continue
		}
		declaration.SourceID = "local"
		entry, err := c.loadPackage(source, directory.Name(), declaration, stagingRoot)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func safeManifestDiagnostic(err error) string {
	if os.IsNotExist(err) {
		return LocalManifestName + " is missing"
	}
	return strings.TrimSpace(err.Error())
}

func (c *Catalog) loadPackage(source config.ResolvedSkillSource, directory string, declaration packageDeclaration, stagingRoot string) (core.SkillCatalogEntry, error) {
	if err := normalizeDeclaration(&declaration); err != nil {
		return unsupportedEntry(source.ID, declaration.Name, directory, err), nil
	}
	if source.ID != declaration.SourceID {
		return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("source identity does not match its declared source")), nil
	}
	packageRoot, err := confinedPath(source.Root, directory)
	if err != nil {
		return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("package directory is invalid: %v", err)), nil
	}
	files := make([]sourceFile, 0, len(declaration.Files))
	var bundleBytes int64
	for _, relative := range declaration.Files {
		path, err := confinedPath(packageRoot, filepath.FromSlash(relative))
		if err != nil {
			return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("declared file %q is invalid: %v", relative, err)), nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("declared file %q is missing", relative)), nil
			}
			return core.SkillCatalogEntry{}, fmt.Errorf("skill package %s/%s file %q: %w", source.ID, declaration.Name, relative, err)
		}
		if !info.Mode().IsRegular() {
			return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("declared file %q is not a regular file", relative)), nil
		}
		if info.Mode()&0o111 != 0 {
			return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("declared file %q is executable content", relative)), nil
		}
		limit := int64(maxAssetBytes)
		limitLabel := "1 MiB"
		if relative == declaration.Instructions {
			limit = maxInstructionsBytes
			limitLabel = "256 KiB"
		}
		if info.Size() > limit {
			return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("declared file %q exceeds the %s limit", relative, limitLabel)), nil
		}
		bundleBytes += info.Size()
		if bundleBytes > maxBundleBytes {
			return unsupportedEntry(source.ID, declaration.Name, directory, invalidPackage("declared package content exceeds the 5 MiB bundle limit")), nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return core.SkillCatalogEntry{}, fmt.Errorf("read skill package %s/%s file %q: %w", source.ID, declaration.Name, relative, err)
		}
		if err := validateStaticFile(relative, declaration.Instructions, data); err != nil {
			return unsupportedEntry(source.ID, declaration.Name, directory, err), nil
		}
		files = append(files, sourceFile{path: relative, data: data})
	}
	packageHash, err := hashPackage(declaration, files)
	if err != nil {
		return core.SkillCatalogEntry{}, err
	}
	pkg := core.SkillPackage{
		SchemaVersion:      declaration.SchemaVersion,
		Identity:           core.SkillPackageIdentity{SourceID: source.ID, Name: declaration.Name, PackageHash: packageHash},
		Description:        declaration.Description,
		License:            declaration.License,
		SourceURL:          declaration.SourceURL,
		SourceRevision:     declaration.SourceRevision,
		InstructionsPath:   declaration.Instructions,
		ArgumentsSchema:    declaration.ArgumentsSchema,
		CompatibleRuntimes: append([]string(nil), declaration.CompatibleRuntimes...),
	}
	for _, file := range files {
		if file.path == declaration.Instructions {
			pkg.Instructions = string(file.data)
			continue
		}
		sum := sha256.Sum256(file.data)
		mediaType := staticMediaType(file.path)
		pkg.Assets = append(pkg.Assets, core.SkillAsset{Path: file.path, SHA256: hex.EncodeToString(sum[:]), MediaType: mediaType})
	}
	if err := c.materialize(pkg, files, stagingRoot); err != nil {
		return core.SkillCatalogEntry{}, err
	}
	return core.SkillCatalogEntry{State: core.SkillReady, Package: pkg}, nil
}

func unsupportedEntry(sourceID, name, directory string, cause error) core.SkillCatalogEntry {
	name = strings.TrimSpace(name)
	if name == "" {
		name = filepath.Base(filepath.Clean(strings.TrimSpace(directory)))
	}
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "invalid-package"
	}
	diagnostic := strings.TrimSpace(cause.Error())
	if len(diagnostic) > maxSkillDiagnosticBytes {
		diagnostic = diagnostic[:maxSkillDiagnosticBytes]
	}
	return core.SkillCatalogEntry{
		State: core.UnsupportedSkillPackage,
		Package: core.SkillPackage{Identity: core.SkillPackageIdentity{
			SourceID: strings.TrimSpace(sourceID),
			Name:     name,
		}},
		Diagnostic: diagnostic,
	}
}

func validateStaticFile(path, instructions string, data []byte) error {
	extension := strings.ToLower(filepath.Ext(path))
	if path == instructions && extension != ".md" && extension != ".markdown" {
		return invalidPackage("instructions file %q must be Markdown", path)
	}
	switch extension {
	case ".md", ".markdown", ".txt", ".json", ".yaml", ".yml", ".svg":
		if !utf8.Valid(data) {
			return invalidPackage("declared text file %q is not valid UTF-8", path)
		}
	case ".png", ".jpg", ".jpeg", ".gif":
		// Static binary images are carried opaquely and never executed.
	default:
		return invalidPackage("declared file %q has a prohibited file type", path)
	}
	return nil
}

func staticMediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

func normalizeDeclaration(declaration *packageDeclaration) error {
	declaration.SourceID = strings.TrimSpace(declaration.SourceID)
	declaration.Name = strings.TrimSpace(declaration.Name)
	declaration.Directory = strings.TrimSpace(declaration.Directory)
	declaration.Description = strings.TrimSpace(declaration.Description)
	declaration.License = strings.TrimSpace(declaration.License)
	declaration.SourceURL = strings.TrimSpace(declaration.SourceURL)
	declaration.SourceRevision = strings.TrimSpace(declaration.SourceRevision)
	var err error
	declaration.Instructions, err = normalizeDeclaredPath(declaration.Instructions)
	if err != nil {
		return invalidPackage("instructions file is invalid: %v", err)
	}
	if declaration.SchemaVersion != core.SkillSchemaVersion {
		return invalidPackage("schema_version must be %d", core.SkillSchemaVersion)
	}
	if declaration.SourceID == "" || declaration.Name == "" || declaration.Description == "" || declaration.License == "" || declaration.SourceURL == "" || declaration.SourceRevision == "" {
		return invalidPackage("source identity, name, description, license, source_url, and source_revision are required")
	}
	if len(declaration.Files) > maxPackageFiles {
		return invalidPackage("package declares %d files; at most 128 files are allowed", len(declaration.Files))
	}
	seen := make(map[string]struct{}, len(declaration.Files))
	files := make([]string, 0, len(declaration.Files))
	for _, rawFile := range declaration.Files {
		file, pathErr := normalizeDeclaredPath(rawFile)
		if pathErr != nil {
			return invalidPackage("declared file %q must be a package-relative path: %v", rawFile, pathErr)
		}
		if _, exists := seen[file]; exists {
			return invalidPackage("declared file %q is duplicated", file)
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	if _, ok := seen[declaration.Instructions]; !ok {
		return invalidPackage("instructions file must be present in files")
	}
	sort.Strings(files)
	declaration.Files = files
	sort.Strings(declaration.CompatibleRuntimes)
	return nil
}

func normalizeDeclaredPath(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" || strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" {
		return "", errors.New("path must be a portable relative path")
	}
	parts := strings.Split(raw, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("path traversal and empty segments are prohibited")
		}
	}
	if len(parts)-1 > maxDirectoryDepth {
		return "", errors.New("path exceeds eight directory levels")
	}
	return strings.Join(parts, "/"), nil
}

func hashPackage(declaration packageDeclaration, files []sourceFile) (string, error) {
	manifest, err := json.Marshal(declaration)
	if err != nil {
		return "", fmt.Errorf("encode normalized skill manifest: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	hash := sha256.New()
	if err := writeHashPart(hash, manifest); err != nil {
		return "", err
	}
	for _, file := range files {
		if err := writeHashPart(hash, []byte(file.path)); err != nil {
			return "", err
		}
		if err := writeHashPart(hash, file.data); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeHashPart(writer io.Writer, value []byte) error {
	if err := binary.Write(writer, binary.BigEndian, uint64(len(value))); err != nil {
		return err
	}
	_, err := writer.Write(value)
	return err
}

func (c *Catalog) materialize(pkg core.SkillPackage, files []sourceFile, stagingRoot string) error {
	storeRoot := filepath.Join(c.catalogRoot, "store")
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		return fmt.Errorf("create skill catalog store: %w", err)
	}
	target := filepath.Join(storeRoot, pkg.Identity.PackageHash)
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect skill store object: %w", err)
	}
	stagingStore := filepath.Join(stagingRoot, "store")
	if err := os.MkdirAll(stagingStore, 0o700); err != nil {
		return fmt.Errorf("create owned skill store staging root: %w", err)
	}
	temporary := filepath.Join(stagingStore, pkg.Identity.PackageHash)
	if err := os.Mkdir(temporary, 0o700); err != nil {
		return fmt.Errorf("create owned skill store staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode stored skill package: %w", err)
	}
	if err := writeFileDurable(filepath.Join(temporary, "package.json"), append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write stored skill package: %w", err)
	}
	for _, file := range files {
		path := filepath.Join(temporary, "files", filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create stored skill package path: %w", err)
		}
		if err := writeFileDurable(path, file.data, 0o644); err != nil {
			return fmt.Errorf("write stored skill package file %q: %w", file.path, err)
		}
	}
	if err := syncTreeDirectories(temporary); err != nil {
		return fmt.Errorf("persist owned skill store staging directory: %w", err)
	}
	if err := c.reachCheckpoint(SyncCheckpointStoreObjectStaged); err != nil {
		return err
	}
	if err := publishImmutableDirectory(temporary, target); err != nil {
		return fmt.Errorf("publish immutable skill store object: %w", err)
	}
	if err := syncDirectory(storeRoot); err != nil {
		return fmt.Errorf("persist immutable skill store object: %w", err)
	}
	return nil
}

func writeFileDurable(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func syncTreeDirectories(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncDirectory(directories[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) stageIndex(stagingRoot string, entries []EntrySummary) (string, error) {
	index := catalogIndex{SchemaVersion: 1, Aliases: cloneAliases(c.settings.Aliases), Entries: entries}
	encoded, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode skill catalog index: %w", err)
	}
	path := filepath.Join(stagingRoot, "index.json")
	if err := writeAtomic(path, append(encoded, '\n')); err != nil {
		return "", fmt.Errorf("write staged skill catalog index: %w", err)
	}
	return path, nil
}

func (c *Catalog) verifyStagedIndex(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read staged skill catalog index: %w", err)
	}
	var index catalogIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("parse staged skill catalog index: %w", err)
	}
	if index.SchemaVersion != 1 {
		return errors.New("staged skill catalog index has an unsupported schema version")
	}
	for _, summary := range index.Entries {
		if summary.State != core.SkillReady {
			continue
		}
		pkg, err := c.readStoredPackage(summary.Identity.PackageHash)
		if err != nil {
			return fmt.Errorf("verify staged skill catalog index: %w", err)
		}
		if pkg.Identity != summary.Identity {
			return fmt.Errorf("verify staged skill catalog index: store identity mismatch for %s", summary.Identity.QualifiedName())
		}
	}
	return nil
}

func (c *Catalog) publishStagedIndex(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read staged skill catalog index for publication: %w", err)
	}
	if err := writeAtomic(filepath.Join(c.catalogRoot, "index.json"), data); err != nil {
		return fmt.Errorf("publish active skill catalog index: %w", err)
	}
	return nil
}

func (c *Catalog) reachCheckpoint(checkpoint SyncCheckpoint) error {
	if c.checkpoint == nil {
		return nil
	}
	return c.checkpoint(checkpoint)
}

func summarizeEntries(entries []core.SkillCatalogEntry) []EntrySummary {
	summaries := make([]EntrySummary, 0, len(entries))
	for _, entry := range entries {
		summaries = append(summaries, EntrySummary{State: entry.State, Identity: entry.Package.Identity, Diagnostic: entry.Diagnostic})
	}
	return summaries
}

func (c *Catalog) readIndex() (catalogIndex, error) {
	index, _, err := c.readIndexData()
	return index, err
}

func (c *Catalog) readIndexData() (catalogIndex, []byte, error) {
	var index catalogIndex
	data, err := os.ReadFile(filepath.Join(c.catalogRoot, "index.json"))
	if err != nil {
		return index, nil, fmt.Errorf("read active skill catalog index: %w", err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, nil, fmt.Errorf("parse active skill catalog index: %w", err)
	}
	if index.SchemaVersion != 1 {
		return index, nil, errors.New("active skill catalog index has an unsupported schema version")
	}
	return index, data, nil
}

func (c *Catalog) readStoredPackage(packageHash string) (core.SkillPackage, error) {
	var pkg core.SkillPackage
	path := filepath.Join(c.catalogRoot, "store", packageHash, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return pkg, fmt.Errorf("read stored skill package %s: %w", packageHash, err)
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return pkg, fmt.Errorf("parse stored skill package %s: %w", packageHash, err)
	}
	return pkg, nil
}

func readStrictYAML(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("YAML input is not a regular file")
	}
	if info.Size() > maxManifestBytes {
		return errors.New("YAML input exceeds the 256 KiB manifest or lock limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxManifestBytes {
		return errors.New("YAML input exceeds the 256 KiB manifest or lock limit")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("YAML input must contain exactly one document")
		}
		return err
	}
	return nil
}

func confinedPath(root, relative string) (string, error) {
	if strings.TrimSpace(relative) == "" || filepath.IsAbs(relative) {
		return "", errors.New("path must be relative")
	}
	resolved := filepath.Clean(filepath.Join(root, relative))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes its declared skill source")
	}
	return resolved, nil
}

func addUniqueEntry(entries *[]core.SkillCatalogEntry, seen map[string]struct{}, entry core.SkillCatalogEntry) error {
	name := entry.Package.Identity.QualifiedName()
	if _, exists := seen[name]; exists {
		return fmt.Errorf("duplicate skill package %q", name)
	}
	seen[name] = struct{}{}
	*entries = append(*entries, entry)
	return nil
}

func cloneAliases(aliases map[string]string) map[string]string {
	if aliases == nil {
		return nil
	}
	cloned := make(map[string]string, len(aliases))
	for name, target := range aliases {
		cloned[name] = target
	}
	return cloned
}

func writeAtomic(path string, data []byte) error {
	return writeAtomicMode(path, data, 0o644)
}

// WritePrivateFile atomically writes data to path with mode 0600, using the
// same cross-platform atomic-replace primitives as the content-addressed
// store. `syntroph skill prepare --output` uses this to hand off a prepared
// Skill Bundle without exposing it to other local users or leaving a
// partially written file behind after an interruption.
func WritePrivateFile(path string, data []byte) error {
	return writeAtomicMode(path, data, 0o600)
}

func writeAtomicMode(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".index-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFileAtomically(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

const maxSyncDiagnosticBytes = 1024

type boundedSyncDiagnostic struct {
	cause   error
	message string
}

func (e boundedSyncDiagnostic) Error() string { return e.message }
func (e boundedSyncDiagnostic) Unwrap() error { return e.cause }

func boundSyncDiagnostic(err error) error {
	if err == nil || len(err.Error()) <= maxSyncDiagnosticBytes {
		return err
	}
	const suffix = "... [truncated]"
	message := err.Error()
	end := maxSyncDiagnosticBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(message[end]) {
		end--
	}
	return boundedSyncDiagnostic{cause: err, message: message[:end] + suffix}
}

var _ core.SkillPort = (*Catalog)(nil)
