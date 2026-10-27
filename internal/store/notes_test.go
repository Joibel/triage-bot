package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/triage"
)

func addNote(t *testing.T, s *Store, it *Item, body string, tags ...string) Note {
	t.Helper()
	n := Note{ItemID: it.ID, Body: body, Tags: tags, Author: AuthorHuman}
	update(t, s, func(tx *Tx) error { return tx.AddNote(context.Background(), &n) })
	return n
}

func TestAddNoteNormalisesTags(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	it := insert(t, s, Item{Number: 1})
	addNote(t, s, it, "  needs a backport to 3.6  ", "Backport", "#customer", "backport", "  ", "in progress")

	notes := view(t, s, func(ctx context.Context, tx *Tx) ([]Note, error) { return tx.Notes(ctx, it.ID) })
	require.Len(t, notes, 1)
	assert.Equal(t, "needs a backport to 3.6", notes[0].Body)
	assert.Equal(t, []string{"backport", "customer", "in-progress"}, notes[0].Tags)
	assert.Equal(t, AuthorHuman, notes[0].Author)
	assert.False(t, notes[0].CreatedAt.IsZero())
}

func TestAddNoteRefusesBadInput(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	it := insert(t, s, Item{Number: 1})
	for _, n := range []Note{
		{ItemID: it.ID, Body: "   ", Author: AuthorHuman},
		{ItemID: it.ID, Body: "text", Author: AuthorBot},
	} {
		err := s.Update(context.Background(), func(tx *Tx) error { return tx.AddNote(context.Background(), &n) })
		assert.Error(t, err)
	}
}

func searchFor(t *testing.T, s *Store, q SearchQuery) []Hit {
	t.Helper()
	return view(t, s, func(ctx context.Context, tx *Tx) ([]Hit, error) { return tx.Search(ctx, q) })
}

// describeHits renders hits as "#N" for a title match and "#N: body" for a
// note, so a test can assert on the whole result at once.
func describeHits(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		s := h.Item.Ref(mainRepo)
		if h.Note != nil {
			s += ": " + h.Note.Body
		}
		out = append(out, s)
	}
	return out
}

