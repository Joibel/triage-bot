package triage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fence wraps a yaml body in the fenced block agents are asked to produce.
func fence(body string) string {
	return "Some prose from the agent.\n\n```yaml\n" + body + "\n```\n"
}

func TestParseValid(t *testing.T) {
	t.Parallel()

	got, err := Parse(fence(`recommendation: close
reason: stale
confidence: 85
reasoning: |
  Dormant since 2021.
suggested_comment: |
  Closing as stale.
suggested_labels: [stale]
evidence:
  - https://example.com/1`), KindIssue)

	require.NoError(t, err)
	assert.Equal(t, Close, got.Recommendation)
	assert.Equal(t, Stale, got.Reason)
	require.NotNil(t, got.Confidence)
	assert.Equal(t, 85, *got.Confidence)
	assert.Equal(t, "Dormant since 2021.\n", got.Reasoning)
	assert.Equal(t, []string{"stale"}, got.SuggestedLabels)
	assert.Equal(t, []string{"https://example.com/1"}, got.Evidence)
}

// Every pair the table declares legal must actually validate, for the kinds
// that accept it. This is the positive half of the contract.
func TestEveryLegalPairValidates(t *testing.T) {
	t.Parallel()

	for _, rec := range Table {
		for _, rs := range rec.Reasons {
			for _, kind := range []Kind{KindIssue, KindPR} {
				if !rs.Accepts(kind) {
					continue
				}
				t.Run(fmt.Sprintf("%s/%s/%s", kind, rec.Recommendation, rs.Reason), func(t *testing.T) {
					t.Parallel()
					r := &Result{
						Recommendation: rec.Recommendation,
						Reason:         rs.Reason,
						Confidence:     new(50),
						Reasoning:      "because",
					}
					// Satisfy the conditionally-required fields.
					switch rs.Reason {
					case Duplicate:
						r.DuplicateOf = new(1234)
					case AlreadyFixed:
						r.FixedIn = "v3.6.0"
					default:
						// No conditionally-required fields.
					}
					assert.NoError(t, r.Validate(kind))
				})
			}
		}
	}
}

func TestValidateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind Kind
		body string
		// wantProblem is a substring the error must contain, so failures point
		// the agent at the offending field.
		wantProblem string
	}{
		{
			name:        "reason belongs to another recommendation",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: still_valid\nconfidence: 50\nreasoning: x",
			wantProblem: "it belongs to `recommendation: keep_open`",
		},
		{
			name:        "unknown reason",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: wontfix\nconfidence: 50\nreasoning: x",
			wantProblem: "`reason: wontfix` is not a recognised reason",
		},
		{
			name:        "unknown recommendation",
			kind:        KindIssue,
			body:        "recommendation: ignore\nreason: stale\nconfidence: 50\nreasoning: x",
			wantProblem: "`recommendation: ignore` is not valid",
		},
		{
			name:        "issue-only reason used on a PR",
			kind:        KindPR,
			body:        "recommendation: request_info\nreason: needs_reproduction\nconfidence: 50\nreasoning: x",
			wantProblem: "cannot be used on a pull request",
		},
		{
			name:        "PR-only reason used on an issue",
			kind:        KindIssue,
			body:        "recommendation: request_info\nreason: still_wanted\nconfidence: 50\nreasoning: x",
			wantProblem: "cannot be used on an issue",
		},
		{
			name:        "good_first_issue on a PR",
			kind:        KindPR,
			body:        "recommendation: respond\nreason: good_first_issue\nconfidence: 50\nreasoning: x",
			wantProblem: "cannot be used on a pull request",
		},
		{
			name:        "confidence missing",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: stale\nreasoning: x",
			wantProblem: "`confidence` is missing",
		},
		{
			name:        "confidence above range",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: stale\nconfidence: 101\nreasoning: x",
			wantProblem: "out of range",
		},
		{
			name:        "confidence below range",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: stale\nconfidence: -1\nreasoning: x",
			wantProblem: "out of range",
		},
		{
			name:        "reasoning empty",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: stale\nconfidence: 50\nreasoning: '   '",
			wantProblem: "`reasoning` is missing or empty",
		},
		{
			name:        "recommendation missing",
			kind:        KindIssue,
			body:        "reason: stale\nconfidence: 50\nreasoning: x",
			wantProblem: "`recommendation` is missing",
		},
		{
			name:        "reason missing",
			kind:        KindIssue,
			body:        "recommendation: close\nconfidence: 50\nreasoning: x",
			wantProblem: "`reason` is missing",
		},
		{
			name:        "duplicate without duplicate_of",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: duplicate\nconfidence: 50\nreasoning: x",
			wantProblem: "`duplicate_of` is required",
		},
		{
			name:        "already_fixed without fixed_in",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: already_fixed\nconfidence: 50\nreasoning: x",
			wantProblem: "`fixed_in` is required",
		},
		{
			name:        "unknown field",
			kind:        KindIssue,
			body:        "recommendation: close\nreason: stale\nconfidence: 50\nreasoning: x\nseverity: high",
			wantProblem: "did not parse",
		},
		{
			name:        "malformed yaml",
			kind:        KindIssue,
			body:        "recommendation: close\n  reason: : :",
			wantProblem: "did not parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(fence(tt.body), tt.kind)
			require.Error(t, err)

			var ve *ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Contains(t, ve.Error(), tt.wantProblem)
			assert.Contains(t, ve.Markdown(), "- ", "markdown should be a bullet list for the bd note")
		})
	}
}

