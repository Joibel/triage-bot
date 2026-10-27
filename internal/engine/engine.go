// Package engine drives one triage cycle: reconcile finished beads, discover
// more backlog, expire stale verdicts, and top the work queue back up.
//
// Named engine rather than sync so files here can still use stdlib sync.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/store"
)

// candidateBuffer is how many untriaged items the engine keeps on hand per WIP
// slot. Holding a few spare means a run of items closed upstream does not empty
// the queue and stall a tick.
const candidateBuffer = 3

// Engine performs triage cycles against one store.
//
// No phase holds a transaction while it talks to GitHub or bd: each reads
// what it needs, does its slow work, then writes in a short transaction that
// re-checks the rows it is about to change. The CLI and the MCP server share
// the database and must never wait on the network.
type Engine struct {
	Store  *store.Store
	GitHub github.Client
	Beads  beads.Client
	Log    *slog.Logger

	// Now is injectable so tests can control age-based expiry.
	Now func() time.Time

	// advisoriesSkipped records that the advisory sync was refused, so the
	// warning is logged once rather than every tick.
	advisoriesSkipped atomic.Bool
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// phase is one named step of a cycle.
type phase struct {
	name string
	fn   func(context.Context) error
}

func (e *Engine) phases() []phase {
	return []phase{
		{"reconcile", e.Reconcile},
		{"discover", e.Discover},
		{"expire", e.Expire},
		{"topup", e.TopUp},
		{"advisories", e.SyncAdvisories},
	}
}

// PhaseNames lists the phases of a cycle, in the order they run.
func (e *Engine) PhaseNames() []string {
	phases := e.phases()
	out := make([]string, 0, len(phases))
	for _, p := range phases {
		out = append(out, p.name)
	}
	return out
}

// Tick runs one full cycle. Each phase is independent: a failure in one is
// logged and the rest still run, because the next tick re-derives everything
// from GitHub and the store. Advisories sync last, so triage never waits on
// them.
func (e *Engine) Tick(ctx context.Context) error {
	return e.Run(ctx, e.PhaseNames()...)
}

// Run runs the named phases, in cycle order, as Tick runs them all.
func (e *Engine) Run(ctx context.Context, names ...string) error {
	var firstErr error
	for _, p := range e.phases() {
		if !slices.Contains(names, p.name) {
			continue
		}
		if err := p.fn(ctx); err != nil {
			e.Log.Error("phase failed", "phase", p.name, "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p.name, err)
			}
		}
	}
	return firstErr
}

// settings reads the operator's settings.
func (e *Engine) settings(ctx context.Context) (store.Settings, error) {
	var s store.Settings
	err := e.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		s, err = tx.Settings(ctx)
		return err
	})
	if err != nil {
		return store.Settings{}, fmt.Errorf("failed to read settings: %w", err)
	}
	return s, nil
}

// update applies a change to the store in one write transaction.
func (e *Engine) update(ctx context.Context, fn func(*store.Tx) error) error {
	if err := e.Store.Update(ctx, fn); err != nil {
		return fmt.Errorf("failed to update the store: %w", err)
	}
	return nil
}
