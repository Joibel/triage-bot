# triage-bot

AI-assisted **stale triage** for a GitHub repository's backlog.

Fresh triage is something maintainers already do. What rots is the tail: issues
and pull requests nobody has looked at in years. triage-bot works that tail
least-recently-active first — it opens a bounded number of [beads](https://github.com/steveyegge/beads)
for an AI agent to assess, parses the verdict each finished bead carries, and
records it in a local SQLite database for a human to work through.

The same database is where everything else we know about the repository's
issues, pull requests and security advisories is kept: your decisions on each
verdict, free notes, typed links between items, and the open advisories with
their private-fork pull requests. It is searchable from the CLI and, over MCP,
from Claude Code sessions on the same machine. It holds pointers and what we
add to them, never bodies or comments: GitHub stays the source of truth for
content.

Ordering is by **last activity, not creation date**. An issue filed in 2018 that
people still discuss is not neglected; one filed in 2023 and untouched since is.
Sorting by last activity ascending also means actively-discussed items are never
reached until the dormant backlog is exhausted.

## What it does not do

These are design invariants, not current limitations:

- **It never writes to GitHub.** No comments, no labels, no closes. Every
  recommendation goes to a human, who decides and acts. The GitHub client has no
  write methods to accidentally call.
- **It never actions beads.** It opens them, reads their closed state, and keeps
  a fixed number in flight so triage work cannot flood whatever is doing the
  assessing.
- **Nothing sensitive leaves the machine in bulk.** Advisories, their private
  forks' pull requests, anything an advisory is about, and every note on them
  are marked sensitive, and bulk exports (`report --json`) leave them out, along
  with all notes.

## How it works

```
       ┌──────────┐  least recently active first, bounded by max_open_beads
       │  GitHub  │───────────────────────────┐
       └──────────┘  (read only)              ▼
                                        ┌──────────┐
                                        │  a bead  │  "assess #8123, reply
                                        │  per     │   with this template"
                                        │  item    │
                                        └────┬─────┘
                                             │  an AI agent (not this tool)
                                             │  assesses and closes it
                                             ▼
  ┌───────────────────┐   parse + validate   ┌──────────────┐
  │ argo-workflows.db │◄─────────────────────│ close reason │
  │ (source of truth) │   invalid → reopen   │  ```yaml ``` │
  └─────────┬─────────┘   with the errors    └──────────────┘
            │
            ▼  triage-bot review / report
      a human actions it on GitHub
```

Each cycle does five things: reconcile finished beads into verdicts, discover
more backlog, expire verdicts that have gone stale, top the queue back up, and
sync the repository's open security advisories.

## Getting started

```sh
# Create the database for a repository
triage-bot init --org argoproj --repo argo-workflows

# Set up the beads database the bot will open work in
bd init --prefix tb

export GITHUB_TOKEN=...     # read-only scope is enough for triage

# Run one cycle, or run continuously
triage-bot sync
triage-bot daemon --interval 5m
```

The database lives at `~/.local/share/triage-bot/argo-workflows.db` (under
`$XDG_DATA_HOME` if that is set), outside any checkout so it cannot be
committed by accident; `--db` points elsewhere. It is created mode 0600. The
daemon, the CLI, `review` and the MCP server can all use it at once.

Syncing advisories needs a token that can read them: the maintainer's own, with
`repo` scope. With a read-only triage token the advisory phase says so once and
steps aside, and triage carries on. `sync --only advisories` runs just that
phase, touching neither beads nor the backlog.

Then work the queue. `review` is the interactive way and probably what you
want; `report` is the same information as plain text, for reading and piping:

```sh
triage-bot review                  # walk the queue one item at a time

triage-bot status                  # backlog, work in flight, coverage
triage-bot report                  # verdicts awaiting your action
triage-bot report --json

triage-bot ack 8123 --applied
triage-bot ack 8123 --rejected --note "still repros on 3.7"
triage-bot retriage 8123

triage-bot reparse --dry-run       # re-read verdicts from their beads
triage-bot reparse
```

`reparse` re-reads the completion template of already-triaged items from the
beads that produced them. Use it after a parser fix: the bead still holds the
agent's original text, so a verdict recorded wrongly is recoverable without
re-running the assessment. Unlike `retriage` it costs no agent time and keeps
the bead, and your own `ack` decisions are preserved — it corrects what the
agent said, not what you decided about it.

`review` shows a one-line-per-item list, opens whichever you pick, and records
your decision without leaving the session. An open item shows its notes and
links under the verdict. From an item: `a`pplied, `r`ejected, `d`efer, `n`ote,
`c`opy the suggested comment to the clipboard, `o`pen it on GitHub, `b`ack to
the list, `q`uit. It needs a terminal and refuses to run in a pipe.

Rejecting a recommendation returns the item to the queue and puts your note in
front of the next agent, so it does not repeat the mistake. The rejected verdict
stays in the item's history, as does one that expires.

## Notes, links and advisories

```sh
triage-bot note '#8123' "reproduced on main with 5000 pods" --tag repro
triage-bot note GHSA-xxxx-xxxx-xxxx - < notes.txt      # text from stdin
triage-bot notes '#8123'

triage-bot link '#9000' fixes '#8123'
triage-bot link GHSA-xxxx-xxxx-xxxx advisory_for '#8123'

triage-bot show '#8123'            # item, verdicts, decisions, notes, links
triage-bot search informer resync  # full text over notes and titles
triage-bot search --tag backport   # filters alone list notes, newest first
triage-bot advisories              # open advisories, their fork PRs and links
```

A ref is `#123` or `123`, a GitHub URL, a GHSA id, or `fork#N` for a pull
request in an advisory's private fork (`argo-workflows-ghsa-xxxx-xxxx-xxxx#2`).
Only argo-workflows and its advisories' private forks are accepted.

Items come in **by touch, not by census**: triage discovery, the advisory sync,
and adding a note or a link to an item the database does not hold yet, which
asks GitHub for its identity and title. An item that comes in through a note is
not queued for triage; discovery enrols it when it reaches it, or `retriage`
queues it now.

Links have fixed types, each read from both ends (`#9000 fixes #8123` shows on
`#8123` as `fixed_by #9000`): `duplicate_of`, `fixes`, `advisory_for`,
`fix_in_fork`, `backport_of`, `related`. `advisory_for` and `fix_in_fork` always
start at the advisory, whichever way round you give them.

`search` filters by `--kind`, `--tag`, `--state` (triage or GitHub state) and
`--sensitive` or `--public`.

### Sensitivity

An advisory, every pull request in a private fork, and every note on either is
sensitive. So is an item an advisory is linked to by `advisory_for` or
`fix_in_fork`. Marking is never undone. `show`, `search` and `advisories` mark
sensitive items; `report --json` leaves them out, and leaves out all notes.

## Claude Code (MCP)

`triage-bot mcp` serves the database over stdio MCP to Claude Code sessions on
this machine, with the tools `show`, `verdicts` (triage verdicts by
recommendation, reason, kind and decision, most confident first), `search`,
`add_note`, `link`, `advisories` and `set_disposition`. Sessions may add notes and links without asking; what
they record names `claude` as its author. The server returns sensitive items
too, and tells the session they never leave the machine.

Register it once, for every project, giving the binary's full path if it is not
on `PATH` (`make` builds it into `bin/`):

```sh
claude mcp add -s user triage-bot -- triage-bot mcp --db ~/.local/share/triage-bot/argo-workflows.db
```

Adding a note to an item the database does not hold yet looks it up with
`GITHUB_TOKEN` or `GH_TOKEN` from the server's environment; advisories and
private forks need the maintainer's own token. To use the `gh` CLI's:

```sh
claude mcp add -s user triage-bot -- sh -c 'GITHUB_TOKEN=$(gh auth token) exec triage-bot mcp --db "$HOME/.local/share/triage-bot/argo-workflows.db"'
```

## The completion template

An agent finishes a bead by closing it with a fenced `yaml` block. Prose around
the block is fine; only the block is parsed.

````
bd close tb-a3f8e9 --reason-file - <<'EOF'
Assessed: this refers to a controller that no longer exists.

```yaml
recommendation: close
reason: already_fixed
confidence: 92
fixed_in: v3.6.0
reasoning: |
  The deadlock was in the legacy controller loop, removed in v3.5.
suggested_comment: |
  Closing - fixed in v3.6.0 by #7001. Please open a fresh issue if you
  still see this on a supported version.
evidence:
  - https://github.com/argoproj/argo-workflows/pull/7001
```
EOF
````

`recommendation`, `reason`, `confidence` (0-100) and `reasoning` are required.
`duplicate_of` and `fixed_in` are required by their respective reasons.

### What agents are told to assess against

Every bead instructs the agent to judge against **`origin/main` as it stands
today**, not the release the reporter was running or the base the author
branched from. If main is already good, the item is closable — `already_fixed`
or `obsolete`. Without this, agents reason about the version in the report
("this was broken in 3.4, so it is a real bug") and reach `keep_open` on things
main fixed years ago, which is the exact class of stale item this tool exists to
clear.

### Recommendations and reasons

Recommendations partition by **who acts next**, which is what `report` groups on.

| `recommendation` | Who acts next | Valid `reason` values |
|---|---|---|
| `keep_open` | Nobody | `still_valid`, `active_discussion` |
| `respond` | A maintainer | `add_context`, `good_first_issue`ⁱ, `workaround_exists` |
| `request_info` | The reporter or author | `needs_reproduction`ⁱ, `needs_detail`, `needs_version_info`, `still_wanted`ᵖ |
| `close` | Whoever closes it | `stale`, `already_fixed`, `duplicate`, `out_of_scope`, `not_a_bug`, `obsolete` |

ⁱ issues only ᵖ pull requests only

Pull requests use the same vocabulary: PR triage asks only whether the change is
still valid, still wanted, and not already implemented — not mechanical states
like needs-rebase.

If a template does not validate, the bead is reopened with the specific errors
attached as a note. After three failed attempts the item is handed to a human
and its slot is released.

## Configuration

Settings live in the database, so the daemon and CLI cannot disagree. The daemon
reads them every cycle, so a change takes effect without a restart:

```sh
triage-bot config                        # list them
triage-bot config max_open_beads 12      # change one
```

| Setting | Default | |
|---|---|---|
| `max_open_beads` | 10 | triage beads in flight at once |
| `retriage_after` | 4320h (180d) | how long a verdict stands |
| `max_template_attempts` | 3 | invalid templates before a human takes over |
| `bead_label` | triage-bot | identifies the beads this bot owns |

`org` and `repo` are set by `init` and fixed after.

Re-triage happens on age, on `retriage`, and on rejection — deliberately *not*
on upstream activity, so a drive-by comment does not requeue work you have
already actioned.

## Development

Requires [Nix](https://nixos.org/download.html), [devenv](https://devenv.sh/getting-started/),
and optionally [direnv](https://direnv.net/). The toolchain is pinned to an exact
`nixpkgs` revision in `devenv.yaml`.

```sh
direnv allow          # or: devenv shell
make all              # all checks, then build
make check            # fmt, lint, audit, test
go test -short ./...  # skip the slow beads integration tests
```

The binary will not build unless every quality check passes. See `make help`.
