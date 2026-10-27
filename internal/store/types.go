package store

import (
	"fmt"
	"time"

	"github.com/Joibel/triage-bot/internal/triage"
)

// KindAdvisory is a GitHub security advisory. It shares triage.Kind with
// issues and pull requests so an item has one kind field, but advisories never
// enter triage and so are not in triage.Kinds.
const KindAdvisory triage.Kind = "advisory"

// ItemState is where an item sits in the triage pipeline. It is independent of
// DispositionState, which tracks what the maintainer did with a finished
// verdict. The empty state means the item is not in the pipeline at all.
type ItemState string

const (
	// Untriaged means known to us and awaiting a triage bead.
	Untriaged ItemState = "untriaged"
	// Queued means a bead is open for it and we are waiting on the agent.
	Queued ItemState = "queued"
	// Triaged means a valid completion template was parsed into a verdict.
	Triaged ItemState = "triaged"
	// NeedsHuman means the agent could not produce a valid template within
	// MaxTemplateAttempts. Terminal until a human intervenes.
	NeedsHuman ItemState = "needs_human"
	// ClosedUpstream means someone closed it on GitHub before we triaged it.
	ClosedUpstream ItemState = "closed_upstream"
)

// ItemStates lists every pipeline state, in pipeline order.
func ItemStates() []ItemState {
	return []ItemState{Untriaged, Queued, Triaged, NeedsHuman, ClosedUpstream}
}

// DispositionState is the maintainer's decision on a verdict.
type DispositionState string

const (
	// Pending means the recommendation has not been acted on yet.
	Pending DispositionState = "pending"
	// Applied means the maintainer actioned it on GitHub.
	Applied DispositionState = "applied"
	// Rejected means the maintainer disagreed; the item is requeued.
	Rejected DispositionState = "rejected"
	// Deferred means left for later, without requeueing.
	Deferred DispositionState = "deferred"
)

// Author says who wrote a note, made a link or decided a disposition.
type Author string

const (
	// AuthorHuman is the maintainer, at the CLI or in review.
	AuthorHuman Author = "human"
	// AuthorClaude is a Claude Code session, through the MCP server.
	AuthorClaude Author = "claude"
	// AuthorBot is triage-bot itself, for links it makes while syncing.
	AuthorBot Author = "triage-bot"
)

// LinkType is the relation a link records. A link reads from its From item to
// its To item: "#9000 fixes #8123".
type LinkType string

const (
	// DuplicateOf means From duplicates To.
	DuplicateOf LinkType = "duplicate_of"
	// Fixes means From (a pull request) fixes To.
	Fixes LinkType = "fixes"
	// AdvisoryFor means From (an advisory) is about To.
	AdvisoryFor LinkType = "advisory_for"
	// FixInFork means From (an advisory) is fixed by To, a pull request in its
	// private fork.
	FixInFork LinkType = "fix_in_fork"
	// BackportOf means From backports To.
	BackportOf LinkType = "backport_of"
	// Related means the two are connected in some other way.
	Related LinkType = "related"
)

// LinkTypes lists every link type.
func LinkTypes() []LinkType {
	return []LinkType{DuplicateOf, Fixes, AdvisoryFor, FixInFork, BackportOf, Related}
}

// Inverse names a link as read from its To end: "#8123 fixed_by #9000".
func (t LinkType) Inverse() string {
	switch t {
	case DuplicateOf:
		return "duplicated_by"
	case Fixes:
		return "fixed_by"
	case AdvisoryFor:
		return "has_advisory"
	case FixInFork:
		return "fork_fix_for"
	case BackportOf:
		return "backported_as"
	default:
		return string(t)
	}
}

// Settings are the operator's knobs, kept in the database so the daemon and
// the CLI cannot disagree about them.
type Settings struct {
	// Org and Repo name the repository being triaged.
	Org  string
	Repo string
	// MaxOpenBeads bounds work in flight so the actioning system is not flooded.
	MaxOpenBeads int
	// RetriageAfter is how long a verdict stands before the item is reassessed.
	RetriageAfter time.Duration
	// MaxTemplateAttempts is how many invalid templates an item tolerates
	// before it goes to NeedsHuman and releases its WIP slot.
	MaxTemplateAttempts int
	// BeadLabel identifies the beads this bot owns.
	BeadLabel string
}

