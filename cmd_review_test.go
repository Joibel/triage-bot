package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/github/githubtest"
	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// session drives the loop with a scripted stdin, capturing everything it wrote
// and everything it tried to copy or open.
type session struct {
	svc    *service.Service
	out    *bytes.Buffer
	copied []string
	opened []string
	copErr error
}

// newTestService opens a fresh database set up for argo-workflows, with a
// fake GitHub that knows nothing.
func newTestService(t *testing.T) *service.Service {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.Update(context.Background(), func(tx *store.Tx) error {
		s := store.DefaultSettings()
		s.Org, s.Repo = "argoproj", "argo-workflows"
		return tx.SaveSettings(context.Background(), s)
	}))
	svc, err := service.New(context.Background(), st, githubtest.New())
	require.NoError(t, err)
	return svc
}

// seedVerdict records a pending verdict on a new main-repository item.
func seedVerdict(t *testing.T, st *store.Store, it store.Item, r *triage.Result) {
	t.Helper()
	ctx := context.Background()
	it.Repo, it.State = "argoproj/argo-workflows", store.Queued
	require.NoError(t, st.Update(ctx, func(tx *store.Tx) error {
		id, err := tx.InsertItem(ctx, &it)
		if err != nil {
			return err
		}
		return tx.RecordVerdict(ctx, id, "tb-1", r, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	}))
}

// newSession sets up a database with two pending verdicts: an issue and a PR.
func newSession(t *testing.T) *session {
	t.Helper()

	svc := newTestService(t)
	seedVerdict(t, svc.Store, store.Item{
		Number: 8123, Kind: triage.KindIssue, Title: "Controller deadlocks on very large workflows",
	}, &triage.Result{
		Recommendation:   triage.Close,
		Reason:           triage.AlreadyFixed,
		Confidence:       new(92),
		FixedIn:          "v3.6.0",
		Reasoning:        "Fixed by #7001.",
		SuggestedComment: "Closing - this was fixed in v3.6.0\nby #7001. Please reopen if needed.",
	})
	seedVerdict(t, svc.Store, store.Item{
		Number: 9500, Kind: triage.KindPR, Title: "feat: add foo to bar",
	}, &triage.Result{
		Recommendation: triage.RequestInfo,
		Reason:         triage.StillWanted,
		Confidence:     new(45),
		Reasoning:      "Author went quiet.",
	})

	return &session{svc: svc, out: &bytes.Buffer{}}
}

func (s *session) reviewer(script string) *reviewer {
	return &reviewer{
		svc: s.svc,
		in:  bufio.NewScanner(strings.NewReader(script)),
		out: s.out,
		copy: func(_ context.Context, text string) error {
			if s.copErr != nil {
				return s.copErr
			}
			s.copied = append(s.copied, text)
			return nil
		},
		open: func(url string) error {
			s.opened = append(s.opened, url)
			return nil
		},
	}
}

func (s *session) run(t *testing.T, script string) {
	t.Helper()
	require.NoError(t, s.reviewer(script).run(context.Background()))
}

func (s *session) item(t *testing.T, number int) *store.Item {
	t.Helper()
	var it *store.Item
	require.NoError(t, s.svc.Store.View(context.Background(), func(tx *store.Tx) error {
		var err error
		it, err = tx.Item(context.Background(), store.Ref{Repo: "argoproj/argo-workflows", Number: number})
		return err
	}))
	return it
}

// disposition reads the decision on an item's verdict: the current one, or,
// once a rejection has requeued the item, the latest in its history.
func (s *session) disposition(t *testing.T, number int) store.Disposition {
	t.Helper()
	it := s.item(t, number)
	if it.Verdict != nil {
		return it.Verdict.Disposition
	}
	var history []store.Verdict
	require.NoError(t, s.svc.Store.View(context.Background(), func(tx *store.Tx) error {
		var err error
		history, err = tx.VerdictHistory(context.Background(), it.ID)
		return err
	}))
	require.NotEmpty(t, history)
	return history[len(history)-1].Disposition
}

func TestReviewMarksApplied(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\na\nq\n")

	assert.Equal(t, store.Applied, s.disposition(t, 8123).State)
	assert.Equal(t, store.AuthorHuman, s.disposition(t, 8123).Author)
	assert.Equal(t, store.Triaged, s.item(t, 8123).State, "applying does not change the triage state")
	assert.Contains(t, s.out.String(), "marked applied")
}

// Rejection requeues the item and carries the note, which is what stops the
// next agent repeating the rejected conclusion.
func TestReviewRejectionRequeuesWithNote(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\nr\nstill repros on 3.7\nq\n")

	it := s.item(t, 8123)
	assert.Equal(t, store.Untriaged, it.State)
	assert.Equal(t, "still repros on 3.7", it.CarryNote)
	assert.Nil(t, it.Verdict, "a requeued item must not keep the rejected verdict in force")
	assert.Equal(t, store.Rejected, s.disposition(t, 8123).State, "the rejection stays on the verdict's history")
}

// An empty note cancels rather than recording a rejection nobody can learn from.
func TestReviewEmptyNoteCancelsRejection(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\nr\n\nb\nq\n")

	assert.Equal(t, store.Triaged, s.item(t, 8123).State, "the item must be untouched")
	assert.Equal(t, store.Pending, s.disposition(t, 8123).State)
	assert.Contains(t, s.out.String(), "Cancelled.")
}

func TestReviewDefers(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "2\nd\nq\n")

	assert.Equal(t, store.Deferred, s.disposition(t, 9500).State)
}

