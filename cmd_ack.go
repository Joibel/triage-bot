package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
)

func ackCmd(o *opts) *cobra.Command {
	var applied, rejected, deferred bool
	var note string

	cmd := &cobra.Command{
		Use:   "ack <ref>",
		Short: "Record what you did with a recommendation",
		Long: "Marks a verdict as applied, rejected or deferred so `report` shows a queue\n" +
			"that burns down. <ref> is #123, 123 or the item's URL.\n\n" +
			"Rejecting returns the item to the triage queue, and --note is put in front of\n" +
			"the next agent so it does not repeat the rejected conclusion.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outcome, err := chooseOutcome(applied, rejected, deferred, note)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				ref, err := svc.ParseRef(args[0])
				if err != nil {
					return err
				}
				if _, err := svc.Decide(ctx, ref, outcome, note, store.AuthorHuman); err != nil {
					return fmt.Errorf("failed to record acknowledgement: %w", err)
				}
				fmt.Printf("%s %s\n", svc.Display(ref), ackMessages[outcome])
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&applied, "applied", false, "You actioned the recommendation on GitHub")
	cmd.Flags().BoolVar(&rejected, "rejected", false, "The recommendation was wrong; requeue for re-triage")
	cmd.Flags().BoolVar(&deferred, "deferred", false, "Leave it for later without requeueing")
	cmd.Flags().StringVar(&note, "note", "", "Why (required with --rejected)")
	return cmd
}

func retriageCmd(o *opts) *cobra.Command {
	var note string

	cmd := &cobra.Command{
		Use:   "retriage <ref>",
		Short: "Force an item back into the triage queue",
		Long: "Returns an item to the queue, keeping its earlier verdicts as history. An\n" +
			"issue or pull request known only through a note or a link joins the queue.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			return o.withStore(ctx, func(st *store.Store) error {
				s, err := settings(ctx, st)
				if err != nil {
					return err
				}
				ref, err := store.ParseRef(args[0], s.MainRepo())
				if err != nil {
					return err
				}
				if err := retriage(ctx, st, s, ref, note); err != nil {
					return err
				}
				fmt.Printf("%s returned to the triage queue\n", ref.Display(s.MainRepo()))
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&note, "note", "", "Context to pass to the next agent")
	return cmd
}

// retriage returns an item to the queue. Only the main repository's issues
// and pull requests are ever triaged.
func retriage(ctx context.Context, st *store.Store, s store.Settings, ref store.Ref, note string) error {
	display := ref.Display(s.MainRepo())
	return st.Update(ctx, func(tx *store.Tx) error {
		item, err := tracked(ctx, tx, ref, display)
		if err != nil {
			return err
		}
		switch {
		case item.Repo != s.MainRepo() || item.Kind == store.KindAdvisory:
			return fmt.Errorf("%s is not an issue or pull request in %s, so is never triaged", display, s.MainRepo())
		case item.State == store.Queued:
			return fmt.Errorf("%s is already queued as bead %s", display, item.BeadID)
		}
		return tx.Requeue(ctx, item.ID, note)
	})
}

// tracked finds the item a ref names, saying so plainly if it is not held.
func tracked(ctx context.Context, tx *store.Tx, ref store.Ref, display string) (*store.Item, error) {
	item, err := tx.Item(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%s is not tracked", display)
	}
	return item, err
}

// ackMessages is what each outcome reports back to the user.
//
//nolint:gochecknoglobals // read-only lookup table
var ackMessages = map[store.DispositionState]string{
	store.Rejected: "rejected and returned to the triage queue",
	store.Applied:  "marked applied",
	store.Deferred: "deferred",
	store.Pending:  "back to pending",
}

// chooseOutcome turns the mutually exclusive flags into a single state.
func chooseOutcome(applied, rejected, deferred bool, note string) (store.DispositionState, error) {
	chosen := 0
	for _, b := range []bool{applied, rejected, deferred} {
		if b {
			chosen++
		}
	}
	if chosen != 1 {
		return "", errors.New("choose exactly one of --applied, --rejected or --deferred")
	}
	switch {
	case rejected:
		if note == "" {
			return "", errors.New("--rejected needs --note explaining why, so the next agent can do better")
		}
		return store.Rejected, nil
	case applied:
		return store.Applied, nil
	default:
		return store.Deferred, nil
	}
}
