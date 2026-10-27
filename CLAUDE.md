# Claude Code Instructions

## Project Overview

triage-bot drives AI-assisted **stale triage** of a GitHub repository's most
neglected open issues and pull requests. It opens a bounded number of beads for
an AI agent to assess, parses the completion template each closed bead carries,
and records the verdict in a local SQLite database for a human to action.

The same database keeps everything else known about argo-workflows items: the
maintainer's decisions, free notes, typed links, and the open security
advisories with their private-fork PRs. The CLI and a stdio MCP server for
Claude Code read and write it. The design is in `docs/store.md`.

The backlog is worked **least-recently-active first** (`updated_at` ascending),
not oldest-created. Dormancy is what makes an item worth triaging; age is not.
`TestQueueOrdersByActivityNotAge` pins this.

Three invariants govern the whole design. Break any and the tool is no longer
what it is meant to be:

1. **It never writes to GitHub.** `internal/github` contains read methods only;
   there are no write methods to accidentally call. Every recommendation is
   delivered to a human, who decides and acts.
2. **It never actions beads.** It opens them, reads their closed state, and
   keeps a bounded number in flight. The one write it makes to a bead is
   reopening one whose completion template did not validate, which is a
   correction to our own request rather than triage work.
3. **Nothing sensitive leaves in bulk.** Advisories, private-fork PRs, items
   linked to an advisory by `advisory_for` or `fix_in_fork`, and every note on
   any of them are sensitive (`store.InsertItem` and `Tx.AddLink` mark them;
   notes take their item's flag). Marking is sticky. Any bulk export
   (`report --json`, a future Today deck feed) leaves out sensitive items and
   all notes. Only the local MCP server returns them. The advisory sync logs
   counts at info, ids only at debug. Never put real advisory data in
   fixtures, tests or commits.

**The database is the source of truth, not beads.** A bead is a disposable
work ticket whose only durable output is the completion template in its close
reason. Losing the beads database costs at most the in-flight work. The store
holds pointers to GitHub items plus what we add to them, never bodies or
comments.

## Architecture

| Path | Role |
|---|---|
| `internal/triage` | The completion-template contract: the recommendation/reason pair table, parsing, validation, and the agent instructions generated from it |
| `internal/store` | The SQLite database: embedded migrations, `Update`/`View` transactions, items, verdicts with history, dispositions, notes (FTS5), links, advisories, settings, cursor, refs |
| `internal/github` | Read-only GitHub client (issues, advisories, fork PRs); `githubtest` is the shared fake |
| `internal/ingest` | GitHub metadata to store rows, for discovery, the advisory sync and notes on unknown items |
| `internal/beads` | `bd` CLI wrapper behind a fakeable interface |
| `internal/engine` | The tick: reconcile, discover, expire, top up, sync advisories |
| `internal/service` | What the CLI and the MCP server share: show, search, notes, links, decisions, advisories |
| `internal/mcpserver` | `triage-bot mcp`: the service over stdio MCP, authoring as `claude` |
| `cmd_*.go` | Cobra commands, one file per group |
| `cmd_review.go` | The interactive queue walker: loop and state |
| `cmd_reparse.go` | Re-reads recorded verdicts from their beads after a parser fix |
| `cmd_review_view.go` | Its rendering, plus the clipboard and browser helpers |
| `cmd_notes.go` | `note`, `notes`, `link`, `show`, `search`, `advisories` and their rendering |

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

### Discovery is bounded by new items, never by rows fetched

`since` is inclusive and the cursor only advances past a *strictly* later
timestamp, so every item sharing the cursor's second comes back on every call.
Bound the fetch by rows and discovery wedges permanently the moment such a
block grows as large as the window: each tick refetches the same known rows,
adds nothing, and leaves the cursor exactly where it was. This is not
hypothetical — a bulk mergeability recompute touched 17 open PRs in one second,
16 of them already tracked, against a 16-row window, and discovery stopped dead
about 250 items short of the backlog with one PR stranded inside the block.

So `ListUpdatedSince` takes a `yield` callback rather than a limit: only the
engine knows which rows are new, so only the engine can say when to stop.
`scanCeiling` bounds the walk, but deliberately cannot stop it inside the block
at the cursor — a stop that neither adds an item nor moves the cursor is the
same livelock again. `TestDiscoverStepsOverATieBlockLargerThanTheWindow` and
`TestDiscoverScanCeilingNeverStopsInsideTheBlockAtTheCursor` pin both halves.

### The store

- **Every write transaction is `BEGIN IMMEDIATE`** (`_txlock=immediate`), so a
  read-modify-write takes the write lock before it reads. That is what replaced
  the flock sidecar's reload-under-lock; `View` is a plain read snapshot. WAL
  lets readers carry on beside the writer. `TestConcurrentWritersFromSeparateHandlesLoseNothing`
  pins it.
- **No transaction is held across a network call.** Engine phases and the
  service read, do their GitHub or bd work, then write in a short transaction
  that re-checks what it changes. `ingest.Fetch` returns the write as a closure
  for exactly this. Holding the lock while waiting on GitHub would stall
  `review` and the MCP server behind the daemon.
- **Times are stored fixed-width** (`2006-01-02T15:04:05.000000000Z`, UTC).
  RFC 3339 with a variable fraction does not sort as text, and the backlog is
  ordered by `updated_at` in SQL. `TestNextUntriagedOrdersByActivityAcrossFractionalSeconds`.
- **A verdict is current only while its item is triaged.** Requeue, expiry and
  rejection keep old verdicts as history; the latest row is current. Rejection
  records a `rejected` disposition on the verdict before requeueing with the
  note carried forward. `reparse` corrects a verdict in place, keeping its
  decisions.
- **Items outside the pipeline have a NULL state**: advisories, fork PRs, and
  anything that came in through a note or link. Discovery enrols such an item
  when it reaches it (`Tx.Enroll`).
- **No CHECK lists recommendations or reasons** in the schema: the pair table
  stays the only list.
- **Migrations are embedded SQL** in `internal/store/migrations`, applied on
  open under the write lock; `PRAGMA user_version` is the schema version, and
  a newer database is refused. Never edit a migration that has shipped; add one.
- **Empty lists are stored as NULL**; `sameVerdict` relies on that.

### Advisories

The `advisories` phase lists every advisory in one call and upserts those in
`triage`, `draft` or `published`; one we hold that has since closed is marked
so, and one that merely vanishes from the listing is left alone (a token losing
sight of it is no evidence it closed). Each open advisory's private fork has its
PRs synced and linked `fix_in_fork`. A token that cannot read advisories gets
`github.ErrNoAccess` (401/403/404), and the phase warns once per process and
returns nil so triage carries on. `TestClientHasNoWriteMethods` fails if the
GitHub client ever gains a method that is not a `Get` or `List`.

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

- Go 1.27.3, golangci-lint 2.15.0, goimports
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
  snake_case and is not ours to change, and the MCP tools' JSON follows it
- `govet` `fieldalignment` disabled — struct fields are ordered for the reader
  (identity, then state), not for padding
- `errcheck` `check-blank: false` — `_ = f.Close()` is how Go states a
  deliberately dropped error
- `exhaustive` `default-signifies-exhaustive: true`
- `wrapcheck` ignores `internal/store` and `internal/service` — store errors
  already say what failed and `Update`/`View` return the callback's error
  as-is; service errors are the messages the user or a Claude session reads

## Testing

The engine tests use an in-memory fake for bd, `githubtest.Fake` for GitHub,
and a temp-file SQLite store, so the tick is tested without the network.
`integration_test.go` additionally drives a real beads database to check the
assumptions the fake encodes. Command tests drive `newRoot` with `opts.gitHub`
pointed at the fake.

Test data is made up. Never copy anything from the real database into
fixtures, tests or commits; advisories especially.

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
