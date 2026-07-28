// Package state owns the triage-bot status file: the source of truth for what
// has been triaged, what each verdict was, and what the human did about it.
//
// Beads are disposable work tickets; this file is not. Everything durable lives
// here, so a lost beads database costs at most the in-flight work.
package state

import (
	"slices"
	"time"

	"github.com/Joibel/triage-bot/internal/triage"
)

// SchemaVersion is the current status-file layout. Load rejects anything newer
// so an older binary cannot silently mangle a file it does not understand.
const SchemaVersion = 1

// ItemState is where an item sits in the triage pipeline. It is independent of
// HumanState, which tracks what the human did with a finished verdict.
type ItemState string

const (
	// Untriaged means known to us and awaiting a triage bead.
	Untriaged ItemState = "untriaged"
	// Queued means a bead is open for it and we are waiting on the agent.
	Queued ItemState = "queued"
	// Triaged means a valid completion template was parsed into Result.
	Triaged ItemState = "triaged"
	// NeedsHuman means the agent could not produce a valid template within
	// MaxTemplateAttempts. Terminal until a human intervenes.
	NeedsHuman ItemState = "needs_human"
	// ClosedUpstream means someone closed it on GitHub before we triaged it.
	ClosedUpstream ItemState = "closed_upstream"
)

// HumanState tracks the maintainer's disposition of a finished recommendation.
// Only meaningful once ItemState is Triaged.
type HumanState string

const (
	// Pending means the recommendation has not been acted on yet.
	Pending HumanState = "pending"
	// Applied means the maintainer actioned it on GitHub.
	Applied HumanState = "applied"
	// Rejected means the maintainer disagreed; the item is requeued.
	Rejected HumanState = "rejected"
	// Deferred means left for later, without requeueing.
	Deferred HumanState = "deferred"
)

// Config holds the whole status file.
type Config struct {
	Version  int      `yaml:"version"`
	Org      string   `yaml:"org"`
	Repo     string   `yaml:"repo"`
	Settings Settings `yaml:"config"`
	Cursor   Cursor   `yaml:"cursor"`
	Items    []*Item  `yaml:"items,omitempty"`
}

// Settings are the operator's knobs, kept in the status file so the daemon and
// the CLI cannot disagree about them.
type Settings struct {
	// MaxOpenBeads bounds work in flight so the actioning system is not flooded.
	MaxOpenBeads int `yaml:"max_open_beads"`
	// RetriageAfter is how long a verdict stands before the item is reassessed.
	RetriageAfter Duration `yaml:"retriage_after"`
	// MaxTemplateAttempts is how many invalid templates an item tolerates
	// before it goes to NeedsHuman and releases its WIP slot.
	MaxTemplateAttempts int `yaml:"max_template_attempts"`
	// BeadLabel identifies the beads this bot owns.
	BeadLabel string `yaml:"bead_label"`
}

// DefaultSettings are applied to any field left zero on load.
func DefaultSettings() Settings {
	return Settings{
		MaxOpenBeads:        10,
		RetriageAfter:       Duration(180 * 24 * time.Hour),
		MaxTemplateAttempts: 3,
		BeadLabel:           "triage-bot",
	}
}

// Cursor records how far through the backlog we have discovered items. We walk
// oldest-created first and only ever fetch what is needed to keep the queue
// supplied, so this is a high-water mark rather than a full census.
type Cursor struct {
	// FetchedThrough is the created_at of the newest item we have ingested.
	// Everything created at or before this is already in Items.
	FetchedThrough *time.Time `yaml:"fetched_through,omitempty"`
}

// Item is one tracked GitHub issue or pull request.
type Item struct {
	Number    int         `yaml:"number"`
	Kind      triage.Kind `yaml:"kind"`
	Title     string      `yaml:"title"`
	CreatedAt time.Time   `yaml:"created_at"`
	UpdatedAt time.Time   `yaml:"updated_at"`

	State  ItemState `yaml:"state"`
	BeadID string    `yaml:"bead_id,omitempty"`

	// Attempts counts invalid completion templates for the current bead.
	Attempts  int        `yaml:"attempts,omitempty"`
	LastError string     `yaml:"last_error,omitempty"`
	QueuedAt  *time.Time `yaml:"queued_at,omitempty"`
	TriagedAt *time.Time `yaml:"triaged_at,omitempty"`

	// Result is present once State is Triaged.
	Result *triage.Result `yaml:"result,omitempty"`
	Human  Human          `yaml:"human,omitempty"`
}

// Human is the maintainer's disposition of a finished recommendation.
type Human struct {
	State HumanState `yaml:"state,omitempty"`
	Note  string     `yaml:"note,omitempty"`
	At    *time.Time `yaml:"at,omitempty"`
}

// Item returns the tracked item with this number, or nil.
func (c *Config) Item(number int) *Item {
	for _, it := range c.Items {
		if it.Number == number {
			return it
		}
	}
	return nil
}

// ItemByBead returns the item currently attached to a bead, or nil.
func (c *Config) ItemByBead(beadID string) *Item {
	if beadID == "" {
		return nil
	}
	for _, it := range c.Items {
		if it.BeadID == beadID {
			return it
		}
	}
	return nil
}

// Upsert inserts an item or refreshes the mutable GitHub-sourced fields of an
// existing one. It never touches triage state: GitHub owns the metadata, we own
// the verdict.
func (c *Config) Upsert(in *Item) *Item {
	if existing := c.Item(in.Number); existing != nil {
		existing.Title = in.Title
		existing.UpdatedAt = in.UpdatedAt
		return existing
	}
	c.Items = append(c.Items, in)
	c.sortItems()
	return in
}

// sortItems keeps Items ordered by number so the file diffs cleanly.
func (c *Config) sortItems() {
	slices.SortFunc(c.Items, func(a, b *Item) int { return a.Number - b.Number })
}

// Count returns how many items are in a given state.
func (c *Config) Count(s ItemState) int {
	n := 0
	for _, it := range c.Items {
		if it.State == s {
			n++
		}
	}
	return n
}

// NextUntriaged returns untriaged items, oldest created first: the order the
// backlog is worked in.
func (c *Config) NextUntriaged() []*Item {
	var out []*Item
	for _, it := range c.Items {
		if it.State == Untriaged {
			out = append(out, it)
		}
	}
	slices.SortFunc(out, func(a, b *Item) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// ExpiredTriage returns triaged items whose verdict is older than
// RetriageAfter, measured from now.
func (c *Config) ExpiredTriage(now time.Time) []*Item {
	var out []*Item
	for _, it := range c.Items {
		if it.State != Triaged || it.TriagedAt == nil {
			continue
		}
		if now.Sub(*it.TriagedAt) >= c.Settings.RetriageAfter.Std() {
			out = append(out, it)
		}
	}
	return out
}

// Requeue returns an item to the untriaged pool, clearing the per-bead attempt
// state. carryNote is embedded in the next bead so the agent does not repeat a
// rejected conclusion; it is empty for age-based expiry.
func (it *Item) Requeue(carryNote string) {
	it.State = Untriaged
	it.BeadID = ""
	it.Attempts = 0
	it.LastError = ""
	it.QueuedAt = nil
	it.TriagedAt = nil
	it.Result = nil
	it.Human = Human{Note: carryNote}
}
