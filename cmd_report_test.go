package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The suggested comment is the one part of the report meant to be selected and
// pasted into GitHub. Agents write it as a YAML block scalar, so their line
// wrapping survives verbatim - and GitHub renders a single newline in a comment
// as a real line break, so a wrapped paragraph pastes as ragged short lines.
func TestUnwrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "wrapped paragraph becomes one line",
			in:   "Closing as this refers to the legacy controller\nremoved in v3.5. Please open a fresh issue if you\nstill see this.",
			want: "Closing as this refers to the legacy controller removed in v3.5. Please open a fresh issue if you still see this.",
		},
		{
			name: "blank line separates paragraphs",
			in:   "First paragraph\nwrapped here.\n\nSecond paragraph\nalso wrapped.",
			want: "First paragraph wrapped here.\n\nSecond paragraph also wrapped.",
		},
		{
			name: "single line is untouched",
			in:   "Already one line.",
			want: "Already one line.",
		},
		{
			name: "bullet list keeps its breaks",
			in:   "Please provide:\n- the workflow YAML\n- the controller version",
			want: "Please provide:\n- the workflow YAML\n- the controller version",
		},
		{
			name: "ordered list keeps its breaks",
			in:   "Steps:\n1. run the workflow\n2. observe the deadlock",
			want: "Steps:\n1. run the workflow\n2. observe the deadlock",
		},
		{
			name: "fenced code is preserved verbatim",
			in:   "Try this:\n\n```yaml\nspec:\n  ttlStrategy:\n    secondsAfterCompletion: 60\n```\n\nThat should\nfix it.",
			want: "Try this:\n\n```yaml\nspec:\n  ttlStrategy:\n    secondsAfterCompletion: 60\n```\n\nThat should fix it.",
		},
		{
			name: "indented code is preserved",
			in:   "Run:\n\n    argo submit wf.yaml\n\nand check\nthe output.",
			want: "Run:\n\n    argo submit wf.yaml\n\nand check the output.",
		},
		{
			name: "headings keep their breaks",
			in:   "## Why\nBecause the code\nno longer exists.",
			want: "## Why\nBecause the code no longer exists.",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, unwrap(tt.in))
		})
	}
}

// Whatever else it does, the output must never carry the report's own
// decoration: a pasted comment beginning "  > " is worse than no suggestion.
func TestUnwrapAddsNoDecoration(t *testing.T) {
	t.Parallel()

	got := unwrap("Closing as stale.\nPlease reopen if needed.")
	assert.NotContains(t, got, ">")
	for line := range strings.SplitSeq(got, "\n") {
		assert.Equal(t, line, strings.TrimSpace(line), "no line may carry padding")
	}
}
