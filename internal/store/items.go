package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Joibel/triage-bot/internal/triage"
)

// itemSelect reads items with their current verdict and its latest
// disposition. A verdict is current only while the item is triaged: a
// requeued item keeps its old verdicts as history, but has none in force.
const itemSelect = `SELECT
	i.id, i.repo, i.kind, i.number, i.ghsa_id, i.title, i.gh_state, i.created_at, i.updated_at,
	i.url, i.sensitive, i.state, i.bead_id, i.attempts, i.last_error, i.queued_at, i.carry_note,
	` + verdictColumns + `
FROM items i
LEFT JOIN verdicts v ON i.state = 'triaged'
	AND v.id = (SELECT max(id) FROM verdicts WHERE item_id = i.id)
LEFT JOIN dispositions d ON d.id = (SELECT max(id) FROM dispositions WHERE verdict_id = v.id)
`

// verdictColumns are a verdict and its disposition, aliased v and d.
const verdictColumns = `v.id, v.bead_id, v.triaged_at, v.recommendation, v.reason, v.confidence,
	v.reasoning, v.suggested_comment, v.suggested_labels, v.evidence, v.duplicate_of, v.fixed_in,
	d.state, d.note, d.at, d.author`

// scanner is a *sql.Row or *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// verdictRow holds a verdict's nullable columns as scanned, since a LEFT JOIN
// can leave every one of them NULL.
type verdictRow struct {
	id, confidence, duplicateOf                          sql.NullInt64
	beadID, triagedAt, rec, reason, reasoning, comment   sql.NullString
	labels, evidence, fixedIn, dState, dNote, dAt, dAuth sql.NullString
}

func (r *verdictRow) dest() []any {
	return []any{&r.id, &r.beadID, &r.triagedAt, &r.rec, &r.reason, &r.confidence,
		&r.reasoning, &r.comment, &r.labels, &r.evidence, &r.duplicateOf, &r.fixedIn,
		&r.dState, &r.dNote, &r.dAt, &r.dAuth}
}

// verdict converts the row, returning nil when the join found no verdict.
func (r *verdictRow) verdict() (*Verdict, error) {
	if !r.id.Valid {
		return nil, nil
	}
	triagedAt, err := parseNullTime(r.triagedAt)
	if err != nil {
		return nil, err
	}
	v := &Verdict{
		ID:        r.id.Int64,
		BeadID:    r.beadID.String,
		TriagedAt: triagedAt,
		Result: triage.Result{
			Recommendation:   triage.Recommendation(r.rec.String),
			Reason:           triage.Reason(r.reason.String),
			Reasoning:        r.reasoning.String,
			SuggestedComment: r.comment.String,
			FixedIn:          r.fixedIn.String,
		},
		Disposition: Disposition{State: Pending},
	}
	if r.confidence.Valid {
		v.Result.Confidence = new(int(r.confidence.Int64))
	}
	if r.duplicateOf.Valid {
		v.Result.DuplicateOf = new(int(r.duplicateOf.Int64))
	}
	if v.Result.SuggestedLabels, err = decodeList(r.labels); err != nil {
		return nil, err
	}
	if v.Result.Evidence, err = decodeList(r.evidence); err != nil {
		return nil, err
	}
	if r.dState.Valid {
		at, err := parseNullTime(r.dAt)
		if err != nil {
			return nil, err
		}
		v.Disposition = Disposition{
			State:  DispositionState(r.dState.String),
			Note:   r.dNote.String,
			At:     at,
			Author: Author(r.dAuth.String),
		}
	}
	return v, nil
}

func scanItem(row scanner) (*Item, error) {
	var (
		it                                   Item
		number                               sql.NullInt64
		ghsa, created, updated, state, queue sql.NullString
		vr                                   verdictRow
	)
	dest := append([]any{
		&it.ID, &it.Repo, &it.Kind, &number, &ghsa, &it.Title, &it.GHState, &created, &updated,
		&it.URL, &it.Sensitive, &state, &it.BeadID, &it.Attempts, &it.LastError, &queue, &it.CarryNote,
	}, vr.dest()...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to read item: %w", err)
	}

	it.Number = int(number.Int64)
	it.GHSA = ghsa.String
	it.State = ItemState(state.String)
	var err error
	if it.CreatedAt, err = parseZeroTime(created); err != nil {
		return nil, err
	}
	if it.UpdatedAt, err = parseZeroTime(updated); err != nil {
		return nil, err
	}
	if it.QueuedAt, err = parseNullTime(queue); err != nil {
		return nil, err
	}
	if it.Verdict, err = vr.verdict(); err != nil {
		return nil, err
	}
	return &it, nil
}