// DefaultSettings are used for any setting the database does not hold.
func DefaultSettings() Settings {
	return Settings{
		MaxOpenBeads:        10,
		RetriageAfter:       180 * 24 * time.Hour,
		MaxTemplateAttempts: 3,
		BeadLabel:           "triage-bot",
	}
}

// MainRepo is the triaged repository as "owner/name".
func (s Settings) MainRepo() string { return s.Org + "/" + s.Repo }

// Item is one GitHub issue, pull request or advisory we have touched.
type Item struct {
	ID int64
	// Repo is "owner/name": the main repository, or an advisory's private fork.
	Repo string
	Kind triage.Kind
	// Number is set for issues and pull requests, GHSA for advisories.
	Number int
	GHSA   string

	Title string
	// GHState is GitHub's own state: open, closed or merged, or an advisory's
	// triage, draft, published, closed or withdrawn. Empty when unknown.
	GHState   string
	CreatedAt time.Time
	UpdatedAt time.Time
	URL       string
	// Sensitive items never leave the store in bulk. See the package notes on
	// sensitivity in links.go.
	Sensitive bool

	// State is the triage pipeline state, empty outside the pipeline.
	State  ItemState
	BeadID string
	// Attempts counts invalid completion templates for the current bead.
	Attempts  int
	LastError string
	QueuedAt  *time.Time
	// CarryNote is put in front of the next agent: why the last verdict was
	// rejected, or what retriage was asked to consider.
	CarryNote string

	// Verdict is the current verdict, present only while State is Triaged.
	Verdict *Verdict
}

// Ref renders the item's reference as a user would type it: "#123" in the main
// repository, "owner/fork#2" in a private fork, or the GHSA id.
func (it *Item) Ref(mainRepo string) string {
	return Ref{Repo: it.Repo, Number: it.Number, GHSA: it.GHSA}.Display(mainRepo)
}

// HTMLURL is the item's page on GitHub. Items imported from the status file
// carry no URL, but GitHub redirects /issues/N to /pull/N, so one derived form
// covers both kinds.
func (it *Item) HTMLURL() string {
	switch {
	case it.URL != "":
		return it.URL
	case it.Kind == KindAdvisory:
		return fmt.Sprintf("https://github.com/%s/security/advisories/%s", it.Repo, it.GHSA)
	default:
		return fmt.Sprintf("https://github.com/%s/issues/%d", it.Repo, it.Number)
	}
}

// Verdict is one parsed completion template.
type Verdict struct {
	ID int64
	// BeadID is the bead the template came from, which is what lets reparse
	// re-read it.
	BeadID    string
	TriagedAt *time.Time
	Result    triage.Result
	// Disposition is the maintainer's latest decision on it.
	Disposition Disposition
}

// Disposition is the maintainer's decision on a verdict.
type Disposition struct {
	State  DispositionState
	Note   string
	At     *time.Time
	Author Author
}

// Note is a timestamped remark on an item.
type Note struct {
	ID        int64
	ItemID    int64
	Body      string
	Tags      []string
	Author    Author
	CreatedAt time.Time
}

// Link is an edge between two items, as seen from one of them.
type Link struct {
	ID   int64
	Type LinkType
	// Outgoing is true when the item it was read from is the From end.
	Outgoing bool
	// Other is the item at the far end.
	Other     Item
	Author    Author
	CreatedAt time.Time
}

// Label names the link from the reading item's side.
func (l Link) Label() string {
	if l.Outgoing {
		return string(l.Type)
	}
	return l.Type.Inverse()
}

// Advisory is a security advisory's item plus what only advisories have.
type Advisory struct {
	Item
	Severity    string
	CVE         string
	PrivateFork string
}

// AdvisoryStates are the states an advisory is synced in. Closed and
// withdrawn advisories are not fetched, but one already in the store keeps
// its notes and links when it closes.
func AdvisoryStates() []string {
	return []string{"triage", "draft", "published"}
}
