package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/store"
)

const fork = "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc"

func advisory(ghsa, state string) github.Advisory {
	return github.Advisory{
		GHSA:      ghsa,
		Summary:   "Made-up advisory " + ghsa,
		Severity:  "high",
		State:     state,
		URL:       "https://github.com/argoproj/argo-workflows/security/advisories/" + ghsa,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
}

func (h *harness) advisories(t *testing.T, all bool) []store.Advisory {
	t.Helper()
	var out []store.Advisory
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		out, err = tx.Advisories(ctx, all)
		return err
	})
	return out
}

func (h *harness) links(t *testing.T, it *store.Item) []store.Link {
	t.Helper()
	var out []store.Link
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		out, err = tx.Links(ctx, it.ID)
		return err
	})
	return out
}

func ghsas(advisories []store.Advisory) []string {
	out := make([]string, 0, len(advisories))
	for _, a := range advisories {
		out = append(out, a.GHSA+" "+a.GHState)
	}
	return out
}

// Advisories in the triage, draft and published states are synced; closed and
// withdrawn ones we never held are not fetched into the store.
func TestSyncAdvisoriesTakesOpenOnes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	h.github.SetAdvisories(
		advisory("GHSA-aaaa-aaaa-aaaa", "triage"),
		advisory("GHSA-bbbb-bbbb-bbbb", "draft"),
		advisory("GHSA-cccc-cccc-cccc", "published"),
		advisory("GHSA-dddd-dddd-dddd", "closed"),
		advisory("GHSA-eeee-eeee-eeee", "withdrawn"),
	)

	require.NoError(t, h.engine.SyncAdvisories(context.Background()))

	got := h.advisories(t, true)
	assert.ElementsMatch(t, []string{
		"GHSA-aaaa-aaaa-aaaa triage", "GHSA-bbbb-bbbb-bbbb draft", "GHSA-cccc-cccc-cccc published",
	}, ghsas(got))
	for _, a := range got {
		assert.True(t, a.Sensitive, "an advisory is always sensitive")
		assert.Equal(t, "high", a.Severity)
		assert.Equal(t, "Made-up advisory "+a.GHSA, a.Title)
	}
}

// An advisory that closes stays in the store, with its notes, marked closed.
func TestSyncAdvisoriesKeepsOneThatCloses(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	ctx := context.Background()
	h.github.SetAdvisories(advisory("GHSA-aaaa-aaaa-aaaa", "draft"))
	require.NoError(t, h.engine.SyncAdvisories(ctx))

	adv := h.advisories(t, true)[0]
	h.update(t, func(ctx context.Context, tx *store.Tx) error {
		return tx.AddNote(ctx, &store.Note{ItemID: adv.ID, Body: "fix agreed", Author: store.AuthorHuman})
	})

	h.github.SetAdvisories(advisory("GHSA-aaaa-aaaa-aaaa", "closed"))
	require.NoError(t, h.engine.SyncAdvisories(ctx))

	assert.Empty(t, h.advisories(t, false), "a closed advisory is no longer open")
	all := h.advisories(t, true)
	require.Len(t, all, 1)
	assert.Equal(t, "closed", all[0].GHState)

	var notes []store.Note
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		notes, err = tx.Notes(ctx, all[0].ID)
		return err
	})
	assert.Len(t, notes, 1, "closing must not lose what we wrote about it")
}

// An advisory missing from a listing is left as it was: the token may simply
// have lost sight of it, which is no evidence it closed.
func TestSyncAdvisoriesLeavesOneThatVanishes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	ctx := context.Background()
	h.github.SetAdvisories(advisory("GHSA-aaaa-aaaa-aaaa", "draft"))
	require.NoError(t, h.engine.SyncAdvisories(ctx))

	h.github.SetAdvisories()
	require.NoError(t, h.engine.SyncAdvisories(ctx))
	assert.Equal(t, []string{"GHSA-aaaa-aaaa-aaaa draft"}, ghsas(h.advisories(t, false)))
}

