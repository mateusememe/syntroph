package githubissues

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/storage"
	"github.com/mateusememe/syntroph/storage/contracttest"
)

func TestGitHubRESTStoragePortContract(t *testing.T) {
	contracttest.Run(t, ProviderID, newRESTContractFixture)
}

func TestGitHubMCPStoragePortContract(t *testing.T) {
	contracttest.Run(t, MCPProviderID, newMCPContractFixture)
}

type restContractRemote struct {
	mu       sync.Mutex
	issue    issue
	comments []comment
	creates  int
}

func newRESTContractFixture(t *testing.T, root string) contracttest.Fixture {
	t.Helper()
	remote := &restContractRemote{}
	server := httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	t.Cleanup(server.Close)
	provider, err := New("mateusememe/syntroph", "contract-token", root, Options{
		HTTPClient: server.Client(), BaseURL: server.URL, UserAgent: "syntroph/contract",
		Sleep: noSleep, Jitter: noJitter,
	})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := storage.NewManagedMirror(root, ProviderID, provider)
	if err != nil {
		t.Fatal(err)
	}
	return contracttest.Fixture{
		Port: managed, Bindings: managed.Bindings, Backend: storage.BackendIssues, Provider: ProviderID,
		CreateCount: func() int { remote.mu.Lock(); defer remote.mu.Unlock(); return remote.creates },
		MutateRemote: func(t *testing.T, _ storage.MirrorResult, content string) {
			remote.mu.Lock()
			defer remote.mu.Unlock()
			remote.issue.Body, remote.issue.UpdatedAt = content, mustTime("2026-09-01T12:05:00Z")
		},
	}
}

func (r *restContractRemote) serveHTTP(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/labels/"):
		name := filepath.Base(request.URL.Path)
		for _, candidate := range reservedLabels {
			if candidate.Name == name {
				_ = json.NewEncoder(w).Encode(candidate)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/issues"):
		if r.issue.Number != 0 && r.issue.State == request.URL.Query().Get("state") {
			_ = json.NewEncoder(w).Encode([]issue{r.issue})
		} else {
			fmt.Fprint(w, `[]`)
		}
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/issues/42/comments"):
		_ = json.NewEncoder(w).Encode(r.comments)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/issues/42"):
		_ = json.NewEncoder(w).Encode(r.issue)
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/issues/42/comments"):
		var payload map[string]string
		_ = json.NewDecoder(request.Body).Decode(&payload)
		value := comment{ID: int64(77 + len(r.comments)), HTMLURL: "https://github.test/issues/42#comment", Body: payload["body"], UpdatedAt: mustTime("2026-09-01T12:06:00Z")}
		r.comments = append(r.comments, value)
		_ = json.NewEncoder(w).Encode(value)
	case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/issues/comments/"):
		id, _ := strconv.ParseInt(filepath.Base(request.URL.Path), 10, 64)
		for _, candidate := range r.comments {
			if candidate.ID == id {
				_ = json.NewEncoder(w).Encode(candidate)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/issues"):
		var payload struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		r.creates++
		r.issue = issue{Number: 42, HTMLURL: "https://github.test/issues/42", Body: payload.Body, UpdatedAt: mustTime("2026-09-01T12:00:00Z"), State: "open"}
		_ = json.NewEncoder(w).Encode(r.issue)
	case request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/issues/42"):
		r.issue.State, r.issue.StateReason, r.issue.UpdatedAt = "closed", "completed", mustTime("2026-09-01T12:01:00Z")
		_ = json.NewEncoder(w).Encode(r.issue)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"message":"unexpected contract request %s %s"}`, request.Method, request.URL.Path)
	}
}

func newMCPContractFixture(t *testing.T, root string) contracttest.Fixture {
	t.Helper()
	state := filepath.Join(t.TempDir(), "remote.json")
	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("GO_WANT_MCP_HELPER", "provider")
	t.Setenv("MCP_HELPER_STATE", state)
	t.Setenv("MCP_HELPER_LOG", logPath)
	remote, err := NewMCP("mateusememe/syntroph", helperCommand(), root, MCPOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := storage.NewManagedMirror(root, MCPProviderID, remote)
	if err != nil {
		t.Fatal(err)
	}
	return contracttest.Fixture{
		Port: managed, Bindings: managed.Bindings, Backend: storage.BackendIssues, Provider: MCPProviderID,
		CreateCount: func() int { data, _ := os.ReadFile(logPath); return strings.Count(string(data), "issue_write:create") },
		MutateRemote: func(t *testing.T, _ storage.MirrorResult, content string) {
			value := readHelperState()
			value.Issue.Body, value.Issue.UpdatedAt = content, mustTime("2026-09-01T12:05:00Z")
			writeHelperState(value)
		},
	}
}
