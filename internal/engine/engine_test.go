package engine

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/github/githubtest"
	"github.com/Joibel/triage-bot/internal/store"
)

// harness wires an engine to fakes over a temp database.
type harness struct {
	engine *Engine
	beads  *fakeBeads
	github *githubtest.Fake
	store  *store.Store
	now    time.Time
}

func newHarness(t *testing.T, maxOpen int, items ...*github.Item) *harness {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	fb := newFakeBeads()
	fg := githubtest.New(items...)
	h := &harness{
		beads:  fb,
		github: fg,
		store:  st,
		now:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	h.engine = &Engine{
		Store:  st,
		GitHub: fg,
		Beads:  fb,
		Log:    slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return h.now },
	}

	h.update(t, func(ctx context.Context, tx *store.Tx) error {
		s := store.DefaultSettings()
		s.Org, s.Repo = "argoproj", "argo-workflows"
		s.MaxOpenBeads = maxOpen
		return tx.SaveSettings(ctx, s)
	})
	return h
}

func (h *harness) update(t *testing.T, fn func(context.Context, *store.Tx) error) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.store.Update(ctx, func(tx *store.Tx) error { return fn(ctx, tx) }))
}

func (h *harness) view(t *testing.T, fn func(context.Context, *store.Tx) error) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.store.View(ctx, func(tx *store.Tx) error { return fn(ctx, tx) }))
}

func ref(number int) store.Ref { return store.Ref{Repo: "argoproj/argo-workflows", Number: number} }

// item reads a tracked item, failing the test if it is not tracked.
func (h *harness) item(t *testing.T, number int) *store.Item {
	t.Helper()
	var it *store.Item
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		it, err = tx.Item(ctx, ref(number))
		return err
	})
	return it
}

// tracked reports whether an item is in the store.
func (h *harness) tracked(t *testing.T, number int) bool {
	t.Helper()
	found := true
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		_, err := tx.Item(ctx, ref(number))
		if errors.Is(err, store.ErrNotFound) {
			found = false
			return nil
		}
		return err
	})
	return found
}

func (h *harness) count(t *testing.T, state store.ItemState) int {
	t.Helper()
	var counts map[store.ItemState]int
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		counts, err = tx.Counts(ctx)
		return err
	})
	return counts[state]
}

func (h *harness) total(t *testing.T) int {
	t.Helper()
	n := 0
	for _, s := range store.ItemStates() {
		n += h.count(t, s)
	}
	return n
}

func (h *harness) cursor(t *testing.T) *time.Time {
	t.Helper()
	var c *time.Time
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		c, err = tx.Cursor(ctx)
		return err
	})
	return c
}

// seed tracks items as already triaged, as an earlier run would have left them.
func (h *harness) seed(t *testing.T, items []*github.Item, cursor time.Time) {
	t.Helper()
	h.update(t, func(ctx context.Context, tx *store.Tx) error {
		for _, in := range items {
			if _, err := tx.InsertItem(ctx, &store.Item{
				Repo: "argoproj/argo-workflows", Kind: in.Kind, Number: in.Number,
				UpdatedAt: in.UpdatedAt, State: store.Triaged,
			}); err != nil {
				return err
			}
		}
		return tx.SetCursor(ctx, &cursor)
	})
}

func TestTopUpFillsToLimit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 3, ghItem(1, 100), ghItem(2, 90), ghItem(3, 80), ghItem(4, 70), ghItem(5, 60))
	ctx := context.Background()

	require.NoError(t, h.engine.Discover(ctx))
	require.NoError(t, h.engine.TopUp(ctx))

	assert.Equal(t, 3, h.beads.openCount(), "should open exactly max_open_beads")
	assert.Equal(t, 3, h.count(t, store.Queued))

	// Least recently updated first.
	for _, n := range []int{1, 2, 3} {
		assert.Equal(t, store.Queued, h.item(t, n).State, "item %d should be queued", n)
	}
	assert.Equal(t, store.Untriaged, h.item(t, 4).State)
}

func TestTopUpDoesNotExceedLimitOnSecondTick(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 2, ghItem(1, 100), ghItem(2, 90), ghItem(3, 80))
	ctx := context.Background()

	require.NoError(t, h.engine.Tick(ctx))
	require.NoError(t, h.engine.Tick(ctx))

	assert.Equal(t, 2, h.beads.openCount(), "a second tick must not open more beads")
}

