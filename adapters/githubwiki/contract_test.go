package githubwiki

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateusememe/syntroph/storage"
	"github.com/mateusememe/syntroph/storage/contracttest"
)

func TestGitHubWikiStoragePortContract(t *testing.T) {
	contracttest.Run(t, ProviderID, func(t *testing.T, root string) contracttest.Fixture {
		remote := initializedWiki(t)
		managed, err := storage.NewManagedMirror(root, ProviderID, testProvider(t, remote))
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Fixture{
			Port: managed, Bindings: managed.Bindings, Backend: storage.BackendWiki, Provider: ProviderID,
			CreateCount: func() int {
				return parseCount(t, strings.TrimSpace(git(t, "--git-dir", remote, "rev-list", "--count", "--all")))
			},
			MutateRemote: func(t *testing.T, created storage.MirrorResult, content string) {
				editWikiPage(t, remote, filepath.ToSlash(created.RemoteID), content)
			},
		}
	})
}

func parseCount(t *testing.T, value string) int {
	t.Helper()
	count := 0
	if _, err := fmt.Sscan(value, &count); err != nil {
		t.Fatalf("parse git commit count %q: %v", value, err)
	}
	return count
}
