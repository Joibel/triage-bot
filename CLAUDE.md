# Claude Code Instructions

## Project Overview

triage-bot drives AI-assisted **stale triage** of a GitHub repository's most
neglected open issues and pull requests. It opens a bounded number of beads for
an AI agent to assess, parses the completion template each closed bead carries,
and records the verdict in a YAML status file for a human to action.

The backlog is worked **least-recently-active first** (`updated_at` ascending),
not oldest-created. Dormancy is what makes an item worth triaging; age is not.
`TestQueueOrdersByActivityNotAge` pins this.

Two invariants govern the whole design. Break either and the tool is no longer
what it is meant to be:

1. **It never writes to GitHub.** `internal/github` contains read methods only;
   there are no write methods to accidentally call. Every recommendation is
   delivered to a human, who decides and acts.
2. **It never actions beads.** It opens them, reads their closed state, and
   keeps a bounded number in flight. The one write it makes to a bead is
   reopening one whose completion template did not validate, which is a
   correction to our own request rather than triage work.

**The status file is the source of truth, not beads.** A bead is a disposable
work ticket whose only durable output is the completion template in its close
reason. Losing the beads database costs at most the in-flight work.

## Architecture

| Path | Role |
|---|---|
| `internal/triage` | The completion-template contract: the recommendation/reason pair table, parsing, validation, and the agent instructions generated from it |
| `internal/state` | The status file: schema, atomic writes, the `Update` transaction |
| `internal/lockfile` | flock on a `<file>.lock` sidecar (copied from the sibling cherry-picker project) |
| `internal/github` | Read-only GitHub client |
| `internal/beads` | `bd` CLI wrapper behind a fakeable interface |
| `internal/engine` | The four-phase tick: reconcile, discover, expire, top up |
| `cmd_*.go` | Cobra commands, one file per group |
| `cmd_review.go` | The interactive queue walker: loop and state |
| `cmd_review_view.go` | Its rendering, plus the clipboard and browser helpers |

`internal/engine` is named engine rather than sync so files there can still use
stdlib `sync`.

### The pair table is the single source of truth

`triage.Table` drives three things at once: validation, the wording of
validation errors, and the instruction text embedded in every bead. Adding a
reason to the table automatically tells agents about it, and `TestTableCoverage`
fails if a value ever fails to reach the generated instructions.

Never hardcode a recommendation or reason list anywhere else.

`ExtractYAML` must treat fences as nesting. A `suggested_comment` routinely
contains a fenced example, and taking the first inner ``` as the end of the
template silently truncated verdicts — dropping the rest of the comment plus
`suggested_labels` and `evidence`, while what remained still parsed and
validated, so nothing flagged it. 32 of 353 verdicts in the first real run were
damaged this way. A bare ``` inside a value is genuinely ambiguous, so that case
is refused with an actionable message rather than silently truncated.

The one part of the bead instructions that is *not* generated is
`assessmentGuidance` in `render.go`, which tells the agent to judge against
`origin/main` rather than the reporter's version. It is a constant because it is
identical for every item and both kinds. `TestInstructionsStateTheAssessmentTarget`
asserts it is present and precedes the reporting contract.

### GitHub: use the issues-list endpoint, not search

`search/issues` now returns 422 unless the query commits to `is:issue` or
`is:pull-request`, so it cannot fetch both kinds in one call. Discovery uses
`/repos/{org}/{repo}/issues` instead, whose `since` parameter filters on
`updated_at` — exactly the cursor bound needed — and which returns issues and
pull requests together. It also has a much higher rate limit (5000/hr against
search's 30/min) and no 1000-result ceiling.

`TestListUpdatedSinceUsesListEndpointNotSearch` asserts the path, so a
well-meaning switch back to search will fail rather than 422 in production.

### Things learned about `bd` that the code depends on

Verified against bd 1.1.0 by `internal/engine/integration_test.go`:

- `bd show --json` **and** `bd query --json` both return `close_reason` verbatim,
  so reconciling closed beads is a single query, not an N+1.
- `bd reopen` **clears** `close_reason`, and its `--reason` text is recorded only
  as an event: invisible in `show`, `show --long` and `history`. Validation
  errors therefore go to the agent via `bd note`, which *is* visible in both
  `bd show` and `show --json`, and accumulates across attempts.

If you change how beads are read or written, run the integration tests — the
in-memory fake will otherwise happily keep agreeing with a wrong assumption.

## Development Environment

devenv (Nix), pinned to an exact `nixpkgs` revision in `devenv.yaml`. Run
`direnv allow` or `devenv shell`:

- Go 1.26.5, golangci-lint 2.12.2, goimports
- `bd` (beads) must be on PATH for the integration tests; they skip without it
- `govulncheck` runs via `go run` at a pinned version in `make audit`

## Build System

- `make all` - all checks then build (default)
- `make check` - fmt, lint, audit, test
- `make test` - tests with the race detector
- `go test -short ./...` - skips the slow beads integration tests

The binary will not build unless every quality check passes.

## Code Style

- Standard Go conventions; all code must pass golangci-lint with this project's
  configuration
- `goimports` with local module grouping; run `make fmt` before committing
- Table-driven tests, `testify` for assertions, **no** testify suite

### Deviations from the template's lint config, and why

`.golangci.yaml` was adjusted where the template's defaults fought this
project's data model. Each has a comment in the file; do not revert them
without reading it:

- `tagliatelle` set to snake_case for yaml and json — bd's JSON output is
  snake_case and is not ours to change
- `govet` `fieldalignment` disabled — struct field order determines key order in
  the status file humans read and edit
- `errcheck` `check-blank: false` — `_ = f.Close()` is how Go states a
  deliberately dropped error
- `exhaustive` `default-signifies-exhaustive: true`

## Testing

The engine tests use in-memory fakes for both bd and GitHub, so the four-phase
tick is tested without either. `integration_test.go` additionally drives a real
beads database to check the assumptions the fake encodes.

When adding a triage outcome, add it to `triage.Table` and nothing else: the
existing tests will cover it automatically.


<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->
