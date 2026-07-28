package engine

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/state"
)

// harness wires an engine to fakes over a temp status file.
type harness struct {
	engine *Engine
	beads  *fakeBeads
	github *fakeGitHub
	path   string
	now    time.Time
}

func newHarness(t *testing.T, maxOpen int, items ...*github.Item) *harness {
	t.Helper()

	path := filepath.Join(t.TempDir(), "triage-bot.yaml")
	fb := newFakeBeads()
	fg := newFakeGitHub(items...)
	h := &harness{
		beads:  fb,
		github: fg,
		path:   path,
		now:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	h.engine = &Engine{
		Path:   path,
		GitHub: fg,
		Beads:  fb,
		Log:    slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return h.now },
	}

	require.NoError(t, state.Update(path, func(c *state.Config) error {
		c.Org, c.Repo = "argoproj", "argo-workflows"
		c.Settings.MaxOpenBeads = maxOpen
		return nil
	}))
	return h
}

func (h *harness) load(t *testing.T) *state.Config {
	t.Helper()
	c, err := state.Load(h.path)
	require.NoError(t, err)
	return c
}

func (h *harness) item(t *testing.T, number int) *state.Item {
	t.Helper()
	it := h.load(t).Item(number)
	require.NotNil(t, it, "item %d should be tracked", number)
	return it
}

func TestTopUpFillsToLimit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 3, ghItem(1, 100), ghItem(2, 90), ghItem(3, 80), ghItem(4, 70), ghItem(5, 60))
	ctx := context.Background()

	require.NoError(t, h.engine.Discover(ctx))
	require.NoError(t, h.engine.TopUp(ctx))

	assert.Equal(t, 3, h.beads.openCount(), "should open exactly max_open_beads")
	assert.Equal(t, 3, h.load(t).Count(state.Queued))

	// Oldest created first.
	for _, n := range []int{1, 2, 3} {
		assert.Equal(t, state.Queued, h.item(t, n).State, "item %d should be queued", n)
	}
	assert.Equal(t, state.Untriaged, h.item(t, 4).State)
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

	h.github.close(1)
	require.NoError(t, h.engine.TopUp(ctx))

	assert.Equal(t, state.ClosedUpstream, h.item(t, 1).State)
	assert.Equal(t, state.Queued, h.item(t, 2).State)
	assert.Equal(t, state.Queued, h.item(t, 3).State, "the skip must not cost a slot")
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
	assert.Equal(t, state.Triaged, it.State)
	require.NotNil(t, it.Result)
	assert.EqualValues(t, "close", it.Result.Recommendation)
	assert.EqualValues(t, "stale", it.Result.Reason)
	assert.Equal(t, 85, *it.Result.Confidence)
	assert.Equal(t, state.Pending, it.Human.State)
	assert.NotNil(t, it.TriagedAt)
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
	assert.Equal(t, state.Queued, it.State, "still queued: the agent gets another go")
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
	assert.Equal(t, state.NeedsHuman, it.State)
	assert.Equal(t, "closed", h.beads.get(beadID).Status, "a hopeless bead is left closed")
	assert.Contains(t, h.beads.get(beadID).Notes, "giving up")

	// The freed slot must go to the next item.
	require.NoError(t, h.engine.TopUp(ctx))
	assert.Equal(t, state.Queued, h.item(t, 2).State)
}

func TestExpireRequeuesStaleVerdicts(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 1, ghItem(1, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))
	h.beads.closeWith(h.item(t, 1).BeadID, validTemplate())
	require.NoError(t, h.engine.Reconcile(ctx))
	require.Equal(t, state.Triaged, h.item(t, 1).State)

	// Just short of the window: nothing happens.
	h.now = h.now.Add(179 * 24 * time.Hour)
	require.NoError(t, h.engine.Expire(ctx))
	assert.Equal(t, state.Triaged, h.item(t, 1).State)

	// Past it: back to the queue.
	h.now = h.now.Add(2 * 24 * time.Hour)
	require.NoError(t, h.engine.Expire(ctx))

	it := h.item(t, 1)
	assert.Equal(t, state.Untriaged, it.State)
	assert.Nil(t, it.Result, "a requeued item must not keep the expired verdict")
	assert.Empty(t, it.BeadID)
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
	require.NoError(t, state.Update(h.path, func(c *state.Config) error {
		c.Item(1).Requeue(note)
		return nil
	}))

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
	cfg := h.load(t)
	require.NotNil(t, cfg.Cursor.FetchedThrough)
	assert.Equal(t, cfg.Items[len(cfg.Items)-1].CreatedAt, *cfg.Cursor.FetchedThrough)

	before := len(cfg.Items)
	require.NoError(t, h.engine.Discover(ctx))
	assert.Len(t, h.load(t).Items, before, "re-running discovery must not duplicate items")
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

	h.github.getErr = assert.AnError
	require.NoError(t, h.engine.Tick(ctx))
	assert.Equal(t, 0, h.beads.openCount(), "no bead should be opened for an unverifiable item")
	assert.Equal(t, state.Untriaged, h.item(t, 1).State,
		"a failed liveness check must leave the item alone, not mark it closed_upstream")

	h.github.getErr = nil
	require.NoError(t, h.engine.Tick(ctx))
	assert.Equal(t, state.Queued, h.item(t, 1).State, "the next tick recovers")
}
