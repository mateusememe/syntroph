// Command syntroph exposes the explicit recovery surface. Recovery commands
// are intentionally read-only unless sync resolve is given an explicit choice.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mateusememe/syntroph/adapters/graphify"
	"github.com/mateusememe/syntroph/core"
	"github.com/mateusememe/syntroph/storage"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "syntroph:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut interface{ Write([]byte) (int, error) }) error {
	if len(args) < 2 {
		return errors.New("usage: syntroph session close | doctor storage | sync <status|recovery|retry|resolve>")
	}
	if args[0] == "session" && args[1] == "close" {
		return closeSession(args[2:], out, errOut)
	}
	if args[0] == "doctor" && args[1] == "storage" {
		return doctorStorage(args[2:], out, errOut)
	}
	if args[0] != "sync" {
		return errors.New("usage: syntroph session close | doctor storage | sync <status|recovery|retry|resolve>")
	}
	journalDir := filepath.Join(".syntroph", "journal")
	if len(args) >= 3 && strings.HasPrefix(args[1], "--journal=") {
		journalDir = strings.TrimPrefix(args[1], "--journal=")
		args = append([]string{args[0], args[2]}, args[3:]...)
	}
	j, e := core.NewSagaJournal(journalDir)
	if e != nil {
		return e
	}
	ctx := context.Background()
	switch args[1] {
	case "status":
		items, e := core.InspectRecovery(ctx, j)
		if e != nil {
			return e
		}
		if len(items) == 0 {
			fmt.Fprintln(out, "No pending synchronization obligations.")
			return nil
		}
		for _, item := range items {
			fmt.Fprintf(out, "%s\t%s\tprovider=%s\tremote=%s\trevision=%s\tfailure=%s\tevidence=%s\terror=%s\tnext=%s\n", item.SagaID, item.State, item.Provider, item.RemoteID, item.RemoteRevision, item.FailureClass, item.ConflictSnapshot, item.LastError, item.NextAction)
		}
		return nil
	case "recovery":
		fs := flag.NewFlagSet("sync recovery", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		clearLock := fs.String("clear-lock", "", "clear a verified orphaned mirror lock")
		root := fs.String("root", filepath.Dir(journalDir), "Syntroph data root")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("sync recovery accepts only --clear-lock and --root")
		}
		if *clearLock != "" {
			locks, err := storage.NewMirrorLocks(filepath.Join(*root, "storage"))
			if err != nil {
				return err
			}
			if err := locks.ClearOrphan(ctx, *clearLock); err != nil {
				return err
			}
			fmt.Fprintf(out, "Verified orphaned mirror lock cleared: %s. No external effect was executed.\n", *clearLock)
			return nil
		}
		items, e := core.InspectRecovery(ctx, j)
		if e != nil {
			return e
		}
		if len(items) == 0 {
			fmt.Fprintln(out, "Recovery plan: nothing to recover.")
			return nil
		}
		fmt.Fprintln(out, "Recovery plan (no external effects are executed automatically):")
		for _, item := range items {
			fmt.Fprintf(out, "- saga %s: %s; provider: %s; remote: %s %s; revision: %s; failure: %s; evidence: %s; attempts: %d; last error: %s; next: %s\n", item.SagaID, item.State, item.Provider, item.RemoteID, item.RemoteURL, item.RemoteRevision, item.FailureClass, item.ConflictSnapshot, len(item.Attempts), item.LastError, item.NextAction)
			for _, attempt := range item.Attempts {
				fmt.Fprintf(out, "  attempt %s/%s at %s: %s", attempt.HandlerID, attempt.EventID, attempt.AttemptedAt.Format(time.RFC3339Nano), attempt.Outcome)
				if attempt.Error != "" {
					fmt.Fprintf(out, " (%s)", attempt.Error)
				}
				fmt.Fprintln(out)
			}
		}
		return nil
	case "retry":
		return retry(args[2:], j, journalDir, out)
	case "resolve":
		return resolve(args[2:], out)
	default:
		return fmt.Errorf("unknown sync command %q", args[1])
	}
}

