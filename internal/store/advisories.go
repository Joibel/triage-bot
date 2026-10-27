package store

import (
	"context"
	"fmt"
	"strings"
)

// UpsertAdvisory inserts or refreshes an advisory and what only advisories
// have, and returns its item.
func (tx *Tx) UpsertAdvisory(ctx context.Context, a *Advisory) (*Item, error) {
	in := a.Item
	in.Kind = KindAdvisory
	it, err := tx.Upsert(ctx, &in)
	if err != nil {
		return nil, err
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO advisories (item_id, severity, cve_id, private_fork)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (item_id) DO UPDATE SET
			severity     = CASE WHEN excluded.severity != '' THEN excluded.severity ELSE severity END,
			cve_id       = CASE WHEN excluded.cve_id != '' THEN excluded.cve_id ELSE cve_id END,
			private_fork = CASE WHEN excluded.private_fork != '' THEN excluded.private_fork ELSE private_fork END`,
		it.ID, a.Severity, a.CVE, strings.ToLower(a.PrivateFork))
	if err != nil {
		return nil, fmt.Errorf("failed to record advisory %s: %w", it.GHSA, err)
	}
	return it, nil
}

// advisorySelect reads an advisory's item id and extra columns.
const advisorySelect = `SELECT i.id, coalesce(a.severity, ''), coalesce(a.cve_id, ''), coalesce(a.private_fork, '')
	FROM items i LEFT JOIN advisories a ON a.item_id = i.id
	WHERE i.kind = 'advisory'`

// Advisories returns advisories, newest first: only those in a synced state
// unless all is set.
func (tx *Tx) Advisories(ctx context.Context, all bool) ([]Advisory, error) {
	query := advisorySelect
	if !all {
		query += " AND i.gh_state IN ('" + strings.Join(AdvisoryStates(), "', '") + "')"
	}
	return tx.queryAdvisories(ctx, query+" ORDER BY i.created_at DESC, i.id DESC")
}

// AdvisoryOf returns the advisory an item is, or ErrNotFound if it is not one.
func (tx *Tx) AdvisoryOf(ctx context.Context, itemID int64) (*Advisory, error) {
	out, err := tx.queryAdvisories(ctx, advisorySelect+" AND i.id = ?", itemID)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

func (tx *Tx) queryAdvisories(ctx context.Context, query string, args ...any) ([]Advisory, error) {
	out, err := queryAll(ctx, tx, "advisories", func(row scanner) (Advisory, error) {
		var a Advisory
		if err := row.Scan(&a.ID, &a.Severity, &a.CVE, &a.PrivateFork); err != nil {
			return a, fmt.Errorf("failed to read advisories: %w", err)
		}
		return a, nil
	}, query, args...)
	if err != nil {
		return nil, err
	}
	for i := range out {
		it, err := tx.ItemByID(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Item = *it
	}
	return out, nil
}
