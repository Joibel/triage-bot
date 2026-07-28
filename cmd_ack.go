package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/state"
)

func ackCmd(o *opts) *cobra.Command {
	var applied, rejected, deferred bool
	var note string

	cmd := &cobra.Command{
		Use:   "ack <number>",
		Short: "Record what you did with a recommendation",
		Long: "Marks a verdict as applied, rejected or deferred so `report` shows a queue\n" +
			"that burns down.\n\n" +
			"Rejecting returns the item to the triage queue, and --note is put in front of\n" +
			"the next agent so it does not repeat the rejected conclusion.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("%q is not an item number: %w", args[0], err)
			}
			outcome, err := chooseOutcome(applied, rejected, deferred, note)
			if err != nil {
				return err
			}

			if err := state.Update(o.statusFile, func(c *state.Config) error {
				item := c.Item(number)
				if item == nil {
					return fmt.Errorf("#%d is not tracked", number)
				}
				if item.State != state.Triaged {
					return fmt.Errorf("#%d is %s, not triaged - there is no recommendation to act on", number, item.State)
				}
				applyOutcome(item, outcome, note)
				return nil
			}); err != nil {
				return fmt.Errorf("failed to record acknowledgement: %w", err)
			}

			fmt.Printf("#%d %s\n", number, ackMessages[outcome])
			return nil
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
		Use:   "retriage <number>",
		Short: "Force an item back into the triage queue",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			number, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("%q is not an item number: %w", args[0], err)
			}

			return state.Update(o.statusFile, func(c *state.Config) error {
				item := c.Item(number)
				if item == nil {
					return fmt.Errorf("#%d is not tracked", number)
				}
				if item.State == state.Queued {
					return fmt.Errorf("#%d is already queued as bead %s", number, item.BeadID)
				}
				item.Requeue(note)
				fmt.Printf("#%d returned to the triage queue\n", number)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&note, "note", "", "Context to pass to the next agent")
	return cmd
}

// ackMessages is what each outcome reports back to the user.
//
//nolint:gochecknoglobals // read-only lookup table
var ackMessages = map[state.HumanState]string{
	state.Rejected: "rejected and returned to the triage queue",
	state.Applied:  "marked applied",
	state.Deferred: "deferred",
}

// chooseOutcome turns the mutually exclusive flags into a single state.
func chooseOutcome(applied, rejected, deferred bool, note string) (state.HumanState, error) {
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
		return state.Rejected, nil
	case applied:
		return state.Applied, nil
	default:
		return state.Deferred, nil
	}
}

// applyOutcome records the human's decision. Rejecting is the only one that
// changes the item's triage state, because it sends the item round again.
func applyOutcome(item *state.Item, outcome state.HumanState, note string) {
	if outcome == state.Rejected {
		item.Requeue(note)
		return
	}
	now := time.Now().UTC()
	item.Human = state.Human{State: outcome, Note: note, At: &now}
}
