package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/triage"
)

// fakeBeads is an in-memory stand-in for the bd CLI. It models only what the
// engine relies on: labels, status, close reason, external ref and notes.
type fakeBeads struct {
	mu      sync.Mutex
	items   map[string]*beads.Bead
	order   []string
	counter int

	createErr error
	// createdBodies records what the engine asked agents to do, so tests can
	// assert on bead contents.
	createdBodies map[string]string
}

func newFakeBeads() *fakeBeads {
	return &fakeBeads{items: map[string]*beads.Bead{}, createdBodies: map[string]string{}}
}

func (f *fakeBeads) Create(_ context.Context, req beads.CreateRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.counter++
	id := fmt.Sprintf("tb-%03d", f.counter)
	f.items[id] = &beads.Bead{
		ID:          id,
		Title:       req.Title,
		Status:      "open",
		ExternalRef: req.ExternalRef,
		Labels:      []string{req.Label},
	}
	f.order = append(f.order, id)
	f.createdBodies[id] = req.Body
	return id, nil
}

// Query supports only the two expressions the engine issues.
func (f *fakeBeads) Query(_ context.Context, expr string) ([]beads.Bead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	wantClosed := strings.Contains(expr, "status=closed")
	var out []beads.Bead
	for _, id := range f.order {
		b := f.items[id]
		if b.Closed() == wantClosed {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (f *fakeBeads) Note(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return fmt.Errorf("no such bead %s", id)
	}
	b.Notes += text
	return nil
}

func (f *fakeBeads) Reopen(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[id]
	if !ok {
		return fmt.Errorf("no such bead %s", id)
	}
	b.Status = "open"
	// bd clears the close reason on reopen; the fake must too, or tests would
	// not exercise the same reconcile path as production.
	b.CloseReason = ""
	return nil
}

// closeWith closes a bead as an agent would, with a completion template.
func (f *fakeBeads) closeWith(id, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.items[id]; ok {
		b.Status = "closed"
		b.CloseReason = reason
	}
}

func (f *fakeBeads) get(id string) beads.Bead {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.items[id]
}

func (f *fakeBeads) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.items {
		if !b.Closed() {
			n++
		}
	}
	return n
}

// fakeGitHub serves a fixed set of items.
type fakeGitHub struct {
	mu     sync.Mutex
	items  map[int]*github.Item
	order  []int
	getErr error
}

func newFakeGitHub(items ...*github.Item) *fakeGitHub {
	f := &fakeGitHub{items: map[int]*github.Item{}}
	for _, it := range items {
		f.items[it.Number] = it
		f.order = append(f.order, it.Number)
	}
	return f
}

func (f *fakeGitHub) ListCreatedSince(_ context.Context, since *time.Time, limit int) ([]github.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []github.Item
	for _, n := range f.order {
		it := f.items[n]
		if !it.Open {
			continue
		}
		if since != nil && it.CreatedAt.Before(*since) {
			continue
		}
		out = append(out, *it)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeGitHub) Get(_ context.Context, number int) (*github.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	it, ok := f.items[number]
	if !ok {
		return nil, fmt.Errorf("no such item %d", number)
	}
	copied := *it
	return &copied, nil
}

func (f *fakeGitHub) close(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[number].Open = false
}

// ghItem builds an open issue created daysAgo days before base.
func ghItem(number int, daysAgo int) *github.Item {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	created := base.AddDate(0, 0, -daysAgo)
	return &github.Item{
		Number:    number,
		Kind:      triage.KindIssue,
		Title:     fmt.Sprintf("issue %d", number),
		CreatedAt: created,
		UpdatedAt: created,
		Author:    "someone",
		URL:       fmt.Sprintf("https://github.com/o/r/issues/%d", number),
		Open:      true,
	}
}

// validTemplate is a completion template that passes validation.
func validTemplate() string {
	return "Assessed.\n\n```yaml\nrecommendation: close\nreason: stale\nconfidence: 85\nreasoning: |\n  Dormant since 2021.\n```\n"
}
