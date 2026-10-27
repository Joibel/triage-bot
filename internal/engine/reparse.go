package engine

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// Change is one verdict that reparsing would alter.
type Change struct {
	Number int
	// Err is set when the bead's template still does not parse, in which case
	// the recorded verdict is left alone.
	Err    error
	Before *triage.Result
	After  *triage.Result

	verdictID int64
}

// Reparse re-reads the completion templates of already-triaged items from their
// beads and replaces the recorded verdicts.
//
// It exists because a parser bug can record a verdict that is valid but
// incomplete - nested code fences once truncated comments silently - and the
// bead still holds the agent's original text. That makes the damage recoverable
// without re-running the assessment, which `retriage` would force by clearing
// the bead and starting again.
//
// The human's disposition is deliberately preserved. Reparsing corrects what
// the agent said, not what the maintainer decided about it.
func (e *Engine) Reparse(ctx context.Context, only []int, dryRun bool) ([]Change, error) {
	set, err := e.settings(ctx)
	if err != nil {
		return nil, err
	}

	closed, err := e.Beads.Query(ctx, fmt.Sprintf("label=%s AND status=closed", set.BeadLabel))
	if err != nil {
		return nil, fmt.Errorf("failed to list closed beads: %w", err)
	}
	byID := make(map[string]beads.Bead, len(closed))
	for _, b := range closed {
		byID[b.ID] = b
	}

	var triaged []*store.Item
	if verr := e.Store.View(ctx, func(tx *store.Tx) error {
		triaged, err = tx.Triaged(ctx, set.MainRepo(), true)
		return err
	}); verr != nil {
		return nil, fmt.Errorf("failed to read triaged items: %w", verr)
	}

	var changes []Change
	for _, item := range triaged {
		if !reparseable(item, only) {
			continue
		}
		bead, ok := byID[item.Verdict.BeadID]
		if !ok {
			continue // bead gone or reopened; nothing to reread
		}
		if c, differs := compare(item, bead); differs {
			changes = append(changes, c)
		}
	}

	if dryRun || len(changes) == 0 {
		return changes, nil
	}
	return changes, e.applyReparse(ctx, set, changes)
}

// reparseable reports whether an item is a candidate: triaged, its verdict
// linked to a bead, and within the requested set.
func reparseable(item *store.Item, only []int) bool {
	if item.State != store.Triaged || item.Verdict == nil || item.Verdict.BeadID == "" {
		return false
	}
	return len(only) == 0 || slices.Contains(only, item.Number)
}

// compare reparses one bead and reports whether the verdict would change.
func compare(item *store.Item, bead beads.Bead) (Change, bool) {
	before := &item.Verdict.Result
	parsed, err := triage.Parse(bead.CloseReason, item.Kind)
	if err != nil {
		return Change{Number: item.Number, Err: err, Before: before}, true
	}
	if sameVerdict(parsed, before) {
		return Change{}, false
	}
	return Change{Number: item.Number, Before: before, After: parsed, verdictID: item.Verdict.ID}, true
}

// applyReparse writes the corrected verdicts, leaving the human's decision and
// the original triage timestamp intact.
func (e *Engine) applyReparse(ctx context.Context, set store.Settings, changes []Change) error {
	return e.update(ctx, func(tx *store.Tx) error {
		for _, ch := range changes {
			if ch.After == nil {
				continue // failed to parse; leave what we have
			}
			item, err := tx.Item(ctx, store.Ref{Repo: set.MainRepo(), Number: ch.Number})
			if err != nil {
				return err
			}
			if item.Verdict == nil || item.Verdict.ID != ch.verdictID {
				continue // changed under us since we read
			}
			if err := tx.ReplaceResult(ctx, ch.verdictID, ch.After); err != nil {
				return err
			}
			e.Log.Info("reparsed verdict", "number", ch.Number)
		}
		return nil
	})
}

// sameVerdict compares two verdicts, treating an absent list and an empty one
// as the same thing.
//
// The store keeps an empty list as no list, as the YAML status file did before
// it, so a template written with `suggested_labels: []` reads back as nil.
// Comparing those literally would report a change on every run and rewrite
// verdicts the maintainer has already acted on, for no gain.
func sameVerdict(a, b *triage.Result) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, y := *a, *b
	normaliseLists(&x)
	normaliseLists(&y)
	return reflect.DeepEqual(x, y)
}

func normaliseLists(r *triage.Result) {
	if len(r.SuggestedLabels) == 0 {
		r.SuggestedLabels = nil
	}
	if len(r.Evidence) == 0 {
		r.Evidence = nil
	}
}
