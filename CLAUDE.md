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

`internal/engine` is named engine rather than sync so files there can still use
stdlib `sync`.

### The pair table is the single source of truth

`triage.Table` drives three things at once: validation, the wording of
validation errors, and the instruction text embedded in every bead. Adding a
reason to the table automatically tells agents about it, and `TestTableCoverage`
fails if a value ever fails to reach the generated instructions.

Never hardcode a recommendation or reason list anywhere else.

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
