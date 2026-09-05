package githubissues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

func testDiary() storage.SessionDiary {
	return storage.SessionDiary{SessionID: "session-7", RepositoryID: "github.com/mateusememe/syntroph", CommitSHA: "abc123", ArtifactHash: strings.Repeat("a", 64), Content: "---\ncreated_at: 2026-08-31T12:00:00Z\n---\n\n# Diary\n\nLearned things.\n"}
}

func TestMirrorCreatesLabelsIssueAndCompletedClosure(t *testing.T) {
	var mu sync.Mutex
	var sequence []string
	now := "2026-08-31T12:00:00Z"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer explicit-token" || r.Header.Get("X-GitHub-Api-Version") != APIVersion || r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("User-Agent") != "syntroph/test" {
			t.Errorf("missing pinned headers: %#v", r.Header)
		}
		mu.Lock()
		sequence = append(sequence, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues") && r.URL.Query().Get("state") != "":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var got label
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(got)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			var got struct {
				Title, Body string
				Labels      []string
			}
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got.Title != "[Syntroph] 2026-08-31 — session-7" || !strings.Contains(got.Body, markerPrefix+testDiary().Key()) || strings.Join(got.Labels, ",") != "syntroph-memory,syntroph-session" {
				t.Errorf("create payload: %+v", got)
			}
			_ = json.NewEncoder(w).Encode(issue{Number: 42, HTMLURL: "https://github.test/issues/42", Body: got.Body, UpdatedAt: mustTime(now), State: "open"})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/42"):
			var got map[string]string
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got["state"] != "closed" || got["state_reason"] != "completed" {
				t.Errorf("close payload: %+v", got)
			}
			_ = json.NewEncoder(w).Encode(issue{Number: 42, HTMLURL: "https://github.test/issues/42", Body: diaryBody(testDiary().Content, testDiary().Key()), UpdatedAt: mustTime(now), State: "closed", StateReason: "completed"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	p := newTestProvider(t, server)
	r := p.Mirror(context.Background(), testDiary())
	if r.State != storage.Mirrored || r.RemoteID != "42" || !strings.HasPrefix(r.RemoteRev, "issue:42:") {
		t.Fatalf("mirror result: %+v", r)
	}
	if strings.Count(strings.Join(sequence, "\n"), "POST /repos/mateusememe/syntroph/labels") != 2 {
		t.Fatalf("sequence: %v", sequence)
	}
}

func TestNewRequiresOnlyTheExplicitSyntrophToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "must-not-be-used")
	_, err := New("mateusememe/syntroph", "", filepath.Join(t.TempDir(), "storage"), Options{})
	if !errors.Is(err, storage.ErrPrerequisiteMissing) || !strings.Contains(err.Error(), "SYNTROPH_GITHUB_TOKEN") {
		t.Fatalf("missing explicit token error = %v", err)
	}
}

