// Package triage defines the completion-template contract between triage-bot
// and the AI agent that actions a bead.
//
// The pair table below is the single source of truth. It drives validation,
// the wording of validation errors, and the instruction text embedded in every
// bead, so a new reason cannot be added without agents being told about it.
package triage

import "slices"

// Kind distinguishes the two sorts of GitHub item triage-bot assesses. Some
// reasons only make sense for one of them.
type Kind string

const (
	// KindIssue is a GitHub issue.
	KindIssue Kind = "issue"
	// KindPR is a GitHub pull request.
	KindPR Kind = "pr"
)

// Recommendation is the action a human should take on the GitHub item. It is
// never something triage-bot performs itself; the bot only ever reads GitHub.
//
// The four values partition by who acts next, which is what the report groups
// on: nobody (KeepOpen), a maintainer (Respond), the reporter or PR author
// (RequestInfo), or a closer (Close).
type Recommendation string

const (
	// KeepOpen means nobody need act: the item is fine as it stands.
	KeepOpen Recommendation = "keep_open"
	// Respond means we should add information; the item stays with maintainers.
	Respond Recommendation = "respond"
	// RequestInfo means we should ask a question; the ball moves to the
	// reporter or PR author.
	RequestInfo Recommendation = "request_info"
	// Close means the item should be closed.
	Close Recommendation = "close"
)

// Reason qualifies a Recommendation. Every reason is legal for exactly one
// recommendation, so a reason on its own identifies the pair.
type Reason string

// Reasons valid with KeepOpen.
const (
	// StillValid means the item is relevant and correctly filed.
	StillValid Reason = "still_valid"
	// ActiveDiscussion means a useful conversation is already under way.
	ActiveDiscussion Reason = "active_discussion"
)

// Reasons valid with Respond.
const (
	// AddContext means post an explanation or an answer.
	AddContext Reason = "add_context"
	// GoodFirstIssue means it is well-scoped for a newcomer. Issues only.
	GoodFirstIssue Reason = "good_first_issue"
	// WorkaroundExists means a workaround should be documented on the item.
	WorkaroundExists Reason = "workaround_exists"
)

// Reasons valid with RequestInfo.
const (
	// NeedsReproduction means it cannot be assessed without a repro. Issues only.
	NeedsReproduction Reason = "needs_reproduction"
	// NeedsDetail means it is too vague to act on.
	NeedsDetail Reason = "needs_detail"
	// NeedsVersionInfo means the affected versions are unclear.
	NeedsVersionInfo Reason = "needs_version_info"
	// StillWanted means ask the author if they still intend to land it. PRs only.
	StillWanted Reason = "still_wanted"
)

// Reasons valid with Close.
const (
	// Stale means long dormant and no longer worth tracking.
	Stale Reason = "stale"
	// AlreadyFixed means it is already fixed or implemented. Requires FixedIn.
	AlreadyFixed Reason = "already_fixed"
	// Duplicate means it duplicates or is superseded by another item. Requires
	// DuplicateOf.
	Duplicate Reason = "duplicate"
	// OutOfScope means it is outside what the project intends to do.
	OutOfScope Reason = "out_of_scope"
	// NotABug means the behaviour is intended.
	NotABug Reason = "not_a_bug"
	// Obsolete means it refers to code or behaviour that no longer exists.
	Obsolete Reason = "obsolete"
)

// ReasonSpec describes one legal reason: which kinds accept it, and the gloss
// shown to agents in the generated instructions.
type ReasonSpec struct {
	Reason Reason
	// Kinds restricts the reason to particular item kinds. Empty means both.
	Kinds []Kind
	Desc  string
}

// Accepts reports whether this reason may be used for the given item kind.
func (s ReasonSpec) Accepts(k Kind) bool {
	return len(s.Kinds) == 0 || slices.Contains(s.Kinds, k)
}

// RecommendationSpec is one row of the pair table.
type RecommendationSpec struct {
	Recommendation Recommendation
	Desc           string
	Reasons        []ReasonSpec
}

