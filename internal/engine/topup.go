package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// TopUp opens beads until the configured number are in flight.
//
// The cap is what stops triage work flooding whatever actions the beads. Items
// found closed on GitHub since we recorded them are marked and skipped without
// consuming a slot, since spending a triage bead on an already-closed item is
// wasted work.
func (e *Engine) TopUp(ctx context.Context) error {
	var (
		set       store.Settings
		untriaged []*store.Item
	)
	err := e.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		if set, err = tx.Settings(ctx); err != nil {
			return err
		}
		untriaged, err = tx.NextUntriaged(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to read the queue: %w", err)
	}

	open, err := e.Beads.Query(ctx, fmt.Sprintf("label=%s AND status!=closed", set.BeadLabel))
	if err != nil {
		return fmt.Errorf("failed to count beads in flight: %w", err)
	}

	slots := set.MaxOpenBeads - len(open)
	if slots <= 0 {
		return nil
	}

	// Beads whose item never got recorded - we crashed between creating the
	// bead and writing the store. Skip those items so the crash does not
	// produce a duplicate bead.
	orphaned := make(map[string]bool, len(open))
	for _, b := range open {
		orphaned[b.ExternalRef] = true
	}

	var opened []created
	var closedUpstream []int64

	for _, item := range untriaged {
		if slots == 0 {
			break
		}
		if orphaned[externalRef(item.Number)] {
			e.Log.Warn("bead already exists for item, adopting on next tick", "number", item.Number)
			continue
		}

		a := e.openBead(ctx, set, item)
		switch {
		case !a.stillOpen:
			closedUpstream = append(closedUpstream, item.ID)
		case a.created != nil:
			opened = append(opened, *a.created)
			slots--
		}
	}

	if len(opened) == 0 && len(closedUpstream) == 0 {
		return nil
	}

	return e.recordTopUp(ctx, opened, closedUpstream)
}

// recordTopUp writes the results of a top-up pass to the store.
func (e *Engine) recordTopUp(ctx context.Context, opened []created, closedUpstream []int64) error {
	now := e.now()
	return e.update(ctx, func(tx *store.Tx) error {
		for _, id := range closedUpstream {
			item, err := tx.ItemByID(ctx, id)
			if err != nil {
				return err
			}
			if item.State != store.Untriaged {
				continue
			}
			if err := tx.MarkClosedUpstream(ctx, id); err != nil {
				return err
			}
			e.Log.Info("item closed on GitHub before triage, skipping", "number", item.Number)
		}
		for _, o := range opened {
			if err := tx.MarkQueued(ctx, o.id, o.beadID, now); err != nil {
				return err
			}
			e.Log.Info("queued for triage", "number", o.number, "bead", o.beadID)
		}
		return nil
	})
}

// created records a bead that was successfully opened for an item.
type created struct {
	id     int64
	number int
	beadID string
}

// attempt is the result of trying to open a bead for one item.
type attempt struct {
	// created is set only when a bead was actually opened.
	created *created
	// stillOpen is false when GitHub says a human already closed the item.
	stillOpen bool
}

// openBead confirms an item is still open on GitHub and, if so, opens a bead
// for it.
//
// The liveness check is what stops a triage bead being spent on something a
// human already closed. A zero created with stillOpen true means the attempt
// failed and will be retried next tick.
func (e *Engine) openBead(ctx context.Context, set store.Settings, item *store.Item) attempt {
	gh, err := e.GitHub.Get(ctx, item.Number)
	if err != nil {
		e.Log.Error("failed to check item is still open", "number", item.Number, "error", err)
		return attempt{stillOpen: true} // unknown, so leave the item alone and retry
	}
	if !gh.Open {
		return attempt{}
	}

	beadID, err := e.Beads.Create(ctx, beads.CreateRequest{
		Title:       fmt.Sprintf("Triage %s#%d", set.MainRepo(), item.Number),
		Body:        BeadBody(set.Org, set.Repo, *gh, item.CarryNote),
		Label:       set.BeadLabel,
		ExternalRef: externalRef(item.Number),
	})
	if err != nil {
		e.Log.Error("failed to create bead", "number", item.Number, "error", err)
		return attempt{stillOpen: true}
	}
	return attempt{created: &created{id: item.ID, number: item.Number, beadID: beadID}, stillOpen: true}
}

// externalRef is the bd external reference for a GitHub item, which keeps the
// bead-to-item mapping recoverable without the store.
func externalRef(number int) string { return fmt.Sprintf("gh-%d", number) }

// BeadBody renders the instructions an agent works from: where the item is,
// the metadata we already hold, and the completion-template contract.
//
// The item body and comments are deliberately not included. The agent fetches
// those itself, which keeps the bot's GitHub reads to cheap metadata and the
// beads small.
func BeadBody(org, repo string, item github.Item, carryNote string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Assess %s/%s#%d and record a triage verdict.\n\n", org, repo, item.Number)

	b.WriteString("## Item\n\n")
	fmt.Fprintf(&b, "- URL: %s\n", item.URL)
	fmt.Fprintf(&b, "- Kind: %s\n", item.Kind)
	fmt.Fprintf(&b, "- Title: %s\n", item.Title)
	fmt.Fprintf(&b, "- Author: %s\n", item.Author)
	fmt.Fprintf(&b, "- Created: %s\n", item.CreatedAt.Format("2006-01-02"))
	fmt.Fprintf(&b, "- Last activity: %s\n", item.UpdatedAt.Format("2006-01-02"))
	fmt.Fprintf(&b, "- Comments: %d\n", item.Comments)
	if len(item.Labels) > 0 {
		labels := slices.Clone(item.Labels)
		fmt.Fprintf(&b, "- Labels: %s\n", strings.Join(labels, ", "))
	}
	b.WriteString("\nFetch the item and its comments yourself before deciding.\n\n")

	if strings.TrimSpace(carryNote) != "" {
		b.WriteString("## A previous verdict on this item was rejected\n\n")
		fmt.Fprintf(&b, "> %s\n\n", strings.ReplaceAll(strings.TrimSpace(carryNote), "\n", "\n> "))
		b.WriteString("Take that into account; do not simply repeat the rejected conclusion.\n\n")
	}

	b.WriteString(triage.Instructions(item.Kind))
	return b.String()
}
