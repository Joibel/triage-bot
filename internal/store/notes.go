package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Joibel/triage-bot/internal/triage"
)

// AddNote records a note on an item. Tags are lower-cased, de-duplicated and
// sorted, and any space inside one becomes a hyphen.
func (tx *Tx) AddNote(ctx context.Context, n *Note) error {
	body := strings.TrimSpace(n.Body)
	if body == "" {
		return errors.New("a note needs some text")
	}
	if n.Author != AuthorHuman && n.Author != AuthorClaude {
		return fmt.Errorf("a note's author is human or claude, not %q", n.Author)
	}
	n.Body, n.Tags, n.CreatedAt = body, normaliseTags(n.Tags), now(n.CreatedAt)
	res, err := tx.tx.ExecContext(ctx, "INSERT INTO notes (item_id, body, author, created_at) VALUES (?, ?, ?, ?)",
		n.ItemID, n.Body, string(n.Author), formatTime(n.CreatedAt))
	if err != nil {
		return fmt.Errorf("failed to add a note: %w", err)
	}
	if n.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("failed to read the new note's id: %w", err)
	}
	for _, tag := range n.Tags {
		if _, err := tx.tx.ExecContext(ctx, "INSERT INTO note_tags (note_id, tag) VALUES (?, ?)", n.ID, tag); err != nil {
			return fmt.Errorf("failed to tag a note: %w", err)
		}
	}
	return nil
}

func normaliseTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		t = strings.ToLower(strings.Join(strings.Fields(strings.TrimPrefix(strings.TrimSpace(t), "#")), "-"))
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// noteColumns are a note, aliased n, with its tags folded into one column.
const noteColumns = `n.id, n.item_id, n.body, n.author, n.created_at,
	coalesce((SELECT group_concat(tag, ' ') FROM (SELECT tag FROM note_tags WHERE note_id = n.id ORDER BY tag)), '')`

func scanNote(row scanner, extra ...any) (Note, error) {
	var (
		n       Note
		created string
		tags    string
	)
	if err := row.Scan(append([]any{&n.ID, &n.ItemID, &n.Body, &n.Author, &created, &tags}, extra...)...); err != nil {
		return Note{}, fmt.Errorf("failed to read note: %w", err)
	}
	t, err := parseNullTime(sql.NullString{String: created, Valid: true})
	if err != nil {
		return Note{}, err
	}
	n.CreatedAt = *t
	n.Tags = strings.Fields(tags)
	return n, nil
}

// Notes returns an item's notes, oldest first.
func (tx *Tx) Notes(ctx context.Context, itemID int64) ([]Note, error) {
	return queryAll(ctx, tx, "notes", func(row scanner) (Note, error) { return scanNote(row) },
		"SELECT "+noteColumns+" FROM notes n WHERE n.item_id = ? ORDER BY n.id", itemID)
}

// SearchQuery narrows a search. Every set field must match.
type SearchQuery struct {
	// Text is matched, word by word, against note bodies and item titles. A
	// trailing * makes a word a prefix. Empty lists notes by the filters
	// alone, newest first.
	Text string
	Kind triage.Kind
	// Tag keeps only notes carrying it, so excludes title matches.
	Tag string
	// State matches the triage pipeline state or GitHub's.
	State string
	// Sensitive, when set, keeps only sensitive (true) or only public (false)
	// items and their notes.
	Sensitive *bool
	Limit     int
}

// Hit is one search result: a note, or an item whose title matched.
type Hit struct {
	Item Item
	// Note is nil for a title match.
	Note *Note
	// Snippet shows the match in context, the matched words in [brackets].
	Snippet string
	rank    float64
}

// defaultSearchLimit bounds a search when the caller does not.
const defaultSearchLimit = 50

// Search finds notes and item titles. Results are best match first.
func (tx *Tx) Search(ctx context.Context, q SearchQuery) ([]Hit, error) {
	if q.Limit <= 0 {
		q.Limit = defaultSearchLimit
	}
	match := ftsQuery(q.Text)
	if match == "" && q.Tag == "" && q.Kind == "" && q.State == "" && q.Sensitive == nil {
		return nil, errors.New("give search some words or a filter")
	}

	hits, err := tx.searchNotes(ctx, q, match)
	if err != nil {
		return nil, err
	}
	if match != "" && q.Tag == "" {
		titles, err := tx.searchTitles(ctx, q, match)
		if err != nil {
			return nil, err
		}
		hits = append(hits, titles...)
		slices.SortStableFunc(hits, func(a, b Hit) int {
			switch {
			case a.rank < b.rank:
				return -1
			case a.rank > b.rank:
				return 1
			default:
				return 0
			}
		})
	}
	if len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	return hits, nil
}

