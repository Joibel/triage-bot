// Package engine drives one triage cycle: reconcile finished beads, discover
// more backlog, expire stale verdicts, and top the work queue back up.
//
// Named engine rather than sync so files here can still use stdlib sync.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/state"
)

// candidateBuffer is how many untriaged items the engine keeps on hand per WIP
// slot. Holding a few spare means a run of items closed upstream does not empty
// the queue and stall a tick.
const candidateBuffer = 3

// Engine performs triage cycles against one status file.
type Engine struct {
	Path   string
	GitHub github.Client
	Beads  beads.Client
	Log    *slog.Logger

	// Now is injectable so tests can control age-based expiry.
	Now func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// Tick runs one full cycle. Each phase is independent: a failure in one is
// logged and the rest still run, because the next tick re-derives everything
// from GitHub and the status file.
func (e *Engine) Tick(ctx context.Context) error {
	phases := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"reconcile", e.Reconcile},
		{"discover", e.Discover},
		{"expire", e.Expire},
		{"topup", e.TopUp},
	}

	var firstErr error
	for _, p := range phases {
		if err := p.fn(ctx); err != nil {
			e.Log.Error("phase failed", "phase", p.name, "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p.name, err)
			}
		}
	}
	return firstErr
}

// load reads the status file, treating absence as an empty config so a first
// run does not need the file to exist. Reads are lock-free; Save's atomic
// rename means a reader never sees a torn file.
func (e *Engine) load() (*state.Config, error) {
	cfg, err := state.Load(e.Path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to load status file: %w", err)
		}
		cfg = &state.Config{Settings: state.DefaultSettings()}
	}
	return cfg, nil
}

// update applies a mutation to the status file under the writer lock.
func (e *Engine) update(mutate func(*state.Config) error) error {
	if err := state.Update(e.Path, mutate); err != nil {
		return fmt.Errorf("failed to update status file: %w", err)
	}
	return nil
}
