package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/state"
)

func statusCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show backlog size, work in flight, and triage coverage",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := state.Load(o.statusFile)
			if err != nil {
				return fmt.Errorf("failed to read status file: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			defer func() { _ = w.Flush() }()

			fmt.Fprintf(w, "Repository:\t%s/%s\n", cfg.Org, cfg.Repo)
			fmt.Fprintf(w, "Status file:\t%s\n", o.statusFile)
			if cfg.Cursor.FetchedThrough != nil {
				fmt.Fprintf(w, "Discovered through:\t%s\n", cfg.Cursor.FetchedThrough.Format("2006-01-02"))
			}
			fmt.Fprintf(w, "Tracked items:\t%d\n", len(cfg.Items))
			fmt.Fprintln(w)

			fmt.Fprintf(w, "Untriaged:\t%d\n", cfg.Count(state.Untriaged))
			fmt.Fprintf(w, "In flight:\t%d\tof %d slots\n", cfg.Count(state.Queued), cfg.Settings.MaxOpenBeads)
			fmt.Fprintf(w, "Triaged:\t%d\n", cfg.Count(state.Triaged))
			fmt.Fprintf(w, "Needs a human:\t%d\n", cfg.Count(state.NeedsHuman))
			fmt.Fprintf(w, "Closed upstream:\t%d\n", cfg.Count(state.ClosedUpstream))

			pending := 0
			for _, it := range cfg.Items {
				if it.State == state.Triaged && it.Human.State == state.Pending {
					pending++
				}
			}
			fmt.Fprintln(w)
			fmt.Fprintf(w, "Awaiting your action:\t%d\t(triage-bot report)\n", pending)
			return nil
		},
	}
}
