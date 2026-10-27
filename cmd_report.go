package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
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
			"--json is a bulk export, so it leaves out anything sensitive (see\n" +
			"`triage-bot show`), and notes.\n\n" +
			"triage-bot never acts on these itself: applying a recommendation is yours.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				items, err := svc.Verdicts(ctx, all)
				if err != nil {
					return err
				}
				if asJSON {
					if err := json.NewEncoder(os.Stdout).Encode(exportItems(items)); err != nil {
						return fmt.Errorf("failed to write JSON: %w", err)
					}
					return nil
				}
				printReport(svc.MainRepo, items)
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON, without sensitive items")
	cmd.Flags().BoolVar(&all, "all", false, "Include verdicts already applied, rejected or deferred")
	return cmd
}

// reportItem is one verdict in `report --json`. Its shape is the one the
// YAML status file's items had when encoded, so consumers of the export do
// not notice the store changed underneath.
type reportItem struct {
	Number    int
	Kind      triage.Kind
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	State     store.ItemState
	BeadID    string
	Attempts  int
	LastError string
	QueuedAt  *time.Time
	TriagedAt *time.Time
	Result    *triage.Result
	Human     reportHuman
}

type reportHuman struct {
	State store.DispositionState
	Note  string
	At    *time.Time
}

// exportItems converts verdicts for a bulk export, dropping sensitive items:
// nothing to do with an advisory leaves the store in bulk.
func exportItems(items []*store.Item) []reportItem {
	out := []reportItem{}
	for _, it := range items {
		if it.Sensitive || it.Verdict == nil {
			continue
		}
		result := it.Verdict.Result
		d := it.Verdict.Disposition
		out = append(out, reportItem{
			Number:    it.Number,
			Kind:      it.Kind,
			Title:     it.Title,
			CreatedAt: it.CreatedAt,
			UpdatedAt: it.UpdatedAt,
			State:     it.State,
			BeadID:    it.BeadID,
			Attempts:  it.Attempts,
			LastError: it.LastError,
			QueuedAt:  it.QueuedAt,
			TriagedAt: it.Verdict.TriagedAt,
			Result:    &result,
			Human:     reportHuman{State: d.State, Note: d.Note, At: d.At},
		})
	}
	return out
}

func printReport(mainRepo string, items []*store.Item) {
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
			printItem(mainRepo, it)
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("-", 60))
	fmt.Println("Record what you did:  triage-bot ack <number> --applied")
	fmt.Println("                      triage-bot ack <number> --rejected --note \"...\"")
}

// printItem renders one verdict: what it is, why, and the material a
// maintainer needs to act on it without redoing the research.
func printItem(mainRepo string, it *store.Item) {
	r := it.Verdict.Result
	marker := ""
	if it.Sensitive {
		marker = "  [sensitive]"
	}
	fmt.Printf("\n  %s#%d  %s%s\n", mainRepo, it.Number, it.Title, marker)
	fmt.Printf("  %s | confidence %d | %s\n", r.Reason, *r.Confidence, it.Kind)

	if r.DuplicateOf != nil {
		fmt.Printf("  duplicate of #%d\n", *r.DuplicateOf)
	}
	if r.FixedIn != "" {
		fmt.Printf("  fixed in %s\n", r.FixedIn)
	}

	fmt.Printf("%s\n", indent(strings.TrimSpace(r.Reasoning), "  | "))

	if c := strings.TrimSpace(r.SuggestedComment); c != "" {
		// Printed flush-left and unwrapped: this is the one part of the report
		// meant to be selected and pasted into GitHub, so it must not carry the
		// report's own decoration or the agent's line wrapping.
		fmt.Printf("\n  Suggested comment, to paste as-is:\n\n%s\n\n", unwrap(c))
	}
	if len(r.SuggestedLabels) > 0 {
		fmt.Printf("  Suggested labels: %s\n", strings.Join(r.SuggestedLabels, ", "))
	}
	for _, e := range r.Evidence {
		fmt.Printf("  Evidence: %s\n", e)
	}
	if d := it.Verdict.Disposition; d.State != store.Pending {
		fmt.Printf("  [%s] %s\n", d.State, d.Note)
	}
}

func filterByRecommendation(items []*store.Item, rec triage.Recommendation) []*store.Item {
	var out []*store.Item
	for _, it := range items {
		if it.Verdict != nil && it.Verdict.Result.Recommendation == rec {
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
