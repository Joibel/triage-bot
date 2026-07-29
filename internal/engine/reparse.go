package engine

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/state"
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
	cfg, err := e.load()
	if err != nil {
		return nil, err
	}

	closed, err := e.Beads.Query(ctx, fmt.Sprintf("label=%s AND status=closed", cfg.Settings.BeadLabel))
	if err != nil {
		return nil, fmt.Errorf("failed to list closed beads: %w", err)
	}
	byID := make(map[string]beads.Bead, len(closed))
	for _, b := range closed {
		byID[b.ID] = b
	}

	var changes []Change
	for _, item := range cfg.Items {
		if !reparseable(item, only) {
			continue
		}
		bead, ok := byID[item.BeadID]
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
	return changes, e.applyReparse(changes)
}

// reparseable reports whether an item is a candidate: triaged, still linked to
// a bead, and within the requested set.
func reparseable(item *state.Item, only []int) bool {
	if item.State != state.Triaged || item.BeadID == "" {
		return false
	}
	return len(only) == 0 || slices.Contains(only, item.Number)
}

// compare reparses one bead and reports whether the verdict would change.
func compare(item *state.Item, bead beads.Bead) (Change, bool) {
	parsed, err := triage.Parse(bead.CloseReason, item.Kind)
	if err != nil {
		return Change{Number: item.Number, Err: err, Before: item.Result}, true
	}
	if sameVerdict(parsed, item.Result) {
		return Change{}, false
	}
	return Change{Number: item.Number, Before: item.Result, After: parsed}, true
}

// applyReparse writes the corrected verdicts, leaving the human's decision and
// the original triage timestamp intact.
func (e *Engine) applyReparse(changes []Change) error {
	return e.update(func(c *state.Config) error {
		for _, ch := range changes {
			if ch.After == nil {
				continue // failed to parse; leave what we have
			}
			item := c.Item(ch.Number)
			if item == nil || item.State != state.Triaged {
				continue // changed under us since we read
			}
			item.Result = ch.After
			e.Log.Info("reparsed verdict", "number", ch.Number)
		}
		return nil
	})
}

// sameVerdict compares two verdicts, treating an absent list and an empty one
// as the same thing.
//
// yaml omits empty slices when saving, so a template written with
// `suggested_labels: []` reloads as nil. Comparing those literally would report
// a change on every run and rewrite verdicts the maintainer has already acted
// on, for no gain.
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
