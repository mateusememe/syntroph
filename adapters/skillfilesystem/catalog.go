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

	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
	"gopkg.in/yaml.v3"
)

const (
	RuntimeLockSchemaVersion = 1
	LocalManifestName        = "skill.yaml"
)

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
	Entries []EntrySummary `json:"entries"`
}

type Catalog struct {
	mu          sync.Mutex
	catalogRoot string
	settings    config.ResolvedSkills
	preparer    core.SkillPort
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
		catalogRoot: filepath.Join(root, ".syntroph", "catalog"),
		settings:    settings,
	}, nil
}

func (c *Catalog) Sync(ctx context.Context) (SyncResult, error) {
	if err := ctx.Err(); err != nil {
		return SyncResult{}, err
	}
	var lock runtimeLock
	if err := readStrictYAML(c.settings.RuntimeLock, &lock); err != nil {
		if os.IsNotExist(err) {
			return SyncResult{}, fmt.Errorf("runtime skill lock is missing at %s; create or restore the configured lock before running syntroph skill sync", c.settings.RuntimeLock)
		}
		return SyncResult{}, fmt.Errorf("read runtime skill lock: %w", err)
	}
	if lock.SchemaVersion != RuntimeLockSchemaVersion {
		return SyncResult{}, fmt.Errorf("runtime skill lock schema_version must be %d", RuntimeLockSchemaVersion)
	}
	sources := make(map[string]config.ResolvedSkillSource, len(c.settings.Sources))
	for _, source := range c.settings.Sources {
		sources[source.ID] = source
	}

	entries := make([]core.SkillCatalogEntry, 0, len(lock.Packages))
	seen := make(map[string]struct{})
	for _, declaration := range lock.Packages {
		if err := ctx.Err(); err != nil {
			return SyncResult{}, err
		}
		if declaration.SourceID == "local" {
			return SyncResult{}, errors.New("local Skill Packages must use skill.yaml manifests, not runtime lock declarations")
		}
		if declaration.SchemaVersion == 0 {
			declaration.SchemaVersion = lock.SchemaVersion
		}
		source, ok := sources[declaration.SourceID]
		if !ok || declaration.SourceID == "" {
			return SyncResult{}, fmt.Errorf("runtime lock package %q references undeclared source %q", declaration.Name, declaration.SourceID)
		}
		entry, err := c.loadPackage(source, declaration.Directory, declaration)
		if err != nil {
			return SyncResult{}, err
		}
		if err := addUniqueEntry(&entries, seen, entry); err != nil {
			return SyncResult{}, err
		}
	}
	if local, ok := sources["local"]; ok {
		localEntries, err := c.loadLocalPackages(ctx, local)
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
	if err := c.publishIndex(summaries); err != nil {
		return SyncResult{}, err
	}
	preparer, err := core.NewInMemorySkillPort(entries, nil)
	if err != nil {
		return SyncResult{}, fmt.Errorf("activate skill catalog preparer: %w", err)
	}
	c.mu.Lock()
	c.preparer = preparer
	c.mu.Unlock()
	return SyncResult{Entries: summaries}, nil
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

func (c *Catalog) Prepare(ctx context.Context, request core.SkillPrepareRequest) (core.SkillBundle, error) {
	c.mu.Lock()
	preparer := c.preparer
	c.mu.Unlock()
	if preparer == nil {
		entries, err := c.loadEntries(ctx)
		if err != nil {
			return core.SkillBundle{}, err
		}
		prepared, err := core.NewInMemorySkillPort(entries, nil)
		if err != nil {
			return core.SkillBundle{}, err
		}
		c.mu.Lock()
		if c.preparer == nil {
			c.preparer = prepared
		}
		preparer = c.preparer
		c.mu.Unlock()
	}
	return preparer.Prepare(ctx, request)
}

func (c *Catalog) loadEntries(ctx context.Context) ([]core.SkillCatalogEntry, error) {
	index, err := c.readIndex()
	if err != nil {
		return nil, err
	}
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

func (c *Catalog) loadLocalPackages(ctx context.Context, source config.ResolvedSkillSource) ([]core.SkillCatalogEntry, error) {
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
			return nil, fmt.Errorf("read local skill manifest %s: %w", manifestPath, err)
		}
		if declaration.SourceID != "" || declaration.Directory != "" {
			return nil, fmt.Errorf("local skill manifest %s must not declare source_id or directory", manifestPath)
		}
		declaration.SourceID = "local"
		entry, err := c.loadPackage(source, directory.Name(), declaration)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (c *Catalog) loadPackage(source config.ResolvedSkillSource, directory string, declaration packageDeclaration) (core.SkillCatalogEntry, error) {
	if err := normalizeDeclaration(&declaration); err != nil {
		return core.SkillCatalogEntry{}, fmt.Errorf("skill package %s/%s: %w", source.ID, declaration.Name, err)
	}
	if source.ID != declaration.SourceID {
		return core.SkillCatalogEntry{}, fmt.Errorf("skill package %s/%s source identity does not match its declared source", source.ID, declaration.Name)
	}
	packageRoot, err := confinedPath(source.Root, directory)
	if err != nil {
		return core.SkillCatalogEntry{}, fmt.Errorf("skill package %s/%s: %w", source.ID, declaration.Name, err)
	}
	files := make([]sourceFile, 0, len(declaration.Files))
	for _, relative := range declaration.Files {
		path, err := confinedPath(packageRoot, filepath.FromSlash(relative))
		if err != nil {
			return core.SkillCatalogEntry{}, fmt.Errorf("skill package %s/%s file %q: %w", source.ID, declaration.Name, relative, err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return core.SkillCatalogEntry{}, fmt.Errorf("skill package %s/%s file %q: %w", source.ID, declaration.Name, relative, err)
		}
		if !info.Mode().IsRegular() {
			return core.SkillCatalogEntry{}, fmt.Errorf("skill package %s/%s file %q is not a regular file", source.ID, declaration.Name, relative)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return core.SkillCatalogEntry{}, fmt.Errorf("read skill package %s/%s file %q: %w", source.ID, declaration.Name, relative, err)
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
	if err := c.materialize(pkg, files); err != nil {
		return core.SkillCatalogEntry{}, err
	}
	return core.SkillCatalogEntry{State: core.SkillReady, Package: pkg}, nil
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
	declaration.Instructions = filepath.ToSlash(filepath.Clean(strings.TrimSpace(declaration.Instructions)))
	if declaration.SchemaVersion != core.SkillSchemaVersion {
		return fmt.Errorf("schema_version must be %d", core.SkillSchemaVersion)
	}
	if declaration.SourceID == "" || declaration.Name == "" || declaration.Description == "" || declaration.License == "" || declaration.SourceURL == "" || declaration.SourceRevision == "" {
		return errors.New("source identity, name, description, license, source_url, and source_revision are required")
	}
	if declaration.Instructions == "." || declaration.Instructions == "" {
		return errors.New("instructions file is required")
	}
	seen := make(map[string]struct{}, len(declaration.Files))
	files := make([]string, 0, len(declaration.Files))
	for _, file := range declaration.Files {
		file = filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
		if file == "." || file == "" || strings.HasPrefix(file, "../") || filepath.IsAbs(file) {
			return fmt.Errorf("declared file %q must be a package-relative path", file)
		}
		if _, exists := seen[file]; exists {
			return fmt.Errorf("declared file %q is duplicated", file)
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	if _, ok := seen[declaration.Instructions]; !ok {
		return errors.New("instructions file must be present in files")
	}
	sort.Strings(files)
	declaration.Files = files
	sort.Strings(declaration.CompatibleRuntimes)
	return nil
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

func (c *Catalog) materialize(pkg core.SkillPackage, files []sourceFile) error {
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
	temporary, err := os.MkdirTemp(storeRoot, ".package-*")
	if err != nil {
		return fmt.Errorf("create skill store staging directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	encoded, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode stored skill package: %w", err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "package.json"), append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write stored skill package: %w", err)
	}
	for _, file := range files {
		path := filepath.Join(temporary, "files", filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create stored skill package path: %w", err)
		}
		if err := os.WriteFile(path, file.data, 0o644); err != nil {
			return fmt.Errorf("write stored skill package file %q: %w", file.path, err)
		}
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("publish immutable skill store object: %w", err)
	}
	return nil
}

func (c *Catalog) publishIndex(entries []EntrySummary) error {
	index := catalogIndex{SchemaVersion: 1, Aliases: cloneAliases(c.settings.Aliases), Entries: entries}
	encoded, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("encode skill catalog index: %w", err)
	}
	if err := os.MkdirAll(c.catalogRoot, 0o755); err != nil {
		return fmt.Errorf("create skill catalog: %w", err)
	}
	return writeAtomic(filepath.Join(c.catalogRoot, "index.json"), append(encoded, '\n'))
}

func summarizeEntries(entries []core.SkillCatalogEntry) []EntrySummary {
	summaries := make([]EntrySummary, 0, len(entries))
	for _, entry := range entries {
		summaries = append(summaries, EntrySummary{State: entry.State, Identity: entry.Package.Identity, Diagnostic: entry.Diagnostic})
	}
	return summaries
}

func (c *Catalog) readIndex() (catalogIndex, error) {
	var index catalogIndex
	data, err := os.ReadFile(filepath.Join(c.catalogRoot, "index.json"))
	if err != nil {
		return index, fmt.Errorf("read active skill catalog index: %w", err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, fmt.Errorf("parse active skill catalog index: %w", err)
	}
	if index.SchemaVersion != 1 {
		return index, errors.New("active skill catalog index has an unsupported schema version")
	}
	return index, nil
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
	data, err := os.ReadFile(path)
	if err != nil {
		return err
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
	temporary, err := os.CreateTemp(filepath.Dir(path), ".index-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o644); err != nil {
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
	return os.Rename(name, path)
}

var _ core.SkillPort = (*Catalog)(nil)