// Copying must hand over the unwrapped comment: that is the entire point of the
// action, given a terminal selection picks up the wrapping.
func TestReviewCopiesUnwrappedComment(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\nc\nb\nq\n")

	require.Len(t, s.copied, 1)
	assert.Equal(t, "Closing - this was fixed in v3.6.0 by #7001. Please reopen if needed.", s.copied[0])
	assert.NotContains(t, s.copied[0], "\n")
	assert.Contains(t, s.out.String(), "copied to clipboard")
}

// A missing clipboard tool is reported and the session continues.
func TestReviewSurvivesClipboardFailure(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.copErr = errors.New("no clipboard tool found")
	s.run(t, "1\nc\na\nq\n")

	assert.Contains(t, s.out.String(), "Could not copy")
	assert.Equal(t, store.Applied, s.disposition(t, 8123).State, "the session carried on")
}

func TestReviewCopyReportsWhenThereIsNoComment(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "2\nc\nb\nq\n")

	assert.Empty(t, s.copied)
	assert.Contains(t, s.out.String(), "No suggested comment")
}

// Items imported from the status file carry no URL, so one is derived. GitHub
// redirects /issues/N to /pull/N, so one form is correct for both kinds.
func TestReviewOpensDerivedURL(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "2\no\nb\nq\n")

	assert.Equal(t, []string{"https://github.com/argoproj/argo-workflows/issues/9500"}, s.opened)
}

func TestReviewEmptyQueueExitsImmediately(t *testing.T) {
	t.Parallel()

	out := &bytes.Buffer{}
	r := &reviewer{svc: newTestService(t), in: bufio.NewScanner(strings.NewReader("")), out: out}
	require.NoError(t, r.run(context.Background()))
	assert.Contains(t, out.String(), "Nothing awaiting action.")
}

// Garbage must re-prompt, never fall through to an action.
func TestReviewRejectsGarbageInput(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "x\n99\n0\n1\nzzz\nb\nq\n")

	assert.Equal(t, store.Pending, s.disposition(t, 8123).State, "nothing may have been acked")
	assert.Contains(t, s.out.String(), "Not a listed number")
	assert.Contains(t, s.out.String(), "Unrecognised")
}

// A recommendation filter hides every other verdict from the list, so the
// numbering the user picks from is the filtered numbering.
// Filtering by recommendation reindexes the list, so "1" after the filter acts
// on the matching item, not the first item of the unfiltered queue.
func TestReviewFilterByRecommendation(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "f\nrequest_info\n1\nd\nq\n")

	assert.Equal(t, store.Deferred, s.disposition(t, 9500).State)
	assert.Equal(t, store.Pending, s.disposition(t, 8123).State, "the filtered-out item is untouched")
	assert.Contains(t, s.out.String(), "[filter: request_info]")
}

