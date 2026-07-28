package triage

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Result is the completion template an agent writes into a bead's close reason.
//
// Confidence and DuplicateOf are pointers so that "absent" is distinguishable
// from a legitimate zero: a missing confidence is a validation failure, whereas
// confidence 0 is merely a very unsure agent.
type Result struct {
	Recommendation   Recommendation `yaml:"recommendation"`
	Reason           Reason         `yaml:"reason"`
	Confidence       *int           `yaml:"confidence"`
	Reasoning        string         `yaml:"reasoning"`
	SuggestedComment string         `yaml:"suggested_comment,omitempty"`
	SuggestedLabels  []string       `yaml:"suggested_labels,omitempty"`
	Evidence         []string       `yaml:"evidence,omitempty"`
	DuplicateOf      *int           `yaml:"duplicate_of,omitempty"`
	FixedIn          string         `yaml:"fixed_in,omitempty"`
}

// ValidationError lists everything wrong with one completion template. All
// problems are collected rather than failing on the first, so an agent gets a
// single round of feedback instead of discovering faults one reopen at a time.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems: %s", len(e.Problems), strings.Join(e.Problems, "; "))
}

// Markdown renders the problems as a bullet list for a bd note, so the agent
// sees them when it picks the reopened bead back up.
func (e *ValidationError) Markdown() string {
	var b strings.Builder
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}

