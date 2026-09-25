package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateusememe/syntroph/adapters/skillfilesystem"
	"github.com/mateusememe/syntroph/config"
	"github.com/mateusememe/syntroph/core"
)

type skillShowView struct {
	State      core.SkillCatalogState `json:"state"`
	Package    skillPackageMetadata   `json:"package"`
	Diagnostic string                 `json:"diagnostic,omitempty"`
}

type skillPackageMetadata struct {
	SchemaVersion      int                       `json:"schema_version"`
	Identity           core.SkillPackageIdentity `json:"identity"`
	Description        string                    `json:"description"`
	License            string                    `json:"license"`
	SourceURL          string                    `json:"source_url"`
	SourceRevision     string                    `json:"source_revision"`
	InstructionsPath   string                    `json:"instructions_path"`
	Assets             []core.SkillAsset         `json:"assets,omitempty"`
	ArgumentsSchema    map[string]any            `json:"arguments_schema,omitempty"`
	CompatibleRuntimes []string                  `json:"compatible_runtimes,omitempty"`
}

func runSkill(args []string, out, errOut interface{ Write([]byte) (int, error) }) error {
	if len(args) == 0 {
		return errors.New("usage: syntroph skill <sync|list|show|verify|prepare|recovery> [--root repository]")
	}
	command := args[0]
	commandArgs := args[1:]
	if (command == "show" || command == "verify" || command == "prepare") && len(commandArgs) > 0 && !strings.HasPrefix(commandArgs[0], "-") {
		commandArgs = append(append([]string(nil), commandArgs[1:]...), commandArgs[0])
	}
	flags := flag.NewFlagSet("skill "+command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	repositoryRoot := flags.String("root", ".", "Repository Installation root")
	var clearOrphan *bool
	if command == "recovery" {
		clearOrphan = flags.Bool("clear-orphan", false, "clear synchronization state after process-liveness verification")
	}
	var prepareRuntime, prepareArguments, prepareArgumentsFile, prepareInvocationID, prepareOutput, prepareRepository, prepareSession *string
	if command == "prepare" {
		prepareRuntime = flags.String("runtime", "", "runtime to validate compatibility against and record as a hint")
		prepareArguments = flags.String("arguments", "", "skill arguments as a JSON object")
		prepareArgumentsFile = flags.String("arguments-file", "", "path to a file containing skill arguments as a JSON object")
		prepareInvocationID = flags.String("invocation-id", "", "explicit invocation id; repeating it replays the same bundle identity")
		prepareOutput = flags.String("output", "", "write the prepared bundle atomically to this file with mode 0600 instead of stdout")
		prepareRepository = flags.String("repository", "", "repository identity recorded on journaled skill events (defaults to the absolute --root path)")
		prepareSession = flags.String("session", "", "optional runtime session identity used to correlate journaled skill events")
	}
	if err := flags.Parse(commandArgs); err != nil {
		return err
	}
	if command == "show" || command == "prepare" {
		if flags.NArg() != 1 {
			return fmt.Errorf("syntroph skill %s requires one qualified name, alias, or unique unqualified name", command)
		}
	} else if command == "verify" {
		if flags.NArg() > 1 {
			return errors.New("syntroph skill verify accepts at most one qualified package name")
		}
	} else if flags.NArg() != 0 {
		return fmt.Errorf("syntroph skill %s does not accept positional arguments", command)
	}
	if command != "sync" && command != "list" && command != "show" && command != "verify" && command != "prepare" && command != "recovery" {
		return fmt.Errorf("unknown skill command %q", command)
	}

	cfg, err := config.Load(filepath.Join(*repositoryRoot, ".syntroph", "config.yaml"))
	if err != nil {
		return err
	}
	resolved, err := cfg.ResolveSkills(*repositoryRoot)
	if err != nil {
		return err
	}
	if !resolved.Enabled {
		return errors.New("skills are disabled; configure skills.enabled in .syntroph/config.yaml")
	}
	catalog, err := skillfilesystem.New(*repositoryRoot, resolved)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if command == "verify" {
		return runSkillVerify(ctx, catalog, flags.Arg(0), out)
	}
	if command == "prepare" {
		return runSkillPrepare(ctx, catalog, *repositoryRoot, flags.Arg(0), skillPrepareOptions{
			runtime:       *prepareRuntime,
			arguments:     *prepareArguments,
			argumentsFile: *prepareArgumentsFile,
			invocationID:  *prepareInvocationID,
			output:        *prepareOutput,
			repository:    *prepareRepository,
			session:       *prepareSession,
		}, out)
	}
	var value any
	switch command {
	case "sync":
		value, err = catalog.Sync(ctx)
	case "list":
		value, err = catalog.List(ctx)
	case "show":
		entry, showErr := catalog.Show(ctx, flags.Arg(0))
		err = showErr
		if showErr == nil {
			pkg := entry.Package
			value = skillShowView{
				State: entry.State,
				Package: skillPackageMetadata{
					SchemaVersion: pkg.SchemaVersion, Identity: pkg.Identity, Description: pkg.Description,
					License: pkg.License, SourceURL: pkg.SourceURL, SourceRevision: pkg.SourceRevision,
					InstructionsPath: pkg.InstructionsPath, Assets: pkg.Assets,
					ArgumentsSchema: pkg.ArgumentsSchema, CompatibleRuntimes: pkg.CompatibleRuntimes,
				},
				Diagnostic: entry.Diagnostic,
			}
		}
	case "recovery":
		if *clearOrphan {
			err = catalog.RecoverOrphanedSync(ctx)
			if err != nil {
				break
			}
		}
		value, err = catalog.InspectSyncRecovery(ctx)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// runSkillVerify reports every requested catalog entry's verification
// outcome before returning a non-zero result, so CI can both inspect what
// was checked and detect failure from the exit code alone. name is empty
// when the whole catalog should be verified.
func runSkillVerify(ctx context.Context, catalog *skillfilesystem.Catalog, name string, out interface{ Write([]byte) (int, error) }) error {
	entries, verifyErr := catalog.Verify(ctx, name)
	if entries != nil {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if encodeErr := encoder.Encode(entries); encodeErr != nil {
			return encodeErr
		}
	}
	return verifyErr
}

// skillPrepareOptions collects the flags accepted by `syntroph skill
// prepare`.
type skillPrepareOptions struct {
	runtime       string
	arguments     string
	argumentsFile string
	invocationID  string
	output        string
	repository    string
	session       string
}

// runSkillPrepare resolves name (a canonical qualified name, an explicit
// alias, or a unique unqualified name), validates and prepares its
// immutable Skill Bundle, and emits the complete bundle as JSON: to stdout
// by default, or atomically to opts.output with private mode 0600. It never
// writes anywhere else, so a caller-selected output path is the only file
// this command can create.
//
// Preparation is journaled through core.SkillPreparer under
// <repositoryRoot>/.syntroph/journal, the same Saga Journal directory used
// by `syntroph sync` and `syntroph session close`. A prepare-requested event
// is durable before Prepare runs, and a prepared or prepare-failed event
// always follows, so the outcome is observable through the journal even
// though this command does not expose a dedicated recovery surface for it:
// a prepare failure is validation feedback, not a pending synchronization
// obligation.
func runSkillPrepare(ctx context.Context, catalog *skillfilesystem.Catalog, repositoryRoot, name string, opts skillPrepareOptions, out interface{ Write([]byte) (int, error) }) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("syntroph skill prepare requires one qualified name, alias, or unique unqualified name")
	}
	if opts.arguments != "" && opts.argumentsFile != "" {
		return errors.New("syntroph skill prepare accepts at most one of --arguments or --arguments-file")
	}
	arguments, err := parseSkillPrepareArguments(opts)
	if err != nil {
		return err
	}
	repositoryID := strings.TrimSpace(opts.repository)
	if repositoryID == "" {
		absRoot, absErr := filepath.Abs(repositoryRoot)
		if absErr != nil {
			return fmt.Errorf("resolve repository identity: %w", absErr)
		}
		repositoryID = absRoot
	}
	journal, err := core.NewSagaJournal(filepath.Join(repositoryRoot, ".syntroph", "journal"))
	if err != nil {
		return err
	}
	bus, err := core.NewEventBus(journal)
	if err != nil {
		return err
	}
	preparer := core.SkillPreparer{Port: catalog, Bus: bus}
	bundle, _, err := preparer.Prepare(ctx, core.SkillPrepareRequest{
		Name:         name,
		InvocationID: strings.TrimSpace(opts.invocationID),
		Runtime:      strings.TrimSpace(opts.runtime),
		Arguments:    arguments,
	}, core.SkillPrepareOptions{
		RepositoryID: repositoryID,
		SessionID:    strings.TrimSpace(opts.session),
	})
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("encode prepared skill bundle: %w", err)
	}
	encoded = append(encoded, '\n')
	if opts.output == "" {
		_, err := out.Write(encoded)
		return err
	}
	return skillfilesystem.WritePrivateFile(opts.output, encoded)
}

func parseSkillPrepareArguments(opts skillPrepareOptions) (map[string]any, error) {
	var raw []byte
	switch {
	case opts.argumentsFile != "":
		data, err := os.ReadFile(opts.argumentsFile)
		if err != nil {
			return nil, fmt.Errorf("read skill arguments file: %w", err)
		}
		raw = data
	case opts.arguments != "":
		raw = []byte(opts.arguments)
	default:
		return nil, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		return nil, fmt.Errorf("parse skill arguments JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("skill arguments must contain exactly one JSON object")
		}
		return nil, fmt.Errorf("parse skill arguments JSON: %w", err)
	}
	return arguments, nil
}
