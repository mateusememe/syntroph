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

	"github.com/mateusememe/syntroph/core"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "syntroph:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut interface{ Write([]byte) (int, error) }) error {
	if len(args) < 2 {
		return errors.New("usage: syntroph session close | sync <status|recovery|retry|resolve>")
	}
	if args[0] == "session" && args[1] == "close" {
		return closeSession(args[2:], out, errOut)
	}
	if args[0] != "sync" {
		return errors.New("usage: syntroph session close | sync <status|recovery|retry|resolve>")
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
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", item.SagaID, item.State, item.LastError, item.NextAction)
		}
		return nil
	case "recovery":
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
			fmt.Fprintf(out, "- saga %s: %s; attempts: %d; last error: %s; next: %s\n", item.SagaID, item.State, len(item.Attempts), item.LastError, item.NextAction)
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
		return retry(args[2:], j, out)
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
	formatValue := core.ArtifactFormat(strings.ToLower(*format))
	diary, delivery, err := (core.SessionCloser{Memory: memory, Bus: bus}).Close(context.Background(), core.SessionCloseRequest{RepositoryID: *repo, CommitSHA: *sha, Author: *author, Runtime: *runtime, Artifact: data, Format: formatValue, ManualSummary: *summary})
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

func retry(args []string, journal *core.SagaJournal, out interface{ Write([]byte) (int, error) }) error {
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
	count := 0
	for _, item := range items {
		if (scope == "graph" && item.State != core.GraphResolutionPending && item.State != core.GraphSyncPending) || (scope == "storage" && item.State == core.GraphResolutionPending) {
			continue
		}
		e := core.Event{EventID: item.SagaID + ":retry:" + scope, Type: "sync.retry.requested", OccurredAt: time.Now().UTC(), RepositoryID: item.RepositoryID, SagaID: item.SagaID, CorrelationID: item.SagaID, CausationID: item.EventID, SchemaVersion: 1, Payload: []byte(fmt.Sprintf(`{"scope":%q}`, scope))}
		if err := journal.AppendEvent(context.Background(), e); err != nil {
			return err
		}
		count++
	}
	fmt.Fprintf(out, "Retry requested for %s synchronization for %d saga(s). External effects require the configured adapter.\n", scope, count)
	return nil
}

func resolve(args []string, out interface{ Write([]byte) (int, error) }) error {
	// Accept the documented `resolve <id> [flags]` form even though the
	// standard flag parser stops at the first positional argument.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	fs := flag.NewFlagSet("sync resolve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	local, remote, keepLocal, keepRemote := fs.String("local-file", "", "local canonical content"), fs.String("remote-file", "", "remote content observed"), fs.Bool("keep-local", false, "overwrite remote with local content"), fs.Bool("keep-remote", false, "acknowledge remote content")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("sync resolve requires a conflict id")
	}
	if *keepLocal == *keepRemote {
		return errors.New("sync resolve requires exactly one of --keep-local or --keep-remote")
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
