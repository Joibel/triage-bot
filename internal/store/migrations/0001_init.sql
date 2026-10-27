-- The first schema: everything the YAML status file held, plus notes, links
-- and advisories. Times are UTC text with nine fractional digits, so they sort
-- as strings in the order they happened.

-- Operator settings, one row per key, so the daemon and the CLI read the same
-- values (see store.Settings for the keys).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- How far through the backlog discovery has walked, by last activity.
CREATE TABLE cursor (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    updated_through TEXT
) STRICT;
INSERT INTO cursor (id, updated_through) VALUES (1, NULL);

-- One row per GitHub thing we have touched: a pointer plus what we add to it,
-- never its body or comments.
--
-- state is the triage pipeline's, and NULL for anything outside it: advisories,
-- private-fork PRs, and items that came in through a note or a link.
CREATE TABLE items (
    id         INTEGER PRIMARY KEY,
    repo       TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('issue', 'pr', 'advisory')),
    number     INTEGER,
    ghsa_id    TEXT,
    title      TEXT    NOT NULL DEFAULT '',
    gh_state   TEXT    NOT NULL DEFAULT '',
    created_at TEXT,
    updated_at TEXT,
    url        TEXT    NOT NULL DEFAULT '',
    sensitive  INTEGER NOT NULL DEFAULT 0 CHECK (sensitive IN (0, 1)),
    state      TEXT CHECK (state IN ('untriaged', 'queued', 'triaged', 'needs_human', 'closed_upstream')),
    bead_id    TEXT    NOT NULL DEFAULT '',
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT    NOT NULL DEFAULT '',
    queued_at  TEXT,
    carry_note TEXT    NOT NULL DEFAULT '',
    CHECK ((kind = 'advisory') = (ghsa_id IS NOT NULL)),
    CHECK ((kind = 'advisory') = (number IS NULL)),
    CHECK (kind != 'advisory' OR state IS NULL)
) STRICT;
CREATE UNIQUE INDEX items_by_number ON items (repo, number) WHERE number IS NOT NULL;
CREATE UNIQUE INDEX items_by_ghsa ON items (ghsa_id) WHERE ghsa_id IS NOT NULL;
CREATE INDEX items_by_state ON items (state, updated_at) WHERE state IS NOT NULL;
CREATE INDEX items_by_bead ON items (bead_id) WHERE bead_id != '';

-- Parsed completion templates, with history: a retriage adds a row, and the
-- latest row is the current verdict while the item is triaged. No CHECK on
-- recommendation or reason: triage.Table is the only list of those.
CREATE TABLE verdicts (
    id                INTEGER PRIMARY KEY,
    item_id           INTEGER NOT NULL REFERENCES items (id),
    bead_id           TEXT    NOT NULL DEFAULT '',
    triaged_at        TEXT,
    recommendation    TEXT    NOT NULL,
    reason            TEXT    NOT NULL,
    confidence        INTEGER,
    reasoning         TEXT    NOT NULL DEFAULT '',
    suggested_comment TEXT    NOT NULL DEFAULT '',
    suggested_labels  TEXT,
    evidence          TEXT,
    duplicate_of      INTEGER,
    fixed_in          TEXT    NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX verdicts_by_item ON verdicts (item_id, id);

-- The maintainer's decisions on a verdict. The latest row is the current one;
-- a verdict with no row is pending.
CREATE TABLE dispositions (
    id         INTEGER PRIMARY KEY,
    verdict_id INTEGER NOT NULL REFERENCES verdicts (id),
    state      TEXT    NOT NULL CHECK (state IN ('pending', 'applied', 'rejected', 'deferred')),
    note       TEXT    NOT NULL DEFAULT '',
    at         TEXT,
    author     TEXT    NOT NULL CHECK (author IN ('human', 'claude'))
) STRICT;
CREATE INDEX dispositions_by_verdict ON dispositions (verdict_id, id);

CREATE TABLE notes (
    id         INTEGER PRIMARY KEY,
    item_id    INTEGER NOT NULL REFERENCES items (id),
    body       TEXT    NOT NULL CHECK (body != ''),
    author     TEXT    NOT NULL CHECK (author IN ('human', 'claude')),
    created_at TEXT    NOT NULL
) STRICT;
CREATE INDEX notes_by_item ON notes (item_id, id);

CREATE TABLE note_tags (
    note_id INTEGER NOT NULL REFERENCES notes (id),
    tag     TEXT    NOT NULL CHECK (tag != ''),
    PRIMARY KEY (note_id, tag)
) STRICT, WITHOUT ROWID;
CREATE INDEX note_tags_by_tag ON note_tags (tag);

-- Typed edges, read in both directions. advisory_for and fix_in_fork always
-- start at the advisory.
CREATE TABLE links (
    id         INTEGER PRIMARY KEY,
    from_item  INTEGER NOT NULL REFERENCES items (id),
    type       TEXT    NOT NULL CHECK (type IN ('duplicate_of', 'fixes', 'advisory_for', 'fix_in_fork', 'backport_of', 'related')),
    to_item    INTEGER NOT NULL REFERENCES items (id),
    author     TEXT    NOT NULL CHECK (author IN ('human', 'claude', 'triage-bot')),
    created_at TEXT    NOT NULL,
    UNIQUE (from_item, type, to_item),
    CHECK (from_item != to_item)
) STRICT;
CREATE INDEX links_by_to ON links (to_item);

-- What an advisory has beyond its item row. Its state is the item's gh_state.
CREATE TABLE advisories (
    item_id      INTEGER PRIMARY KEY REFERENCES items (id),
    severity     TEXT NOT NULL DEFAULT '',
    cve_id       TEXT NOT NULL DEFAULT '',
    private_fork TEXT NOT NULL DEFAULT ''
) STRICT;

-- Full-text search over notes and item titles. External content, kept in step
-- by triggers.
CREATE VIRTUAL TABLE notes_fts USING fts5 (body, content = 'notes', content_rowid = 'id', tokenize = 'porter unicode61');
CREATE TRIGGER notes_fts_insert AFTER INSERT ON notes BEGIN
    INSERT INTO notes_fts (rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER notes_fts_delete AFTER DELETE ON notes BEGIN
    INSERT INTO notes_fts (notes_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER notes_fts_update AFTER UPDATE OF body ON notes BEGIN
    INSERT INTO notes_fts (notes_fts, rowid, body) VALUES ('delete', old.id, old.body);
    INSERT INTO notes_fts (rowid, body) VALUES (new.id, new.body);
END;

CREATE VIRTUAL TABLE items_fts USING fts5 (title, content = 'items', content_rowid = 'id', tokenize = 'porter unicode61');
CREATE TRIGGER items_fts_insert AFTER INSERT ON items BEGIN
    INSERT INTO items_fts (rowid, title) VALUES (new.id, new.title);
END;
CREATE TRIGGER items_fts_delete AFTER DELETE ON items BEGIN
    INSERT INTO items_fts (items_fts, rowid, title) VALUES ('delete', old.id, old.title);
END;
CREATE TRIGGER items_fts_update AFTER UPDATE OF title ON items BEGIN
    INSERT INTO items_fts (items_fts, rowid, title) VALUES ('delete', old.id, old.title);
    INSERT INTO items_fts (rowid, title) VALUES (new.id, new.title);
END;