// A reason identifies its recommendation, so one token narrows to the pair.
func TestReviewFilterByReason(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "f\nstill_wanted\n1\nd\nq\n")

	assert.Equal(t, store.Deferred, s.disposition(t, 9500).State)
	assert.Equal(t, store.Pending, s.disposition(t, 8123).State)
	assert.Contains(t, s.out.String(), "[filter: request_info/still_wanted]")
}

// Filtering by kind is the other axis of the same filter: it narrows to issues
// or to PRs without naming a verdict.
func TestReviewFilterByKind(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "f\npr\n1\nd\nq\n")

	assert.Equal(t, store.Deferred, s.disposition(t, 9500).State)
	assert.Equal(t, store.Pending, s.disposition(t, 8123).State, "the issue is filtered out")
	assert.Contains(t, s.out.String(), "[filter: pr]")
}

// Kind and verdict combine, so a filter can name both.
func TestReviewFilterCombinesKindAndVerdict(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	// The issue is close/already_fixed, so `issue close` leaves just that one.
	s.run(t, "f\nissue close\n1\na\nq\n")

	assert.Equal(t, store.Applied, s.disposition(t, 8123).State)
	assert.Equal(t, store.Pending, s.disposition(t, 9500).State)
	assert.Contains(t, s.out.String(), "[filter: issue close]")
}

// A kind that excludes the verdict's items matches nothing rather than being
// quietly widened to one of the two.
func TestReviewFilterKindAndVerdictIntersect(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "f\npr close\nq\n")

	assert.Contains(t, s.out.String(), "0 awaiting action [filter: pr close]")
}

// An empty token clears the filter, so the whole queue comes back.
func TestReviewFilterClears(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	// Filter to the PR, clear it, then item 1 is the issue again.
	s.run(t, "f\nrequest_info\nf\n\n1\na\nq\n")

	assert.Equal(t, store.Applied, s.disposition(t, 8123).State)
}

// An unknown token is reported and leaves the current filter untouched, so the
// unfiltered item 1 still opens.
func TestReviewFilterUnknownIsIgnored(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "f\nnonsense\n1\na\nq\n")

	assert.Equal(t, store.Applied, s.disposition(t, 8123).State)
	assert.Contains(t, s.out.String(), "no such kind, recommendation or reason")
}

// A filter matching nothing does not exit: it says so and keeps the prompt, so
// the user can change or clear it rather than being dropped out of review.
func TestReviewFilterWithNoMatchesStaysInteractive(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "f\nkeep_open\nq\n")

	out := s.out.String()
	assert.Contains(t, out, "0 awaiting action [filter: keep_open]")
	assert.Contains(t, out, "Nothing matches this filter")
}

func TestParseFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		line       string
		wantErr    string
		wantKind   triage.Kind
		wantRec    triage.Recommendation
		wantReason triage.Reason
	}{
		{name: "recommendation", line: "close", wantRec: triage.Close},
		{name: "reason implies recommendation", line: "already_fixed",
			wantRec: triage.Close, wantReason: triage.AlreadyFixed},
		{name: "kind", line: "pr", wantKind: triage.KindPR},
		{name: "kind and recommendation", line: "issue close",
			wantKind: triage.KindIssue, wantRec: triage.Close},
		{name: "order does not matter", line: "already_fixed pr",
			wantKind: triage.KindPR, wantRec: triage.Close, wantReason: triage.AlreadyFixed},
		{name: "a recommendation agreeing with its reason is not a conflict",
			line: "close already_fixed", wantRec: triage.Close, wantReason: triage.AlreadyFixed},
		{name: "empty clears"},
		{name: "unknown", line: "vibes", wantErr: "no such kind, recommendation or reason"},
		{name: "two kinds conflict", line: "issue pr", wantErr: "cannot filter on both issue and pr"},
		{name: "two recommendations conflict", line: "close respond",
			wantErr: "cannot filter on both close and respond"},
		{name: "a reason conflicting with a recommendation", line: "close still_wanted",
			wantErr: "cannot filter on both close and request_info"},
		{name: "two reasons conflict", line: "stale obsolete",
			wantErr: "cannot filter on both stale and obsolete"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f, err := parseFilter(tc.line)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantKind, f.kind)
			assert.Equal(t, tc.wantRec, f.rec)
			assert.Equal(t, tc.wantReason, f.reason)
		})
	}
}

