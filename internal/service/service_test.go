package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/github/githubtest"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

const (
	mainRepo = "argoproj/argo-workflows"
	fork     = "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc"
	ghsa     = "GHSA-aaaa-bbbb-cccc"
)

var when = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // fixed test clock

// newService builds a service over a fresh store and a fake GitHub that knows
// one issue, one pull request, one advisory and one fork pull request, all
// made up.
func newService(t *testing.T) (*Service, *githubtest.Fake) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.Update(ctx, func(tx *store.Tx) error {
		s := store.DefaultSettings()
		s.Org, s.Repo = "argoproj", "argo-workflows"
		return tx.SaveSettings(ctx, s)
	}))

	gh := githubtest.New(
		&github.Item{Number: 100, Kind: triage.KindIssue, Title: "Widgets wobble", State: "open", Open: true},
		&github.Item{Number: 200, Kind: triage.KindPR, Title: "Stop the wobble", State: "merged"},
	)
	gh.SetAdvisories(github.Advisory{GHSA: ghsa, Summary: "Made-up leak", State: "draft", Severity: "low", PrivateFork: fork})
	gh.Pulls[fork] = []github.Pull{{Repo: fork, Number: 1, Title: "Fix the made-up leak", State: "open"}}

	svc, err := New(ctx, st, gh)
	require.NoError(t, err)
	svc.Now = func() time.Time { return when }
	return svc, gh
}

func ref(t *testing.T, s *Service, r string) store.Ref {
	t.Helper()
	out, err := s.ParseRef(r)
	require.NoError(t, err)
	return out
}

func TestNewNeedsARepository(t *testing.T) {
	t.Parallel()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	_, err = New(context.Background(), st, githubtest.New())
	require.ErrorContains(t, err, "does not name an org and repo")
}

// A note on something we have never touched brings it in, by touch: its
// identity and title from GitHub, and nothing else. It does not join the
// triage queue.
func TestAddNoteBringsInAnUnknownItem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ref           string
		wantTitle     string
		wantKind      triage.Kind
		wantSensitive bool
		wantFetch     string
	}{
		{name: "issue", ref: "#100", wantTitle: "Widgets wobble", wantKind: triage.KindIssue, wantFetch: "Get"},
		{name: "pull request", ref: "https://github.com/argoproj/argo-workflows/pull/200",
			wantTitle: "Stop the wobble", wantKind: triage.KindPR, wantFetch: "Get"},
		{name: "advisory", ref: ghsa, wantTitle: "Made-up leak", wantKind: store.KindAdvisory,
			wantSensitive: true, wantFetch: "GetAdvisory"},
		{name: "fork pull request", ref: "argo-workflows-ghsa-aaaa-bbbb-cccc#1", wantTitle: "Fix the made-up leak",
			wantKind: triage.KindPR, wantSensitive: true, wantFetch: "GetPull"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, gh := newService(t)

			it, note, err := svc.AddNote(context.Background(), ref(t, svc, tc.ref), "worth a look", []string{"Backport"},
				store.AuthorClaude)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTitle, it.Title)
			assert.Equal(t, tc.wantKind, it.Kind)
			assert.Equal(t, tc.wantSensitive, it.Sensitive)
			assert.Empty(t, it.State, "an item noted is not an item queued for triage")
			assert.Equal(t, 1, gh.Calls[tc.wantFetch])

			assert.Equal(t, store.AuthorClaude, note.Author)
			assert.Equal(t, []string{"backport"}, note.Tags)
			assert.True(t, when.Equal(note.CreatedAt))

			// The second note finds it held and does not ask GitHub again.
			_, _, err = svc.AddNote(context.Background(), ref(t, svc, tc.ref), "again", nil, store.AuthorHuman)
			require.NoError(t, err)
			assert.Equal(t, 1, gh.Calls[tc.wantFetch])

			d, err := svc.Show(context.Background(), ref(t, svc, tc.ref))
			require.NoError(t, err)
			assert.Len(t, d.Notes, 2)
		})
	}
}

// Something GitHub cannot tell us about is refused, not invented.
func TestAddNoteRefusesWhatGitHubCannotFind(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	_, _, err := svc.AddNote(context.Background(), ref(t, svc, "#999"), "hello", nil, store.AuthorHuman)
	require.ErrorContains(t, err, "GitHub could not say what it is")

	_, err = svc.Show(context.Background(), ref(t, svc, "#999"))
	var notTracked *NotTrackedError
	require.ErrorAs(t, err, &notTracked)
}

