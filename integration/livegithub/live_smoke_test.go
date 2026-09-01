//go:build livegithub

package livegithub

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/adapters/githubissues"
	"github.com/mateusememe/syntroph/adapters/githubwiki"
	"github.com/mateusememe/syntroph/storage"
)

const liveConfirmation = "I_UNDERSTAND_THIS_CREATES_PERSISTENT_GITHUB_RESOURCES"

type livePort interface {
	Mirror(context.Context, storage.SessionDiary) storage.MirrorResult
	Status(context.Context, storage.SessionDiary) storage.MirrorResult
	Resolve(context.Context, storage.SessionDiary, storage.Resolution, string) storage.MirrorResult
}

func TestConfiguredGitHubStorageProvider(t *testing.T) {
	if os.Getenv("SYNTROPH_LIVE_GITHUB_CONFIRM") != liveConfirmation {
		t.Skip("set SYNTROPH_LIVE_GITHUB_CONFIRM to the documented confirmation value")
	}
	providerID := requiredEnv(t, "SYNTROPH_LIVE_STORAGE_PROVIDER")
	repository := requiredEnv(t, "SYNTROPH_LIVE_GITHUB_REPOSITORY")
	runID := safeID(requiredEnv(t, "SYNTROPH_LIVE_RUN_ID"))
	root := filepath.Join(t.TempDir(), "storage")
	port := configuredProvider(t, providerID, repository, root)
	diary := liveDiary(providerID, runID, repository)

	first := port.Mirror(context.Background(), diary)
	if first.State != storage.Mirrored {
		t.Fatalf("live mirror failed: state=%s class=%s cause=%v", first.State, first.FailureClass, first.Cause)
	}
	if err := storage.RemoteRevision(first.RemoteRev).Validate(); err != nil {
		t.Fatalf("live mirror returned invalid typed revision %q: %v", first.RemoteRev, err)
	}
	createdRevision := first.RemoteRev
	second := port.Mirror(context.Background(), diary)
	if second.State != storage.Mirrored || second.RemoteID != first.RemoteID || second.RemoteRev != createdRevision {
		t.Fatalf("live idempotent replay changed mirror identity: first=%+v second=%+v", first, second)
	}
	status := port.Status(context.Background(), diary)
	if status.State != storage.Mirrored || status.RemoteID != first.RemoteID {
		t.Fatalf("live status did not observe the mirror: %+v", status)
	}
	bindingStore, err := storage.NewBindingStore(root)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok, err := bindingStore.Load(context.Background(), diary.Key())
	if err != nil || !ok || binding.Provider != providerID || binding.RemoteID != first.RemoteID {
		t.Fatalf("live Remote Binding mismatch: binding=%+v ok=%v err=%v", binding, ok, err)
	}
	t.Logf("LIVE_SMOKE_RESOURCE provider=%s run_id=%s key=%s remote_id=%s url=%s", providerID, runID, diary.Key(), first.RemoteID, first.RemoteURL)
}

func configuredProvider(t *testing.T, providerID, repository, root string) livePort {
	t.Helper()
	switch providerID {
	case githubissues.ProviderID:
		remote, err := githubissues.New(repository, requiredEnv(t, storage.GitHubTokenEnvironment), root, githubissues.Options{UserAgent: "syntroph/live-storage-smoke"})
		if err != nil {
			t.Fatal(err)
		}
		return managed(t, root, providerID, remote)
	case githubissues.MCPProviderID:
		var command []string
		if err := json.Unmarshal([]byte(requiredEnv(t, "SYNTROPH_LIVE_MCP_COMMAND_JSON")), &command); err != nil || len(command) == 0 {
			t.Fatalf("SYNTROPH_LIVE_MCP_COMMAND_JSON must be a JSON string array: %v", err)
		}
		remote, err := githubissues.NewMCP(repository, command, root, githubissues.MCPOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return managed(t, root, providerID, remote)
	case githubwiki.ProviderID:
		remoteURL := strings.TrimSpace(os.Getenv("SYNTROPH_LIVE_WIKI_REMOTE_URL"))
		if remoteURL == "" {
			remoteURL = "https://github.com/" + repository + ".wiki.git"
		}
		provider, err := githubwiki.NewManaged(root, githubwiki.Options{
			Repository: repository, RemoteURL: remoteURL, WebURL: "https://github.com/" + repository + "/wiki",
			GitExecutable: "git", Author: githubwiki.Author{Name: "Syntroph Live Smoke", Email: "syntroph-live-smoke@users.noreply.github.com"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return provider
	default:
		t.Fatalf("unsupported SYNTROPH_LIVE_STORAGE_PROVIDER %q", providerID)
		return nil
	}
}

func managed(t *testing.T, root, providerID string, remote livePort) *storage.ManagedMirror {
	t.Helper()
	provider, err := storage.NewManagedMirror(root, providerID, remote)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func liveDiary(providerID, runID, repository string) storage.SessionDiary {
	artifact := sha256.Sum256([]byte("syntroph-live-storage-smoke\n" + providerID + "\n" + runID))
	createdAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(binary.BigEndian.Uint64(artifact[:8])%(20*365*24*60*60)) * time.Second)
	diary := storage.SessionDiary{
		SessionID: "live-" + safeID(providerID) + "-" + runID, RepositoryID: "github.com/" + repository,
		CommitSHA: "live-smoke-" + runID, ArtifactHash: hex.EncodeToString(artifact[:]),
	}
	diary.Content = fmt.Sprintf("---\nsession_id: %s\nrepository_id: %s\ncommit_sha: %s\nartifact_hash: %s\nidempotency_key: %s\nauthor: syntroph-live-smoke\ncreated_at: %s\n---\n\n# Syntroph live storage smoke\n\nProvider: `%s`\n\nRun identity: `%s`\n", diary.SessionID, diary.RepositoryID, diary.CommitSHA, diary.ArtifactHash, diary.Key(), createdAt.Format(time.RFC3339Nano), providerID, runID)
	return diary
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required for live GitHub smoke", name)
	}
	return value
}

var unsafeID = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safeID(value string) string {
	value = strings.Trim(unsafeID.ReplaceAllString(value, "-"), "-._")
	if len(value) > 48 {
		value = value[:48]
	}
	if value == "" {
		return "unnamed"
	}
	return value
}
