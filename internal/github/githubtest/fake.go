// Package githubtest provides an in-memory github.Client for tests.
package githubtest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Joibel/triage-bot/internal/github"
)

// Fake serves a fixed set of items, advisories and fork pull requests. Its
// fields may be set directly before use; the methods lock.
type Fake struct {
	mu    sync.Mutex
	items map[int]*github.Item
	order []int

	// GetErr, when set, fails every Get.
	GetErr error
	// Advisories is what ListAdvisories returns, and what GetAdvisory finds.
	Advisories []github.Advisory
	// AdvisoryErr, when set, fails ListAdvisories and GetAdvisory.
	AdvisoryErr error
	// Pulls holds each fork's pull requests, by "owner/name".
	Pulls map[string][]github.Pull
	// PullErr fails ListPulls and GetPull for a fork, by "owner/name".
	PullErr map[string]error

	// Calls counts calls by method name, so a test can check what was fetched.
	Calls map[string]int
}

var _ github.Client = (*Fake)(nil)

// New builds a fake serving these items.
func New(items ...*github.Item) *Fake {
	f := &Fake{
		items:   map[int]*github.Item{},
		Pulls:   map[string][]github.Pull{},
		PullErr: map[string]error{},
		Calls:   map[string]int{},
	}
	for _, it := range items {
		f.items[it.Number] = it
		f.order = append(f.order, it.Number)
	}
	return f
}

// ListUpdatedSince serves open items least-recently-updated first, and treats
// since as inclusive, because both are what the real list endpoint does and
// both are what the cursor's behaviour at a timestamp tie depends on.
func (f *Fake) ListUpdatedSince(_ context.Context, since *time.Time, yield func(github.Item) bool) error {
	f.mu.Lock()
	f.Calls["ListUpdatedSince"]++
	var out []github.Item
	for _, n := range f.order {
		it := f.items[n]
		if !it.Open {
			continue
		}
		if since != nil && it.UpdatedAt.Before(*since) {
			continue
		}
		out = append(out, *it)
	}
	f.mu.Unlock()

	slices.SortStableFunc(out, func(a, b github.Item) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	for _, it := range out {
		if !yield(it) {
			return nil
		}
	}
	return nil
}

// Get returns one item, whatever its state.
func (f *Fake) Get(_ context.Context, number int) (*github.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["Get"]++
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	it, ok := f.items[number]
	if !ok {
		return nil, fmt.Errorf("no such item %d", number)
	}
	copied := *it
	return &copied, nil
}

// Close marks an item closed, as a maintainer would on GitHub.
func (f *Fake) Close(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[number].Open = false
	f.items[number].State = "closed"
}

// ListAdvisories returns every advisory, whatever its state.
func (f *Fake) ListAdvisories(context.Context) ([]github.Advisory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["ListAdvisories"]++
	if f.AdvisoryErr != nil {
		return nil, f.AdvisoryErr
	}
	return slices.Clone(f.Advisories), nil
}

// GetAdvisory returns one advisory by GHSA id.
func (f *Fake) GetAdvisory(_ context.Context, ghsa string) (*github.Advisory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["GetAdvisory"]++
	if f.AdvisoryErr != nil {
		return nil, f.AdvisoryErr
	}
	for _, a := range f.Advisories {
		if a.GHSA == ghsa {
			return &a, nil
		}
	}
	return nil, fmt.Errorf("%w: no such advisory %s", github.ErrNoAccess, ghsa)
}

// ListPulls returns a fork's pull requests.
func (f *Fake) ListPulls(_ context.Context, repo string) ([]github.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["ListPulls"]++
	if err := f.PullErr[repo]; err != nil {
		return nil, err
	}
	return slices.Clone(f.Pulls[repo]), nil
}

// GetPull returns one of a fork's pull requests.
func (f *Fake) GetPull(_ context.Context, repo string, number int) (*github.Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls["GetPull"]++
	if err := f.PullErr[repo]; err != nil {
		return nil, err
	}
	for _, p := range f.Pulls[repo] {
		if p.Number == number {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("%w: no such pull request %s#%d", github.ErrNoAccess, repo, number)
}

// SetAdvisories replaces the advisories served, as GitHub's would change
// between syncs.
func (f *Fake) SetAdvisories(advisories ...github.Advisory) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Advisories = advisories
}