// ExtractYAML pulls the single fenced yaml block out of a bead close reason.
// Prose may surround the block freely; only the fenced content is parsed.
//
// Exactly one block must be present: zero means the agent did not follow the
// contract, and more than one is ambiguous about which is the verdict.
func ExtractYAML(closeReason string) (string, error) {
	var blocks []string
	var cur []string
	inBlock := false

	for line := range strings.SplitSeq(closeReason, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if info, ok := strings.CutPrefix(trimmed, "```"); ok {
				lang := strings.ToLower(strings.TrimSpace(info))
				if lang == "yaml" || lang == "yml" {
					inBlock = true
					cur = nil
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			blocks = append(blocks, strings.Join(cur, "\n"))
			inBlock = false
			continue
		}
		cur = append(cur, line)
	}

	if inBlock {
		return "", &ValidationError{Problems: []string{
			"the ```yaml block is not closed - add a closing ``` fence",
		}}
	}
	switch len(blocks) {
	case 1:
		return blocks[0], nil
	case 0:
		return "", &ValidationError{Problems: []string{
			"no ```yaml block found in the close reason - the completion template must be inside a fenced yaml block",
		}}
	default:
		return "", &ValidationError{Problems: []string{
			fmt.Sprintf("found %d ```yaml blocks - include exactly one, containing the completion template", len(blocks)),
		}}
	}
}

// Parse extracts, decodes and validates a completion template from a bead close
// reason. Any error it returns is a *ValidationError whose Markdown is suitable
// for feeding straight back to the agent.
func Parse(closeReason string, kind Kind) (*Result, error) {
	raw, err := ExtractYAML(closeReason)
	if err != nil {
		return nil, err
	}

	var r Result
	dec := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.KnownFields(true) // reject unrecognised keys rather than silently dropping them
	if err := dec.Decode(&r); err != nil {
		return nil, &ValidationError{Problems: []string{
			fmt.Sprintf("the yaml block did not parse: %v", err),
		}}
	}

	if err := r.Validate(kind); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks a decoded template against the pair table for the given item
// kind, collecting every problem it finds.
func (r *Result) Validate(kind Kind) error {
	var problems []string

	spec, recKnown := recSpec(r.Recommendation)
	switch {
	case r.Recommendation == "":
		problems = append(problems, fmt.Sprintf(
			"`recommendation` is missing - must be one of: %s", joinRecs(Recommendations())))
	case !recKnown:
		problems = append(problems, fmt.Sprintf(
			"`recommendation: %s` is not valid - must be one of: %s",
			r.Recommendation, joinRecs(Recommendations())))
	}

	switch {
	case r.Reason != "":
		problems = append(problems, r.validateReason(kind, spec, recKnown)...)
	case recKnown:
		problems = append(problems, fmt.Sprintf(
			"`reason` is missing - with `recommendation: %s` on %s it must be one of: %s",
			r.Recommendation, article(kind), joinReasons(ReasonsFor(r.Recommendation, kind))))
	default:
		problems = append(problems, "`reason` is missing")
	}

	problems = append(problems, r.validateFields()...)

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// validateFields checks the fields whose rules do not depend on the pair table:
// confidence, reasoning, and the two conditionally-required references.
func (r *Result) validateFields() []string {
	var problems []string

	switch {
	case r.Confidence == nil:
		problems = append(problems, "`confidence` is missing - required, an integer from 0 to 100")
	case *r.Confidence < 0 || *r.Confidence > 100:
		problems = append(problems, fmt.Sprintf(
			"`confidence: %d` is out of range - must be an integer from 0 to 100", *r.Confidence))
	}

	if strings.TrimSpace(r.Reasoning) == "" {
		problems = append(problems, "`reasoning` is missing or empty - explain how you reached this conclusion")
	}

	// Conditionally required, so that these reasons are actionable rather than
	// bare assertions.
	switch r.Reason {
	case Duplicate:
		if r.DuplicateOf == nil {
			problems = append(problems, "`duplicate_of` is required with `reason: duplicate` - give the issue or PR number this duplicates")
		}
	case AlreadyFixed:
		if strings.TrimSpace(r.FixedIn) == "" {
			problems = append(problems, "`fixed_in` is required with `reason: already_fixed` - give the version or PR that fixed it")
		}
	default:
		// Every other reason stands on its reasoning alone.
	}

	return problems
}

// validateReason checks the reason exists, belongs to the stated
// recommendation, and is allowed for this item kind.
func (r *Result) validateReason(kind Kind, spec RecommendationSpec, recKnown bool) []string {
	owner, reasonSpec, known := lookup(r.Reason)
	if !known {
		msg := fmt.Sprintf("`reason: %s` is not a recognised reason", r.Reason)
		if recKnown {
			msg += fmt.Sprintf(" - with `recommendation: %s` on %s it must be one of: %s",
				spec.Recommendation, article(kind), joinReasons(ReasonsFor(spec.Recommendation, kind)))
		}
		return []string{msg}
	}

	var problems []string
	if recKnown && owner != r.Recommendation {
		// The most common agent mistake: a real reason paired with the wrong
		// recommendation. Name the recommendation it does belong to.
		problems = append(problems, fmt.Sprintf(
			"`reason: %s` is not valid with `recommendation: %s` - it belongs to `recommendation: %s`. Valid reasons here are: %s",
			r.Reason, r.Recommendation, owner, joinReasons(ReasonsFor(r.Recommendation, kind))))
	}
	if !reasonSpec.Accepts(kind) {
		problems = append(problems, fmt.Sprintf(
			"`reason: %s` cannot be used on %s - it applies to %s only",
			r.Reason, article(kind), joinKinds(reasonSpec.Kinds)))
	}
	return problems
}

func article(k Kind) string {
	if k == KindPR {
		return "a pull request"
	}
	return "an issue"
}

// plural names a kind as agents and humans read it, rather than as it is
// spelled in the schema ("prs" reads badly in generated prose).
func plural(k Kind) string {
	if k == KindPR {
		return "pull requests"
	}
	return "issues"
}

func joinKinds(ks []Kind) string {
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, plural(k))
	}
	return strings.Join(out, ", ")
}

func joinReasons(rs []Reason) string {
	if len(rs) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return strings.Join(out, ", ")
}

func joinRecs(rs []Recommendation) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, string(r))
	}
	return strings.Join(out, ", ")
}
