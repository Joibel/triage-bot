package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/store"
)

func configCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "config [key [value]]",
		Short: "Show or change a setting",
		Long: "With no arguments, lists every setting; with a key, shows one; with a key\n" +
			"and a value, changes it. The daemon reads settings each cycle, so a change\n" +
			"takes effect without a restart.\n\n" +
			"Settings: max_open_beads (triage beads in flight at once), retriage_after\n" +
			"(how long a verdict stands, e.g. 4320h), max_template_attempts, bead_label.\n" +
			"org and repo are set by init and cannot be changed.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			return o.withStore(ctx, func(st *store.Store) error {
				s, err := settings(ctx, st)
				if err != nil {
					return err
				}
				switch len(args) {
				case 0:
					return listSettings(s)
				case 1:
					if !slices.Contains(store.SettingKeys(), args[0]) {
						return fmt.Errorf("no such setting %q", args[0])
					}
					fmt.Println(s.Get(args[0]))
					return nil
				default:
					return changeSetting(ctx, st, s, args[0], args[1])
				}
			})
		},
	}
}

func listSettings(s store.Settings) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, key := range store.SettingKeys() {
		fmt.Fprintf(w, "%s\t%s\n", key, s.Get(key))
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("failed to write settings: %w", err)
	}
	return nil
}

func changeSetting(ctx context.Context, st *store.Store, s store.Settings, key, value string) error {
	if key == store.KeyOrg || key == store.KeyRepo {
		return fmt.Errorf("%s is fixed: every item in the database belongs to %s", key, s.MainRepo())
	}
	if err := st.Update(ctx, func(tx *store.Tx) error {
		// Re-read under the lock so a concurrent change to another setting is
		// not written back over.
		current, err := tx.Settings(ctx)
		if err != nil {
			return err
		}
		if err := current.Set(key, value); err != nil {
			return err
		}
		return tx.SaveSettings(ctx, current)
	}); err != nil {
		return fmt.Errorf("failed to change %s: %w", key, err)
	}
	fmt.Printf("%s = %s\n", key, value)
	return nil
}