// An item closed on GitHub before we triaged it must be skipped without
// consuming a WIP slot.
func TestTopUpSkipsClosedUpstreamAndTakesNext(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 2, ghItem(1, 100), ghItem(2, 90), ghItem(3, 80))
	ctx := context.Background()
	require.NoError(t, h.engine.Discover(ctx))

	h.github.Close(1)
	require.NoError(t, h.engine.TopUp(ctx))

	assert.Equal(t, store.ClosedUpstream, h.item(t, 1).State)
	assert.Equal(t, store.Queued, h.item(t, 2).State)
	assert.Equal(t, store.Queued, h.item(t, 3).State, "the skip must not cost a slot")
	assert.Equal(t, 2, h.beads.openCount())
}

func TestReconcileRecordsValidVerdict(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))

	beadID := h.item(t, 1).BeadID
	require.NotEmpty(t, beadID)
	h.beads.closeWith(beadID, validTemplate())

	require.NoError(t, h.engine.Reconcile(ctx))

	it := h.item(t, 1)
	assert.Equal(t, store.Triaged, it.State)
	require.NotNil(t, it.Verdict)
	assert.EqualValues(t, "close", it.Verdict.Result.Recommendation)
	assert.EqualValues(t, "stale", it.Verdict.Result.Reason)
	assert.Equal(t, 85, *it.Verdict.Result.Confidence)
	assert.Equal(t, store.Pending, it.Verdict.Disposition.State)
	assert.NotNil(t, it.Verdict.TriagedAt)
	assert.Equal(t, beadID, it.Verdict.BeadID, "the verdict remembers its bead, so reparse can reread it")
}

// An invalid template earns a note naming the problem, plus a reopen, so the
// agent can see what to fix.
func TestReconcileReopensOnInvalidTemplate(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))

	beadID := h.item(t, 1).BeadID
	h.beads.closeWith(beadID, "```yaml\nrecommendation: close\nreason: still_valid\nconfidence: 50\nreasoning: x\n```")

	require.NoError(t, h.engine.Reconcile(ctx))

	it := h.item(t, 1)
	assert.Equal(t, store.Queued, it.State, "still queued: the agent gets another go")
	assert.Equal(t, 1, it.Attempts)
	assert.Contains(t, it.LastError, "still_valid")

	bead := h.beads.get(beadID)
	assert.Equal(t, "open", bead.Status, "bead must be reopened")
	assert.Contains(t, bead.Notes, "attempt 1")
	assert.Contains(t, bead.Notes, "keep_open", "the note must name the correct recommendation")
}

// After the attempt limit the item goes to a human and stops consuming a slot.
func TestReconcileGivesUpAfterAttemptLimit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100), ghItem(2, 90))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))

	beadID := h.item(t, 1).BeadID
	bad := "```yaml\nrecommendation: close\nreason: nonsense\nconfidence: 50\nreasoning: x\n```"

	for attempt := 1; attempt <= 3; attempt++ {
		h.beads.closeWith(beadID, bad)
		require.NoError(t, h.engine.Reconcile(ctx))
		assert.Equal(t, attempt, h.item(t, 1).Attempts)
	}

	it := h.item(t, 1)
	assert.Equal(t, store.NeedsHuman, it.State)
	assert.Equal(t, "closed", h.beads.get(beadID).Status, "a hopeless bead is left closed")
	assert.Contains(t, h.beads.get(beadID).Notes, "giving up")

	// The freed slot must go to the next item.
	require.NoError(t, h.engine.TopUp(ctx))
	assert.Equal(t, store.Queued, h.item(t, 2).State)
}

