package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/ingest"
	"github.com/Joibel/triage-bot/internal/store"
)

// SyncAdvisories brings the repository's open security advisories into the
// store, with the pull requests in each one's private fork, linked fix_in_fork.
//
// Advisories in the triage, draft or published state are synced. A closed or
// withdrawn one is not fetched into the store, but one already there has its
// state updated and keeps its notes and links. One listing serves both, and
// costs one request per tick plus one per private fork.
//
// Reading advisories needs a token that can see them, which the triage-only
// read token cannot. Without one the sync says so once and skips; triage
// carries on.
func (e *Engine) SyncAdvisories(ctx context.Context) error {
	advisories, err := e.GitHub.ListAdvisories(ctx)
	if errors.Is(err, github.ErrNoAccess) {
		e.advisoriesRefused(err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to list advisories: %w", err)
	}

	pulls := e.forkPulls(ctx, advisories)
	var open, linked int
	if err := e.update(ctx, func(tx *store.Tx) error {
		open, linked = 0, 0
		for _, a := range advisories {
			n, err := e.recordAdvisory(ctx, tx, a, pulls[a.PrivateFork])
			if err != nil {
				return fmt.Errorf("%s: %w", a.GHSA, err)
			}
			if isOpenAdvisory(a) {
				open++
			}
			linked += n
		}
		return nil
	}); err != nil {
		return err
	}
	// Counts only: which advisories exist is itself sensitive, and a daemon's
	// log is read more widely than its database.
	e.Log.Info("synced advisories", "open", open, "new_fork_links", linked)
	return nil
}

// advisoriesRefused logs a refused advisory listing once per engine, so a
// daemon without the token says why once rather than every tick.
func (e *Engine) advisoriesRefused(err error) {
	if e.advisoriesSkipped.Swap(true) {
		e.Log.Debug("skipping advisory sync", "error", err)
		return
	}
	e.Log.Warn("skipping advisory sync: the GitHub token cannot read advisories (it needs repo scope); triage is unaffected",
		"error", err)
}

func isOpenAdvisory(a github.Advisory) bool {
	return slices.Contains(store.AdvisoryStates(), a.State)
}

// forkPulls lists the pull requests in each open advisory's private fork. A
// fork the token cannot see is skipped: the advisory itself still syncs.
func (e *Engine) forkPulls(ctx context.Context, advisories []github.Advisory) map[string][]github.Pull {
	out := map[string][]github.Pull{}
	for _, a := range advisories {
		if !isOpenAdvisory(a) || a.PrivateFork == "" {
			continue
		}
		pulls, err := e.GitHub.ListPulls(ctx, a.PrivateFork)
		if err != nil {
			e.Log.Warn("skipping a private fork's pull requests", "error", err)
			continue
		}
		out[a.PrivateFork] = pulls
	}
	return out
}

// recordAdvisory writes one advisory and its fork's pull requests, and says
// how many fix_in_fork links it added.
func (e *Engine) recordAdvisory(ctx context.Context, tx *store.Tx, a github.Advisory, pulls []github.Pull) (int, error) {
	set, err := tx.Settings(ctx)
	if err != nil {
		return 0, err
	}
	if !isOpenAdvisory(a) {
		// Not synced, but if we hold it, record that it closed.
		_, ierr := tx.Item(ctx, store.Ref{Repo: set.MainRepo(), GHSA: a.GHSA})
		if errors.Is(ierr, store.ErrNotFound) {
			return 0, nil
		}
		if ierr != nil {
			return 0, ierr
		}
	}

	adv, err := tx.UpsertAdvisory(ctx, ingest.Advisory(set.MainRepo(), a))
	if err != nil {
		return 0, err
	}
	linked := 0
	for _, p := range pulls {
		pr, err := tx.Upsert(ctx, ingest.Pull(p))
		if err != nil {
			return 0, err
		}
		added, err := tx.AddLink(ctx, adv, pr, store.FixInFork, store.AuthorBot, e.now())
		if err != nil {
			return 0, err
		}
		if added {
			linked++
			e.Log.Debug("linked a private-fork pull request to its advisory", "advisory", a.GHSA, "pr", p.Number)
		}
	}
	return linked, nil
}