// Show reads; it never fetches.
func TestShowDoesNotFetch(t *testing.T) {
	t.Parallel()

	svc, gh := newService(t)
	_, err := svc.Show(context.Background(), ref(t, svc, "#100"))
	require.ErrorContains(t, err, "#100 is not in the store")
	assert.Zero(t, gh.Calls["Get"])
}

// Linking an issue to an advisory, written either way round, stores the
// advisory first and makes the issue sensitive: what an advisory is about is
// as private as the advisory.
func TestLinkToAnAdvisory(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	linked, err := svc.Link(context.Background(), ref(t, svc, "#100"), store.AdvisoryFor, ref(t, svc, ghsa),
		store.AuthorClaude)
	require.NoError(t, err)
	assert.True(t, linked.Added)
	assert.Equal(t, ghsa, linked.From.GHSA, "the advisory is the From end")
	assert.Equal(t, 100, linked.To.Number)
	assert.True(t, linked.To.Sensitive)

	d, err := svc.Show(context.Background(), ref(t, svc, "#100"))
	require.NoError(t, err)
	assert.True(t, d.Item.Sensitive)
	require.Len(t, d.Links, 1)
	assert.Equal(t, "has_advisory", d.Links[0].Label())
	assert.Equal(t, store.AuthorClaude, d.Links[0].Author)

	again, err := svc.Link(context.Background(), ref(t, svc, ghsa), store.AdvisoryFor, ref(t, svc, "#100"),
		store.AuthorHuman)
	require.NoError(t, err)
	assert.False(t, again.Added, "the same link, given the other way round, is already there")
}

func TestLinkRefusesAMismatchedPair(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	_, err := svc.Link(context.Background(), ref(t, svc, "#100"), store.FixInFork, ref(t, svc, "#200"),
		store.AuthorHuman)
	require.ErrorContains(t, err, "links an advisory")
}

func TestAdvisoriesCarryTheirLinks(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	ctx := context.Background()
	_, err := svc.Link(ctx, ref(t, svc, ghsa), store.FixInFork, ref(t, svc, fork+"#1"), store.AuthorHuman)
	require.NoError(t, err)

	got, err := svc.Advisories(ctx, false)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "low", got[0].Severity)
	assert.Equal(t, fork, got[0].PrivateFork)
	require.Len(t, got[0].Links, 1)
	assert.Equal(t, fork, got[0].Links[0].Other.Repo)
}

func TestDecide(t *testing.T) {
	t.Parallel()

	svc, _ := newService(t)
	ctx := context.Background()
	require.NoError(t, svc.Store.Update(ctx, func(tx *store.Tx) error {
		id, err := tx.InsertItem(ctx, &store.Item{Repo: mainRepo, Kind: triage.KindIssue, Number: 100, State: store.Queued})
		if err != nil {
			return err
		}
		return tx.RecordVerdict(ctx, id, "tb-1", &triage.Result{
			Recommendation: triage.Close, Reason: triage.Stale, Confidence: new(70), Reasoning: "dormant",
		}, when)
	}))

	_, err := svc.Decide(ctx, ref(t, svc, "#100"), store.Rejected, "", store.AuthorClaude)
	require.ErrorIs(t, err, store.ErrNoteRequired)

	it, err := svc.Decide(ctx, ref(t, svc, "#100"), store.Deferred, "after the release", store.AuthorClaude)
	require.NoError(t, err)
	assert.Equal(t, store.Deferred, it.Verdict.Disposition.State)
	assert.Equal(t, store.AuthorClaude, it.Verdict.Disposition.Author)
	assert.True(t, when.Equal(*it.Verdict.Disposition.At))

	it, err = svc.Decide(ctx, ref(t, svc, "#100"), store.Rejected, "still happens on main", store.AuthorHuman)
	require.NoError(t, err)
	assert.Equal(t, store.Untriaged, it.State)
	assert.Equal(t, "still happens on main", it.CarryNote)

	_, err = svc.Decide(ctx, ref(t, svc, "#100"), store.Applied, "", store.AuthorHuman)
	require.ErrorIs(t, err, store.ErrNotTriaged)
}
