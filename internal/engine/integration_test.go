package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/store"
)

// These tests drive a real bd database. Everything the engine assumes about bd
// - that close_reason survives in `bd query --json`, that reopen clears it, that
// notes are visible to the agent - is a claim about an external binary, and the
// fakes elsewhere in this package would happily keep agreeing with a wrong
// assumption. GitHub stays faked; only the bd half is real.

// realBeads sets up a throwaway beads database, skipping if bd is unavailable.
func realBeads(t *testing.T) (*beads.CLI, string) {
	t.Helper()

	if testing.Short() {
		t.Skip("integration test: drives a real beads database, which is slow")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}

	dir := t.TempDir()
	// bd wants a git repo to sit in.
	run(t, dir, "git", "init", "-q", ".")
	run(t, dir, "git", "config", "user.email", "test@example.com")
	run(t, dir, "git", "config", "user.name", "test")
	run(t, dir, "bd", "init", "--prefix", "tb")

	return &beads.CLI{Dir: dir}, dir
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s %s: %s", name, strings.Join(args, " "), out)
	return string(out)
}

// realHarness wires the engine to a real bd and a fake GitHub. It returns the
// harness, whose beads field is unused, and bd's working directory.
func realHarness(t *testing.T, maxOpen int, items ...*github.Item) (*harness, string) {
	t.Helper()

	bd, dir := realBeads(t)
	h := newHarness(t, maxOpen, items...)
	h.engine.Beads = bd
	h.beads = nil
	return h, dir
}

// The happy path against a real bd: a bead is opened, an agent closes it with a
// completion template, and the verdict lands in the store.
func TestIntegrationValidTemplateRoundTrip(t *testing.T) {
	t.Parallel()

	h, dir := realHarness(t, 2, ghItem(8123, 100), ghItem(8124, 90))
	ctx := context.Background()

	require.NoError(t, h.engine.Tick(ctx))
	require.Equal(t, 2, h.count(t, store.Queued))

	item := h.item(t, 8123)
	require.NotEmpty(t, item.BeadID)

	// The bead must carry the pointer and the contract the agent needs.
	show := run(t, dir, "bd", "show", item.BeadID)
	assert.Contains(t, show, "8123")

	// Close it the way an agent would.
	closeBead(t, dir, item.BeadID, "Assessed.\n\n```yaml\n"+
		"recommendation: close\nreason: already_fixed\nfixed_in: v3.6.0\n"+
		"confidence: 92\nreasoning: |\n  Fixed by #7001.\n"+
		"suggested_comment: |\n  Closing - fixed in v3.6.0.\n```\n")

	require.NoError(t, h.engine.Reconcile(ctx))

	got := h.item(t, 8123)
	assert.Equal(t, store.Triaged, got.State)
	require.NotNil(t, got.Verdict)
	assert.EqualValues(t, "close", got.Verdict.Result.Recommendation)
	assert.EqualValues(t, "already_fixed", got.Verdict.Result.Reason)
	assert.Equal(t, "v3.6.0", got.Verdict.Result.FixedIn)
	assert.Equal(t, 92, *got.Verdict.Result.Confidence)
	assert.Contains(t, got.Verdict.Result.SuggestedComment, "Closing")
	assert.Equal(t, store.Pending, got.Verdict.Disposition.State)
}

// The correction loop against a real bd. This is the assumption most worth
// checking: bd clears close_reason on reopen, so the validation errors have to
// reach the agent through notes instead.
func TestIntegrationInvalidTemplateReopensWithVisibleErrors(t *testing.T) {
	t.Parallel()

	h, dir := realHarness(t, 1, ghItem(8123, 100))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))

	beadID := h.item(t, 8123).BeadID

	// `still_valid` is a real reason, but it belongs to keep_open.
	closeBead(t, dir, beadID, "```yaml\nrecommendation: close\nreason: still_valid\nconfidence: 50\nreasoning: x\n```")

	require.NoError(t, h.engine.Reconcile(ctx))

	item := h.item(t, 8123)
	assert.Equal(t, store.Queued, item.State)
	assert.Equal(t, 1, item.Attempts)

	// The agent must be able to see what was wrong from a plain `bd show`.
	show := run(t, dir, "bd", "show", beadID)
	assert.Contains(t, show, "OPEN", "bead must have been reopened")
	assert.Contains(t, show, "still_valid")
	assert.Contains(t, show, "keep_open", "the note must name the recommendation it belongs to")

	// Now the agent gets it right.
	closeBead(t, dir, beadID, "```yaml\nrecommendation: keep_open\nreason: still_valid\nconfidence: 70\nreasoning: fine\n```")
	require.NoError(t, h.engine.Reconcile(ctx))

	got := h.item(t, 8123)
	assert.Equal(t, store.Triaged, got.State)
	assert.EqualValues(t, "keep_open", got.Verdict.Result.Recommendation)
}

// After the attempt limit the item is handed to a human and the bead is left
// closed, freeing the slot for the next item.
func TestIntegrationGivesUpAndFreesSlot(t *testing.T) {
	t.Parallel()

	h, dir := realHarness(t, 1, ghItem(8123, 100), ghItem(8124, 90))
	ctx := context.Background()
	require.NoError(t, h.engine.Tick(ctx))

	beadID := h.item(t, 8123).BeadID

	for range 3 {
		closeBead(t, dir, beadID, "```yaml\nrecommendation: close\nreason: nonsense\nconfidence: 50\nreasoning: x\n```")
		require.NoError(t, h.engine.Reconcile(ctx))
	}

	assert.Equal(t, store.NeedsHuman, h.item(t, 8123).State)

	require.NoError(t, h.engine.TopUp(ctx))
	assert.Equal(t, store.Queued, h.item(t, 8124).State, "the freed slot must go to the next item")
}

// closeBead closes a bead with the given reason, as an agent would.
func closeBead(t *testing.T, dir, id, reason string) {
	t.Helper()
	cmd := exec.Command("bd", "close", id, "--reason-file", "-")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(reason)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bd close: %s", out)
}
