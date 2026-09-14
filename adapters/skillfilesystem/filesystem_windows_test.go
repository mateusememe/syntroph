//go:build windows

package skillfilesystem

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsDurablePublicationPrimitives(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	lock := filepath.Join(root, "lock")
	if err := os.WriteFile(candidate, []byte("owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if published, err := publishLockCandidate(candidate, lock); err != nil || !published {
		t.Fatalf("publish lock = %v, %v", published, err)
	}
	second := filepath.Join(root, "second")
	if err := os.WriteFile(second, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if published, err := publishLockCandidate(second, lock); published || !errors.Is(err, os.ErrExist) {
		t.Fatalf("no-replace lock = %v, %v", published, err)
	}

	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("next"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFileAtomically(replacement, lock); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(lock); err != nil || string(data) != "next" {
		t.Fatalf("replacement = %q, %v", data, err)
	}

	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "object"), []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "published")
	if err := publishImmutableDirectory(directory, target); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "object")); err != nil || string(data) != "durable" {
		t.Fatalf("published directory = %q, %v", data, err)
	}
}
