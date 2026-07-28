package triage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every bead must tell the agent what to judge against. Without it agents
// reason about the version in the report rather than the current state of the
// code, and reach keep_open on things main fixed years ago.
func TestInstructionsStateTheAssessmentTarget(t *testing.T) {
	t.Parallel()

	for _, kind := range []Kind{KindIssue, KindPR} {
		text := Instructions(kind)
		assert.Contains(t, text, "origin/main",
			"%s instructions must name the assessment target", kind)
		assert.Contains(t, text, "closable",
			"%s instructions must say that a good main makes the item closable", kind)

		// The guidance is useless after the agent has been told how to report;
		// it must come first.
		assert.Less(t, strings.Index(text, "How to assess"), strings.Index(text, "How to complete"),
			"assessment guidance must precede the reporting contract")
	}
}
