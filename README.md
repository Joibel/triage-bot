# triage-bot

AI-assisted **stale triage** for a GitHub repository's backlog.

Fresh triage is something maintainers already do. What rots is the tail: issues
and pull requests nobody has looked at in years. triage-bot works that tail
least-recently-active first — it opens a bounded number of [beads](https://github.com/steveyegge/beads)
for an AI agent to assess, parses the verdict each finished bead carries, and
records it in a YAML status file for a human to work through.

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
  │ triage-bot.yaml   │◄─────────────────────│ close reason │
  │ (source of truth) │   invalid → reopen   │  ```yaml ``` │
  └─────────┬─────────┘   with the errors    └──────────────┘
            │
            ▼  triage-bot report
      a human actions it on GitHub
```

Each cycle does four things: reconcile finished beads into verdicts, discover
more backlog, expire verdicts that have gone stale, and top the queue back up.

## Getting started

```sh
# Create the status file for a repository
triage-bot init --org argoproj --repo argo-workflows

# Set up the beads database the bot will open work in
bd init --prefix tb

export GITHUB_TOKEN=...     # read-only scope is sufficient

# Run one cycle, or run continuously
triage-bot sync
triage-bot daemon --interval 5m
```

Then work the queue:

```sh
triage-bot status                  # backlog, work in flight, coverage
triage-bot report                  # verdicts awaiting your action
triage-bot report --json

triage-bot ack 8123 --applied
triage-bot ack 8123 --rejected --note "still repros on 3.7"
triage-bot retriage 8123
```

Rejecting a recommendation returns the item to the queue and puts your note in
front of the next agent, so it does not repeat the mistake.

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

Everything lives in the status file, so the daemon and CLI cannot disagree:

```yaml
config:
  max_open_beads: 10          # triage beads in flight at once
  retriage_after: 4320h       # 180d; how long a verdict stands
  max_template_attempts: 3
  bead_label: triage-bot
```

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
