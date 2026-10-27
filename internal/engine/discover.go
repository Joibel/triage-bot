package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/ingest"
	"github.com/Joibel/triage-bot/internal/store"
)

// scanCeiling bounds how many rows one Discover call will walk before giving
// up on finding anything new. Items the cursor has already passed reappear
// ahead of it whenever something bumps them, and after a long gap there can be
// a great many; without a ceiling one tick could re-page most of the backlog.
const scanCeiling = 500

// backlog is what discovery needs to know before it walks GitHub.
type backlog struct {
	set    store.Settings
	cursor *time.Time
	known  map[int]bool
	have   int
}

// Discover pulls more of the backlog into the store, least recently updated
// first, but only when the untriaged buffer is running low.
//
// Discovery is incremental: the cursor records the last-activity time we have
// ingested up to, and each call asks GitHub only for items updated at or after
// that point. The whole open backlog is never fetched in one go.
func (e *Engine) Discover(ctx context.Context) error {
	b, err := e.readBacklog(ctx)
	if err != nil {
		return err
	}

	want := b.set.MaxOpenBeads * candidateBuffer
	if b.have >= want {
		return nil
	}

	items, err := e.scanBacklog(ctx, b, want-b.have)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	return e.update(ctx, func(tx *store.Tx) error { return e.enroll(ctx, tx, b, items) })
}

func (e *Engine) readBacklog(ctx context.Context) (backlog, error) {
	var b backlog
	err := e.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		if b.set, err = tx.Settings(ctx); err != nil {
			return err
		}
		if b.cursor, err = tx.Cursor(ctx); err != nil {
			return err
		}
		if b.known, err = tx.PipelineNumbers(ctx, b.set.MainRepo()); err != nil {
			return err
		}
		counts, err := tx.Counts(ctx)
		b.have = counts[store.Untriaged]
		return err
	})
	if err != nil {
		return backlog{}, fmt.Errorf("failed to read the backlog: %w", err)
	}
	return b, nil
}

// enroll records what a scan saw and moves the cursor past it.
func (e *Engine) enroll(ctx context.Context, tx *store.Tx, b backlog, items []github.Item) error {
	cursor, err := tx.Cursor(ctx)
	if err != nil {
		return err
	}
	added := 0
	for _, in := range items {
		isNew, err := tx.Enroll(ctx, ingest.Issue(b.set.MainRepo(), in))
		if err != nil {
			return err
		}
		if isNew {
			added++
		}
		if cursor == nil || in.UpdatedAt.After(*cursor) {
			cursor = &in.UpdatedAt
		}
	}
	if err := tx.SetCursor(ctx, cursor); err != nil {
		return err
	}
	if added > 0 {
		e.Log.Info("discovered backlog items", "added", added, "total", len(b.known)+added)
	}
	return nil
}

// scanBacklog walks the backlog from the cursor and returns the rows it saw,
// stopping once it has seen need items the store does not already hold.
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
func (e *Engine) scanBacklog(ctx context.Context, b backlog, need int) ([]github.Item, error) {
	var (
		out   []github.Item
		fresh int
		moved bool
	)
	err := e.GitHub.ListUpdatedSince(ctx, b.cursor, func(in github.Item) bool {
		out = append(out, in)
		if !b.known[in.Number] {
			fresh++
		}
		if b.cursor == nil || in.UpdatedAt.After(*b.cursor) {
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
// advice about a long-lived backlog does not go stale unnoticed. The expired
// verdict stays in the item's history.
func (e *Engine) Expire(ctx context.Context) error {
	now := e.now()
	return e.update(ctx, func(tx *store.Tx) error {
		set, err := tx.Settings(ctx)
		if err != nil {
			return err
		}
		expired, err := tx.ExpiredTriage(ctx, now, set.RetriageAfter)
		if err != nil {
			return err
		}
		for _, item := range expired {
			e.Log.Info("triage expired, requeueing", "number", item.Number, "triaged_at", item.Verdict.TriagedAt)
			if err := tx.Requeue(ctx, item.ID, ""); err != nil {
				return err
			}
		}
		return nil
	})
}
