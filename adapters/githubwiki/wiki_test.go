package githubwiki

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/storage"
)

func TestMirrorCreatesDeterministicPageAndIsIdempotent(t *testing.T) {
	remote := initializedWiki(t)
	provider := testProvider(t, remote)
	diary := testDiary()

	first := provider.Mirror(context.Background(), diary)
	if first.State != storage.Mirrored || !strings.HasPrefix(first.RemoteRev, "git:") {
		t.Fatalf("first mirror = %+v", first)
	}
	clone := filepath.Join(t.TempDir(), "inspect")
	git(t, "clone", "-q", remote, clone)
	path := filepath.Join(clone, "Sessions", "2026", "08", "session-1-abcdef123456.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"idempotency_key: " + diary.Key(), "syntroph_remote_binding: " + diary.Key(), "# Session diary"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("page missing %q:\n%s", want, content)
		}
	}
	logBefore := strings.TrimSpace(git(t, "-C", clone, "rev-list", "--count", "HEAD"))
	second := provider.Status(context.Background(), diary)
	if second.State != storage.Mirrored || second.RemoteRev != first.RemoteRev {
		t.Fatalf("idempotent mirror = %+v, first=%+v", second, first)
	}
	git(t, "-C", clone, "pull", "-q", "--ff-only")
	if got := strings.TrimSpace(git(t, "-C", clone, "rev-list", "--count", "HEAD")); got != logBefore {
		t.Fatalf("redelivery added commit: before=%s after=%s", logBefore, got)
	}
}

func TestManagedMirrorReconstructsBindingAndPersistsPrivateConflict(t *testing.T) {
	remote := initializedWiki(t)
	provider := testProvider(t, remote)
	root := t.TempDir()
	managed, err := storage.NewManagedMirror(root, ProviderID, provider)
	if err != nil {
		t.Fatal(err)
	}
	diary := testDiary()
	first := managed.Mirror(context.Background(), diary)
	if first.State != storage.Mirrored {
		t.Fatalf("first mirror = %+v", first)
	}
	bindingPath, _ := managed.Bindings.Path(diary.Key())
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	withoutRecovery := managed.Mirror(context.Background(), diary)
	if withoutRecovery.State != storage.StorageSyncPending || !withoutRecovery.UnverifiedIdentity {
		t.Fatalf("normal mirror reconstructed a missing Wiki binding: %+v", withoutRecovery)
	}
	recovered := managed.Recover(context.Background(), diary)
	if recovered.State != storage.Mirrored {
		t.Fatalf("lost-binding recovery = %+v", recovered)
	}
	if _, ok, err := managed.Bindings.Load(context.Background(), diary.Key()); err != nil || !ok {
		t.Fatalf("binding not reconstructed: ok=%v err=%v", ok, err)
	}

	editWikiPage(t, remote, recovered.RemoteID, "# Human edit\n")
	conflict := managed.Recover(context.Background(), diary)
	if conflict.State != storage.StorageSyncConflict || !errors.Is(conflict.Cause, storage.ErrConflict) || conflict.ConflictSnapshot == "" {
		t.Fatalf("conflict = %+v", conflict)
	}
	info, err := os.Stat(conflict.ConflictSnapshot)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private conflict snapshot: info=%v err=%v", info, err)
	}
	if b, _ := os.ReadFile(conflict.ConflictSnapshot); string(b) != "# Human edit\n" {
		t.Fatalf("conflict evidence = %q", b)
	}
	accepted := managed.Resolve(context.Background(), diary, storage.KeepRemote, conflict.RemoteRev)
	if accepted.State != storage.Mirrored || accepted.EffectiveRemoteHash == accepted.LocalHash {
		t.Fatalf("keep remote = %+v", accepted)
	}
}

func TestLostBindingIsNotReconstructedWithoutExactFrontmatterMarker(t *testing.T) {
	remote := initializedWiki(t)
	root := t.TempDir()
	managed, err := storage.NewManagedMirror(root, ProviderID, testProvider(t, remote))
	if err != nil {
		t.Fatal(err)
	}
	diary := testDiary()
	created := managed.Mirror(context.Background(), diary)
	if created.State != storage.Mirrored {
		t.Fatalf("first mirror = %+v", created)
	}
	bindingPath, _ := managed.Bindings.Path(diary.Key())
	if err := os.Remove(bindingPath); err != nil {
		t.Fatal(err)
	}
	editWikiPage(t, remote, created.RemoteID, "# Page without Syntroph marker\n")

	conflict := managed.Recover(context.Background(), diary)
	if conflict.State != storage.StorageSyncConflict || !conflict.UnverifiedIdentity || conflict.ConflictSnapshot == "" {
		t.Fatalf("unverified deterministic path = %+v", conflict)
	}
	if _, ok, err := managed.Bindings.Load(context.Background(), diary.Key()); err != nil || ok {
		t.Fatalf("unverified marker reconstructed binding: ok=%v err=%v", ok, err)
	}
}

