package engine

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/state"
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
// engine, the status file path and the working directory.
func realHarness(t *testing.T, maxOpen int, items ...*github.Item) (*Engine, string, string) {
	t.Helper()

	bd, dir := realBeads(t)
	path := filepath.Join(dir, "triage-bot.yaml")
	fg := newFakeGitHub(items...)

	e := &Engine{
		Path:   path,
		GitHub: fg,
		Beads:  bd,
		Log:    slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}

	require.NoError(t, state.Update(path, func(c *state.Config) error {
		c.Org, c.Repo = "argoproj", "argo-workflows"
		c.Settings.MaxOpenBeads = maxOpen
		return nil
	}))
	return e, path, dir
}

// The happy path against a real bd: a bead is opened, an agent closes it with a
// completion template, and the verdict lands in the status file.
func TestIntegrationValidTemplateRoundTrip(t *testing.T) {
	t.Parallel()

	e, path, dir := realHarness(t, 2, ghItem(8123, 100), ghItem(8124, 90))
	ctx := context.Background()

	require.NoError(t, e.Tick(ctx))

	cfg, err := state.Load(path)
	require.NoError(t, err)
	require.Equal(t, 2, cfg.Count(state.Queued))

	item := cfg.Item(8123)
	require.NotEmpty(t, item.BeadID)

	// The bead must carry the pointer and the contract the agent needs.
	show := run(t, dir, "bd", "show", item.BeadID)
	assert.Contains(t, show, "8123")

	// Close it the way an agent would.
	closeBead(t, dir, item.BeadID, "Assessed.\n\n```yaml\n"+
		"recommendation: close\nreason: already_fixed\nfixed_in: v3.6.0\n"+
		"confidence: 92\nreasoning: |\n  Fixed by #7001.\n"+
		"suggested_comment: |\n  Closing - fixed in v3.6.0.\n```\n")

	require.NoError(t, e.Reconcile(ctx))

	cfg, err = state.Load(path)
	require.NoError(t, err)
	got := cfg.Item(8123)
	assert.Equal(t, state.Triaged, got.State)
	require.NotNil(t, got.Result)
	assert.EqualValues(t, "close", got.Result.Recommendation)
	assert.EqualValues(t, "already_fixed", got.Result.Reason)
	assert.Equal(t, "v3.6.0", got.Result.FixedIn)
	assert.Equal(t, 92, *got.Result.Confidence)
	assert.Contains(t, got.Result.SuggestedComment, "Closing")
	assert.Equal(t, state.Pending, got.Human.State)
}

// The correction loop against a real bd. This is the assumption most worth
// checking: bd clears close_reason on reopen, so the validation errors have to
// reach the agent through notes instead.
func TestIntegrationInvalidTemplateReopensWithVisibleErrors(t *testing.T) {
	t.Parallel()

	e, path, dir := realHarness(t, 1, ghItem(8123, 100))
	ctx := context.Background()
	require.NoError(t, e.Tick(ctx))

	cfg, err := state.Load(path)
	require.NoError(t, err)
	beadID := cfg.Item(8123).BeadID

	// `still_valid` is a real reason, but it belongs to keep_open.
	closeBead(t, dir, beadID, "```yaml\nrecommendation: close\nreason: still_valid\nconfidence: 50\nreasoning: x\n```")

	require.NoError(t, e.Reconcile(ctx))

	cfg, err = state.Load(path)
	require.NoError(t, err)
	item := cfg.Item(8123)
	assert.Equal(t, state.Queued, item.State)
	assert.Equal(t, 1, item.Attempts)

	// The agent must be able to see what was wrong from a plain `bd show`.
	show := run(t, dir, "bd", "show", beadID)
	assert.Contains(t, show, "OPEN", "bead must have been reopened")
	assert.Contains(t, show, "still_valid")
	assert.Contains(t, show, "keep_open", "the note must name the recommendation it belongs to")

	// Now the agent gets it right.
	closeBead(t, dir, beadID, "```yaml\nrecommendation: keep_open\nreason: still_valid\nconfidence: 70\nreasoning: fine\n```")
	require.NoError(t, e.Reconcile(ctx))

	cfg, err = state.Load(path)
	require.NoError(t, err)
	assert.Equal(t, state.Triaged, cfg.Item(8123).State)
	assert.EqualValues(t, "keep_open", cfg.Item(8123).Result.Recommendation)
}

// After the attempt limit the item is handed to a human and the bead is left
// closed, freeing the slot for the next item.
func TestIntegrationGivesUpAndFreesSlot(t *testing.T) {
	t.Parallel()

	e, path, dir := realHarness(t, 1, ghItem(8123, 100), ghItem(8124, 90))
	ctx := context.Background()
	require.NoError(t, e.Tick(ctx))

	cfg, err := state.Load(path)
	require.NoError(t, err)
	beadID := cfg.Item(8123).BeadID

	for range 3 {
		closeBead(t, dir, beadID, "```yaml\nrecommendation: close\nreason: nonsense\nconfidence: 50\nreasoning: x\n```")
		require.NoError(t, e.Reconcile(ctx))
	}

	cfg, err = state.Load(path)
	require.NoError(t, err)
	assert.Equal(t, state.NeedsHuman, cfg.Item(8123).State)

	require.NoError(t, e.TopUp(ctx))
	cfg, err = state.Load(path)
	require.NoError(t, err)
	assert.Equal(t, state.Queued, cfg.Item(8124).State, "the freed slot must go to the next item")
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
