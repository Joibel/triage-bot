package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/store"
)

func statusCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show backlog size, work in flight, and triage coverage",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			return o.withStore(ctx, func(st *store.Store) error {
				s, err := settings(ctx, st)
				if err != nil {
					return err
				}
				sum, err := readStatus(ctx, st, s)
				if err != nil {
					return err
				}
				printStatus(st.Path, s, sum)
				return nil
			})
		},
	}
}

// status is everything `status` reports.
type status struct {
	counts     map[store.ItemState]int
	cursor     *time.Time
	pending    int
	advisories int
	notes      int
}

func readStatus(ctx context.Context, st *store.Store, s store.Settings) (status, error) {
	var out status
	err := st.View(ctx, func(tx *store.Tx) error {
		var err error
		if out.counts, err = tx.Counts(ctx); err != nil {
			return err
		}
		if out.cursor, err = tx.Cursor(ctx); err != nil {
			return err
		}
		pending, err := tx.Triaged(ctx, s.MainRepo(), false)
		if err != nil {
			return err
		}
		advisories, err := tx.Advisories(ctx, false)
		if err != nil {
			return err
		}
		out.pending, out.advisories = len(pending), len(advisories)
		out.notes, err = tx.NoteCount(ctx)
		return err
	})
	if err != nil {
		return status{}, fmt.Errorf("failed to read the database: %w", err)
	}
	return out, nil
}

func printStatus(path string, s store.Settings, sum status) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	tracked := 0
	for _, n := range sum.counts {
		tracked += n
	}

	fmt.Fprintf(w, "Repository:\t%s\n", s.MainRepo())
	fmt.Fprintf(w, "Database:\t%s\n", path)
	if sum.cursor != nil {
		fmt.Fprintf(w, "Discovered through:\t%s\t(by last activity)\n", sum.cursor.Format("2006-01-02"))
	}
	fmt.Fprintf(w, "Tracked items:\t%d\n", tracked)
	fmt.Fprintln(w)

	fmt.Fprintf(w, "Untriaged:\t%d\n", sum.counts[store.Untriaged])
	fmt.Fprintf(w, "In flight:\t%d\tof %d slots\n", sum.counts[store.Queued], s.MaxOpenBeads)
	fmt.Fprintf(w, "Triaged:\t%d\n", sum.counts[store.Triaged])
	fmt.Fprintf(w, "Needs a human:\t%d\n", sum.counts[store.NeedsHuman])
	fmt.Fprintf(w, "Closed upstream:\t%d\n", sum.counts[store.ClosedUpstream])

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Awaiting your action:\t%d\t(triage-bot report)\n", sum.pending)
	fmt.Fprintf(w, "Open advisories:\t%d\n", sum.advisories)
	fmt.Fprintf(w, "Notes:\t%d\n", sum.notes)
}