func TestResolveKeepLocalCreatesAuditableCommit(t *testing.T) {
	remote := initializedWiki(t)
	provider := testProvider(t, remote)
	diary := testDiary()
	created := provider.Mirror(context.Background(), diary)
	editWikiPage(t, remote, created.RemoteID, "# Human edit\n")
	conflict := provider.Status(context.Background(), diary)
	resolved := provider.Resolve(context.Background(), diary, storage.KeepLocal, conflict.RemoteRev)
	if resolved.State != storage.Mirrored || resolved.RemoteRev == conflict.RemoteRev {
		t.Fatalf("keep local = %+v", resolved)
	}
	clone := filepath.Join(t.TempDir(), "inspect")
	git(t, "clone", "-q", remote, clone)
	if got := strings.TrimSpace(git(t, "-C", clone, "log", "-1", "--format=%s")); got != "syntroph: mirror session session-1" {
		t.Fatalf("commit message = %q", got)
	}
	content, _ := os.ReadFile(filepath.Join(clone, filepath.FromSlash(created.RemoteID)))
	if !strings.Contains(string(content), "# Session diary") {
		t.Fatalf("local diary was not restored: %s", content)
	}
}

func TestNonFastForwardBecomesConflict(t *testing.T) {
	remote := initializedWiki(t)
	provider := testProvider(t, remote)
	provider.beforePush = func() {
		editWikiPage(t, remote, "Sessions/2026/08/session-1-abcdef123456.md", "# Concurrent remote edit\n")
	}
	managed, err := storage.NewManagedMirror(t.TempDir(), ProviderID, provider)
	if err != nil {
		t.Fatal(err)
	}
	result := managed.Mirror(context.Background(), testDiary())
	if result.State != storage.StorageSyncConflict || result.FailureClass != storage.FailureConflict || !errors.Is(result.Cause, storage.ErrConflict) || result.ConflictSnapshot == "" {
		t.Fatalf("non-fast-forward = %+v", result)
	}
	evidence, err := os.ReadFile(result.ConflictSnapshot)
	if err != nil || string(evidence) != "# Concurrent remote edit\n" {
		t.Fatalf("non-fast-forward evidence = %q err=%v", evidence, err)
	}
}

func TestConfiguredGitExecutableFetchesObservedHeadAndCleansTemporaryCheckout(t *testing.T) {
	remote := initializedWiki(t)
	temporaryRoot := t.TempDir()
	t.Setenv("TMPDIR", temporaryRoot)
	logPath := filepath.Join(t.TempDir(), "git-commands.log")
	t.Setenv("SYNTROPH_WIKI_GIT_LOG", logPath)
	wrapper := filepath.Join(t.TempDir(), "git-wrapper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SYNTROPH_WIKI_GIT_LOG\"\nexec git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := New(Options{
		Repository: "acme/repo", RemoteURL: remote, WebURL: "https://github.com/acme/repo/wiki",
		GitExecutable: wrapper, Author: Author{Name: "Syntroph", Email: "syntroph@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := provider.Mirror(context.Background(), testDiary()); result.State != storage.Mirrored {
		t.Fatalf("mirror through configured executable = %+v", result)
	}
	commands, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clone --quiet --no-tags", " fetch --quiet --no-tags origin", " push --porcelain origin"} {
		if !strings.Contains(string(commands), want) {
			t.Errorf("configured Git command log missing %q:\n%s", want, commands)
		}
	}
	entries, err := os.ReadDir(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "syntroph-wiki-") {
			t.Fatalf("temporary Wiki checkout leaked: %s", entry.Name())
		}
	}
}

