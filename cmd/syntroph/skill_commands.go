package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
		return errors.New("usage: syntroph skill <sync|list|show> [--root repository]")
	}
	command := args[0]
	commandArgs := args[1:]
	if command == "show" && len(commandArgs) > 0 && !strings.HasPrefix(commandArgs[0], "-") {
		commandArgs = append(append([]string(nil), commandArgs[1:]...), commandArgs[0])
	}
	flags := flag.NewFlagSet("skill "+command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	repositoryRoot := flags.String("root", ".", "Repository Installation root")
	if err := flags.Parse(commandArgs); err != nil {
		return err
	}
	if command == "show" {
		if flags.NArg() != 1 {
			return errors.New("syntroph skill show requires one qualified package name")
		}
	} else if flags.NArg() != 0 {
		return fmt.Errorf("syntroph skill %s does not accept positional arguments", command)
	}
	if command != "sync" && command != "list" && command != "show" {
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
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
