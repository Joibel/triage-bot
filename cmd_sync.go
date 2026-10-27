package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/engine"
	"github.com/Joibel/triage-bot/internal/store"
)

func initCmd(o *opts) *cobra.Command {
	var org, repo string
	var maxOpen int

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the database for a repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.db == "" {
				return fmt.Errorf("no database path: pass --db")
			}
			if _, err := os.Stat(o.db); err == nil {
				return fmt.Errorf("%s already exists", o.db)
			}
			st, err := store.Open(cmd.Context(), o.db)
			if err != nil {
				return fmt.Errorf("failed to create the database: %w", err)
			}
			defer func() { _ = st.Close() }()

			if err := st.Update(cmd.Context(), func(tx *store.Tx) error {
				s := store.DefaultSettings()
				s.Org, s.Repo = org, repo
				if maxOpen > 0 {
					s.MaxOpenBeads = maxOpen
				}
				return tx.SaveSettings(cmd.Context(), s)
			}); err != nil {
				return fmt.Errorf("failed to save settings: %w", err)
			}
			fmt.Printf("Created %s for %s/%s\n", o.db, org, repo)
			return nil
		},
	}

	cmd.Flags().StringVar(&org, "org", "", "GitHub organisation or user (required)")
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub repository (required)")
	cmd.Flags().IntVar(&maxOpen, "max-open-beads", 0, "Triage beads to keep in flight (default 10)")
	_ = cmd.MarkFlagRequired("org")
	_ = cmd.MarkFlagRequired("repo")
	return cmd
}

func syncCmd(o *opts) *cobra.Command {
	var only []string

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Run one triage cycle",
		Long: "Reconciles finished beads into verdicts, discovers more backlog, expires\n" +
			"stale verdicts, tops the work queue back up to the configured limit, and\n" +
			"syncs the repository's open security advisories.\n\n" +
			"--only runs just the named phases: reconcile, discover, expire, topup,\n" +
			"advisories. `sync --only advisories` touches neither beads nor the backlog.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.withStore(cmd.Context(), func(st *store.Store) error {
				e, err := o.newEngine(cmd.Context(), st)
				if err != nil {
					return err
				}
				if len(only) == 0 {
					return e.Tick(cmd.Context())
				}
				for _, name := range only {
					if !slices.Contains(e.PhaseNames(), name) {
						return fmt.Errorf("no such phase %q: choose from %s", name, strings.Join(e.PhaseNames(), ", "))
					}
				}
				return e.Run(cmd.Context(), only...)
			})
		},
	}

	cmd.Flags().StringSliceVar(&only, "only", nil, "Run only these phases (comma-separated)")
	return cmd
}

func daemonCmd(o *opts) *cobra.Command {
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run triage cycles on an interval until interrupted",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return o.withStore(ctx, func(st *store.Store) error {
				e, err := o.newEngine(ctx, st)
				if err != nil {
					return err
				}

				log := o.logger()
				log.Info("starting daemon", "interval", interval, "db", o.db)

				// Tick immediately so a restart does not idle for a whole interval.
				tick(ctx, e)

				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						log.Info("shutting down")
						return nil
					case <-ticker.C:
						tick(ctx, e)
					}
				}
			})
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "Time between triage cycles")
	return cmd
}

// tick runs one cycle, logging rather than propagating failures: the next cycle
// re-derives everything from GitHub and the store, so a transient error must
// not stop the daemon.
func tick(ctx context.Context, e *engine.Engine) {
	if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
		e.Log.Error("triage cycle failed, will retry next interval", "error", err)
	}
}
