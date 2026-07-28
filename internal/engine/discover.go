package engine

import (
	"context"
	"fmt"

	"github.com/Joibel/triage-bot/internal/state"
)

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

	// The cursor bound is inclusive, so the newest item we already hold comes
	// back each time. Ask for one extra to compensate; Upsert makes the repeat
	// harmless.
	items, err := e.GitHub.ListUpdatedSince(ctx, cfg.Cursor.UpdatedThrough, want-have+1)
	if err != nil {
		return fmt.Errorf("failed to list backlog: %w", err)
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
