package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/engine"
)

func reparseCmd(o *opts) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "reparse [number...]",
		Short: "Re-read recorded verdicts from their beads",
		Long: "Re-reads the completion template of already-triaged items from the beads\n" +
			"that produced them, and replaces the recorded verdict.\n\n" +
			"Use this after a parser fix: the bead still holds the agent's original text,\n" +
			"so a verdict recorded wrongly can be corrected without re-running the\n" +
			"assessment. Unlike `retriage` this costs no agent time and keeps the bead.\n\n" +
			"Your own decisions are preserved - reparsing corrects what the agent said,\n" +
			"not what you decided about it.\n\n" +
			"With no arguments every triaged item is considered.",
		RunE: func(cmd *cobra.Command, args []string) error {
			numbers, err := parseNumbers(args)
			if err != nil {
				return err
			}

			e, err := o.newEngine()
			if err != nil {
				return err
			}

			changes, err := e.Reparse(cmd.Context(), numbers, dryRun)
			if err != nil {
				return fmt.Errorf("failed to reparse: %w", err)
			}
			printChanges(changes, dryRun)
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would change without writing")
	return cmd
}

func parseNumbers(args []string) ([]int, error) {
	numbers := make([]int, 0, len(args))
	for _, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil {
			return nil, fmt.Errorf("%q is not an item number: %w", a, err)
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}

func printChanges(changes []engine.Change, dryRun bool) {
	if len(changes) == 0 {
		fmt.Println("Nothing to reparse: every recorded verdict already matches its bead.")
		return
	}

	updated, failed := 0, 0
	for _, c := range changes {
		if c.Err != nil {
			failed++
			fmt.Printf("#%-7d still does not parse: %v\n", c.Number, c.Err)
			continue
		}
		updated++
		fmt.Printf("#%-7d %s\n", c.Number, describe(c))
	}

	fmt.Println()
	verb := "updated"
	if dryRun {
		verb = "would be updated"
	}
	fmt.Printf("%d %s", updated, verb)
	if failed > 0 {
		fmt.Printf(", %d left alone because they still do not parse", failed)
	}
	fmt.Println()
	if dryRun && updated > 0 {
		fmt.Println("Re-run without --dry-run to write them.")
	}
}

// describe summarises what reparsing recovered, which is usually a comment that
// was cut short along with the fields that followed it.
func describe(c engine.Change) string {
	if c.Before == nil {
		return "verdict recovered"
	}
	var parts []string
	if n := len(c.After.SuggestedComment) - len(c.Before.SuggestedComment); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d chars of comment", n))
	}
	if n := len(c.After.SuggestedLabels) - len(c.Before.SuggestedLabels); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d labels", n))
	}
	if n := len(c.After.Evidence) - len(c.Before.Evidence); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d evidence", n))
	}
	if len(parts) == 0 {
		return "verdict differs"
	}
	return strings.Join(parts, ", ")
}