// All problems are reported at once so the agent gets one round of feedback.
func TestValidateCollectsEveryProblem(t *testing.T) {
	t.Parallel()

	_, err := Parse(fence("recommendation: close\nreason: duplicate"), KindIssue)
	require.Error(t, err)

	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Len(t, ve.Problems, 3, "missing confidence, missing reasoning, missing duplicate_of")
}

func TestExtractYAML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          string
		want        string
		wantProblem string
	}{
		{
			name: "surrounded by prose",
			in:   "before\n\n```yaml\na: 1\n```\n\nafter",
			want: "a: 1",
		},
		{
			name: "yml info string",
			in:   "```yml\na: 1\n```",
			want: "a: 1",
		},
		{
			name: "indented fence",
			in:   "  ```yaml\n  a: 1\n  ```",
			want: "  a: 1",
		},
		{
			name:        "no block",
			in:          "I recommend closing this as stale.",
			wantProblem: "no ```yaml block found",
		},
		{
			name:        "non-yaml fence ignored",
			in:          "```\na: 1\n```",
			wantProblem: "no ```yaml block found",
		},
		{
			name:        "two blocks",
			in:          "```yaml\na: 1\n```\n```yaml\nb: 2\n```",
			wantProblem: "found 2 ```yaml blocks",
		},
		{
			name:        "unterminated",
			in:          "```yaml\na: 1",
			wantProblem: "is not closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExtractYAML(tt.in)
			if tt.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantProblem)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The generated instructions must mention every value the validator accepts,
// or agents will be judged against rules they were never told.
func TestTableCoverage(t *testing.T) {
	t.Parallel()

	for _, kind := range []Kind{KindIssue, KindPR} {
		text := Instructions(kind)
		for _, rec := range Table {
			assert.Contains(t, text, string(rec.Recommendation),
				"recommendation %s missing from %s instructions", rec.Recommendation, kind)
			for _, rs := range rec.Reasons {
				if !rs.Accepts(kind) {
					assert.NotContains(t, text, "`"+string(rs.Reason)+"`",
						"reason %s is not valid for %s but appears in its instructions", rs.Reason, kind)
					continue
				}
				assert.Contains(t, text, string(rs.Reason),
					"reason %s missing from %s instructions", rs.Reason, kind)
			}
		}
	}
}

// The worked example in the instructions must itself pass validation.
func TestInstructionsExampleIsValid(t *testing.T) {
	t.Parallel()

	for _, kind := range []Kind{KindIssue, KindPR} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			_, err := Parse("```yaml\n"+example(kind)+"```", kind)
			assert.NoError(t, err)
		})
	}
}

