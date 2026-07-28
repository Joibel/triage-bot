package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/state"
	"github.com/Joibel/triage-bot/internal/triage"
)

// session drives the loop with a scripted stdin, capturing everything it wrote
// and everything it tried to copy or open.
type session struct {
	path   string
	out    *bytes.Buffer
	copied []string
	opened []string
	copErr error
}

// newSession writes a status file with two pending verdicts: an issue and a PR.
func newSession(t *testing.T) *session {
	t.Helper()

	path := filepath.Join(t.TempDir(), "triage-bot.yaml")
	require.NoError(t, state.Update(path, func(c *state.Config) error {
		c.Org, c.Repo = "argoproj", "argo-workflows"
		c.Items = []*state.Item{
			{
				Number: 8123, Kind: triage.KindIssue, State: state.Triaged,
				Title: "Controller deadlocks on very large workflows",
				Result: &triage.Result{
					Recommendation:   triage.Close,
					Reason:           triage.AlreadyFixed,
					Confidence:       new(92),
					FixedIn:          "v3.6.0",
					Reasoning:        "Fixed by #7001.",
					SuggestedComment: "Closing - this was fixed in v3.6.0\nby #7001. Please reopen if needed.",
				},
				Human: state.Human{State: state.Pending},
			},
			{
				Number: 9500, Kind: triage.KindPR, State: state.Triaged,
				Title: "feat: add foo to bar",
				Result: &triage.Result{
					Recommendation: triage.RequestInfo,
					Reason:         triage.StillWanted,
					Confidence:     new(45),
					Reasoning:      "Author went quiet.",
				},
				Human: state.Human{State: state.Pending},
			},
		}
		return nil
	}))

	return &session{path: path, out: &bytes.Buffer{}}
}

func (s *session) run(t *testing.T, script string) {
	t.Helper()
	r := &reviewer{
		path: s.path,
		in:   bufio.NewScanner(strings.NewReader(script)),
		out:  s.out,
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
	require.NoError(t, r.run(context.Background()))
}

func (s *session) item(t *testing.T, number int) *state.Item {
	t.Helper()
	cfg, err := state.Load(s.path)
	require.NoError(t, err)
	it := cfg.Item(number)
	require.NotNil(t, it)
	return it
}

func TestReviewMarksApplied(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\na\nq\n")

	it := s.item(t, 8123)
	assert.Equal(t, state.Applied, it.Human.State)
	assert.Equal(t, state.Triaged, it.State, "applying does not change the triage state")
	assert.Contains(t, s.out.String(), "marked applied")
}

// Rejection requeues the item and carries the note, which is what stops the
// next agent repeating the rejected conclusion.
func TestReviewRejectionRequeuesWithNote(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\nr\nstill repros on 3.7\nq\n")

	it := s.item(t, 8123)
	assert.Equal(t, state.Untriaged, it.State)
	assert.Equal(t, "still repros on 3.7", it.Human.Note)
	assert.Nil(t, it.Result, "a requeued item must not keep the rejected verdict")
}

// An empty note cancels rather than recording a rejection nobody can learn from.
func TestReviewEmptyNoteCancelsRejection(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\nr\n\nb\nq\n")

	it := s.item(t, 8123)
	assert.Equal(t, state.Triaged, it.State, "the item must be untouched")
	assert.Equal(t, state.Pending, it.Human.State)
	assert.Contains(t, s.out.String(), "Cancelled.")
}

func TestReviewDefers(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "2\nd\nq\n")

	assert.Equal(t, state.Deferred, s.item(t, 9500).Human.State)
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
	assert.Equal(t, state.Applied, s.item(t, 8123).Human.State, "the session carried on")
}

func TestReviewCopyReportsWhenThereIsNoComment(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "2\nc\nb\nq\n")

	assert.Empty(t, s.copied)
	assert.Contains(t, s.out.String(), "No suggested comment")
}

// The URL is derived, not stored. GitHub redirects /issues/N to /pull/N, so one
// form is correct for both kinds.
func TestReviewOpensDerivedURL(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "2\no\nb\nq\n")

	assert.Equal(t, []string{"https://github.com/argoproj/argo-workflows/issues/9500"}, s.opened)
}

func TestReviewEmptyQueueExitsImmediately(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "triage-bot.yaml")
	require.NoError(t, state.Update(path, func(c *state.Config) error {
		c.Org, c.Repo = "argoproj", "argo-workflows"
		return nil
	}))

	out := &bytes.Buffer{}
	r := &reviewer{path: path, in: bufio.NewScanner(strings.NewReader("")), out: out}
	require.NoError(t, r.run(context.Background()))
	assert.Contains(t, out.String(), "Nothing awaiting action.")
}

// Garbage must re-prompt, never fall through to an action.
func TestReviewRejectsGarbageInput(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "x\n99\n0\n1\nzzz\nb\nq\n")

	assert.Equal(t, state.Pending, s.item(t, 8123).Human.State, "nothing may have been acked")
	assert.Contains(t, s.out.String(), "Not a listed number")
	assert.Contains(t, s.out.String(), "Unrecognised")
}

// End of input is a quit, not an error: a truncated pipe must not hang or panic.
func TestReviewTreatsEOFAsQuit(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	s.run(t, "1\n")

	assert.Equal(t, state.Pending, s.item(t, 8123).Human.State)
}

// If the item moved out of Triaged while it was on screen - a daemon requeue,
// another shell acking it - the decision must be refused, not written over.
func TestReviewRefusesToActOnAConcurrentlyChangedItem(t *testing.T) {
	t.Parallel()

	s := newSession(t)
	require.NoError(t, state.Update(s.path, func(c *state.Config) error {
		c.Item(8123).Requeue("daemon expired it")
		return nil
	}))

	r := &reviewer{path: s.path, out: s.out, in: bufio.NewScanner(strings.NewReader(""))}
	err := r.record(8123, state.Applied, "")
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
