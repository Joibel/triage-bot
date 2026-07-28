package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/engine"
	"github.com/Joibel/triage-bot/internal/state"
)

func initCmd(o *opts) *cobra.Command {
	var org, repo string
	var maxOpen int

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a status file for a repository",
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, err := os.Stat(o.statusFile); err == nil {
				return fmt.Errorf("%s already exists", o.statusFile)
			}
			if err := state.Update(o.statusFile, func(c *state.Config) error {
				c.Org, c.Repo = org, repo
				if maxOpen > 0 {
					c.Settings.MaxOpenBeads = maxOpen
				}
				return nil
			}); err != nil {
				return fmt.Errorf("failed to create status file: %w", err)
			}
			fmt.Printf("Created %s for %s/%s\n", o.statusFile, org, repo)
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
	return &cobra.Command{
		Use:   "sync",
		Short: "Run one triage cycle",
		Long: "Reconciles finished beads into verdicts, discovers more backlog, expires\n" +
			"stale verdicts, and tops the work queue back up to the configured limit.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := o.newEngine()
			if err != nil {
				return err
			}
			return e.Tick(cmd.Context())
		},
	}
}

func daemonCmd(o *opts) *cobra.Command {
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run triage cycles on an interval until interrupted",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := o.newEngine()
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			log := o.logger()
			log.Info("starting daemon", "interval", interval, "file", o.statusFile)

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
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "Time between triage cycles")
	return cmd
}

// tick runs one cycle, logging rather than propagating failures: the next cycle
// re-derives everything from GitHub and the status file, so a transient error
// must not stop the daemon.
func tick(ctx context.Context, e *engine.Engine) {
	if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
		e.Log.Error("triage cycle failed, will retry next interval", "error", err)
	}
}
