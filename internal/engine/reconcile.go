package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/state"
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
	cfg, err := e.load()
	if err != nil {
		return err
	}

	closed, err := e.Beads.Query(ctx, fmt.Sprintf("label=%s AND status=closed", cfg.Settings.BeadLabel))
	if err != nil {
		return fmt.Errorf("failed to list closed beads: %w", err)
	}

	var outcomes []outcome
	for _, bead := range closed {
		item := cfg.ItemByBead(bead.ID)
		if item == nil || item.State != state.Queued {
			continue // not ours, or already reconciled
		}
		if o, ok := e.assess(ctx, cfg, bead, item); ok {
			outcomes = append(outcomes, o)
		}
	}
	if len(outcomes) == 0 {
		return nil
	}

	now := e.now()
	return e.update(func(c *state.Config) error {
		for _, o := range outcomes {
			item := c.ItemByBead(o.beadID)
			if item == nil || item.State != state.Queued {
				continue // changed under us since we read
			}
			e.record(item, o, now)
		}
		return nil
	})
}

// assess parses one bead's completion template and performs the bead-side
// effects that follow from it.
//
// Those effects happen before the status file is written. If we crash in
// between, the bead is already reopened and its close reason cleared, so the
// next tick simply finds nothing to reconcile and waits - rather than re-reading
// the same bad template and counting the same failure twice.
func (e *Engine) assess(ctx context.Context, cfg *state.Config, bead beads.Bead, item *state.Item) (outcome, bool) {
	result, perr := triage.Parse(bead.CloseReason, item.Kind)
	if perr == nil {
		return outcome{beadID: bead.ID, result: result}, true
	}

	var verr *triage.ValidationError
	if !errors.As(perr, &verr) {
		e.Log.Error("unexpected parse failure", "bead", bead.ID, "error", perr)
		return outcome{}, false
	}

	if item.Attempts+1 >= cfg.Settings.MaxTemplateAttempts {
		// Leave the bead closed: further attempts will not help, and the item
		// needs a human. The note explains why nobody reopened it.
		e.note(ctx, bead.ID, giveUpNote(cfg.Settings.MaxTemplateAttempts, verr))
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
func (e *Engine) record(item *state.Item, o outcome, now time.Time) {
	switch {
	case o.result != nil:
		item.State = state.Triaged
		item.Result = o.result
		item.TriagedAt = &now
		item.LastError = ""
		item.Human = state.Human{State: state.Pending}
		e.Log.Info("triaged",
			"number", item.Number,
			"recommendation", o.result.Recommendation,
			"reason", o.result.Reason,
			"confidence", *o.result.Confidence)
	case o.giveUp:
		item.Attempts++
		item.LastError = o.verr.Markdown()
		item.State = state.NeedsHuman
		e.Log.Warn("giving up on item after repeated invalid templates",
			"number", item.Number, "attempts", item.Attempts)
	default:
		item.Attempts++
		item.LastError = o.verr.Markdown()
		e.Log.Info("reopened bead with invalid template",
			"number", item.Number, "attempts", item.Attempts)
	}
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