func TestSearch(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	controller := insert(t, s, Item{Number: 1, Title: "Controller deadlocks on large workflows", State: Triaged})
	ui := insert(t, s, Item{Number: 2, Title: "UI shows the wrong timezone", Kind: triage.KindPR})
	advisory := insert(t, s, Item{Kind: KindAdvisory, GHSA: "GHSA-aaaa-bbbb-cccc", Title: "Token leak in the server"})
	addNote(t, s, controller, "reproduced with 5000 pods", "repro")
	addNote(t, s, ui, "needs a backport to 3.6", "backport")
	addNote(t, s, advisory, "fix is in the fork, waiting on the release", "security")

	tests := []struct {
		name string
		q    SearchQuery
		want []string
	}{
		{name: "title", q: SearchQuery{Text: "deadlock*"}, want: []string{"#1"}},
		{name: "stemmed note", q: SearchQuery{Text: "reproducing"}, want: []string{"#1: reproduced with 5000 pods"}},
		{name: "every word must match", q: SearchQuery{Text: "backport release"}, want: []string{}},
		{name: "punctuation is searched for, not parsed", q: SearchQuery{Text: `3.6 "fork"-ish AND OR(`},
			want: []string{}},
		{name: "notes and titles together", q: SearchQuery{Text: "the"}, want: []string{
			"GHSA-aaaa-bbbb-cccc", "#2", "GHSA-aaaa-bbbb-cccc: fix is in the fork, waiting on the release"}},
		{name: "tag without words lists tagged notes", q: SearchQuery{Tag: "Backport"},
			want: []string{"#2: needs a backport to 3.6"}},
		{name: "tag excludes title matches", q: SearchQuery{Text: "timezone", Tag: "backport"}, want: []string{}},
		{name: "public only", q: SearchQuery{Text: "fork", Sensitive: new(false)}, want: []string{}},
		{name: "sensitive only", q: SearchQuery{Sensitive: new(true)},
			want: []string{"GHSA-aaaa-bbbb-cccc: fix is in the fork, waiting on the release"}},
		{name: "pipeline state", q: SearchQuery{Text: "pods", State: "triaged"},
			want: []string{"#1: reproduced with 5000 pods"}},
		{name: "kind", q: SearchQuery{Kind: triage.KindPR}, want: []string{"#2: needs a backport to 3.6"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := describeHits(searchFor(t, s, tc.q))
			if tc.name == "notes and titles together" {
				// Ranking between the two indexes is not meaningful; compare as sets.
				assert.ElementsMatch(t, tc.want, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSearchNeedsWordsOrAFilter(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	err := s.View(context.Background(), func(tx *Tx) error {
		_, err := tx.Search(context.Background(), SearchQuery{Text: "  * "})
		return err
	})
	require.ErrorContains(t, err, "words or a filter")
}

func TestSearchSnippetMarksTheMatch(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	it := insert(t, s, Item{Number: 1})
	addNote(t, s, it, "the controller panics when the informer resyncs")

	hits := searchFor(t, s, SearchQuery{Text: "informer"})
	require.Len(t, hits, 1)
	assert.Contains(t, hits[0].Snippet, "[informer]")
}

func TestLinksReadBothWays(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	issue := insert(t, s, Item{Number: 1})
	pr := insert(t, s, Item{Number: 2, Kind: triage.KindPR})
	update(t, s, func(tx *Tx) error {
		_, err := tx.AddLink(context.Background(), pr, issue, Fixes, AuthorHuman, at(1))
		return err
	})

	fromIssue := view(t, s, func(ctx context.Context, tx *Tx) ([]Link, error) { return tx.Links(ctx, issue.ID) })
	require.Len(t, fromIssue, 1)
	assert.Equal(t, "fixed_by", fromIssue[0].Label())
	assert.Equal(t, 2, fromIssue[0].Other.Number)

	fromPR := view(t, s, func(ctx context.Context, tx *Tx) ([]Link, error) { return tx.Links(ctx, pr.ID) })
	require.Len(t, fromPR, 1)
	assert.Equal(t, "fixes", fromPR[0].Label())
	assert.Equal(t, 1, fromPR[0].Other.Number)
}

func TestAddLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		from, to  string // "issue", "pr", "advisory" or "forkpr"
		linkType  LinkType
		wantErr   string
		wantFrom  string // which end ends up as From
		sensitive string // which end, other than an advisory, is then sensitive
	}{
		{name: "advisory first", from: "advisory", to: "issue", linkType: AdvisoryFor,
			wantFrom: "advisory", sensitive: "issue"},
		{name: "advisory second is turned round", from: "issue", to: "advisory", linkType: AdvisoryFor,
			wantFrom: "advisory", sensitive: "issue"},
		{name: "fork fix", from: "forkpr", to: "advisory", linkType: FixInFork, wantFrom: "advisory"},
		{name: "fix in fork must be a PR", from: "advisory", to: "issue", linkType: FixInFork,
			wantErr: "to a pull request"},
		{name: "advisory_for needs an advisory", from: "issue", to: "pr", linkType: AdvisoryFor,
			wantErr: "links an advisory"},
		{name: "duplicate_of is for issues and PRs", from: "advisory", to: "issue", linkType: DuplicateOf,
			wantErr: "link an advisory with"},
		{name: "related may touch an advisory", from: "issue", to: "advisory", linkType: Related, wantFrom: "issue"},
		{name: "unknown type", from: "issue", to: "pr", linkType: "blocks", wantErr: "no such link type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := openTest(t)
			items := map[string]*Item{
				"issue":    insert(t, s, Item{Number: 1}),
				"pr":       insert(t, s, Item{Number: 2, Kind: triage.KindPR}),
				"advisory": insert(t, s, Item{Kind: KindAdvisory, GHSA: "GHSA-aaaa-bbbb-cccc"}),
				"forkpr":   insert(t, s, Item{Repo: mainRepo + "-ghsa-aaaa-bbbb-cccc", Kind: triage.KindPR, Number: 1}),
			}
			err := s.Update(context.Background(), func(tx *Tx) error {
				_, err := tx.AddLink(context.Background(), items[tc.from], items[tc.to], tc.linkType, AuthorHuman, at(1))
				return err
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			links := view(t, s, func(ctx context.Context, tx *Tx) ([]Link, error) {
				return tx.Links(ctx, items[tc.wantFrom].ID)
			})
			require.Len(t, links, 1)
			assert.True(t, links[0].Outgoing, "%s must be the From end", tc.wantFrom)

			if tc.sensitive != "" {
				got := view(t, s, func(ctx context.Context, tx *Tx) (*Item, error) {
					return tx.ItemByID(ctx, items[tc.sensitive].ID)
				})
				assert.True(t, got.Sensitive, "an item an advisory is about is sensitive")
			}
		})
	}
}

// Adding the same link twice, or a related link the other way round, is a
// no-op rather than an error, so syncing and Claude sessions can re-add freely.
func TestAddLinkIsIdempotent(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	a := insert(t, s, Item{Number: 1})
	b := insert(t, s, Item{Number: 2})

	var added []bool
	update(t, s, func(tx *Tx) error {
		for _, pair := range [][2]*Item{{a, b}, {a, b}, {b, a}} {
			ok, err := tx.AddLink(context.Background(), pair[0], pair[1], Related, AuthorClaude, at(1))
			if err != nil {
				return err
			}
			added = append(added, ok)
		}
		return nil
	})
	assert.Equal(t, []bool{true, false, false}, added)
}

func TestAddLinkRefusesASelfLink(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	a := insert(t, s, Item{Number: 1})
	err := s.Update(context.Background(), func(tx *Tx) error {
		_, err := tx.AddLink(context.Background(), a, a, Related, AuthorHuman, at(1))
		return err
	})
	require.ErrorContains(t, err, "itself")
}

func TestAdvisories(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	update(t, s, func(tx *Tx) error {
		for _, a := range []Advisory{
			{Repo: mainRepo, GHSA: "GHSA-aaaa-aaaa-aaaa", GHState: "draft", CreatedAt: at(2),
				Severity: "high", PrivateFork: "argoproj/Argo-Workflows-GHSA-aaaa-aaaa-aaaa"},
			{Repo: mainRepo, GHSA: "GHSA-bbbb-bbbb-bbbb", GHState: "closed", CreatedAt: at(3)},
			{Repo: mainRepo, GHSA: "GHSA-cccc-cccc-cccc", GHState: "published", CreatedAt: at(4), CVE: "CVE-2026-0001"},
		} {
			if _, err := tx.UpsertAdvisory(context.Background(), &a); err != nil {
				return err
			}
		}
		// A later sync without a severity must not blank the recorded one.
		_, err := tx.UpsertAdvisory(context.Background(), &Advisory{Repo: mainRepo, GHSA: "GHSA-aaaa-aaaa-aaaa"})
		return err
	})

	open := view(t, s, func(ctx context.Context, tx *Tx) ([]Advisory, error) { return tx.Advisories(ctx, false) })
	require.Len(t, open, 2, "closed advisories are not listed as open")
	assert.Equal(t, "GHSA-cccc-cccc-cccc", open[0].GHSA, "newest first")
	assert.Equal(t, "CVE-2026-0001", open[0].CVE)
	assert.Equal(t, "high", open[1].Severity)
	assert.Equal(t, "argoproj/argo-workflows-ghsa-aaaa-aaaa-aaaa", open[1].PrivateFork, "fork names are lower-cased")
	assert.True(t, open[1].Sensitive)

	all := view(t, s, func(ctx context.Context, tx *Tx) ([]Advisory, error) { return tx.Advisories(ctx, true) })
	assert.Len(t, all, 3)
}
