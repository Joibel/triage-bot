package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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
		// Printed flush-left and unwrapped: this is the one part of the report
		// meant to be selected and pasted into GitHub, so it must not carry the
		// report's own decoration or the agent's line wrapping.
		fmt.Printf("\n  Suggested comment, to paste as-is:\n\n%s\n\n", unwrap(c))
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

// listItem matches the start of an ordered list item ("1." or "1)").
var listItem = regexp.MustCompile(`^\d+[.)]\s`)

// unwrap joins soft-wrapped prose back into single-line paragraphs.
//
// Agents write suggested_comment as a YAML block scalar, so their line wrapping
// is preserved verbatim. GitHub renders a single newline in a comment as a real
// line break, which turns a wrapped paragraph into ragged short lines once
// pasted. Joining them lets the recipient's client wrap it instead.
//
// Blank lines, fenced code, indented code and markdown block markers are left
// alone, since a line break is meaningful in all of those.
func unwrap(s string) string {
	var out, para []string
	inFence := false

	flush := func() {
		if len(para) > 0 {
			out = append(out, strings.Join(para, " "))
			para = nil
		}
	}

	for line := range strings.SplitSeq(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			flush()
			inFence = !inFence
			out = append(out, line)
		case inFence, strings.HasPrefix(line, "    "):
			out = append(out, line)
		case trimmed == "":
			flush()
			out = append(out, "")
		case isBlockMarker(trimmed):
			flush()
			out = append(out, line)
		default:
			para = append(para, trimmed)
		}
	}
	flush()
	return strings.Join(out, "\n")
}

// isBlockMarker reports whether a line starts markdown structure whose line
// break must be preserved.
func isBlockMarker(trimmed string) bool {
	for _, p := range []string{"- ", "* ", "+ ", "#", ">", "|"} {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return listItem.MatchString(trimmed)
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
