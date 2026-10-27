# The store: triage notes for argo-workflows

triage-bot started as stale-backlog triage: it opens beads, parses verdicts, and
keeps them in a YAML status file. This document extends it into the place where
**everything we know about argo-workflows issues, pull requests and security
advisories** is recorded: the bot's verdicts, the maintainer's decisions, free
notes, and links between items, searchable from the CLI and from Claude Code
sessions over MCP.

Decided 2026-10-26. Scope is argo-workflows only, including its security
advisories (GHSAs) and their private forks.

## Why it changes

- **The status file has outgrown YAML.** 1,480 items (1,459 triaged, 1,205
  verdicts still pending a decision) in one 7.6 MB file, rewritten whole on
  every change under a flock sidecar.
- **There's nowhere to put what isn't a verdict.** Notes on a PR, the link from
  an advisory to its fix in a private fork, "this duplicates #1234": all of it
  lives in a head, and the advisories are what overflows first.
- **Some of it is security-sensitive.** Advisory details must not go anywhere
  networked, which rules out the Today deck's database (internet-facing, read
  by claude.ai). This machine is trusted for it; the notes stay here.
  Backups are out of scope.

## What it is not

**Not a mirror of argo-workflows.** The store holds pointers to GitHub items and
what *we* add to them: no bodies, no comments, no diffs. GitHub stays the source
of truth for content.

The existing invariants stand: **triage-bot never writes to GitHub**, and it
never actions beads (it opens them, reads their close reason, and reopens one
whose template failed validation).

## Storage

One SQLite database, by default `~/.local/share/triage-bot/argo-workflows.db`
(under `$XDG_DATA_HOME` when that is set), outside any checkout so it can't be
committed by accident. `--db` overrides it. File mode 0600, in a 0700 directory
if the bot creates it; SQLite gives the WAL files the same mode.

- Pure-Go driver (`modernc.org/sqlite`, which includes FTS5), so the binary
  stays CGO-free.
- WAL mode with a busy timeout: the daemon, the CLI, `review` and the MCP server
  run concurrently against the same file. Write transactions are `BEGIN
  IMMEDIATE`, so a read-modify-write holds the lock before it reads. This
  replaces `internal/lockfile`. No transaction is held across a GitHub or bd
  call.
- Schema migrations are embedded and run on open; the schema version is
  recorded, and a binary refuses a database newer than it understands.
- Settings and the discovery cursor move into the database (a `settings` table
  and a `cursor` row), so the daemon and the CLI still can't disagree.

### Tables

| Table | Holds |
| --- | --- |
| `items` | One row per GitHub thing we've touched. Identity: repo (`argoproj/argo-workflows`, or a private fork's name), kind (`issue`, `pr`, `advisory`), number or GHSA id. Plus title, GitHub state (open/closed/merged; an advisory's own state), `created_at`, `updated_at`, URL, and `sensitive`. No bodies. Pipeline state (`untriaged`, `queued`, `triaged`, `needs_human`, `closed_upstream`; NULL outside the pipeline), bead id, template attempts, last error, queued time, and the carry note for the next bead. |
| `verdicts` | triage-bot's parsed completion templates (`triage.Result`, one column per field), **with history**: a retriage adds a row, and the latest is current while the item is triaged. Requeue, expiry and rejection keep the old rows. Records the bead it came from and when, so `reparse` keeps working; reparse corrects a row in place. |
| `dispositions` | The maintainer's pending / applied / rejected / deferred decisions on a verdict, with note, time and author (`human` or `claude`). The latest is current; none means pending. |
| `notes` | Free text, timestamped, many per item, author (`human` or `claude`). Tags (e.g. `security`, `backport`, `customer`, `blocked`) in `note_tags`. Full-text indexed (FTS5, porter stemming), as are item titles. |
| `links` | Typed edges between items: `duplicate_of`, `fixes`, `advisory_for`, `fix_in_fork`, `backport_of`, `related`. Read in both directions. Who added it (`human`, `claude`, or `triage-bot` for the sync), and when. |
| `advisories` | Per advisory item: severity, CVE id, private fork repo. Its GHSA id and state are the item's identity and GitHub state. |

The verdict contract (`internal/triage`, the pair table) does not change: notes
are free-form, verdicts and dispositions keep their strict structure, and links
have fixed types so cross-referencing is reliable.

### Sensitivity

An advisory, every PR in a private fork, and every note on either is
`sensitive`. So is an item linked to an advisory by `advisory_for` or
`fix_in_fork`. Notes take their item's flag. Marking is sticky: nothing clears
it. Anything that leaves the store in bulk (`report --json`, and any future
export such as the Today deck's milestone 11) excludes sensitive items and all
notes. The MCP server is local (stdio) and returns everything: it serves Claude
Code sessions on this machine, and its instructions tell them sensitive items
never leave it.

## How items get in

**By touch, not by census** — the guard against mirroring the repo:

1. **Triage discovery**, as today: least-recently-active first, bounded by
   `max_open_beads`.
2. **Advisories**, synced automatically each tick: every advisory in the
   `triage`, `draft` or `published` state (closed ones are not synced). One
   listing of every state serves the whole sync. An advisory that closes stays
   in the store with its notes, marked closed; one that only vanishes from the
   listing is left as it was.
3. **Private-fork PRs**, synced automatically for each synced advisory with a
   private fork, and linked to it (`fix_in_fork`).
4. **Notes and links.** Adding a note to, or linking, an item we don't have
   yet fetches its identity and title from GitHub and inserts it, outside the
   triage pipeline: discovery enrols it if it reaches it, or `retriage` queues
   it. `show` never fetches.

Reading advisories needs a token that can see them (the maintainer's own, with
`repo` scope; the triage-only read token is not enough). Without it, the
advisory sync logs once and skips; triage carries on.

## Interfaces

- **CLI**: everything today (`sync`, `daemon`, `status`, `report`, `review`,
  `ack`, `retriage`, `reparse`) on the new store, plus `config` for the
  settings that used to be edited in the YAML, `sync --only <phase>`, and:
  - `note <ref> <text> [--tag t]`, `notes <ref>`
  - `link <ref> <type> <ref>`, `show <ref>` (item, current verdict and history,
    disposition, notes, links both ways)
  - `search <query>` (full text over notes and titles; filters by kind, tag,
    state, sensitive)
  - `advisories` (open advisories with their fork PRs and links)

  A ref is `#123`, `123`, a GitHub URL, a GHSA id, or `fork-repo#N`.
- **MCP**: `triage-bot mcp`, stdio, registered in Claude Code by the user. Tools:
  `show` (one item, as the CLI), `search`, `add_note`, `link`, `advisories`,
  `set_disposition`. Claude sessions **may add notes and links without
  asking**; notes record `claude` as their author.
- **`review`** gains the notes and links of the item on screen, and a key to add
  a note.

## Migration

Done on 2026-10-27. A one-off `import-yaml` command copied the YAML status file
into the database (every item, verdict, decision, bead id, setting and the
cursor), checking it field by field, and was then removed with its package; git
history has both. The `--file`/`-f` flag is replaced by `--db`.

## Today deck (milestone 11)

The deck may later show triage verdicts as cards. It gets public items' current
verdicts only: nothing sensitive and no notes. That integration is not part of
this change.