func TestMissingWikiAndMissingAuthorArePrerequisites(t *testing.T) {
	t.Run("Wiki is not initialized", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.wiki.git")
		result := testProvider(t, missing).Mirror(context.Background(), testDiary())
		if result.State != storage.StoragePrerequisiteMissing || !errors.Is(result.Cause, storage.ErrPrerequisiteMissing) {
			t.Fatalf("missing Wiki = %+v", result)
		}
	})
	t.Run("author is unavailable", func(t *testing.T) {
		t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "missing-global-config"))
		p, err := New(Options{Repository: "acme/repo", RemoteURL: initializedWiki(t), RepositoryRoot: t.TempDir(), GitExecutable: "git"})
		if err != nil {
			t.Fatal(err)
		}
		result := p.Mirror(context.Background(), testDiary())
		if result.State != storage.StoragePrerequisiteMissing || !errors.Is(result.Cause, storage.ErrPrerequisiteMissing) {
			t.Fatalf("missing author = %+v", result)
		}
	})
	t.Run("credentials are unavailable", func(t *testing.T) {
		gitExecutable := filepath.Join(t.TempDir(), "git-denied")
		secret := "git-secret"
		script := "#!/bin/sh\necho 'https://alice:" + secret + "@github.com/acme/repo.wiki.git: Permission denied " + strings.Repeat("diagnostic", 400) + "' >&2\nexit 128\n"
		if err := os.WriteFile(gitExecutable, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		p, err := New(Options{
			Repository: "acme/repo", RemoteURL: "git@github.com:acme/repo.wiki.git",
			GitExecutable: gitExecutable, Author: Author{Name: "Syntroph", Email: "syntroph@example.test"},
		})
		if err != nil {
			t.Fatal(err)
		}
		result := p.Mirror(context.Background(), testDiary())
		if result.State != storage.StoragePrerequisiteMissing || !errors.Is(result.Cause, storage.ErrPrerequisiteMissing) || !strings.Contains(result.Cause.Error(), "credential-helper or SSH") || strings.Contains(result.Cause.Error(), secret) || len(result.Cause.Error()) > 2200 {
			t.Fatalf("missing credentials = %+v", result)
		}
	})
}

func TestWikiProviderRejectsCredentialBearingRemoteBeforeGit(t *testing.T) {
	secret := "wiki-secret"
	_, err := New(Options{
		Repository: "acme/repo", RemoteURL: "https://alice:" + secret + "@github.com/acme/repo.wiki.git",
		GitExecutable: "git", Author: Author{Name: "Syntroph", Email: "syntroph@example.test"},
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("credential-bearing Wiki remote was not rejected safely: %v", err)
	}
}

func TestFallbackAuthorUsesRepositoryConfigWithoutMutation(t *testing.T) {
	repositoryRoot := t.TempDir()
	git(t, "init", "-q", repositoryRoot)
	git(t, "-C", repositoryRoot, "config", "user.name", "Existing User")
	git(t, "-C", repositoryRoot, "config", "user.email", "existing@example.test")
	p, err := New(Options{Repository: "acme/repo", RemoteURL: initializedWiki(t), RepositoryRoot: repositoryRoot, GitExecutable: "git"})
	if err != nil {
		t.Fatal(err)
	}
	result := p.Mirror(context.Background(), testDiary())
	if result.State != storage.Mirrored {
		t.Fatalf("fallback author mirror = %+v", result)
	}
	if got := strings.TrimSpace(git(t, "-C", repositoryRoot, "config", "--get", "user.name")); got != "Existing User" {
		t.Fatalf("repository config changed: %q", got)
	}
	clone := filepath.Join(t.TempDir(), "inspect")
	git(t, "clone", "-q", p.options.RemoteURL, clone)
	if got := strings.TrimSpace(git(t, "-C", clone, "log", "-1", "--format=%an <%ae>")); got != "Existing User <existing@example.test>" {
		t.Fatalf("commit author = %q", got)
	}
}

func initializedWiki(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "repo.wiki.git")
	git(t, "init", "-q", "--bare", remote)
	seed := filepath.Join(root, "seed")
	git(t, "clone", "-q", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "Home.md"), []byte("# Home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", seed, "add", "Home.md")
	git(t, "-C", seed, "-c", "user.name=Seed", "-c", "user.email=seed@example.test", "commit", "-q", "-m", "initialize wiki")
	git(t, "-C", seed, "push", "-q", "origin", "HEAD")
	return remote
}

func editWikiPage(t *testing.T, remote, page, content string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "edit")
	git(t, "clone", "-q", remote, clone)
	path := filepath.Join(clone, filepath.FromSlash(page))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, "-C", clone, "add", "--", page)
	git(t, "-C", clone, "-c", "user.name=Human", "-c", "user.email=human@example.test", "commit", "-q", "-m", "human edit")
	git(t, "-C", clone, "push", "-q", "origin", "HEAD")
}

func testProvider(t *testing.T, remote string) *Provider {
	t.Helper()
	p, err := New(Options{
		Repository: "acme/repo", RemoteURL: remote, WebURL: "https://github.com/acme/repo/wiki",
		GitExecutable: "git", Author: Author{Name: "Syntroph", Email: "syntroph@example.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testDiary() storage.SessionDiary {
	created := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	content := "---\nsession_id: session-1\nidempotency_key: placeholder\nartifact_hash: abcdef1234567890\ncreated_at: " + created + "\n---\n\n# Session diary\n\nLearned something.\n"
	d := storage.SessionDiary{SessionID: "session-1", RepositoryID: "github.com/acme/repo", CommitSHA: "abc123", ArtifactHash: "abcdef1234567890", Content: content}
	d.Content = strings.Replace(d.Content, "placeholder", d.Key(), 1)
	return d
}

func git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