func closeSession(args []string, out, errOut interface{ Write([]byte) (int, error) }) error {
	fs := flag.NewFlagSet("session close", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	artifact, format, summary := fs.String("artifact", "", "Markdown or JSON artifact"), fs.String("format", "", "json or markdown"), fs.String("summary", "", "manual summary")
	repo, sha, author, runtime, root := fs.String("repository", "", "repository identity"), fs.String("commit", "", "commit SHA"), fs.String("author", "", "author"), fs.String("runtime", "", "runtime"), fs.String("root", ".syntroph", "Syntroph data root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" || *sha == "" {
		return errors.New("session close requires --repository and --commit")
	}
	var data []byte
	var err error
	if *artifact != "" {
		data, err = os.ReadFile(*artifact)
		if err != nil {
			return err
		}
	}
	journal, err := core.NewSagaJournal(filepath.Join(*root, "journal"))
	if err != nil {
		return err
	}
	bus, err := core.NewEventBus(journal)
	if err != nil {
		return err
	}
	memory := core.LocalMemoryStore{Root: filepath.Join(*root, "memory")}
	// LocalGraphPort is the deterministic default. Set graphify_executable in
	// .syntroph/config.yaml to exercise the optional real adapter.
	var graph core.GraphPort
	if executable := configuredGraphExecutable(*root); executable != "" {
		graph = graphify.New(executable)
	} else if local, e := core.NewLocalGraphPort(filepath.Join(*root, "graph")); e == nil {
		graph = local
	}
	formatValue := core.ArtifactFormat(strings.ToLower(*format))
	storagePort := configuredStorage(*root, filepath.Dir(*root))
	diary, delivery, err := (core.SessionCloser{Memory: memory, Bus: bus, Graph: graph, Storage: storagePort}).Close(context.Background(), core.SessionCloseRequest{RepositoryID: *repo, CommitSHA: *sha, Author: *author, Runtime: *runtime, Artifact: data, Format: formatValue, ManualSummary: *summary})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(struct {
		Diary    core.SessionDiary `json:"diary"`
		Delivery core.Delivery     `json:"delivery"`
	}{diary, delivery}, "", "  ")
	_, _ = out.Write(append(b, '\n'))
	return nil
}

func retry(args []string, journal *core.SagaJournal, journalDir string, out interface{ Write([]byte) (int, error) }) error {
	fs := flag.NewFlagSet("sync retry", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	graph, storage := fs.Bool("graph", false, "retry graph obligations"), fs.Bool("storage", false, "retry storage obligations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *graph == *storage {
		return errors.New("sync retry requires exactly one of --graph or --storage")
	}
	scope := "graph"
	if *storage {
		scope = "storage"
	}
	items, err := core.InspectRecovery(context.Background(), journal)
	if err != nil {
		return err
	}
	var storagePort core.StoragePort
	var storageLifecycle interface {
		BeginCommand()
		EndCommand() error
	}
	storageLifecycleEnded := false
	if scope == "storage" {
		syntrophRoot := filepath.Dir(journalDir)
		storagePort = configuredStorage(syntrophRoot, filepath.Dir(syntrophRoot))
		if lifecycle, ok := storagePort.(interface {
			BeginCommand()
			EndCommand() error
		}); ok {
			storageLifecycle = lifecycle
			storageLifecycle.BeginCommand()
			defer func() {
				if !storageLifecycleEnded {
					_ = storageLifecycle.EndCommand()
				}
			}()
		}
	}
	count := 0
	var storageItems []core.RecoveryItem
	for _, item := range items {
		if (scope == "graph" && item.State != core.GraphResolutionPending && item.State != core.GraphSyncPending) || (scope == "storage" && item.State != "StorageSyncPending" && item.State != "StoragePrerequisiteMissing") {
			continue
		}
		e := core.Event{EventID: item.SagaID + ":retry:" + scope, Type: "sync.retry.requested", OccurredAt: time.Now().UTC(), RepositoryID: item.RepositoryID, SagaID: item.SagaID, CorrelationID: item.SagaID, CausationID: item.EventID, SchemaVersion: 1, Payload: []byte(fmt.Sprintf(`{"scope":%q}`, scope))}
		if err := journal.AppendEvent(context.Background(), e); err != nil {
			return err
		}
		if scope == "graph" {
			if err := retryGraph(context.Background(), journal, item); err != nil {
				attempt := core.HandlerAttempt{EventID: e.EventID, SagaID: item.SagaID, HandlerID: "graph-retry", AttemptedAt: time.Now().UTC(), Outcome: "failed", Error: err.Error()}
				if appendErr := journal.AppendAttempt(context.Background(), attempt); appendErr != nil {
					return appendErr
				}
				continue
			}
			if err := journal.AppendAttempt(context.Background(), core.HandlerAttempt{EventID: e.EventID, SagaID: item.SagaID, HandlerID: "graph-retry", AttemptedAt: time.Now().UTC(), Outcome: "succeeded"}); err != nil {
				return err
			}
		} else {
			storageItems = append(storageItems, item)
			result, retryErr := retryStorage(context.Background(), journal, item, storagePort)
			if retryErr != nil {
				if appendErr := journal.AppendAttempt(context.Background(), core.HandlerAttempt{EventID: e.EventID, SagaID: item.SagaID, HandlerID: "storage-retry", AttemptedAt: time.Now().UTC(), Outcome: "failed", Error: retryErr.Error()}); appendErr != nil {
					return appendErr
				}
				continue
			}
			attempt := core.HandlerAttempt{EventID: e.EventID, SagaID: item.SagaID, HandlerID: "storage-retry", AttemptedAt: time.Now().UTC(), Outcome: "succeeded"}
			if result.State != "mirrored" {
				attempt.Outcome = "failed"
				if result.Cause != nil {
					attempt.Error = result.Cause.Error()
				}
			}
			if err := journal.AppendAttempt(context.Background(), attempt); err != nil {
				return err
			}
			payload, _ := json.Marshal(result)
			if err := journal.AppendEvent(context.Background(), core.Event{EventID: item.SagaID + ":storage-retry-result", Type: storageResultEventType(string(result.State)), OccurredAt: time.Now().UTC(), RepositoryID: item.RepositoryID, SagaID: item.SagaID, CorrelationID: item.SagaID, CausationID: e.EventID, SchemaVersion: 1, Payload: payload}); err != nil {
				return err
			}
		}
		count++
	}
	if storageLifecycle != nil {
		closeErr := storageLifecycle.EndCommand()
		storageLifecycleEnded = true
		if closeErr != nil {
			for _, item := range storageItems {
				if err := journalStorageShutdownFailure(context.Background(), journal, item, closeErr); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "MCP shutdown failed after the storage retry and was journaled for %d saga(s): %v\n", len(storageItems), closeErr)
		}
	}
	fmt.Fprintf(out, "Retry requested for %s synchronization for %d saga(s). External effects require the configured adapter.\n", scope, count)
	return nil
}

func diaryFromSaga(ctx context.Context, journal *core.SagaJournal, saga string) (core.SessionDiary, error) {
	records, err := journal.ReadSaga(ctx, saga)
	if err != nil {
		return core.SessionDiary{}, err
	}
	for _, r := range records {
		if r.Event != nil && r.Event.Type == "session.closed" {
			var d core.SessionDiary
			if json.Unmarshal(r.Event.Payload, &d) == nil {
				return d, nil
			}
		}
	}
	return core.SessionDiary{}, errors.New("session diary not found in saga journal")
}

func retryStorage(ctx context.Context, journal *core.SagaJournal, item core.RecoveryItem, port core.StoragePort) (core.StorageResult, error) {
	diary, err := diaryFromSaga(ctx, journal, item.SagaID)
	if err != nil {
		return core.StorageResult{}, err
	}
	resolver, ok := port.(core.StorageRecoveryPort)
	if !ok || resolver == nil {
		return core.StorageResult{}, errors.New("storage adapter does not support explicit recovery")
	}
	r := resolver.RecoverEvent(ctx, item.SagaID+":storage-retry", diary)
	return r, nil
}

func journalStorageShutdownFailure(ctx context.Context, journal *core.SagaJournal, item core.RecoveryItem, cause error) error {
	now := time.Now().UTC()
	eventID := fmt.Sprintf("%s:storage-shutdown:%d", item.SagaID, now.UnixNano())
	attempt := core.HandlerAttempt{EventID: eventID, SagaID: item.SagaID, HandlerID: "storage-shutdown", AttemptedAt: now, Outcome: "failed", Error: cause.Error()}
	if err := journal.AppendAttempt(ctx, attempt); err != nil {
		return err
	}
	result := core.StorageResult{
		EventID: eventID, State: storage.StorageSyncPending, Backend: core.StorageBackend(item.Backend),
		Provider: item.Provider, Key: item.IdempotencyKey, RemoteID: item.RemoteID,
		RemoteURL: item.RemoteURL, RemoteRev: item.RemoteRevision,
		FailureClass: storage.FailureTransient, Error: cause.Error(), Cause: cause,
	}
	payload, _ := json.Marshal(result)
	return journal.AppendEvent(ctx, core.Event{
		EventID: eventID, Type: "storage.sync.pending", OccurredAt: now,
		RepositoryID: item.RepositoryID, SagaID: item.SagaID, CorrelationID: item.SagaID,
		CausationID: item.EventID, SchemaVersion: 1, Payload: payload,
	})
}

func storageResultEventType(state string) string {
	switch state {
	case "mirrored":
		return "storage.sync.succeeded"
	case "StorageSyncConflict":
		return "storage.sync.conflict"
	case "StoragePrerequisiteMissing":
		return "storage.prerequisite.missing"
	default:
		return "storage.sync.pending"
	}
}

func retryGraph(ctx context.Context, journal *core.SagaJournal, item core.RecoveryItem) error {
	records, err := journal.ReadSaga(ctx, item.SagaID)
	if err != nil {
		return err
	}
	var diary core.SessionDiary
	for _, r := range records {
		if r.Event != nil {
			_ = json.Unmarshal(r.Event.Payload, &diary)
		}
	}
	graph, err := core.NewLocalGraphPort(filepath.Join(".syntroph", "graph"))
	if err != nil {
		return err
	}
	res, err := graph.Resolve(ctx, core.GraphResolveRequest{RepositoryID: diary.RepositoryID, CommitSHA: diary.CommitSHA, References: diary.CodeReferences})
	if err != nil {
		return err
	}
	_, err = graph.Publish(ctx, core.GraphSnapshot{RepositoryID: diary.RepositoryID, CommitSHA: diary.CommitSHA, References: res.References})
	return err
}

func resolve(args []string, out interface{ Write([]byte) (int, error) }) error {
	// Accept the documented `resolve <id> [flags]` form even though the
	// standard flag parser stops at the first positional argument.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	fs := flag.NewFlagSet("sync resolve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	local, remote, keepLocal, keepRemote := fs.String("local-file", "", "local canonical content"), fs.String("remote-file", "", "remote content observed"), fs.Bool("keep-local", false, "append a correction containing local content"), fs.Bool("keep-remote", false, "acknowledge remote content")
	revision, root := fs.String("revision", "", "remote revision observed during diff"), fs.String("root", ".syntroph", "Syntroph data root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("sync resolve requires a conflict id")
	}
	if *keepLocal == *keepRemote {
		return errors.New("sync resolve requires exactly one of --keep-local or --keep-remote")
	}
	// With an adapter configured, resolve the immutable diary through the
	// provider-neutral StoragePort and require the observed remote revision.
	if *local == "" && *remote == "" {
		journal, err := core.NewSagaJournal(filepath.Join(*root, "journal"))
		if err != nil {
			return err
		}
		diary, err := diaryFromSaga(context.Background(), journal, fs.Arg(0))
		if err != nil {
			return err
		}
		items, err := core.InspectRecovery(context.Background(), journal)
		if err != nil {
			return err
		}
		var conflict core.RecoveryItem
		for _, item := range items {
			if item.SagaID == fs.Arg(0) {
				conflict = item
				break
			}
		}
		if conflict.State != "StorageSyncConflict" || conflict.ConflictSnapshot == "" {
			return errors.New("sync resolve requires a persisted storage conflict with offline diff evidence")
		}
		remoteContent, err := os.ReadFile(conflict.ConflictSnapshot)
		if err != nil {
			return fmt.Errorf("read remote conflict snapshot: %w", err)
		}
		localContent := core.RenderSessionDiary(diary)
		fmt.Fprintf(out, "Conflict %s\n--- local\n%s\n--- remote\n%s\n", fs.Arg(0), localContent, remoteContent)
		port := configuredStorage(*root, filepath.Dir(*root))
		resolver, ok := port.(core.StorageResolver)
		if !ok || resolver == nil {
			return errors.New("sync resolve requires --local-file and --remote-file, or a configured storage adapter")
		}
		observedRevision := conflict.RemoteRevision
		if *revision != "" && *revision != observedRevision {
			return errors.New("--revision does not match the revision persisted with the displayed conflict")
		}
		if observedRevision == "" {
			return errors.New("storage conflict does not contain an observed remote revision")
		}
		choice := map[bool]string{true: "keep-local", false: "keep-remote"}[*keepLocal]
		r := resolver.Resolve(context.Background(), diary, choice, observedRevision)
		attempt := core.HandlerAttempt{EventID: fs.Arg(0) + ":storage-resolve", SagaID: fs.Arg(0), HandlerID: "storage-resolve", AttemptedAt: time.Now().UTC(), Outcome: "succeeded"}
		if r.State != "mirrored" {
			attempt.Outcome = "failed"
			if r.Cause != nil {
				attempt.Error = r.Cause.Error()
				_ = journal.AppendAttempt(context.Background(), attempt)
				return r.Cause
			}
			attempt.Error = "storage conflict remains unresolved"
			_ = journal.AppendAttempt(context.Background(), attempt)
			return errors.New("storage conflict remains unresolved")
		}
		if err := journal.AppendAttempt(context.Background(), attempt); err != nil {
			return err
		}
		payload, _ := json.Marshal(r)
		if err := journal.AppendEvent(context.Background(), core.Event{EventID: fs.Arg(0) + ":storage-resolved", Type: "storage.sync.resolved", OccurredAt: time.Now().UTC(), RepositoryID: diary.RepositoryID, SagaID: fs.Arg(0), CorrelationID: fs.Arg(0), CausationID: conflict.EventID, SchemaVersion: 1, Payload: payload}); err != nil {
			return err
		}
		fmt.Fprintf(out, "Resolution recorded: %s.\n", choice)
		return nil
	}
	if *local == "" || *remote == "" {
		return errors.New("sync resolve requires --local-file and --remote-file to display a diff")
	}
	lb, err := os.ReadFile(*local)
	if err != nil {
		return err
	}
	rb, err := os.ReadFile(*remote)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Conflict %s\n--- local\n%s\n--- remote\n%s\n", fs.Arg(0), lb, rb)
	if *keepRemote {
		fmt.Fprintln(out, "Resolution recorded: keep-remote.")
		return nil
	}
	if err := os.WriteFile(*remote, lb, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(out, "Resolution recorded: keep-local.")
	return nil
}
