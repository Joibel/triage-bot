package beads

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bd's query defaults to 50 results and truncates silently. Reconcile matches
// queued items against closed beads, so a dropped bead leaves its item queued
// forever, holding a work slot that never frees.
func TestQueryAsksForEveryResult(t *testing.T) {
	t.Parallel()

	args := queryArgs("label=triage-bot AND status=closed")
	assert.Equal(t,
		[]string{"query", "label=triage-bot AND status=closed", "--json", "--limit", "0"},
		args)
}

func TestDecodeBeads(t *testing.T) {
	t.Parallel()

	got, err := decodeBeads(`[{"id":"tb-1","status":"closed","close_reason":"x","external_ref":"gh-1"}]`)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.True(t, got[0].Closed())
	assert.Equal(t, "gh-1", got[0].ExternalRef)

	// bd emits nothing rather than "[]" for some commands.
	empty, err := decodeBeads("  \n")
	require.NoError(t, err)
	assert.Empty(t, empty)
}