// A private fork's pull requests come in with the advisory, sensitive and
// linked fix_in_fork, and syncing again adds nothing twice.
func TestSyncAdvisoriesLinksForkPullRequests(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	ctx := context.Background()
	a := advisory("GHSA-aaaa-bbbb-cccc", "draft")
	a.PrivateFork = fork
	h.github.SetAdvisories(a)
	h.github.Pulls[fork] = []github.Pull{
		{Repo: fork, Number: 1, Title: "Fix the made-up leak", State: "open"},
		{Repo: fork, Number: 2, Title: "Backport", State: "merged"},
	}

	require.NoError(t, h.engine.SyncAdvisories(ctx))
	require.NoError(t, h.engine.SyncAdvisories(ctx))

	adv := h.advisories(t, false)
	require.Len(t, adv, 1)
	assert.Equal(t, fork, adv[0].PrivateFork)

	links := h.links(t, &adv[0].Item)
	require.Len(t, links, 2, "one link per pull request, however often it syncs")
	for _, l := range links {
		assert.Equal(t, "fix_in_fork", l.Label())
		assert.Equal(t, store.AuthorBot, l.Author)
		assert.Equal(t, fork, l.Other.Repo)
		assert.True(t, l.Other.Sensitive, "a private fork's pull request is sensitive")
		assert.Empty(t, l.Other.State, "fork pull requests are never triaged")
	}
	assert.Equal(t, "merged", links[1].Other.GHState)
}

// A closed advisory's fork is not fetched: it is no longer synced.
func TestSyncAdvisoriesSkipsClosedAdvisoriesForks(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	a := advisory("GHSA-aaaa-bbbb-cccc", "closed")
	a.PrivateFork = fork
	h.github.SetAdvisories(a)

	require.NoError(t, h.engine.SyncAdvisories(context.Background()))
	assert.Zero(t, h.github.Calls["ListPulls"])
}

// Without a token that can read advisories, the sync says so once and steps
// aside: triage must carry on, and the log must not fill with the same line.
func TestSyncAdvisoriesWithoutAccessSkipsQuietly(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	var logs bytes.Buffer
	h.engine.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h.github.AdvisoryErr = github.ErrNoAccess

	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx), "a refused advisory listing is not a failed tick")
	require.NoError(t, h.engine.Tick(ctx))

	assert.Equal(t, 1, strings.Count(logs.String(), "skipping advisory sync"), "warn once, not every tick")
	assert.Equal(t, store.Queued, h.item(t, 1).State, "triage carried on")
}

// A fork the token cannot see costs its pull requests, not the advisory.
func TestSyncAdvisoriesSurvivesAnUnreadableFork(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	a := advisory("GHSA-aaaa-bbbb-cccc", "triage")
	a.PrivateFork = fork
	h.github.SetAdvisories(a)
	h.github.PullErr[fork] = github.ErrNoAccess

	require.NoError(t, h.engine.SyncAdvisories(context.Background()))
	assert.Len(t, h.advisories(t, false), 1)
}

// Any other failure is a failed phase, reported like the others.
func TestSyncAdvisoriesReportsOtherFailures(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	h.github.AdvisoryErr = assert.AnError
	require.ErrorIs(t, h.engine.SyncAdvisories(context.Background()), assert.AnError)
}

// Which advisories exist is itself sensitive. The sync reports counts at info
// and leaves ids to debug, since a daemon's log is read more widely than its
// database.
func TestSyncAdvisoriesLogsCountsNotIDs(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1)
	var logs bytes.Buffer
	h.engine.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a := advisory("GHSA-aaaa-bbbb-cccc", "draft")
	a.PrivateFork = fork
	h.github.SetAdvisories(a)
	h.github.Pulls[fork] = []github.Pull{{Repo: fork, Number: 1, State: "open"}}

	require.NoError(t, h.engine.SyncAdvisories(context.Background()))

	assert.Contains(t, logs.String(), `msg="synced advisories" open=1 new_fork_links=1`)
	assert.NotContains(t, logs.String(), "GHSA-aaaa-bbbb-cccc")
	assert.NotContains(t, logs.String(), "ghsa-aaaa-bbbb-cccc")
}