// End of input is a quit, not an error: a truncated pipe must not hang or panic.
func TestReviewTreatsEOFAsQuit(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\n")

	assert.Equal(t, store.Pending, s.disposition(t, 8123).State)
}

// If the item moved out of Triaged while it was on screen - a daemon requeue,
// another shell acking it - the decision must be refused, not written over.
func TestReviewRefusesToActOnAConcurrentlyChangedItem(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	onScreen := s.item(t, 8123)
	require.NoError(t, s.svc.Store.Update(context.Background(), func(tx *store.Tx) error {
		return tx.Requeue(context.Background(), onScreen.ID, "daemon expired it")
	}))

	err := s.reviewer("").record(context.Background(), onScreen, store.Applied, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not triaged")
}

// The list is the summary the whole feature exists for: it must fit a line per
// item and carry enough to choose by.
func TestReviewListIsOneLinePerItem(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "q\n")

	out := s.out.String()
	assert.Contains(t, out, "argoproj/argo-workflows - 2 awaiting action")
	assert.Contains(t, out, "8123")
	assert.Contains(t, out, "close/already_fixed")
	assert.Contains(t, out, "92")
	assert.Contains(t, out, "(pr)", "PRs must be distinguishable in the list")
	assert.NotContains(t, out, "Fixed by #7001.", "reasoning belongs in the detail view, not the list")
}

// An open item shows what else is known about it, so the decision is made
// with the notes and links in front of the maintainer.
func TestReviewShowsNotesAndLinks(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	ctx := context.Background()
	_, _, err := s.svc.AddNote(ctx, store.Ref{Repo: "argoproj/argo-workflows", Number: 8123},
		"reproduced on main with 5000 pods", []string{"repro"}, store.AuthorClaude)
	require.NoError(t, err)
	_, err = s.svc.Link(ctx, store.Ref{Repo: "argoproj/argo-workflows", Number: 9500}, store.Fixes,
		store.Ref{Repo: "argoproj/argo-workflows", Number: 8123}, store.AuthorHuman)
	require.NoError(t, err)

	s.run(t, "1\nb\nq\n")

	out := s.out.String()
	assert.Contains(t, out, "reproduced on main with 5000 pods")
	assert.Contains(t, out, "claude [repro]")
	assert.Contains(t, out, "fixed_by")
	assert.Contains(t, out, "feat: add foo to bar", "the linked item is named")
}

func TestReviewAddsANote(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\nn\nchecked on main today\nbackport needs-docs\nb\nq\n")

	out := s.out.String()
	assert.Contains(t, out, "Noted.")
	assert.Contains(t, out, "human [backport, needs-docs]", "the item is shown again with its new note")

	d, err := s.svc.Show(context.Background(), store.Ref{Repo: "argoproj/argo-workflows", Number: 8123})
	require.NoError(t, err)
	require.Len(t, d.Notes, 1)
	assert.Equal(t, "checked on main today", d.Notes[0].Body)
	assert.Equal(t, store.AuthorHuman, d.Notes[0].Author)
	assert.Equal(t, store.Pending, s.disposition(t, 8123).State, "a note is not a decision")
}

func TestReviewEmptyNoteCancels(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\nn\n\nb\nq\n")

	assert.Contains(t, s.out.String(), "Cancelled.")
	d, err := s.svc.Show(context.Background(), store.Ref{Repo: "argoproj/argo-workflows", Number: 8123})
	require.NoError(t, err)
	assert.Empty(t, d.Notes)
}
