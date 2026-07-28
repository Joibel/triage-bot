package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/state"
	"github.com/Joibel/triage-bot/internal/triage"
)

func reportCmd(o *opts) *cobra.Command {
	var asJSON bool
	var all bool

	cmd := &cobra.Command{
		Use:   "report",
		Short: "List triage verdicts awaiting your action",
		Long: "Groups finished verdicts by recommendation, so what needs a maintainer's\n" +
			"words is separated from what is waiting on a reporter.\n\n" +
			"triage-bot never acts on these itself: applying a recommendation is yours.",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := state.Load(o.statusFile)
			if err != nil {
				return fmt.Errorf("failed to read status file: %w", err)
			}

			items := pendingItems(cfg, all)
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(items)
			}
			printReport(cfg, items)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON")
	cmd.Flags().BoolVar(&all, "all", false, "Include verdicts already applied, rejected or deferred")
	return cmd
}

// pendingItems selects triaged items still awaiting a human decision.
func pendingItems(cfg *state.Config, all bool) []*state.Item {
	var out []*state.Item
	for _, it := range cfg.Items {
		if it.State != state.Triaged {
			continue
		}
		if !all && it.Human.State != state.Pending {
			continue
		}
		out = append(out, it)
	}
	return out
}

func printReport(cfg *state.Config, items []*state.Item) {
	if len(items) == 0 {
		fmt.Println("Nothing awaiting action.")
		return
	}

	// Group in the pair table's order so the report reads consistently.
	for _, rec := range triage.Recommendations() {
		group := filterByRecommendation(items, rec)
		if len(group) == 0 {
			continue
		}

		fmt.Printf("\n%s (%d)\n", strings.ToUpper(string(rec)), len(group))
		fmt.Println(strings.Repeat("-", 60))

		for _, it := range group {
			printItem(cfg, it)
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("-", 60))
	fmt.Println("Record what you did:  triage-bot ack <number> --applied")
	fmt.Println("                      triage-bot ack <number> --rejected --note \"...\"")
}

// printItem renders one verdict: what it is, why, and the material a
// maintainer needs to act on it without redoing the research.
func printItem(cfg *state.Config, it *state.Item) {
	fmt.Printf("\n  %s/%s#%d  %s\n", cfg.Org, cfg.Repo, it.Number, it.Title)
	fmt.Printf("  %s | confidence %d | %s\n", it.Result.Reason, *it.Result.Confidence, it.Kind)

	if it.Result.DuplicateOf != nil {
		fmt.Printf("  duplicate of #%d\n", *it.Result.DuplicateOf)
	}
	if it.Result.FixedIn != "" {
		fmt.Printf("  fixed in %s\n", it.Result.FixedIn)
	}

	fmt.Printf("%s\n", indent(strings.TrimSpace(it.Result.Reasoning), "  | "))

	if c := strings.TrimSpace(it.Result.SuggestedComment); c != "" {
		fmt.Printf("\n  Suggested comment:\n%s\n", indent(c, "  > "))
	}
	if len(it.Result.SuggestedLabels) > 0 {
		fmt.Printf("  Suggested labels: %s\n", strings.Join(it.Result.SuggestedLabels, ", "))
	}
	for _, e := range it.Result.Evidence {
		fmt.Printf("  Evidence: %s\n", e)
	}
	if it.Human.State != "" && it.Human.State != state.Pending {
		fmt.Printf("  [%s] %s\n", it.Human.State, it.Human.Note)
	}
}

func filterByRecommendation(items []*state.Item, rec triage.Recommendation) []*state.Item {
	var out []*state.Item
	for _, it := range items {
		if it.Result != nil && it.Result.Recommendation == rec {
			out = append(out, it)
		}
	}
	return out
}

func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
