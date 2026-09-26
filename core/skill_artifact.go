package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ArtifactAvailabilityVerified means session close found the artifact
	// at its repository-relative path and recorded its observed SHA-256,
	// media type, and size.
	ArtifactAvailabilityVerified = "verified"
	// ArtifactAvailabilityMissing means the artifact's repository-relative
	// path was safe, but nothing was there for session close to observe.
	// A missing artifact is historical evidence, not a failure: it never
	// blocks Session Diary persistence.
	ArtifactAvailabilityMissing = "missing"
)

// ErrSkillInvocationArtifactUnsafe reports that a Skill Invocation
// Artifact's path is not a safe, repository-relative location -- absolute,
// containing traversal segments, or escaping the repository root through a
// symlink. Session close rejects the record rather than ever opening a
// host file outside the repository root to satisfy it.
var ErrSkillInvocationArtifactUnsafe = errors.New("skill invocation artifact path is unsafe")

// normalizeArtifactPath is the syntactic half of artifact path safety and
// performs no filesystem access. It matches the portable-relative-path
// convention already used for skill package assets: no absolute paths, no
// backslashes or volume names, and no "." or ".." segments.
func normalizeArtifactPath(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", fmt.Errorf("%w: artifact path is required", ErrSkillInvocationArtifactUnsafe)
	}
	if strings.Contains(raw, "\\") || strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" {
		return "", fmt.Errorf("%w: artifact path must be a portable, repository-relative path", ErrSkillInvocationArtifactUnsafe)
	}
	parts := strings.Split(raw, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: artifact path must not contain traversal or empty segments", ErrSkillInvocationArtifactUnsafe)
		}
	}
	return strings.Join(parts, "/"), nil
}

// resolveArtifactPath re-validates an artifact's repository-relative path
// and resolves it against repositoryRoot, rejecting any path that would
// escape the repository root -- directly, or through a symlink on an
// existing ancestor -- before any file is opened. It is safe to call even
// when the artifact does not exist yet: a missing leaf, or a missing chain
// of parent directories, is walked up to the nearest existing ancestor for
// the symlink check and the missing segments are rejoined afterward,
// matching the pattern config.resolveRepositoryPath uses for Repository
// Installation paths.
func resolveArtifactPath(repositoryRoot, relative string) (string, error) {
	relative, err := normalizeArtifactPath(relative)
	if err != nil {
		return "", err
	}
	resolved := filepath.Clean(filepath.Join(repositoryRoot, relative))
	rel, err := filepath.Rel(repositoryRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: artifact path escapes the repository root", ErrSkillInvocationArtifactUnsafe)
	}
	canonicalRoot, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root symlinks: %w", err)
	}
	canonicalResolved, err := evalArtifactPathWithExistingParent(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve artifact path symlinks: %w", err)
	}
	canonicalRel, err := filepath.Rel(canonicalRoot, canonicalResolved)
	if err != nil || canonicalRel == ".." || strings.HasPrefix(canonicalRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: artifact path escapes the repository root through a symlink", ErrSkillInvocationArtifactUnsafe)
	}
	return resolved, nil
}

// evalArtifactPathWithExistingParent resolves symlinks along path, walking
// up to the nearest existing ancestor when the leaf (or several trailing
// segments) does not exist yet, then rejoins the missing segments onto the
// resolved ancestor. This lets a missing artifact still be checked for
// symlink escape through whatever part of its path does exist.
func evalArtifactPathWithExistingParent(path string) (string, error) {
	candidate := path
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", err
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}

// verifyInvocationArtifacts resolves each artifact's repository-relative
// path safely, then records observed SHA-256, best-effort media type, and
// size for whatever currently exists there. A missing artifact is recorded
// with Missing availability and never fails session close; an unsafe path
// does fail it, since accepting one would let a Session Artifact make Core
// read an arbitrary host file. Artifact contents are read only to hash and
// sniff them -- they are never retained on the returned record, so nothing
// here (and nothing in the immutable Session Diary it feeds, local or
// remotely mirrored) ever carries a file's body.
func verifyInvocationArtifacts(repositoryRoot string, artifacts []SkillInvocationArtifact) ([]SkillInvocationArtifact, error) {
	if len(artifacts) == 0 {
		return artifacts, nil
	}
	out := make([]SkillInvocationArtifact, len(artifacts))
	for i, artifact := range artifacts {
		resolved, err := resolveArtifactPath(repositoryRoot, artifact.Path)
		if err != nil {
			return nil, err
		}
		verified, err := verifyArtifactFile(artifact.Path, resolved)
		if err != nil {
			return nil, err
		}
		out[i] = verified
	}
	return out, nil
}

// verifyArtifactFile observes resolvedPath -- already safety-checked by
// resolveArtifactPath -- and reports declaredPath (the portable,
// repository-relative path) alongside whatever it found. Any failure to
// observe the file (missing, a directory in its place, a transient I/O
// error) is recorded as Missing rather than propagated: only an unsafe
// path is ever allowed to fail session close.
func verifyArtifactFile(declaredPath, resolvedPath string) (SkillInvocationArtifact, error) {
	missing := SkillInvocationArtifact{Path: declaredPath, Availability: ArtifactAvailabilityMissing}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return missing, nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return missing, nil
	}
	sniff := make([]byte, 512)
	n, err := io.ReadFull(file, sniff)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return missing, nil
	}
	mediaType := http.DetectContentType(sniff[:n])
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return missing, nil
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return missing, nil
	}
	return SkillInvocationArtifact{
		Path:         declaredPath,
		SHA256:       hex.EncodeToString(hash.Sum(nil)),
		MediaType:    mediaType,
		SizeBytes:    size,
		Availability: ArtifactAvailabilityVerified,
	}, nil
}
