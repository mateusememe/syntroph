package storage

import (
	"context"
	"errors"
	"os"
	"testing"
)

type fakeClient struct {
	docs     map[string]RemoteDocument
	putCalls int
	err      error
}

func (f *fakeClient) Get(_ context.Context, b Backend, repo, key string) (RemoteDocument, error) {
	if f.err != nil {
		return RemoteDocument{}, f.err
	}
	d, ok := f.docs[string(b)+":"+repo+":"+key]
	if !ok {
		return RemoteDocument{NotFound: true}, nil
	}
	return d, nil
}
func (f *fakeClient) Put(_ context.Context, b Backend, repo, key, content, expected string) (RemoteDocument, error) {
	if f.err != nil {
		return RemoteDocument{}, f.err
	}
	f.putCalls++
	k := string(b) + ":" + repo + ":" + key
	d := f.docs[k]
	if !d.NotFound && expected != "" && d.Revision != expected {
		return RemoteDocument{}, ErrConflict
	}
	d = RemoteDocument{ID: key, Revision: "rev-" + string(rune(f.putCalls+'0')), Content: content}
	f.docs[k] = d
	return d, nil
}

func diary() SessionDiary {
	return SessionDiary{SessionID: "s1", RepositoryID: "github.com/acme/repo", CommitSHA: "abc", ArtifactHash: "hash", Content: "# Diary\nlesson"}
}

func TestMirrorCreatesAndRedeliveryIsIdempotent(t *testing.T) {
	f := &fakeClient{docs: map[string]RemoteDocument{}}
	m, err := NewMirror(BackendWiki, "acme/repo", f)
	if err != nil {
		t.Fatal(err)
	}
	r := m.Mirror(context.Background(), diary())
	if r.State != Mirrored || f.putCalls != 1 {
		t.Fatalf("first mirror: %+v calls=%d", r, f.putCalls)
	}
	r = m.Mirror(context.Background(), diary())
	if r.State != Mirrored || f.putCalls != 1 {
		t.Fatalf("redelivery duplicated: %+v calls=%d", r, f.putCalls)
	}
}

func TestMirrorFailurePreservesCanonicalDiaryAndIsPending(t *testing.T) {
	f := &fakeClient{docs: map[string]RemoteDocument{}, err: errors.New("network down")}
	m, _ := NewMirror(BackendIssues, "acme/repo", f)
	r := m.Mirror(context.Background(), diary())
	if !r.Pending() || !errors.Is(r.Cause, ErrUnavailable) {
		t.Fatalf("want pending unavailable: %+v", r)
	}
}

func TestMirrorDetectsRemoteConflictWithoutOverwrite(t *testing.T) {
	d := diary()
	f := &fakeClient{docs: map[string]RemoteDocument{}}
	f.docs[string(BackendWiki)+":acme/repo:"+d.Key()] = RemoteDocument{ID: "i", Revision: "r1", Content: "human edit"}
	m, _ := NewMirror(BackendWiki, "acme/repo", f)
	r := m.Mirror(context.Background(), d)
	if !r.Conflict() || f.putCalls != 0 || !errors.Is(r.Cause, ErrConflict) {
		t.Fatalf("want conflict: %+v calls=%d", r, f.putCalls)
	}
}

func TestResolveRequiresObservedRevisionAndExplicitChoice(t *testing.T) {
	d := diary()
	f := &fakeClient{docs: map[string]RemoteDocument{}}
	f.docs[string(BackendWiki)+":acme/repo:"+d.Key()] = RemoteDocument{ID: "i", Revision: "r1", Content: "human edit"}
	m, _ := NewMirror(BackendWiki, "acme/repo", f)
	if r := m.Resolve(context.Background(), d, KeepLocal, ""); !r.Conflict() {
		t.Fatal("missing observed revision must conflict")
	}
	if r := m.Resolve(context.Background(), d, KeepLocal, "r1"); r.State != Mirrored || f.putCalls != 1 {
		t.Fatalf("keep local failed: %+v", r)
	}
	if r := m.Resolve(context.Background(), d, KeepRemote, "r2"); !r.Conflict() {
		t.Fatal("stale revision must conflict")
	}
}

func TestCredentialProvidersStayBehindAuthenticatedClientBoundary(t *testing.T) {
	old := os.Getenv("SYNTROPH_TEST_TOKEN")
	defer os.Setenv("SYNTROPH_TEST_TOKEN", old)
	os.Setenv("SYNTROPH_TEST_TOKEN", "secret")
	want := &fakeClient{docs: map[string]RemoteDocument{}}
	p := EnvTokenProvider{TokenEnv: "SYNTROPH_TEST_TOKEN", Factory: func(token string) (GitHubClient, error) {
		if token != "secret" {
			t.Fatal("wrong token")
		}
		return want, nil
	}}
	got, err := p.Client(context.Background())
	if err != nil || got != want {
		t.Fatalf("provider: %v", err)
	}
	if _, err := (EnvTokenProvider{TokenEnv: "MISSING", Factory: func(string) (GitHubClient, error) { return want, nil }}).Client(context.Background()); err == nil {
		t.Fatal("missing token should fail")
	}
	mcp := MCPProvider{ClientFactory: func(context.Context) (GitHubClient, error) { return want, nil }}
	if got, err := mcp.Client(context.Background()); err != nil || got != want {
		t.Fatal("mcp provider")
	}
}