// itemFilter renders the item-level filters as SQL over items aliased i.
func itemFilter(q SearchQuery) (string, []any) {
	var (
		where []string
		args  []any
	)
	if q.Kind != "" {
		where = append(where, "i.kind = ?")
		args = append(args, string(q.Kind))
	}
	if q.State != "" {
		where = append(where, "(i.state = ? OR i.gh_state = ?)")
		args = append(args, q.State, q.State)
	}
	if q.Sensitive != nil {
		where = append(where, "i.sensitive = ?")
		args = append(args, *q.Sensitive)
	}
	if len(where) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(where, " AND "), args
}

func (tx *Tx) searchNotes(ctx context.Context, q SearchQuery, match string) ([]Hit, error) {
	filter, args := itemFilter(q)
	if tags := normaliseTags([]string{q.Tag}); len(tags) == 1 {
		filter += " AND EXISTS (SELECT 1 FROM note_tags WHERE note_id = n.id AND tag = ?)"
		args = append(args, tags[0])
	}

	var query string
	if match != "" {
		query = "SELECT " + noteColumns + `, snippet(notes_fts, 0, '[', ']', '…', 16), bm25(notes_fts)
			FROM notes n
			JOIN notes_fts ON notes_fts.rowid = n.id
			JOIN items i ON i.id = n.item_id
			WHERE notes_fts MATCH ?` + filter + " ORDER BY bm25(notes_fts) LIMIT ?"
		args = append([]any{match}, args...)
	} else {
		query = "SELECT " + noteColumns + `, n.body, 0.0
			FROM notes n
			JOIN items i ON i.id = n.item_id
			WHERE 1` + filter + " ORDER BY n.id DESC LIMIT ?"
	}
	args = append(args, q.Limit)

	hits, err := queryAll(ctx, tx, "matching notes", func(row scanner) (Hit, error) {
		var h Hit
		n, err := scanNote(row, &h.Snippet, &h.rank)
		h.Note = &n
		return h, err
	}, query, args...)
	if err != nil {
		return nil, err
	}
	return hits, tx.fillItems(ctx, hits)
}

func (tx *Tx) searchTitles(ctx context.Context, q SearchQuery, match string) ([]Hit, error) {
	filter, args := itemFilter(q)
	query := `SELECT i.id, snippet(items_fts, 0, '[', ']', '…', 16), bm25(items_fts)
		FROM items_fts JOIN items i ON i.id = items_fts.rowid
		WHERE items_fts MATCH ?` + filter + " ORDER BY bm25(items_fts) LIMIT ?"
	hits, err := queryAll(ctx, tx, "matching titles", func(row scanner) (Hit, error) {
		var h Hit
		if err := row.Scan(&h.Item.ID, &h.Snippet, &h.rank); err != nil {
			return h, fmt.Errorf("failed to search titles: %w", err)
		}
		return h, nil
	}, query, append(append([]any{match}, args...), q.Limit)...)
	if err != nil {
		return nil, err
	}
	return hits, tx.fillItems(ctx, hits)
}

// fillItems loads each hit's item. A search returns at most a page of hits,
// so one indexed lookup each is cheap.
func (tx *Tx) fillItems(ctx context.Context, hits []Hit) error {
	for i := range hits {
		id := hits[i].Item.ID
		if hits[i].Note != nil {
			id = hits[i].Note.ItemID
		}
		it, err := tx.ItemByID(ctx, id)
		if err != nil {
			return err
		}
		hits[i].Item = *it
	}
	return nil
}

// ftsQuery turns what a user typed into an FTS5 query that matches every
// word. Each word is quoted, so punctuation a user types ("#123", "don't",
// "foo-bar") is searched for rather than read as query syntax.
func ftsQuery(text string) string {
	var terms []string
	for f := range strings.FieldsSeq(text) {
		prefix := strings.HasSuffix(f, "*")
		f = strings.TrimRight(f, "*")
		if f == "" {
			continue
		}
		term := `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
		if prefix {
			term += "*"
		}
		terms = append(terms, term)
	}
	return strings.Join(terms, " ")
}

// NoteCount returns how many notes the store holds.
func (tx *Tx) NoteCount(ctx context.Context) (int, error) {
	var n int
	if err := tx.tx.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count notes: %w", err)
	}
	return n, nil
}

// now is the time notes and links record when the caller gives none.
func now(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}
