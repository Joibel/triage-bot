package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Joibel/triage-bot/internal/triage"
)

// Sensitivity
//
// An advisory, every pull request in a private fork, and every note on either
// is sensitive. So is an item linked to an advisory by advisory_for or
// fix_in_fork. InsertItem marks the first two and AddLink the last; notes take
// their item's sensitivity. Marking is sticky: nothing here ever clears it,
// because a detail that has been sensitive once may have been written down in
// a note since.
//
// Anything that leaves the store in bulk excludes sensitive items and all
// notes.

// AddLink records "from <t> to" and reports whether it is new.
//
// advisory_for and fix_in_fork always start at the advisory, whichever way
// round they were given: their direction is fixed by the kinds at each end,
// so there is nothing to get wrong. A related link is the same whichever way
// round, so its reverse counts as already present.
func (tx *Tx) AddLink(ctx context.Context, from, to *Item, t LinkType, author Author, at time.Time) (bool, error) {
	if !slices.Contains(LinkTypes(), t) {
		return false, fmt.Errorf("no such link type %q", t)
	}
	if from.ID == to.ID {
		return false, errors.New("an item cannot be linked to itself")
	}
	from, to, err := Orient(from, to, t)
	if err != nil {
		return false, err
	}

	if t == Related {
		var n int
		if qerr := tx.tx.QueryRowContext(ctx,
			"SELECT count(*) FROM links WHERE type = 'related' AND from_item = ? AND to_item = ?",
			to.ID, from.ID).Scan(&n); qerr != nil {
			return false, fmt.Errorf("failed to check for a reverse link: %w", qerr)
		}
		if n > 0 {
			return false, nil
		}
	}

	res, err := tx.tx.ExecContext(ctx, `INSERT INTO links (from_item, type, to_item, author, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		from.ID, string(t), to.ID, string(author), formatTime(now(at)))
	if err != nil {
		return false, fmt.Errorf("failed to add a link: %w", err)
	}
	added, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to add a link: %w", err)
	}

	if t == AdvisoryFor || t == FixInFork {
		// to is the caller's own item, so its Sensitive flag is updated too.
		if _, err := tx.tx.ExecContext(ctx, "UPDATE items SET sensitive = 1 WHERE id = ?", to.ID); err != nil {
			return false, fmt.Errorf("failed to mark item %d sensitive: %w", to.ID, err)
		}
		to.Sensitive = true
	}
	return added > 0, nil
}

// Orient checks a link's ends suit its type, and puts the advisory first for
// the two types that start at one. AddLink does this itself; callers use it
// to know which way round a link was stored.
func Orient(from, to *Item, t LinkType) (*Item, *Item, error) {
	if t != AdvisoryFor && t != FixInFork {
		if t != Related && (from.Kind == KindAdvisory || to.Kind == KindAdvisory) {
			return nil, nil, fmt.Errorf(
				"%s links issues and pull requests; link an advisory with advisory_for, fix_in_fork or related", t)
		}
		return from, to, nil
	}

	if to.Kind == KindAdvisory && from.Kind != KindAdvisory {
		from, to = to, from
	}
	if from.Kind != KindAdvisory || to.Kind == KindAdvisory {
		return nil, nil, fmt.Errorf("%s links an advisory to an issue or pull request", t)
	}
	if t == FixInFork && to.Kind != triage.KindPR {
		return nil, nil, fmt.Errorf("fix_in_fork links an advisory to a pull request, not an %s", to.Kind)
	}
	return from, to, nil
}

// Links returns every link touching an item, in both directions, oldest first.
func (tx *Tx) Links(ctx context.Context, itemID int64) ([]Link, error) {
	type row struct {
		link  Link
		other int64
	}
	rows, err := queryAll(ctx, tx, "links", func(sc scanner) (row, error) {
		var (
			r       row
			created string
		)
		if err := sc.Scan(&r.link.ID, &r.link.Type, &r.link.Outgoing, &r.other, &r.link.Author, &created); err != nil {
			return r, fmt.Errorf("failed to read links: %w", err)
		}
		t, err := time.Parse(timeLayout, created)
		if err != nil {
			return r, fmt.Errorf("stored time %q is malformed: %w", created, err)
		}
		r.link.CreatedAt = t
		return r, nil
	}, `SELECT id, type, from_item = ?1, CASE WHEN from_item = ?1 THEN to_item ELSE from_item END, author, created_at
		FROM links WHERE from_item = ?1 OR to_item = ?1 ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}

	out := make([]Link, 0, len(rows))
	for _, r := range rows {
		it, err := tx.ItemByID(ctx, r.other)
		if err != nil {
			return nil, err
		}
		r.link.Other = *it
		out = append(out, r.link)
	}
	return out, nil
}
