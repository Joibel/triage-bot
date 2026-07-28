package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/triage"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "triage-bot.yaml")
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	created := time.Date(2022, 3, 14, 10, 22, 11, 0, time.UTC)
	in := &Config{
		Version: SchemaVersion,
		Org:     "argoproj",
		Repo:    "argo-workflows",
		Items: []*Item{{
			Number:    8123,
			Kind:      triage.KindIssue,
			Title:     "Controller deadlocks",
			CreatedAt: created,
			UpdatedAt: created,
			State:     Triaged,
			BeadID:    "tb-a3f8e9",
			TriagedAt: &created,
			Result: &triage.Result{
				Recommendation: triage.Close,
				Reason:         triage.Stale,
				Confidence:     new(85),
				Reasoning:      "dormant",
			},
			Human: Human{State: Pending},
		}},
	}
	in.applyDefaults()

	require.NoError(t, Save(path, in))

	out, err := Load(path)
	require.NoError(t, err)
	require.Len(t, out.Items, 1)
	assert.Equal(t, "argoproj", out.Org)
	assert.Equal(t, 8123, out.Items[0].Number)
	assert.Equal(t, triage.Close, out.Items[0].Result.Recommendation)
	assert.Equal(t, 85, *out.Items[0].Result.Confidence)
	assert.Equal(t, Pending, out.Items[0].Human.State)
	assert.Equal(t, in.Settings.RetriageAfter, out.Settings.RetriageAfter)
}

// The duration must be readable in the file, not raw nanoseconds, because the
// status file is meant to be hand-editable.
func TestDurationIsHumanReadable(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	c := &Config{}
	c.applyDefaults()
	require.NoError(t, Save(path, c))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "retriage_after: 4320h0m0s")
	assert.NotContains(t, string(data), "15552000000000000")
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	require.NoError(t, os.WriteFile(path, []byte("org: argoproj\nrepo: argo-workflows\n"), 0600))

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, DefaultSettings().MaxOpenBeads, c.Settings.MaxOpenBeads)
	assert.Equal(t, DefaultSettings().BeadLabel, c.Settings.BeadLabel)
	assert.Equal(t, SchemaVersion, c.Version)
}

// A partially-specified settings block keeps the values it names and defaults
// only the rest.
func TestLoadDefaultsOnlyUnsetFields(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	require.NoError(t, os.WriteFile(path, []byte(
		"config:\n  max_open_beads: 3\n  retriage_after: 24h\n"), 0600))

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 3, c.Settings.MaxOpenBeads)
	assert.Equal(t, 24*time.Hour, c.Settings.RetriageAfter.Std())
	assert.Equal(t, DefaultSettings().MaxTemplateAttempts, c.Settings.MaxTemplateAttempts)
}

// A file from a newer build must be refused rather than silently rewritten in
// a format that would lose its unknown fields.
func TestLoadRejectsNewerSchema(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	require.NoError(t, os.WriteFile(path, []byte("version: 99\n"), 0600))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema version 99")
}

func TestUpdateCreatesMissingFile(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	require.NoError(t, Update(path, func(c *Config) error {
		c.Org = "argoproj"
		return nil
	}))

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "argoproj", c.Org)
	assert.Equal(t, DefaultSettings().MaxOpenBeads, c.Settings.MaxOpenBeads)
}

// A mutate error must abort the write, leaving the previous file intact.
func TestUpdateRollsBackOnError(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	require.NoError(t, Update(path, func(c *Config) error {
		c.Org = "original"
		return nil
	}))

	err := Update(path, func(c *Config) error {
		c.Org = "clobbered"
		return assert.AnError
	})
	require.Error(t, err)

	c, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "original", c.Org)
}

// Concurrent writers must serialize: the flock plus reload-inside-the-lock is
// what stops the daemon and a CLI command clobbering each other.
func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	require.NoError(t, Update(path, func(*Config) error { return nil }))

	const writers = 20
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			assert.NoError(t, Update(path, func(c *Config) error {
				c.Items = append(c.Items, &Item{Number: i + 1, State: Untriaged})
				return nil
			}))
		}()
	}
	wg.Wait()

	c, err := Load(path)
	require.NoError(t, err)
	assert.Len(t, c.Items, writers, "every increment should survive; a lost update means the lock failed")
}

// Items are stored sorted by number so the file diffs cleanly between runs.
func TestSaveSortsItems(t *testing.T) {
	t.Parallel()

	path := tmpPath(t)
	c := &Config{Items: []*Item{{Number: 30}, {Number: 10}, {Number: 20}}}
	c.applyDefaults()
	require.NoError(t, Save(path, c))

	out, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []int{10, 20, 30}, []int{out.Items[0].Number, out.Items[1].Number, out.Items[2].Number})
}
