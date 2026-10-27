package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// width is the layout width. Fixed rather than detected: terminal-size
// detection would mean another dependency for very little.
const width = 78

func (r *reviewer) printList(items []*store.Item) {
	filterNote := ""
	if r.filter.active() {
		filterNote = fmt.Sprintf(" [filter: %s]", r.filter.describe())
	}
	fmt.Fprintf(r.out, "\n%s - %d awaiting action%s\n\n", r.svc.MainRepo, len(items), filterNote)

	if len(items) == 0 {
		fmt.Fprintln(r.out, "  Nothing matches this filter - press f to change it.")
		fmt.Fprintln(r.out)
		return
	}

	fmt.Fprintf(r.out, "  %-3s %-40s %-24s %s\n", "#", "item", "verdict", "conf")

	for i, it := range items {
		kind := ""
		if it.Kind == triage.KindPR {
			kind = " (pr)"
		}
		item := truncate(fmt.Sprintf("%d %s%s", it.Number, it.Title, kind), 40)
		res := it.Verdict.Result
		verdict := truncate(fmt.Sprintf("%s/%s", res.Recommendation, res.Reason), 24)
		fmt.Fprintf(r.out, "  %-3d %-40s %-24s %4d\n", i+1, item, verdict, *res.Confidence)
	}
	fmt.Fprintln(r.out)
}

func (r *reviewer) printDetail(it *store.Item, pos, total int) {
	res := it.Verdict.Result
	head := fmt.Sprintf("- %s#%d ", r.svc.MainRepo, it.Number)
	tail := fmt.Sprintf(" (%d of %d)", pos, total)
	fmt.Fprintf(r.out, "\n%s%s%s\n", head, strings.Repeat("-", max(width-len(head)-len(tail), 0)), tail)

	fmt.Fprintf(r.out, "%s\n", it.Title)
	if it.Sensitive {
		fmt.Fprintln(r.out, "[sensitive: an advisory is about this item]")
	}
	fmt.Fprintf(r.out, "%s / %s - confidence %d", res.Recommendation, res.Reason, *res.Confidence)
	if res.DuplicateOf != nil {
		fmt.Fprintf(r.out, " - duplicate of #%d", *res.DuplicateOf)
	}
	if res.FixedIn != "" {
		fmt.Fprintf(r.out, " - fixed in %s", res.FixedIn)
	}
	fmt.Fprintf(r.out, "\n\nReasoning\n%s\n", indent(strings.TrimSpace(res.Reasoning), "  "))

	// Flush-left and unwrapped, for the same reason as in `report`: this is the
	// text a maintainer pastes into GitHub.
	if c := strings.TrimSpace(res.SuggestedComment); c != "" {
		fmt.Fprintf(r.out, "\nSuggested comment\n%s\n", unwrap(c))
	}
	if len(res.SuggestedLabels) > 0 {
		fmt.Fprintf(r.out, "\nSuggested labels: %s\n", strings.Join(res.SuggestedLabels, ", "))
	}
	for _, e := range res.Evidence {
		fmt.Fprintf(r.out, "Evidence: %s\n", e)
	}
	fmt.Fprintln(r.out)
}

// printContext shows what else is known about the item on screen: its links
// and notes, read fresh so a note just added appears. A failure to read them
// is reported and the review carries on, since the verdict is still usable.
func (r *reviewer) printContext(ctx context.Context, it *store.Item) {
	d, err := r.svc.Show(ctx, store.Ref{Repo: it.Repo, Number: it.Number})
	if err != nil {
		fmt.Fprintf(r.out, "Could not read notes and links: %v\n\n", err)
		return
	}
	if len(d.Links) > 0 {
		fmt.Fprintln(r.out, "Links")
		printLinks(r.out, r.svc.MainRepo, d.Links)
		fmt.Fprintln(r.out)
	}
	if len(d.Notes) > 0 {
		fmt.Fprintln(r.out, "Notes")
		printNotes(r.out, d.Notes)
		fmt.Fprintln(r.out)
	}
}

// truncate shortens s to n runes, marking the cut so a clipped title is not
// mistaken for the whole thing.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// clipboardTools are tried in order; the first one present wins.
//
//nolint:gochecknoglobals // read-only lookup table
var clipboardTools = [][]string{
	{"wl-copy"},
	{"xclip", "-selection", "clipboard"},
	{"xsel", "--clipboard", "--input"},
	{"pbcopy"},
}

// copyToClipboard pipes text to whichever clipboard tool is installed.
func copyToClipboard(ctx context.Context, text string) error {
	for _, tool := range clipboardTools {
		if _, err := exec.LookPath(tool[0]); err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, tool[0], tool[1:]...) //nolint:gosec // fixed table above, not user input
		cmd.Stdin = strings.NewReader(text)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s failed: %w", tool[0], err)
		}
		return nil
	}
	return fmt.Errorf("no clipboard tool found (tried wl-copy, xclip, xsel, pbcopy)")
}

// openInBrowser hands the URL to the desktop's opener.
//
// The opener is started and not waited on: it typically forks a browser that
// outlives the review session, so binding it to the command's context would
// kill the browser on exit.
func openInBrowser(url string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if _, err := exec.LookPath(opener); err != nil {
		return fmt.Errorf("%s not found", opener)
	}
	//nolint:gosec,noctx // opener is a fixed name, url is GitHub's or derived; see above on context
	if err := exec.Command(opener, url).Start(); err != nil {
		return fmt.Errorf("%s failed: %w", opener, err)
	}
	return nil
}
