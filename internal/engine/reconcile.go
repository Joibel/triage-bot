package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// outcome is what reconciling one closed bead concluded.
type outcome struct {
	beadID string
	result *triage.Result
	verr   *triage.ValidationError
	giveUp bool
}

// Reconcile turns closed beads into verdicts.
//
// A bead's only durable output is the completion template in its close reason.
// A valid one is recorded and the bead is done; an invalid one earns a note
// explaining exactly what was wrong and a reopen, until the attempt limit is
// reached and the item is handed to a human.
func (e *Engine) Reconcile(ctx context.Context) error {
	set, err := e.settings(ctx)
	if err != nil {
		return err
	}

	closed, err := e.Beads.Query(ctx, fmt.Sprintf("label=%s AND status=closed", set.BeadLabel))
	if err != nil {
		return fmt.Errorf("failed to list closed beads: %w", err)
	}

	queued, err := e.queuedByBead(ctx, set)
	if err != nil {
		return err
	}

	var outcomes []outcome
	for _, bead := range closed {
		item, ok := queued[bead.ID]
		if !ok {
			continue // not ours, or already reconciled
		}
		if o, ok := e.assess(ctx, set, bead, item); ok {
			outcomes = append(outcomes, o)
		}
	}
	if len(outcomes) == 0 {
		return nil
	}

	now := e.now()
	return e.update(ctx, func(tx *store.Tx) error {
		for _, o := range outcomes {
			if err := e.recordOutcome(ctx, tx, o, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// recordOutcome applies one outcome, if its item is still waiting on the bead.
func (e *Engine) recordOutcome(ctx context.Context, tx *store.Tx, o outcome, now time.Time) error {
	item, err := tx.ItemByBead(ctx, o.beadID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.State != store.Queued {
		return nil // changed under us since we read
	}
	return e.record(ctx, tx, item, o, now)
}

// queuedByBead maps each queued item's bead to the item.
func (e *Engine) queuedByBead(ctx context.Context, set store.Settings) (map[string]*store.Item, error) {
	out := map[string]*store.Item{}
	err := e.Store.View(ctx, func(tx *store.Tx) error {
		items, err := tx.Pipeline(ctx, set.MainRepo(), store.Queued)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.BeadID != "" {
				out[it.BeadID] = it
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read queued items: %w", err)
	}
	return out, nil
}

// assess parses one bead's completion template and performs the bead-side
// effects that follow from it.
//
// Those effects happen before the store is written. If we crash in between,
// the bead is already reopened and its close reason cleared, so the next tick
// simply finds nothing to reconcile and waits - rather than re-reading the same
// bad template and counting the same failure twice.
func (e *Engine) assess(ctx context.Context, set store.Settings, bead beads.Bead, item *store.Item) (outcome, bool) {
	result, perr := triage.Parse(bead.CloseReason, item.Kind)
	if perr == nil {
		return outcome{beadID: bead.ID, result: result}, true
	}

	var verr *triage.ValidationError
	if !errors.As(perr, &verr) {
		e.Log.Error("unexpected parse failure", "bead", bead.ID, "error", perr)
		return outcome{}, false
	}

	if item.Attempts+1 >= set.MaxTemplateAttempts {
		// Leave the bead closed: further attempts will not help, and the item
		// needs a human. The note explains why nobody reopened it.
		e.note(ctx, bead.ID, giveUpNote(set.MaxTemplateAttempts, verr))
		return outcome{beadID: bead.ID, verr: verr, giveUp: true}, true
	}

	e.note(ctx, bead.ID, retryNote(item.Attempts+1, verr))
	if err := e.Beads.Reopen(ctx, bead.ID, "completion template invalid; see notes"); err != nil {
		e.Log.Error("failed to reopen bead", "bead", bead.ID, "error", err)
	}
	return outcome{beadID: bead.ID, verr: verr}, true
}

// note appends to a bead, logging rather than failing: the note is feedback for
// the agent, and losing it must not stop the verdict being recorded.
func (e *Engine) note(ctx context.Context, beadID, text string) {
	if err := e.Beads.Note(ctx, beadID, text); err != nil {
		e.Log.Error("failed to note on bead", "bead", beadID, "error", err)
	}
}

// record applies one outcome to its item.
func (e *Engine) record(ctx context.Context, tx *store.Tx, item *store.Item, o outcome, now time.Time) error {
	switch {
	case o.result != nil:
		if err := tx.RecordVerdict(ctx, item.ID, o.beadID, o.result, now); err != nil {
			return err
		}
		e.Log.Info("triaged",
			"number", item.Number,
			"recommendation", o.result.Recommendation,
			"reason", o.result.Reason,
			"confidence", *o.result.Confidence)
	case o.giveUp:
		if err := tx.RecordInvalid(ctx, item.ID, o.verr.Markdown(), true); err != nil {
			return err
		}
		e.Log.Warn("giving up on item after repeated invalid templates",
			"number", item.Number, "attempts", item.Attempts+1)
	default:
		if err := tx.RecordInvalid(ctx, item.ID, o.verr.Markdown(), false); err != nil {
			return err
		}
		e.Log.Info("reopened bead with invalid template",
			"number", item.Number, "attempts", item.Attempts+1)
	}
	return nil
}

func retryNote(attempt int, verr *triage.ValidationError) string {
	return fmt.Sprintf(
		"## Completion template rejected (attempt %d)\n\n%s\nFix these and close the bead again with a corrected ```yaml block.\n",
		attempt, verr.Markdown())
}

func giveUpNote(limit int, verr *triage.ValidationError) string {
	return fmt.Sprintf(
		"## Completion template rejected (attempt %d of %d - giving up)\n\n%s\nThis item has been handed to a human; the bead is being left closed.\n",
		limit, limit, verr.Markdown())
}