// queryItems runs itemSelect with a WHERE/ORDER tail.
func (tx *Tx) queryItems(ctx context.Context, tail string, args ...any) ([]*Item, error) {
	return queryAll(ctx, tx, "items", scanItem, itemSelect+tail, args...)
}

func (tx *Tx) queryItem(ctx context.Context, tail string, args ...any) (*Item, error) {
	return scanItem(tx.tx.QueryRowContext(ctx, itemSelect+tail, args...))
}

// Item finds an item by reference, or returns ErrNotFound.
func (tx *Tx) Item(ctx context.Context, ref Ref) (*Item, error) {
	if ref.IsAdvisory() {
		return tx.queryItem(ctx, "WHERE i.ghsa_id = ?", ref.GHSA)
	}
	return tx.queryItem(ctx, "WHERE i.repo = ? AND i.number = ?", ref.Repo, ref.Number)
}

// ItemByID finds an item by its row id.
func (tx *Tx) ItemByID(ctx context.Context, id int64) (*Item, error) {
	return tx.queryItem(ctx, "WHERE i.id = ?", id)
}

// ItemByBead finds the item currently attached to a bead, or returns
// ErrNotFound.
func (tx *Tx) ItemByBead(ctx context.Context, beadID string) (*Item, error) {
	if beadID == "" {
		return nil, ErrNotFound
	}
	return tx.queryItem(ctx, "WHERE i.bead_id = ? ORDER BY i.id LIMIT 1", beadID)
}