func TestMirrorReconcilesReservedLabelsBeforeCreatingIssue(t *testing.T) {
	d := testDiary()
	var sequence []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sequence = append(sequence, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("state") != "":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels/syntroph-memory"):
			_ = json.NewEncoder(w).Encode(label{Name: "syntroph-memory", Color: "ffffff", Description: "wrong"})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/labels/syntroph-memory"):
			var got map[string]string
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got["color"] != "00e5ff" || got["description"] != reservedLabels[0].Description {
				t.Errorf("memory label update = %#v", got)
			}
			_ = json.NewEncoder(w).Encode(reservedLabels[0])
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels/syntroph-session"):
			_ = json.NewEncoder(w).Encode(reservedLabels[1])
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			var got struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(issue{Number: 2, HTMLURL: "https://github.test/issues/2", Body: got.Body, UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "open"})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/2"):
			_ = json.NewEncoder(w).Encode(issue{Number: 2, HTMLURL: "https://github.test/issues/2", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:01:00Z"), State: "closed", StateReason: "completed"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	result := newTestProvider(t, server).Mirror(context.Background(), d)
	if result.State != storage.Mirrored {
		t.Fatalf("mirror result = %+v", result)
	}
	wantOrder := []string{
		"GET /repos/mateusememe/syntroph/labels/syntroph-memory",
		"PATCH /repos/mateusememe/syntroph/labels/syntroph-memory",
		"GET /repos/mateusememe/syntroph/labels/syntroph-session",
		"POST /repos/mateusememe/syntroph/issues",
		"PATCH /repos/mateusememe/syntroph/issues/2",
	}
	if strings.Join(sequence, "\n") != strings.Join(wantOrder, "\n") {
		t.Fatalf("request order:\n%s", strings.Join(sequence, "\n"))
	}
}

func TestMirrorDoesNotCreateIssueWhenLabelsCannotBeManaged(t *testing.T) {
	issueCreates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("state") != "":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			issueCreates++
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()

	result := newTestProvider(t, server).Mirror(context.Background(), testDiary())
	if result.State != storage.StoragePrerequisiteMissing || result.FailureClass != storage.FailurePrerequisite || issueCreates != 0 {
		t.Fatalf("result=%+v issueCreates=%d", result, issueCreates)
	}
}

func TestEnsureLabelsRecoversFromConcurrentCreateRace(t *testing.T) {
	gets := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name, _ := url.PathUnescape(filepath.Base(r.URL.Path))
		switch {
		case r.Method == http.MethodGet:
			gets[name]++
			if gets[name] == 1 {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
				return
			}
			for _, candidate := range reservedLabels {
				if candidate.Name == name {
					_ = json.NewEncoder(w).Encode(candidate)
					return
				}
			}
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"code":"already_exists"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	p := newTestProvider(t, server)
	if err := p.ensureLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range reservedLabels {
		if gets[want.Name] != 2 {
			t.Fatalf("label %s GET count = %d", want.Name, gets[want.Name])
		}
	}
}

func TestRecoveryIgnoresMarkedOpenIssueAndNormalMirrorDoesNotScan(t *testing.T) {
	d := testDiary()
	creates := 0
	closes := 0
	labels := 0
	scans := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("state") == "closed":
			scans++
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			labels++
			name, _ := url.PathUnescape(filepath.Base(r.URL.Path))
			_ = json.NewEncoder(w).Encode(label{Name: name, Color: map[string]string{"syntroph-memory": "00e5ff", "syntroph-session": "a855f7"}[name], Description: map[string]string{"syntroph-memory": "Immutable Syntroph session memory.", "syntroph-session": "Syntroph session diary mirror."}[name]})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/9"):
			closes++
			_ = json.NewEncoder(w).Encode(issue{Number: 9, HTMLURL: "https://github.test/issues/9", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:01:00Z"), State: "closed", StateReason: "completed"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			creates++
			_ = json.NewEncoder(w).Encode(issue{Number: 9, HTMLURL: "https://github.test/issues/9", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "open"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	p := newTestProvider(t, server)
	r := p.Mirror(context.Background(), d)
	if r.State != storage.Mirrored || r.RemoteID != "9" || creates != 1 || closes != 1 || scans != 0 || labels != 2 {
		t.Fatalf("normal result=%+v creates=%d closes=%d scans=%d", r, creates, closes, scans)
	}
}

func TestMirrorResumesAfterCreateWithoutDuplicatingTheIssue(t *testing.T) {
	d := testDiary()
	created := false
	closeFailures := 0
	issueCreates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("state") == "closed":
			if closeFailures == 3 {
				_ = json.NewEncoder(w).Encode([]issue{{Number: 31, HTMLURL: "https://github.test/issues/31", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:01:00Z"), State: "closed", StateReason: "completed"}})
			} else {
				fmt.Fprint(w, `[]`)
			}
		case r.Method == http.MethodGet && r.URL.Query().Get("state") == "open":
			if !created {
				fmt.Fprint(w, `[]`)
				return
			}
			_ = json.NewEncoder(w).Encode([]issue{{Number: 31, HTMLURL: "https://github.test/issues/31", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "open"}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			name, _ := url.PathUnescape(filepath.Base(r.URL.Path))
			for _, candidate := range reservedLabels {
				if candidate.Name == name {
					_ = json.NewEncoder(w).Encode(candidate)
					return
				}
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			issueCreates++
			created = true
			_ = json.NewEncoder(w).Encode(issue{Number: 31, HTMLURL: "https://github.test/issues/31", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "open"})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/31"):
			if closeFailures < 3 {
				closeFailures++
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"message":"temporarily unavailable"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(issue{Number: 31, HTMLURL: "https://github.test/issues/31", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:01:00Z"), State: "closed", StateReason: "completed"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	p := newTestProvider(t, server)

	first := p.Mirror(context.Background(), d)
	if first.State != storage.StorageSyncPending || issueCreates != 1 || closeFailures != 3 {
		t.Fatalf("first=%+v creates=%d closeFailures=%d", first, issueCreates, closeFailures)
	}
	second := p.Recover(context.Background(), d)
	if second.State != storage.Mirrored || second.RemoteID != "31" || issueCreates != 1 {
		t.Fatalf("second=%+v creates=%d", second, issueCreates)
	}
}

func TestMirrorRecoversClosedIssueWhenBindingWasNotPersisted(t *testing.T) {
	d := testDiary()
	closed := false
	issueCreates := 0
	closeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("state") == "closed":
			if closed {
				_ = json.NewEncoder(w).Encode([]issue{{Number: 32, HTMLURL: "https://github.test/issues/32", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:01:00Z"), State: "closed", StateReason: "completed"}})
			} else {
				fmt.Fprint(w, `[]`)
			}
		case r.Method == http.MethodGet && r.URL.Query().Get("state") == "open":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			name, _ := url.PathUnescape(filepath.Base(r.URL.Path))
			for _, candidate := range reservedLabels {
				if candidate.Name == name {
					_ = json.NewEncoder(w).Encode(candidate)
					return
				}
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			issueCreates++
			_ = json.NewEncoder(w).Encode(issue{Number: 32, HTMLURL: "https://github.test/issues/32", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "open"})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/32"):
			closeCalls++
			closed = true
			if closeCalls <= 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"message":"response lost"}`)
				return
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	p := newTestProvider(t, server)

	first := p.Mirror(context.Background(), d)
	if first.State != storage.StorageSyncPending || !closed {
		t.Fatalf("first=%+v closed=%v", first, closed)
	}
	second := p.Recover(context.Background(), d)
	if second.State != storage.Mirrored || issueCreates != 1 || closeCalls != 3 {
		t.Fatalf("second=%+v creates=%d closes=%d", second, issueCreates, closeCalls)
	}
}

func TestConflictResolutionIsAppendOnly(t *testing.T) {
	d := testDiary()
	remoteBody := "human edit"
	issueURL := "https://github.test/issues/9"
	patches := 0
	comments := 0
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9"):
			_ = json.NewEncoder(w).Encode(issue{Number: 9, HTMLURL: issueURL, Body: remoteBody, UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "closed", StateReason: "completed"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			comments++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !strings.Contains(body["body"], "syntroph-remote-correction") || !strings.HasPrefix(body["body"], d.Content) {
				t.Errorf("correction body %q", body["body"])
			}
			_ = json.NewEncoder(w).Encode(comment{ID: 77, HTMLURL: "https://github.test/issues/9#issuecomment-77", Body: body["body"], UpdatedAt: mustTime("2026-08-31T12:02:00Z")})
		case r.Method == http.MethodPatch:
			patches++
			w.WriteHeader(500)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(root, "storage"), Options{HTTPClient: server.Client(), BaseURL: server.URL, UserAgent: "syntroph/test", Sleep: noSleep, Jitter: noJitter})
	if err != nil {
		t.Fatal(err)
	}
	binding := storage.RemoteBinding{IdempotencyKey: d.Key(), Backend: storage.BackendIssues, Provider: ProviderID, RemoteID: "9", URL: issueURL, RemoteRevision: storage.RemoteRevision("issue:9:2026-08-31T12:00:00Z:" + hash(remoteBody)), LocalHash: hash(d.Content), EffectiveRemoteHash: hash(remoteBody), UpdatedAt: time.Now().UTC()}
	if err := p.bindings.Save(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	r := p.Resolve(context.Background(), d, storage.KeepLocal, string(binding.RemoteRevision))
	if r.State != storage.Mirrored || !strings.HasPrefix(r.RemoteRev, "comment:77:") || comments != 1 || patches != 0 {
		t.Fatalf("result=%+v comments=%d patches=%d", r, comments, patches)
	}
	r = p.Resolve(context.Background(), d, storage.KeepRemote, "issue:9:wrong")
	if r.State != storage.StorageSyncConflict || comments != 1 || patches != 0 {
		t.Fatalf("stale resolution: %+v", r)
	}
}

func TestKeepRemoteAcceptsCanonicalRemoteContentWithoutMutation(t *testing.T) {
	d := testDiary()
	remoteContent := "human-authored correction"
	remote := issue{Number: 18, HTMLURL: "https://github.test/issues/18", Body: diaryBody(remoteContent, d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "closed", StateReason: "completed"}
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/18") {
			_ = json.NewEncoder(w).Encode(remote)
			return
		}
		mutations++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	root := t.TempDir()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(root, "storage"), testOptions(server))
	if err != nil {
		t.Fatal(err)
	}
	binding := storage.RemoteBinding{IdempotencyKey: d.Key(), Backend: storage.BackendIssues, Provider: ProviderID, RemoteID: "18", URL: remote.HTMLURL, RemoteRevision: storage.RemoteRevision(issueRevision(remote)), LocalHash: hash(d.Content), EffectiveRemoteHash: hash(remoteContent), UpdatedAt: time.Now().UTC()}
	if err := p.bindings.Save(context.Background(), binding); err != nil {
		t.Fatal(err)
	}

	result := p.Resolve(context.Background(), d, storage.KeepRemote, issueRevision(remote))
	if result.State != storage.Mirrored || result.EffectiveRemoteHash != hash(remoteContent) || result.RemoteContent != remoteContent || mutations != 0 {
		t.Fatalf("result=%+v mutations=%d", result, mutations)
	}
}

func TestCorrectionRevisionCanBeResolvedAgain(t *testing.T) {
	d := testDiary()
	oldContent := "edited correction"
	oldCorrection := comment{ID: 77, Body: oldContent + "\n\n<!-- syntroph-remote-correction:key=" + d.Key() + ";version=" + hash("older") + " -->\n", UpdatedAt: mustTime("2026-08-31T12:02:00Z")}
	comments := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/comments/77"):
			_ = json.NewEncoder(w).Encode(oldCorrection)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			comments++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(comment{ID: 78, Body: body["body"], UpdatedAt: mustTime("2026-08-31T12:03:00Z")})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(root, "storage"), testOptions(server))
	if err != nil {
		t.Fatal(err)
	}
	binding := storage.RemoteBinding{IdempotencyKey: d.Key(), Backend: storage.BackendIssues, Provider: ProviderID, RemoteID: "9", URL: "https://github.test/issues/9", RemoteRevision: storage.RemoteRevision(commentRevision(oldCorrection)), LocalHash: hash(d.Content), EffectiveRemoteHash: hash(oldContent), UpdatedAt: time.Now().UTC()}
	if err := p.bindings.Save(context.Background(), binding); err != nil {
		t.Fatal(err)
	}

	result := p.Resolve(context.Background(), d, storage.KeepLocal, commentRevision(oldCorrection))
	if result.State != storage.Mirrored || !strings.HasPrefix(result.RemoteRev, "comment:78:") || comments != 1 {
		t.Fatalf("result=%+v comments=%d", result, comments)
	}
}

func TestCorrectionReplayReusesExactVersionedCommentAfterBindingCrash(t *testing.T) {
	d := testDiary()
	remoteBody := "human edit"
	predecessor := "issue:9:2026-08-31T12:00:00Z:" + hash(remoteBody)
	existing := comment{ID: 91, HTMLURL: "https://github.test/issues/9#issuecomment-91", Body: correctionBody(d.Content, d.Key(), predecessor), UpdatedAt: mustTime("2026-08-31T12:03:00Z")}
	commentCreates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9"):
			_ = json.NewEncoder(w).Encode(issue{Number: 9, HTMLURL: "https://github.test/issues/9", Body: remoteBody, UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "closed", StateReason: "completed"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			_ = json.NewEncoder(w).Encode([]comment{existing})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			commentCreates++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(root, "storage"), testOptions(server))
	if err != nil {
		t.Fatal(err)
	}
	binding := storage.RemoteBinding{IdempotencyKey: d.Key(), Backend: storage.BackendIssues, Provider: ProviderID, RemoteID: "9", URL: "https://github.test/issues/9", RemoteRevision: storage.RemoteRevision("issue:9:2026-08-31T12:00:00Z:" + hash(remoteBody)), LocalHash: hash(d.Content), EffectiveRemoteHash: hash(remoteBody), UpdatedAt: time.Now().UTC()}
	if err := p.bindings.Save(context.Background(), binding); err != nil {
		t.Fatal(err)
	}

	result := p.Resolve(context.Background(), d, storage.KeepLocal, string(binding.RemoteRevision))
	if result.State != storage.Mirrored || result.RemoteRev != commentRevision(existing) || commentCreates != 0 {
		t.Fatalf("result=%+v commentCreates=%d", result, commentCreates)
	}
}

func TestCorrectionReplayRequiresTheObservedPredecessorRevision(t *testing.T) {
	d := testDiary()
	oldRemote, newerRemote := "first human edit", "newer human edit"
	oldRevision := "issue:9:2026-08-31T12:00:00Z:" + hash(oldRemote)
	newerIssue := issue{Number: 9, HTMLURL: "https://github.test/issues/9", Body: newerRemote, UpdatedAt: mustTime("2026-08-31T12:04:00Z"), State: "closed", StateReason: "completed"}
	oldCorrection := comment{ID: 91, Body: correctionBody(d.Content, d.Key(), oldRevision), UpdatedAt: mustTime("2026-08-31T12:03:00Z")}
	created := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9"):
			_ = json.NewEncoder(w).Encode(newerIssue)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			_ = json.NewEncoder(w).Encode([]comment{oldCorrection})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/9/comments"):
			created++
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			marker, ok := parseCorrectionMarker(payload["body"], d.Key())
			if !ok || marker.Predecessor != issueRevision(newerIssue) {
				t.Fatalf("new correction predecessor = %+v ok=%v", marker, ok)
			}
			_ = json.NewEncoder(w).Encode(comment{ID: 92, Body: payload["body"], UpdatedAt: mustTime("2026-08-31T12:05:00Z")})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
	}))
	defer server.Close()
	p := newTestProvider(t, server)
	binding := storage.RemoteBinding{IdempotencyKey: d.Key(), Backend: storage.BackendIssues, Provider: ProviderID, RemoteID: "9", URL: newerIssue.HTMLURL, RemoteRevision: storage.RemoteRevision(oldRevision), LocalHash: hash(d.Content), EffectiveRemoteHash: hash(oldRemote), UpdatedAt: time.Now().UTC()}
	if err := p.bindings.Save(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	result := p.Resolve(context.Background(), d, storage.KeepLocal, issueRevision(newerIssue))
	if result.State != storage.Mirrored || !strings.HasPrefix(result.RemoteRev, "comment:92:") || created != 1 {
		t.Fatalf("old correction was reused after newer edit: result=%+v created=%d", result, created)
	}
}

func TestCorrectionMarkerRejectsMismatchedContentVersion(t *testing.T) {
	d := testDiary()
	body := d.Content + "\n\n<!-- syntroph-remote-correction:key=" + d.Key() + ";version=" + hash("different content") + " -->\n"
	if content, exact := stripCorrectionMarker(body, d.Key()); exact || content != "" {
		t.Fatalf("invalid correction marker accepted: exact=%v content=%q", exact, content)
	}
}

func TestManagedMirrorPersistsTypedBindingWithoutCredentialsOrContent(t *testing.T) {
	d := testDiary()
	server := completedMirrorServer(t, d, 23)
	defer server.Close()
	root := filepath.Join(t.TempDir(), "storage")
	p, err := New("mateusememe/syntroph", "secret-value", root, testOptions(server))
	if err != nil {
		t.Fatal(err)
	}
	managed, err := storage.NewManagedMirror(root, ProviderID, p)
	if err != nil {
		t.Fatal(err)
	}
	result := managed.Mirror(context.Background(), d)
	if result.State != storage.Mirrored {
		t.Fatalf("result = %+v", result)
	}
	binding, ok, err := managed.Bindings.Load(context.Background(), d.Key())
	if err != nil || !ok {
		t.Fatalf("binding ok=%v err=%v", ok, err)
	}
	if binding.Provider != ProviderID || binding.RemoteID != "23" || !strings.HasPrefix(string(binding.RemoteRevision), "issue:23:") || binding.LocalHash != hash(d.Content) || binding.EffectiveRemoteHash != hash(d.Content) {
		t.Fatalf("binding = %+v", binding)
	}
	data, err := os.ReadFile(filepath.Join(root, "bindings", d.Key()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("secret-value")) || bytes.Contains(data, []byte(d.Content)) {
		t.Fatalf("binding leaked credentials or diary content: %s", data)
	}
}

func TestRetriesOnlyApprovedResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		calls   int
		state   storage.MirrorState
		class   storage.FailureClass
	}{
		{name: "secondary 403", status: 403, body: `{"message":"You have exceeded a secondary rate limit"}`, calls: 3, state: storage.StorageSyncPending, class: storage.FailureTransient},
		{name: "primary 403", status: 403, body: `{"message":"API rate limit exceeded"}`, headers: map[string]string{"X-RateLimit-Remaining": "0"}, calls: 1, state: storage.StorageSyncPending, class: storage.FailureTransient},
		{name: "permission 403", status: 403, body: `{"message":"Resource not accessible by integration"}`, calls: 1, state: storage.StoragePrerequisiteMissing, class: storage.FailurePrerequisite},
		{name: "too many requests", status: 429, body: `{"message":"slow down"}`, calls: 3, state: storage.StorageSyncPending, class: storage.FailureTransient},
		{name: "bad gateway", status: 502, body: `{"message":"bad gateway"}`, calls: 3, state: storage.StorageSyncPending, class: storage.FailureTransient},
		{name: "service unavailable", status: 503, body: `{"message":"unavailable"}`, calls: 3, state: storage.StorageSyncPending, class: storage.FailureTransient},
		{name: "gateway timeout", status: 504, body: `{"message":"timeout"}`, calls: 3, state: storage.StorageSyncPending, class: storage.FailureTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			r := newTestProvider(t, server).Mirror(context.Background(), testDiary())
			if calls != tt.calls || r.State != tt.state || r.FailureClass != tt.class {
				t.Fatalf("calls=%d result=%+v", calls, r)
			}
		})
	}
}

func TestRESTFailureClassificationIsBoundedAndExplicit(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		header http.Header
		class  storage.FailureClass
		retry  bool
	}{
		{name: "unauthorized", status: 401, class: storage.FailurePrerequisite},
		{name: "forbidden", status: 403, class: storage.FailurePrerequisite},
		{name: "masked not found", status: 404, class: storage.FailurePrerequisite},
		{name: "issues disabled", status: 410, class: storage.FailurePrerequisite},
		{name: "validation", status: 422, class: storage.FailurePrerequisite},
		{name: "conflict", status: 409, class: storage.FailureConflict},
		{name: "primary limit", status: 403, header: http.Header{"X-Ratelimit-Remaining": []string{"0"}}, class: storage.FailureTransient},
		{name: "secondary body", status: 403, body: `{"message":"secondary rate limit"}`, class: storage.FailureTransient, retry: true},
		{name: "secondary retry after", status: 403, header: http.Header{"Retry-After": []string{"2"}}, class: storage.FailureTransient, retry: true},
		{name: "429", status: 429, class: storage.FailureTransient, retry: true},
		{name: "502", status: 502, class: storage.FailureTransient, retry: true},
		{name: "503", status: 503, class: storage.FailureTransient, retry: true},
		{name: "504", status: 504, class: storage.FailureTransient, retry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, retry := classify(tt.status, tt.header, []byte(tt.body))
			if class != tt.class || retry != tt.retry {
				t.Fatalf("class=%s retry=%v", class, retry)
			}
		})
	}
}

func TestRetryHonorsRetryAfterWithInjectedClock(t *testing.T) {
	now := mustTime("2026-08-31T12:00:00Z")
	durations := []time.Duration{}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", now.Add(3*time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"slow down"}`)
	}))
	defer server.Close()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(t.TempDir(), "storage"), Options{
		HTTPClient: server.Client(), BaseURL: server.URL, UserAgent: "syntroph/test",
		Now: func() time.Time { return now }, Jitter: noJitter,
		Sleep: func(_ context.Context, duration time.Duration) error {
			durations = append(durations, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := p.Mirror(context.Background(), testDiary())
	if result.State != storage.StorageSyncPending || calls != 3 || len(durations) != 2 || durations[0] != 3*time.Second || durations[1] != 3*time.Second {
		t.Fatalf("result=%+v calls=%d durations=%v", result, calls, durations)
	}
}

func TestNetworkFailureIsPendingWithoutAutomaticReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client, baseURL := server.Client(), server.URL
	server.Close()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(t.TempDir(), "storage"), Options{HTTPClient: client, BaseURL: baseURL, UserAgent: "syntroph/test", Sleep: noSleep, Jitter: noJitter})
	if err != nil {
		t.Fatal(err)
	}
	result := p.Mirror(context.Background(), testDiary())
	if result.State != storage.StorageSyncPending || result.FailureClass != storage.FailureTransient || result.Cause == nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestMutationsAreSerialized(t *testing.T) {
	var mu sync.Mutex
	active, maximum := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		active++
		if active > maximum {
			maximum = active
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	p := newTestProvider(t, server)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := p.request(context.Background(), http.MethodPost, "/mutation", map[string]string{"value": "x"}, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum != 1 {
		t.Fatalf("maximum concurrent mutations = %d", maximum)
	}
}

func TestLostBindingRecoveryFollowsPaginationAndSkipsPullRequests(t *testing.T) {
	d := testDiary()
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("state") != "closed" {
			fmt.Fprint(w, `[]`)
			return
		}
		page++
		if page == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/mateusememe/syntroph/issues?state=closed&labels=syntroph-memory%%2Csyntroph-session&per_page=100&page=2>; rel="next"`, "http://"+r.Host))
			fmt.Fprint(w, `[{"number":1,"body":"<!-- syntroph-idempotency-key:`+d.Key()+` -->","pull_request":{}},{"number":2,"body":"\n\n<!-- syntroph-idempotency-key:`+d.Key()+`-near-match -->\n"}]`)
			return
		}
		_ = json.NewEncoder(w).Encode([]issue{{Number: 12, HTMLURL: "https://github.test/issues/12", Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "closed", StateReason: "completed"}})
	}))
	defer server.Close()
	r := newTestProvider(t, server).Recover(context.Background(), d)
	if r.State != storage.Mirrored || r.RemoteID != "12" || page != 2 {
		t.Fatalf("page=%d result=%+v", page, r)
	}
}

func TestConflictIsPersistedPrivatelyByManagedMirror(t *testing.T) {
	d := testDiary()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("state") == "closed" {
			_ = json.NewEncoder(w).Encode([]issue{{Number: 14, HTMLURL: "https://github.test/issues/14", Body: diaryBody("human edit", d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "closed", StateReason: "completed"}})
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	root := t.TempDir()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(root, "storage"), Options{HTTPClient: server.Client(), BaseURL: server.URL, UserAgent: "syntroph/test", Sleep: noSleep, Jitter: noJitter})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := storage.NewManagedMirror(filepath.Join(root, "storage"), ProviderID, p)
	if err != nil {
		t.Fatal(err)
	}
	r := managed.Recover(context.Background(), d)
	if r.State != storage.StorageSyncConflict || r.ConflictSnapshot == "" {
		t.Fatalf("result %+v", r)
	}
	info, err := os.Stat(r.ConflictSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

func newTestProvider(t *testing.T, server *httptest.Server) *Provider {
	t.Helper()
	p, err := New("mateusememe/syntroph", "explicit-token", filepath.Join(t.TempDir(), "storage"), testOptions(server))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func testOptions(server *httptest.Server) Options {
	return Options{HTTPClient: server.Client(), BaseURL: server.URL, UserAgent: "syntroph/test", Sleep: noSleep, Jitter: noJitter, Now: func() time.Time { return mustTime("2026-08-31T12:00:00Z") }}
}

func completedMirrorServer(t *testing.T, d storage.SessionDiary, number int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("state") != "":
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			name, _ := url.PathUnescape(filepath.Base(r.URL.Path))
			for _, candidate := range reservedLabels {
				if candidate.Name == name {
					_ = json.NewEncoder(w).Encode(candidate)
					return
				}
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			_ = json.NewEncoder(w).Encode(issue{Number: number, HTMLURL: fmt.Sprintf("https://github.test/issues/%d", number), Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:00:00Z"), State: "open"})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, fmt.Sprintf("/issues/%d", number)):
			_ = json.NewEncoder(w).Encode(issue{Number: number, HTMLURL: fmt.Sprintf("https://github.test/issues/%d", number), Body: diaryBody(d.Content, d.Key()), UpdatedAt: mustTime("2026-08-31T12:01:00Z"), State: "closed", StateReason: "completed"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
}
func noSleep(context.Context, time.Duration) error { return nil }
func noJitter(time.Duration) time.Duration         { return 0 }
func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
