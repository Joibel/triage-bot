package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// truncatedTemplate is the shape that used to be recorded wrongly: a comment
// carrying a fenced example, with fields after it.
func truncatedTemplate() string {
	return "```yaml\n" +
		"recommendation: close\nreason: stale\nconfidence: 80\nreasoning: |\n  Dormant.\n" +
		"suggested_comment: |\n  Use this instead:\n\n  ```yaml\n  a: 1\n  ```\n\n  Closing.\n" +
		"suggested_labels: [stale]\n" +
		"evidence:\n  - https://example.com/1\n" +
		"```\n"
}

// triageOne drives an item all the way to a recorded verdict.
func triageOne(t *testing.T, h *harness, number int, template string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))
	h.beads.closeWith(h.item(t, number).BeadID, template)
	require.NoError(t, h.engine.Reconcile(ctx))
	require.Equal(t, store.Triaged, h.item(t, number).State)
}

// rewrite replaces an item's current verdict, as an older parser would have
// recorded it.
func rewrite(t *testing.T, h *harness, number int, change func(*triage.Result)) {
	t.Helper()
	it := h.item(t, number)
	r := it.Verdict.Result
	change(&r)
	h.update(t, func(ctx context.Context, tx *store.Tx) error { return tx.ReplaceResult(ctx, it.Verdict.ID, &r) })
}

func TestReparseIsANoOpWhenNothingChanged(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	triageOne(t, h, 1, truncatedTemplate())

	changes, err := h.engine.Reparse(context.Background(), nil, false)
	require.NoError(t, err)
	assert.Empty(t, changes, "a verdict already matching its bead must not be rewritten")
}

// The repair case: a verdict recorded by an older, broken parser is corrected
// from the bead, which still holds the agent's original text.
func TestReparseRecoversATruncatedVerdict(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	triageOne(t, h, 1, truncatedTemplate())

	// Simulate what the old parser stored: comment cut at the inner fence, and
	// the fields that followed it lost entirely.
	rewrite(t, h, 1, func(r *triage.Result) {
		r.SuggestedComment = "Use this instead:\n"
		r.SuggestedLabels = nil
		r.Evidence = nil
	})

	changes, err := h.engine.Reparse(context.Background(), nil, false)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, 1, changes[0].Number)

	got := h.item(t, 1).Verdict.Result
	assert.Contains(t, got.SuggestedComment, "a: 1", "the fenced example must come back")
	assert.Contains(t, got.SuggestedComment, "Closing.", "and the text after it")
	assert.Equal(t, []string{"stale"}, got.SuggestedLabels)
	assert.Equal(t, []string{"https://example.com/1"}, got.Evidence)
}

// Reparsing corrects what the agent said, not what the maintainer decided.
func TestReparsePreservesTheHumanDecision(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	triageOne(t, h, 1, truncatedTemplate())

	rewrite(t, h, 1, func(r *triage.Result) { r.SuggestedComment = "cut short" })
	triaged := h.item(t, 1)
	h.update(t, func(ctx context.Context, tx *store.Tx) error {
		return tx.Dispose(ctx, triaged, store.Disposition{State: store.Applied, Note: "done it", Author: store.AuthorHuman})
	})
	before := h.item(t, 1).Verdict.TriagedAt

	_, err := h.engine.Reparse(context.Background(), nil, false)
	require.NoError(t, err)

	v := h.item(t, 1).Verdict
	assert.Equal(t, store.Applied, v.Disposition.State, "an applied verdict must not revert to pending")
	assert.Equal(t, "done it", v.Disposition.Note)
	assert.Equal(t, before, v.TriagedAt, "the original triage time must stand")
	assert.Contains(t, v.Result.SuggestedComment, "Closing.", "but the verdict is corrected")
}

func TestReparseDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	triageOne(t, h, 1, truncatedTemplate())
	rewrite(t, h, 1, func(r *triage.Result) { r.SuggestedComment = "cut short" })

	changes, err := h.engine.Reparse(context.Background(), nil, true)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, "cut short", h.item(t, 1).Verdict.Result.SuggestedComment, "dry run must not write")
}

func TestReparseHonoursAnExplicitList(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 2, ghItem(1, 100), ghItem(2, 90))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))
	for _, n := range []int{1, 2} {
		h.beads.closeWith(h.item(t, n).BeadID, truncatedTemplate())
	}
	require.NoError(t, h.engine.Reconcile(ctx))
	for _, n := range []int{1, 2} {
		rewrite(t, h, n, func(r *triage.Result) { r.SuggestedComment = "cut short" })
	}

	changes, err := h.engine.Reparse(ctx, []int{2}, false)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Equal(t, 2, changes[0].Number)
	assert.Equal(t, "cut short", h.item(t, 1).Verdict.Result.SuggestedComment, "unlisted items are untouched")
}

// Items that are not triaged, or whose bead has gone, are skipped rather than
// left stuck or overwritten.
func TestReparseSkipsItemsWithoutAClosedBead(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))
	require.Equal(t, store.Queued, h.item(t, 1).State)

	changes, err := h.engine.Reparse(ctx, nil, false)
	require.NoError(t, err)
	assert.Empty(t, changes, "a queued item has no recorded verdict to correct")
}

// A bead whose template still will not parse is reported, not silently skipped
// and not allowed to wipe the recorded verdict.
func TestReparseReportsStillUnparseableBeads(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	triageOne(t, h, 1, truncatedTemplate())

	h.beads.closeWith(h.item(t, 1).BeadID, "the agent lost the plot entirely")

	changes, err := h.engine.Reparse(context.Background(), nil, false)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.Error(t, changes[0].Err)
	assert.Nil(t, changes[0].After)
	assert.NotNil(t, h.item(t, 1).Verdict, "the existing verdict must survive")
}

// The store keeps an empty list as no list, so a template written with
// `suggested_labels: []` reads back as nil. Treating that as a change would rewrite
// verdicts the maintainer has already acted on, every single run.
func TestReparseIgnoresEmptyVersusAbsentLists(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	triageOne(t, h, 1, "```yaml\n"+
		"recommendation: close\nreason: stale\nconfidence: 60\nreasoning: |\n  Dormant.\n"+
		"suggested_labels: []\n"+
		"```\n")

	changes, err := h.engine.Reparse(context.Background(), nil, false)
	require.NoError(t, err)
	assert.Empty(t, changes, "an empty list and an absent one are the same verdict")
}
