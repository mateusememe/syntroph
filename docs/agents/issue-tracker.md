# Issue tracker: GitHub

Issues and specs for this repository live in GitHub Issues at `mateusememe/syntroph`. Use the `gh` CLI for all operations. Infer the repository from the Git remote when running commands inside this clone.

## Conventions

- Create: `gh issue create --title "..." --body "..."`
- Read: `gh issue view <number> --comments`
- List: `gh issue list --state open`
- Comment: `gh issue comment <number> --body "..."`
- Labels: `gh issue edit <number> --add-label "..."` or `--remove-label "..."`
- Close: `gh issue close <number> --comment "..."`

## Pull requests as a triage surface

PRs as a request surface: no. Triage covers GitHub Issues only unless this file is explicitly changed.

## Skill publication rule

When a skill says to publish a spec, create one GitHub issue and apply the `ready-for-agent` label.
