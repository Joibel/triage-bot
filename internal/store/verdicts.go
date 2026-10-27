package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Joibel/triage-bot/internal/triage"
)

var (
	// ErrNotTriaged is returned when a decision is recorded on an item with no
	// verdict in force.
	ErrNotTriaged = errors.New("there is no recommendation to act on")
	// ErrNoteRequired is returned for a rejection without a reason.
	ErrNoteRequired = errors.New("a rejection needs a note explaining why, so the next agent can do better")
)

// RecordVerdict stores a freshly parsed completion template as the item's
// current verdict, pending a decision, and marks the item triaged.
func (tx *Tx) RecordVerdict(ctx context.Context, itemID int64, beadID string, r *triage.Result, at time.Time) error {
	if _, err := tx.InsertVerdict(ctx, itemID, &Verdict{BeadID: beadID, TriagedAt: &at, Result: *r}); err != nil {
		return err
	}
	_, err := tx.tx.ExecContext(ctx,
		"UPDATE items SET state = 'triaged', last_error = '', carry_note = '' WHERE id = ?", itemID)
	if err != nil {
		return fmt.Errorf("failed to mark item %d triaged: %w", itemID, err)
	}
	return nil
}

// InsertVerdict adds a verdict to an item's history exactly as given, without
// touching the item. The latest verdict is the current one while the item is
// triaged.
func (tx *Tx) InsertVerdict(ctx context.Context, itemID int64, v *Verdict) (int64, error) {
	labels, err := encodeList(v.Result.SuggestedLabels)
	if err != nil {
		return 0, err
	}
	evidence, err := encodeList(v.Result.Evidence)
	if err != nil {
		return 0, err
	}
	res, err := tx.tx.ExecContext(ctx, `INSERT INTO verdicts
		(item_id, bead_id, triaged_at, recommendation, reason, confidence, reasoning,
		 suggested_comment, suggested_labels, evidence, duplicate_of, fixed_in)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		itemID, v.BeadID, nullTime(v.TriagedAt), string(v.Result.Recommendation), string(v.Result.Reason),
		v.Result.Confidence, v.Result.Reasoning, v.Result.SuggestedComment, labels, evidence,
		v.Result.DuplicateOf, v.Result.FixedIn)
	if err != nil {
		return 0, fmt.Errorf("failed to record a verdict on item %d: %w", itemID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to read the new verdict's id: %w", err)
	}
	v.ID = id
	return id, nil
}

// ReplaceResult corrects a verdict in place, keeping its bead, its time and
// the decisions recorded on it. This is reparse: it corrects what the agent
// said, not what the maintainer decided about it.
func (tx *Tx) ReplaceResult(ctx context.Context, verdictID int64, r *triage.Result) error {
	labels, err := encodeList(r.SuggestedLabels)
	if err != nil {
		return err
	}
	evidence, err := encodeList(r.Evidence)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, `UPDATE verdicts SET recommendation = ?, reason = ?, confidence = ?,
		reasoning = ?, suggested_comment = ?, suggested_labels = ?, evidence = ?, duplicate_of = ?, fixed_in = ?
		WHERE id = ?`,
		string(r.Recommendation), string(r.Reason), r.Confidence, r.Reasoning, r.SuggestedComment,
		labels, evidence, r.DuplicateOf, r.FixedIn, verdictID)
	if err != nil {
		return fmt.Errorf("failed to replace verdict %d: %w", verdictID, err)
	}
	return nil
}

// InsertDisposition records a decision on a verdict exactly as given.
func (tx *Tx) InsertDisposition(ctx context.Context, verdictID int64, d Disposition) error {
	_, err := tx.tx.ExecContext(ctx,
		"INSERT INTO dispositions (verdict_id, state, note, at, author) VALUES (?, ?, ?, ?, ?)",
		verdictID, string(d.State), d.Note, nullTime(d.At), string(d.Author))
	if err != nil {
		return fmt.Errorf("failed to record a decision on verdict %d: %w", verdictID, err)
	}
	return nil
}

// Dispose records the maintainer's decision on an item's current verdict.
//
// Rejecting is the only decision that changes the item's triage state: it
// sends the item round again, with the note in front of the next agent so it
// does not repeat the rejected conclusion. A rejection without a note is
// refused, since nobody could learn from it.
func (tx *Tx) Dispose(ctx context.Context, it *Item, d Disposition) error {
	if it.State == "" {
		return fmt.Errorf("it is not in triage - %w", ErrNotTriaged)
	}
	if it.State != Triaged || it.Verdict == nil {
		return fmt.Errorf("it is %s, not triaged - %w", it.State, ErrNotTriaged)
	}
	if d.State == Rejected && d.Note == "" {
		return ErrNoteRequired
	}
	if err := tx.InsertDisposition(ctx, it.Verdict.ID, d); err != nil {
		return err
	}
	if d.State == Rejected {
		return tx.Requeue(ctx, it.ID, d.Note)
	}
	return nil
}

// VerdictHistory returns every verdict recorded on an item, oldest first, each
// with its latest disposition.
func (tx *Tx) VerdictHistory(ctx context.Context, itemID int64) ([]Verdict, error) {
	return queryAll(ctx, tx, "verdict history", func(row scanner) (Verdict, error) {
		var vr verdictRow
		if err := row.Scan(vr.dest()...); err != nil {
			return Verdict{}, fmt.Errorf("failed to read verdict history: %w", err)
		}
		v, err := vr.verdict()
		if err != nil {
			return Verdict{}, err
		}
		return *v, nil
	}, `SELECT `+verdictColumns+`
		FROM verdicts v
		LEFT JOIN dispositions d ON d.id = (SELECT max(id) FROM dispositions WHERE verdict_id = v.id)
		WHERE v.item_id = ? ORDER BY v.id`, itemID)
}