func TestExpireRequeuesStaleVerdicts(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))
	h.beads.closeWith(h.item(t, 1).BeadID, validTemplate())
	require.NoError(t, h.engine.Reconcile(ctx))
	require.Equal(t, store.Triaged, h.item(t, 1).State)

	// Just short of the window: nothing happens.
	h.now = h.now.Add(179 * 24 * time.Hour)
	require.NoError(t, h.engine.Expire(ctx))
	assert.Equal(t, store.Triaged, h.item(t, 1).State)

	// Past it: back to the queue.
	h.now = h.now.Add(2 * 24 * time.Hour)
	require.NoError(t, h.engine.Expire(ctx))

	it := h.item(t, 1)
	assert.Equal(t, store.Untriaged, it.State)
	assert.Nil(t, it.Verdict, "a requeued item must not keep the expired verdict in force")
	assert.Empty(t, it.BeadID)

	var history []store.Verdict
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		history, err = tx.VerdictHistory(ctx, it.ID)
		return err
	})
	assert.Len(t, history, 1, "but it stays in the item's history")
}

// A rejected recommendation goes back to the queue, and the human's reason is
// put in front of the next agent so it does not repeat the mistake.
func TestRejectionCarriesNoteIntoNextBead(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))
	h.beads.closeWith(h.item(t, 1).BeadID, validTemplate())
	require.NoError(t, h.engine.Reconcile(ctx))

	const note = "still repros on 3.7"
	triaged := h.item(t, 1)
	h.update(t, func(ctx context.Context, tx *store.Tx) error {
		return tx.Dispose(ctx, triaged, store.Disposition{State: store.Rejected, Note: note, Author: store.AuthorHuman})
	})

	require.NoError(t, h.engine.TopUp(ctx))

	newBead := h.item(t, 1).BeadID
	body := h.beads.createdBodies[newBead]
	assert.Contains(t, body, note, "the rejection reason must reach the next agent")
	assert.Contains(t, body, "rejected")
}

func TestDiscoverAdvancesCursorAndIsIncremental(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100), ghItem(2, 90))
	ctx := context.Background()

	require.NoError(t, h.engine.Discover(ctx))
	cursor := h.cursor(t)
	require.NotNil(t, cursor)
	assert.True(t, h.item(t, 2).UpdatedAt.Equal(*cursor), "the cursor is the most recent activity seen")

	before := h.total(t)
	require.NoError(t, h.engine.Discover(ctx))
	assert.Equal(t, before, h.total(t), "re-running discovery must not duplicate items")
}

// A block of items sharing one updated_at second can be larger than the
// discovery window. The cursor bound is inclusive and the cursor only advances
// past a strictly later timestamp, so bounding the fetch by rows rather than by
// new items wedged discovery for good: every tick refetched the same known
// rows, added nothing, and left the cursor where it was.
//
// argo-workflows hit this for real. A bulk mergeability recompute touched 17
// open PRs in the same second, 16 of which were already tracked, against a
// 16-row window — discovery stopped dead about 250 items short of the backlog,
// with one PR stranded inside the block it could never step over.
func TestDiscoverStepsOverATieBlockLargerThanTheWindow(t *testing.T) {
	t.Parallel()

	// Deliberately larger than the window newHarness below produces.
	const tie = 20

	items := make([]*github.Item, 0, tie+1)
	for i := range tie {
		items = append(items, ghItem(100+i, 500))
	}
	stranded := items[tie-1]
	beyond := ghItem(900, 400)
	items = append(items, beyond)

	h := newHarness(t, 1, items...) // window = 1 * candidateBuffer = 3
	ctx := context.Background()

	// Seed the wedge: every item in the block but the last is already tracked
	// and triaged, so the queue is empty, and the cursor sits on the shared
	// timestamp with nothing strictly newer inside the block to move it.
	tieAt := items[0].UpdatedAt
	h.seed(t, items[:tie-1], tieAt)

	require.NoError(t, h.engine.Discover(ctx))

	assert.True(t, h.tracked(t, stranded.Number),
		"the item stranded inside the tie block must be discovered, not skipped over")
	assert.True(t, h.tracked(t, beyond.Number),
		"discovery must reach past the block, not stall on it")
	cursor := h.cursor(t)
	require.NotNil(t, cursor)
	assert.True(t, cursor.After(tieAt),
		"the cursor must step past the block, or the next tick repeats this one")
}

