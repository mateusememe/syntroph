//go:build !windows

package skillfilesystem

import (
	"os"
	"path/filepath"
)

func publishLockCandidate(candidate, target string) (bool, error) {
	if err := os.Link(candidate, target); err != nil {
		return false, err
	}
	if err := os.Remove(candidate); err != nil {
		return true, err
	}
	return true, nil
}

func replaceFileAtomically(source, target string) error {
	return os.Rename(source, target)
}

func publishImmutableDirectory(source, target string) error {
	return os.Rename(source, target)
}

func syncDirectory(path string) error {
	directory, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
