package engine

import (
	"context"
	"fmt"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/state"
)

// scanCeiling bounds how many rows one Discover call will walk before giving
// up on finding anything new. Items the cursor has already passed reappear
// ahead of it whenever something bumps them, and after a long gap there can be
// a great many; without a ceiling one tick could re-page most of the backlog.
const scanCeiling = 500

// Discover pulls more of the backlog into the status file, least recently
// updated first, but only when the untriaged buffer is running low.
//
// Discovery is incremental: the cursor records the last-activity time we have
// ingested up to, and each call asks GitHub only for items updated at or after
// that point. The whole open backlog is never fetched in one go.
func (e *Engine) Discover(ctx context.Context) error {
	cfg, err := e.load()
	if err != nil {
		return err
	}

	want := cfg.Settings.MaxOpenBeads * candidateBuffer
	have := len(cfg.NextUntriaged())
	if have >= want {
		return nil
	}

	items, err := e.scanBacklog(ctx, cfg, want-have)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}

	return e.update(func(c *state.Config) error {
		added := 0
		for _, in := range items {
			if c.Item(in.Number) == nil {
				added++
			}
			c.Upsert(&state.Item{
				Number:    in.Number,
				Kind:      in.Kind,
				Title:     in.Title,
				CreatedAt: in.CreatedAt,
				UpdatedAt: in.UpdatedAt,
				State:     state.Untriaged,
			})
			if c.Cursor.UpdatedThrough == nil || in.UpdatedAt.After(*c.Cursor.UpdatedThrough) {
				at := in.UpdatedAt
				c.Cursor.UpdatedThrough = &at
			}
		}
		if added > 0 {
			e.Log.Info("discovered backlog items", "added", added, "total", len(c.Items))
		}
		return nil
	})
}

// scanBacklog walks the backlog from the cursor and returns the rows it saw,
// stopping once it has seen need items the status file does not already hold.
//
// The bound is on new items, not on rows fetched. The cursor bound is
// inclusive and the cursor only advances past a strictly later timestamp, so
// every item sharing the cursor's second comes back on every call. Bounding
// the fetch instead wedges discovery permanently as soon as such a block grows
// as large as the window: each tick refetches the same known rows, adds
// nothing, and leaves the cursor exactly where it was. argo-workflows hit this
// when a bulk mergeability recompute touched 17 open PRs in one second against
// a 16-row window, stopping discovery ~250 items short of the backlog.
//
// Rows we already hold are returned too rather than dropped: they carry the
// cursor forward and refresh their titles.
func (e *Engine) scanBacklog(ctx context.Context, cfg *state.Config, need int) ([]github.Item, error) {
	var (
		out   []github.Item
		fresh int
		moved bool
	)
	err := e.GitHub.ListUpdatedSince(ctx, cfg.Cursor.UpdatedThrough, func(in github.Item) bool {
		out = append(out, in)
		if cfg.Item(in.Number) == nil {
			fresh++
		}
		if cursor := cfg.Cursor.UpdatedThrough; cursor == nil || in.UpdatedAt.After(*cursor) {
			moved = true
		}

		if fresh >= need {
			return false
		}
		// Past the ceiling, give up — but only from a position the next tick
		// can improve on. Stopping having neither found an item nor moved the
		// cursor would leave the next tick to repeat this one exactly, which
		// is the very livelock the new-item bound exists to prevent. A tie
		// block longer than the ceiling must still be walked to its end.
		progressed := fresh > 0 || moved
		return len(out) < scanCeiling || !progressed
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list backlog: %w", err)
	}
	return out, nil
}

// Expire returns verdicts older than the configured window to the queue, so
// advice about a long-lived backlog does not go stale unnoticed.
func (e *Engine) Expire(ctx context.Context) error {
	_ = ctx
	now := e.now()
	return e.update(func(c *state.Config) error {
		for _, item := range c.ExpiredTriage(now) {
			e.Log.Info("triage expired, requeueing", "number", item.Number, "triaged_at", item.TriagedAt)
			item.Requeue("")
		}
		return nil
	})
}