// Table is the pair table. It is a slice rather than a map so that generated
// instructions and error messages have a stable, deliberate order.
//
//nolint:gochecknoglobals // read-only data: this table is the contract itself
var Table = []RecommendationSpec{
	{
		Recommendation: KeepOpen,
		Desc:           "No action needed; the item is fine as it stands.",
		Reasons: []ReasonSpec{
			{Reason: StillValid, Desc: "Still relevant and correctly filed."},
			{Reason: ActiveDiscussion, Desc: "A useful conversation is already in progress."},
		},
	},
	{
		Recommendation: Respond,
		Desc:           "We should add information. The item stays with the maintainers.",
		Reasons: []ReasonSpec{
			{Reason: AddContext, Desc: "Post context, an explanation, or an answer."},
			{Reason: GoodFirstIssue, Kinds: []Kind{KindIssue}, Desc: "Well-scoped for a newcomer; explain the approach and label it."},
			{Reason: WorkaroundExists, Desc: "A workaround exists and should be documented on the item."},
		},
	},
	{
		Recommendation: RequestInfo,
		Desc:           "We should ask a question. The ball moves to the reporter or author.",
		Reasons: []ReasonSpec{
			{Reason: NeedsReproduction, Kinds: []Kind{KindIssue}, Desc: "Cannot be assessed without a reproduction."},
			{Reason: NeedsDetail, Desc: "Too vague to act on; specifics are needed."},
			{Reason: NeedsVersionInfo, Desc: "Unclear which versions are affected."},
			{Reason: StillWanted, Kinds: []Kind{KindPR}, Desc: "Ask the author whether they still intend to land this."},
		},
	},
	{
		Recommendation: Close,
		Desc:           "Recommend closing the item.",
		Reasons: []ReasonSpec{
			{Reason: Stale, Desc: "Long dormant and no longer worth tracking."},
			{Reason: AlreadyFixed, Desc: "Already fixed or implemented. Requires fixed_in."},
			{Reason: Duplicate, Desc: "Duplicated or superseded by another item. Requires duplicate_of."},
			{Reason: OutOfScope, Desc: "Outside what this project intends to do."},
			{Reason: NotABug, Desc: "Working as intended."},
			{Reason: Obsolete, Desc: "Refers to code or behaviour that no longer exists."},
		},
	},
}

// lookup finds the pair-table entries for a reason. Returns the owning
// recommendation and the spec; ok is false if the reason is not in the table.
func lookup(r Reason) (Recommendation, ReasonSpec, bool) {
	for _, rec := range Table {
		for _, spec := range rec.Reasons {
			if spec.Reason == r {
				return rec.Recommendation, spec, true
			}
		}
	}
	return "", ReasonSpec{}, false
}

// recSpec finds the row for a recommendation.
func recSpec(r Recommendation) (RecommendationSpec, bool) {
	for _, rec := range Table {
		if rec.Recommendation == r {
			return rec, true
		}
	}
	return RecommendationSpec{}, false
}

// ReasonsFor lists the reasons legal for a recommendation and item kind.
func ReasonsFor(r Recommendation, k Kind) []Reason {
	spec, ok := recSpec(r)
	if !ok {
		return nil
	}
	var out []Reason
	for _, rs := range spec.Reasons {
		if rs.Accepts(k) {
			out = append(out, rs.Reason)
		}
	}
	return out
}

// Recommendations lists every recommendation, in table order.
func Recommendations() []Recommendation {
	out := make([]Recommendation, 0, len(Table))
	for _, rec := range Table {
		out = append(out, rec.Recommendation)
	}
	return out
}

// Reasons lists every reason across all recommendations, in table order.
func Reasons() []Reason {
	var out []Reason
	for _, rec := range Table {
		for _, spec := range rec.Reasons {
			out = append(out, spec.Reason)
		}
	}
	return out
}

// RecommendationFor reports which recommendation a reason belongs to. Since
// every reason is legal for exactly one recommendation, the reason identifies
// the pair; ok is false if the reason is not in the table.
func RecommendationFor(r Reason) (Recommendation, bool) {
	rec, _, ok := lookup(r)
	return rec, ok
}
