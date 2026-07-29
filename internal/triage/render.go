package triage

import (
	"fmt"
	"strings"
)

// Instructions renders the completion-template contract for one item kind, for
// embedding in a bead body.
//
// It is generated from Table rather than written by hand so that the agent is
// always told about exactly the reasons the validator will accept. TestTableCoverage
// asserts every enum value reaches this text.
func Instructions(kind Kind) string {
	var b strings.Builder

	b.WriteString(assessmentGuidance)

	b.WriteString("## How to complete this bead\n\n")
	b.WriteString("Assess the item, then close this bead with your verdict in a fenced `yaml`\n")
	b.WriteString("block. Prose around the block is fine; only the block is parsed.\n\n")
	b.WriteString("```\nbd close <this-bead> --reason-file -\n```\n\n")
	b.WriteString("If your `suggested_comment` contains a fenced code block, wrap the whole\n")
	b.WriteString("template in ````yaml (four backticks) so the inner fence cannot end it.\n\n")

	b.WriteString("### Required fields\n\n")
	b.WriteString("- `recommendation` - what a human should do (see below)\n")
	b.WriteString("- `reason` - must be legal for that recommendation\n")
	b.WriteString("- `confidence` - integer 0-100, how sure you are\n")
	b.WriteString("- `reasoning` - how you reached the conclusion\n\n")

	b.WriteString("### Optional fields\n\n")
	b.WriteString("- `suggested_comment` - draft text a maintainer can paste onto the item\n")
	b.WriteString("- `suggested_labels` - labels to add\n")
	b.WriteString("- `evidence` - URLs supporting your conclusion\n")
	b.WriteString("- `duplicate_of` - **required** with `reason: duplicate`\n")
	b.WriteString("- `fixed_in` - **required** with `reason: already_fixed`\n\n")

	fmt.Fprintf(&b, "### Valid recommendation / reason pairs for %s\n\n", plural(kind))
	for _, rec := range Table {
		reasons := reasonSpecsFor(rec, kind)
		if len(reasons) == 0 {
			continue
		}
		fmt.Fprintf(&b, "`%s` - %s\n", rec.Recommendation, rec.Desc)
		for _, rs := range reasons {
			fmt.Fprintf(&b, "  - `%s` - %s\n", rs.Reason, rs.Desc)
		}
		b.WriteString("\n")
	}

	b.WriteString("### Example\n\n")
	b.WriteString("````\n")
	b.WriteString("```yaml\n")
	b.WriteString(example(kind))
	b.WriteString("```\n")
	b.WriteString("````\n\n")

	b.WriteString("Do not invent recommendation or reason values: anything outside the lists\n")
	b.WriteString("above is rejected and this bead will be reopened for you to correct.\n")

	return b.String()
}

// assessmentGuidance states what the agent judges against. It is the same for
// every item and both kinds, so it is a constant rather than generated.
//
// Without it agents reason about the version in the report ("this was broken in
// 3.4, so it is a real bug") and reach keep_open on things main fixed years ago
// — which is exactly the class of stale item this tool exists to clear.
const assessmentGuidance = `## How to assess

Judge the item against ` + "`origin/main`" + ` as it stands today, not the release the
reporter was running or the base the author branched from. If main is already
good the item is closable: ` + "`already_fixed`" + ` (give ` + "`fixed_in`" + `), or ` + "`obsolete`" + ` if the
code it concerned has gone.

Check main before concluding. "Broken in 3.4" is not a finding; "still broken
on main" is.

`

// reasonSpecsFor filters a row of the table to the reasons legal for a kind.
func reasonSpecsFor(rec RecommendationSpec, kind Kind) []ReasonSpec {
	var out []ReasonSpec
	for _, rs := range rec.Reasons {
		if rs.Accepts(kind) {
			out = append(out, rs)
		}
	}
	return out
}

func example(kind Kind) string {
	if kind == KindPR {
		return `recommendation: close
reason: already_fixed
confidence: 90
fixed_in: v3.6.0
reasoning: |
  The same change landed in #7001 in March 2023. This PR's diff is now a
  no-op against main.
suggested_comment: |
  Thanks for this - the same fix landed in #7001, so I'm closing this as
  already implemented.
evidence:
  - https://github.com/argoproj/argo-workflows/pull/7001
`
	}
	return `recommendation: close
reason: stale
confidence: 85
reasoning: |
  Last activity 2021. The reporter never answered the maintainer's question,
  and the affected controller was removed in v3.5.
suggested_comment: |
  Closing as this refers to the legacy controller removed in v3.5. Please
  open a fresh issue if you still see this on a supported version.
suggested_labels: [stale]
`
}
