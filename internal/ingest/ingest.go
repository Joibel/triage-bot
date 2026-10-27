// Package ingest turns what GitHub says about an item into the row the store
// keeps: a pointer and its metadata, never a body or a comment. Discovery, the
// advisory sync and adding a note to an unknown item all come through here, so
// they cannot disagree about what an item's row holds.
package ingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// Issue is an issue or pull request of the main repository.
func Issue(mainRepo string, in github.Item) *store.Item {
	return &store.Item{
		Repo:      mainRepo,
		Kind:      in.Kind,
		Number:    in.Number,
		Title:     in.Title,
		GHState:   in.State,
		CreatedAt: in.CreatedAt,
		UpdatedAt: in.UpdatedAt,
		URL:       in.URL,
	}
}

// Advisory is a security advisory of the main repository.
func Advisory(mainRepo string, a github.Advisory) *store.Advisory {
	return &store.Advisory{
		Repo:        mainRepo,
		Kind:        store.KindAdvisory,
		GHSA:        a.GHSA,
		Title:       a.Summary,
		GHState:     a.State,
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.UpdatedAt,
		URL:         a.URL,
		Severity:    a.Severity,
		CVE:         a.CVE,
		PrivateFork: a.PrivateFork,
	}
}

// Pull is a pull request in an advisory's private fork. Fork names are
// lower-cased, as the store keeps them.
func Pull(p github.Pull) *store.Item {
	return &store.Item{
		Repo:      strings.ToLower(p.Repo),
		Kind:      triage.KindPR,
		Number:    p.Number,
		Title:     p.Title,
		GHState:   p.State,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
		URL:       p.URL,
	}
}

// Upsert writes a fetched item to the store and returns it as stored.
type Upsert func(context.Context, *store.Tx) (*store.Item, error)

// Fetch asks GitHub about the item a ref names, and returns the write that
// records it. The fetch happens now, outside any transaction; the write runs
// whenever the caller's transaction does.
func Fetch(ctx context.Context, gh github.Client, mainRepo string, ref store.Ref) (Upsert, error) {
	switch {
	case ref.IsAdvisory():
		a, err := gh.GetAdvisory(ctx, ref.GHSA)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch %s: %w", ref.GHSA, err)
		}
		row := Advisory(mainRepo, *a)
		return func(ctx context.Context, tx *store.Tx) (*store.Item, error) { return tx.UpsertAdvisory(ctx, row) }, nil
	case ref.Repo == mainRepo:
		it, err := gh.Get(ctx, ref.Number)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch #%d: %w", ref.Number, err)
		}
		return upsert(Issue(mainRepo, *it)), nil
	default:
		p, err := gh.GetPull(ctx, ref.Repo, ref.Number)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch %s#%d: %w", ref.Repo, ref.Number, err)
		}
		return upsert(Pull(*p)), nil
	}
}

func upsert(row *store.Item) Upsert {
	return func(ctx context.Context, tx *store.Tx) (*store.Item, error) { return tx.Upsert(ctx, row) }
}