// InsertItem adds an item exactly as given, returning its id. The verdict, if
// any, is not written: see InsertVerdict.
//
// An advisory, and anything outside the main repository (which can only be a
// private fork), is marked sensitive whatever the caller said.
func (tx *Tx) InsertItem(ctx context.Context, it *Item) (int64, error) {
	s, err := tx.Settings(ctx)
	if err != nil {
		return 0, err
	}
	sensitive := it.Sensitive || it.Kind == KindAdvisory || it.Repo != s.MainRepo()

	var number, ghsa, state any
	if it.Kind == KindAdvisory {
		ghsa = it.GHSA
	} else {
		number = it.Number
	}
	if it.State != "" {
		state = string(it.State)
	}

	res, err := tx.tx.ExecContext(ctx, `INSERT INTO items
		(repo, kind, number, ghsa_id, title, gh_state, created_at, updated_at, url, sensitive,
		 state, bead_id, attempts, last_error, queued_at, carry_note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.Repo, string(it.Kind), number, ghsa, it.Title, it.GHState, zeroNull(it.CreatedAt), zeroNull(it.UpdatedAt),
		it.URL, sensitive, state, it.BeadID, it.Attempts, it.LastError, nullTime(it.QueuedAt), it.CarryNote)
	if err != nil {
		return 0, fmt.Errorf("failed to insert %s: %w", it.Ref(s.MainRepo()), err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to read the new item's id: %w", err)
	}
	it.ID, it.Sensitive = id, sensitive
	return id, nil
}

// Refresh updates what GitHub owns about an item: title, state, URL and
// timestamps. Empty or zero values leave the stored ones alone, so a partial
// source cannot blank what a fuller one recorded. It never touches triage
// state: GitHub owns the metadata, we own the verdict.
func (tx *Tx) Refresh(ctx context.Context, id int64, from *Item) error {
	_, err := tx.tx.ExecContext(ctx, `UPDATE items SET
		title      = CASE WHEN ? != '' THEN ? ELSE title END,
		gh_state   = CASE WHEN ? != '' THEN ? ELSE gh_state END,
		url        = CASE WHEN ? != '' THEN ? ELSE url END,
		created_at = coalesce(?, created_at),
		updated_at = coalesce(?, updated_at)
		WHERE id = ?`,
		from.Title, from.Title, from.GHState, from.GHState, from.URL, from.URL,
		zeroNull(from.CreatedAt), zeroNull(from.UpdatedAt), id)
	if err != nil {
		return fmt.Errorf("failed to refresh item %d: %w", id, err)
	}
	return nil
}

// Upsert inserts an item we do not hold, or refreshes the GitHub metadata of
// one we do, and returns it. It never touches triage state, so an item that
// comes in this way stays outside the pipeline until discovery reaches it.
func (tx *Tx) Upsert(ctx context.Context, in *Item) (*Item, error) {
	ref := Ref{Repo: in.Repo, Number: in.Number, GHSA: in.GHSA}
	existing, err := tx.Item(ctx, ref)
	switch {
	case errors.Is(err, ErrNotFound):
		fresh := *in
		fresh.State, fresh.BeadID, fresh.Verdict = "", "", nil
		if _, ierr := tx.InsertItem(ctx, &fresh); ierr != nil {
			return nil, ierr
		}
		return tx.ItemByID(ctx, fresh.ID)
	case err != nil:
		return nil, err
	}
	if err := tx.Refresh(ctx, existing.ID, in); err != nil {
		return nil, err
	}
	return tx.ItemByID(ctx, existing.ID)
}

// Enroll brings a discovered main-repository item into the triage pipeline. A
// new item is inserted untriaged; one we already hold has its GitHub metadata
// refreshed, and if it came in through a note or a link and so is not yet in
// the pipeline, it joins it untriaged. It reports whether the item is new to
// the pipeline.
func (tx *Tx) Enroll(ctx context.Context, in *Item) (bool, error) {
	existing, err := tx.Item(ctx, Ref{Repo: in.Repo, Number: in.Number})
	if errors.Is(err, ErrNotFound) {
		enrolled := *in
		enrolled.State = Untriaged
		_, ierr := tx.InsertItem(ctx, &enrolled)
		return ierr == nil, ierr
	}
	if err != nil {
		return false, err
	}
	if err := tx.Refresh(ctx, existing.ID, in); err != nil {
		return false, err
	}
	if existing.State != "" {
		return false, nil
	}
	return true, tx.setState(ctx, existing.ID, Untriaged)
}

func (tx *Tx) setState(ctx context.Context, id int64, s ItemState) error {
	if _, err := tx.tx.ExecContext(ctx, "UPDATE items SET state = ? WHERE id = ?", string(s), id); err != nil {
		return fmt.Errorf("failed to set item %d %s: %w", id, s, err)
	}
	return nil
}

// PipelineNumbers returns the numbers of every main-repository item in the
// triage pipeline, for discovery to tell new rows from known ones.
func (tx *Tx) PipelineNumbers(ctx context.Context, mainRepo string) (map[int]bool, error) {
	numbers, err := queryAll(ctx, tx, "tracked items", scanInt,
		"SELECT number FROM items WHERE repo = ? AND number IS NOT NULL AND state IS NOT NULL", mainRepo)
	if err != nil {
		return nil, err
	}
	out := make(map[int]bool, len(numbers))
	for _, n := range numbers {
		out[n] = true
	}
	return out, nil
}

// Counts returns how many items are in each pipeline state.
func (tx *Tx) Counts(ctx context.Context) (map[ItemState]int, error) {
	type count struct {
		state ItemState
		n     int
	}
	counts, err := queryAll(ctx, tx, "item counts", func(row scanner) (count, error) {
		var c count
		if err := row.Scan(&c.state, &c.n); err != nil {
			return c, fmt.Errorf("failed to count items: %w", err)
		}
		return c, nil
	}, "SELECT state, count(*) FROM items WHERE state IS NOT NULL GROUP BY state")
	if err != nil {
		return nil, err
	}
	out := make(map[ItemState]int, len(counts))
	for _, c := range counts {
		out[c.state] = c.n
	}
	return out, nil
}

// scanInt reads a row holding one integer.
func scanInt(row scanner) (int, error) {
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to read row: %w", err)
	}
	return n, nil
}

// Pipeline returns a repository's items in one pipeline state, in number
// order.
func (tx *Tx) Pipeline(ctx context.Context, repo string, state ItemState) ([]*Item, error) {
	return tx.queryItems(ctx, "WHERE i.repo = ? AND i.state = ? ORDER BY i.number", repo, string(state))
}

// NextUntriaged returns untriaged items, least recently updated first: the
// order the backlog is worked in. Dormancy, not age, is what makes an item
// worth triaging.
func (tx *Tx) NextUntriaged(ctx context.Context) ([]*Item, error) {
	return tx.queryItems(ctx, "WHERE i.state = 'untriaged' ORDER BY i.updated_at, i.number")
}

// ExpiredTriage returns triaged items whose verdict is older than after,
// measured from now.
func (tx *Tx) ExpiredTriage(ctx context.Context, now time.Time, after time.Duration) ([]*Item, error) {
	return tx.queryItems(ctx, "WHERE i.state = 'triaged' AND v.triaged_at <= ? ORDER BY i.number",
		formatTime(now.Add(-after)))
}

// Triaged returns the main repository's triaged items, in number order: only
// those still awaiting a decision, unless all is set.
func (tx *Tx) Triaged(ctx context.Context, mainRepo string, all bool) ([]*Item, error) {
	tail := "WHERE i.state = 'triaged' AND i.repo = ?"
	if !all {
		tail += " AND (d.state IS NULL OR d.state = 'pending')"
	}
	return tx.queryItems(ctx, tail+" ORDER BY i.number", mainRepo)
}

// AnyDisposition matches a verdict whatever the maintainer decided, in a
// VerdictQuery.
const AnyDisposition DispositionState = "any"

// verdictLimit is how many verdicts a VerdictQuery returns unless it says;
// maxVerdictLimit caps what it may ask for.
const (
	verdictLimit    = 20
	maxVerdictLimit = 200
)

// VerdictQuery selects triage verdicts. Empty fields match anything, except
// Disposition, which defaults to Pending: verdicts still awaiting a decision.
type VerdictQuery struct {
	Sensitive      *bool
	Recommendation triage.Recommendation
	Reason         triage.Reason
	Kind           triage.Kind
	// Disposition is the maintainer's latest decision on the verdict, or
	// AnyDisposition.
	Disposition DispositionState
	// Limit is the most verdicts returned: verdictLimit if zero, at most
	// maxVerdictLimit.
	Limit int
}

// Verdicts returns the main repository's triaged items whose current verdict
// matches q, most confident first.
func (tx *Tx) Verdicts(ctx context.Context, mainRepo string, q VerdictQuery) ([]*Item, error) {
	where := []string{"i.state = 'triaged'", "i.repo = ?"}
	args := []any{mainRepo}
	for _, f := range []struct {
		column, value string
	}{
		{"v.recommendation", string(q.Recommendation)},
		{"v.reason", string(q.Reason)},
		{"i.kind", string(q.Kind)},
	} {
		if f.value != "" {
			where = append(where, f.column+" = ?")
			args = append(args, f.value)
		}
	}
	switch q.Disposition {
	case "", Pending:
		where = append(where, "(d.state IS NULL OR d.state = 'pending')")
	case AnyDisposition:
	default:
		where = append(where, "d.state = ?")
		args = append(args, string(q.Disposition))
	}
	if q.Sensitive != nil {
		where = append(where, "i.sensitive = ?")
		args = append(args, *q.Sensitive)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = verdictLimit
	}
	args = append(args, min(limit, maxVerdictLimit))
	return tx.queryItems(ctx, "WHERE "+strings.Join(where, " AND ")+
		" ORDER BY v.confidence IS NULL, v.confidence DESC, i.number LIMIT ?", args...)
}

// Requeue returns an item to the untriaged pool, clearing the per-bead attempt
// state. Its verdicts stay as history. carryNote is embedded in the next bead
// so the agent does not repeat a rejected conclusion; it is empty for
// age-based expiry.
func (tx *Tx) Requeue(ctx context.Context, id int64, carryNote string) error {
	_, err := tx.tx.ExecContext(ctx, `UPDATE items SET state = 'untriaged', bead_id = '', attempts = 0,
		last_error = '', queued_at = NULL, carry_note = ? WHERE id = ?`, carryNote, id)
	if err != nil {
		return fmt.Errorf("failed to requeue item %d: %w", id, err)
	}
	return nil
}

// MarkQueued records the bead opened for an untriaged item.
func (tx *Tx) MarkQueued(ctx context.Context, id int64, beadID string, at time.Time) error {
	_, err := tx.tx.ExecContext(ctx, `UPDATE items SET state = 'queued', bead_id = ?, queued_at = ?,
		attempts = 0, last_error = '' WHERE id = ?`, beadID, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("failed to mark item %d queued: %w", id, err)
	}
	return nil
}

// MarkClosedUpstream records that GitHub says an untriaged item was closed.
func (tx *Tx) MarkClosedUpstream(ctx context.Context, id int64) error {
	_, err := tx.tx.ExecContext(ctx,
		"UPDATE items SET state = 'closed_upstream', gh_state = 'closed' WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("failed to mark item %d closed upstream: %w", id, err)
	}
	return nil
}

// RecordInvalid counts one invalid completion template against a queued item,
// handing it to a human when giveUp is set.
func (tx *Tx) RecordInvalid(ctx context.Context, id int64, lastError string, giveUp bool) error {
	state := Queued
	if giveUp {
		state = NeedsHuman
	}
	_, err := tx.tx.ExecContext(ctx,
		"UPDATE items SET attempts = attempts + 1, last_error = ?, state = ? WHERE id = ?",
		lastError, string(state), id)
	if err != nil {
		return fmt.Errorf("failed to record an invalid template on item %d: %w", id, err)
	}
	return nil
}

// encodeList stores a list as JSON, and an empty one as NULL: the status file
// never distinguished the two, and nor does sameVerdict.
func encodeList(l []string) (any, error) {
	if len(l) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("failed to encode list: %w", err)
	}
	return string(b), nil
}

func decodeList(s sql.NullString) ([]string, error) {
	if !s.Valid {
		return nil, nil
	}
	var l []string
	if err := json.NewDecoder(strings.NewReader(s.String)).Decode(&l); err != nil {
		return nil, fmt.Errorf("stored list %q is malformed: %w", s.String, err)
	}
	return l, nil
}
