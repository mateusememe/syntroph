// Command syntroph exposes the explicit recovery surface. Recovery commands
// are intentionally read-only unless sync resolve is given an explicit choice.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateusememe/syntroph/core"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "syntroph:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut interface{ Write([]byte) (int, error) }) error {
	if len(args) < 2 || args[0] != "sync" {
		return errors.New("usage: syntroph sync <status|recovery|retry|resolve>")
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
			fmt.Fprintf(out, "- saga %s: %s; next: %s\n", item.SagaID, item.State, item.NextAction)
		}
		return nil
	case "retry":
		return retry(args[2:], out)
	case "resolve":
		return resolve(args[2:], out)
	default:
		return fmt.Errorf("unknown sync command %q", args[1])
	}
}

func retry(args []string, out interface{ Write([]byte) (int, error) }) error {
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
	// The explicit command records intent at the boundary. Adapter wiring is
	// supplied by the embedding runtime/MCP; no effect is attempted here.
	fmt.Fprintf(out, "Retry requested for %s synchronization. No external effects were executed; run through the configured adapter.\n", scope)
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