// Every reason belongs to exactly one recommendation, which is what lets a
// reason on its own identify the pair.
func TestReasonsAreUnique(t *testing.T) {
	t.Parallel()

	seen := map[Reason]Recommendation{}
	for _, rec := range Table {
		for _, rs := range rec.Reasons {
			if prev, dup := seen[rs.Reason]; dup {
				t.Errorf("reason %s appears under both %s and %s", rs.Reason, prev, rec.Recommendation)
			}
			seen[rs.Reason] = rec.Recommendation
		}
	}
}

// A close reason with no fenced block at all is the commonest agent failure;
// the message must tell them what to do rather than just what is wrong.
func TestMissingBlockMessageIsActionable(t *testing.T) {
	t.Parallel()

	_, err := Parse("I think this should be closed.", KindIssue)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "fenced yaml block")
}

// A suggested_comment routinely contains a fenced example. Treating the first
// inner ``` as the end of the template truncated the verdict silently: the rest
// of the comment, suggested_labels and evidence were all dropped, and what
// remained still parsed and validated, so nothing flagged it.
func TestExtractYAMLKeepsNestedFencedBlocks(t *testing.T) {
	t.Parallel()

	got, err := Parse("Assessed against origin/main.\n\n"+
		"```yaml\n"+
		"recommendation: close\n"+
		"reason: already_fixed\n"+
		"fixed_in: v4.1.0\n"+
		"confidence: 88\n"+
		"reasoning: |\n"+
		"  Implemented on main.\n"+
		"suggested_comment: |\n"+
		"  This is now implemented, via a `pendingTimeout` field:\n"+
		"\n"+
		"  ```yaml\n"+
		"  - name: my-step\n"+
		"    pendingTimeout: 5m\n"+
		"  ```\n"+
		"\n"+
		"  Closing as resolved.\n"+
		"suggested_labels: [solution/implemented]\n"+
		"evidence:\n"+
		"  - https://example.com/pr\n"+
		"```\n", KindIssue)

	require.NoError(t, err)
	assert.Contains(t, got.SuggestedComment, "pendingTimeout: 5m", "the fenced example must survive")
	assert.Contains(t, got.SuggestedComment, "Closing as resolved.", "the comment must not stop at the inner fence")
	assert.Equal(t, []string{"solution/implemented"}, got.SuggestedLabels, "fields after the inner fence must survive")
	assert.Equal(t, []string{"https://example.com/pr"}, got.Evidence)
}

// A four-backtick outer fence is the markdown-correct way to embed a bare ```
// block, and must be accepted.
func TestExtractYAMLAcceptsLongerOuterFence(t *testing.T) {
	t.Parallel()

	got, err := Parse("````yaml\n"+
		"recommendation: close\nreason: stale\nconfidence: 50\nreasoning: x\n"+
		"suggested_comment: |\n  Run:\n\n  ```\n  argo submit wf.yaml\n  ```\n"+
		"````\n", KindIssue)

	require.NoError(t, err)
	assert.Contains(t, got.SuggestedComment, "argo submit wf.yaml")
}

// A bare ``` fence inside a value is indistinguishable from the end of the
// template. That must be refused with an actionable message rather than
// silently dropping everything after it.
func TestExtractYAMLRefusesSilentTruncation(t *testing.T) {
	t.Parallel()

	_, err := Parse("```yaml\n"+
		"recommendation: close\nreason: stale\nconfidence: 50\nreasoning: x\n"+
		"suggested_comment: |\n  Run:\n\n  ```\n  argo submit wf.yaml\n  ```\n"+
		"suggested_labels: [stale]\n"+
		"```\n", KindIssue)

	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Contains(t, ve.Error(), "ended before the template did")
	assert.Contains(t, ve.Error(), "````yaml", "the message must say how to fix it")
}

// Prose after a correctly-closed block is not mistaken for truncation.
func TestExtractYAMLAllowsTrailingProse(t *testing.T) {
	t.Parallel()

	_, err := Parse("```yaml\nrecommendation: close\nreason: stale\nconfidence: 50\nreasoning: x\n```\n\n"+
		"Happy to reconsider: just say so.\n", KindIssue)
	assert.NoError(t, err)
}