// The scan ceiling must not reintroduce the wedge it sits alongside. A tie
// block bigger than the ceiling is still a block the cursor has to clear in one
// call: stopping inside it would add nothing and move nothing, leaving the next
// tick to repeat this one for ever.
func TestDiscoverScanCeilingNeverStopsInsideTheBlockAtTheCursor(t *testing.T) {
	t.Parallel()

	tie := scanCeiling + 1

	items := make([]*github.Item, 0, tie+1)
	for i := range tie {
		items = append(items, ghItem(1000+i, 500))
	}
	beyond := ghItem(9000, 400)
	items = append(items, beyond)

	h := newHarness(t, 1, items...)

	// The whole block is already tracked, so nothing in it can satisfy the
	// window and nothing in it can move the cursor.
	tieAt := items[0].UpdatedAt
	h.seed(t, items[:tie], tieAt)

	require.NoError(t, h.engine.Discover(context.Background()))

	assert.True(t, h.tracked(t, beyond.Number), "the scan must clear the whole block, ceiling or not")
	cursor := h.cursor(t)
	require.NotNil(t, cursor)
	assert.True(t, cursor.After(tieAt), "the cursor must end up past the block")
}

// The scan is bounded: once it has the new items the queue needs it stops,
// rather than walking the rest of the backlog.
func TestDiscoverStopsOnceTheWindowIsFilled(t *testing.T) {
	t.Parallel()

	items := make([]*github.Item, 0, 20)
	for i := range 20 {
		items = append(items, ghItem(100+i, 500-i))
	}

	h := newHarness(t, 2, items...) // window = 2 * candidateBuffer = 6
	require.NoError(t, h.engine.Discover(context.Background()))

	assert.Equal(t, 6, h.total(t), "discovery must not pull more than the queue needs")
}

// Bead bodies point at the item and carry the contract, but never the body or
// comments: the agent fetches those itself.
func TestBeadBodyIsAPointerNotASnapshot(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(42, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))

	body := h.beads.createdBodies[h.item(t, 42).BeadID]
	assert.Contains(t, body, "https://github.com/o/r/issues/42")
	assert.Contains(t, body, "Fetch the item and its comments yourself")
	assert.Contains(t, body, "recommendation")
	assert.Contains(t, body, "confidence")
}

// A GitHub failure while checking one item is logged and skipped rather than
// aborting the phase, and must not corrupt the item's state. The next tick
// picks it up again.
func TestTickSurvivesGitHubErrors(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Discover(ctx))

	h.github.GetErr = assert.AnError
	require.NoError(t, h.engine.Tick(ctx))
	assert.Equal(t, 0, h.beads.openCount(), "no bead should be opened for an unverifiable item")
	assert.Equal(t, store.Untriaged, h.item(t, 1).State,
		"a failed liveness check must leave the item alone, not mark it closed_upstream")

	h.github.GetErr = nil
	require.NoError(t, h.engine.Tick(ctx))
	assert.Equal(t, store.Queued, h.item(t, 1).State, "the next tick recovers")
}

// The queue is ordered by last activity, not creation date. This is the
// distinction that makes it stale triage: argo-workflows' oldest-created open
// issue is a 2018 design discussion people still comment on, which is not
// neglected and should not be triaged ahead of genuinely dormant items.
func TestQueueOrdersByActivityNotAge(t *testing.T) {
	t.Parallel()

	// Created oldest of the three, but active last week.
	ancient := ghItem(100, 7)
	ancient.CreatedAt = time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)

	// Created recently, but untouched for two years.
	dormant := ghItem(900, 730)
	dormant.CreatedAt = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	middling := ghItem(500, 200)

	h := newHarness(t, 1, ancient, dormant, middling)
	ctx := context.Background()

	require.NoError(t, h.engine.Discover(ctx))
	require.NoError(t, h.engine.TopUp(ctx))

	assert.Equal(t, store.Queued, h.item(t, 900).State,
		"the dormant item must be triaged first, despite being the newest")
	assert.Equal(t, store.Untriaged, h.item(t, 100).State,
		"the oldest-created item is actively discussed and must wait")

	// And the whole queue order, not just the head.
	var untriaged []*store.Item
	h.view(t, func(ctx context.Context, tx *store.Tx) error {
		var err error
		untriaged, err = tx.NextUntriaged(ctx)
		return err
	})
	order := make([]int, 0, len(untriaged))
	for _, it := range untriaged {
		order = append(order, it.Number)
	}
	assert.Equal(t, []int{500, 100}, order, "least recently updated first")
}
