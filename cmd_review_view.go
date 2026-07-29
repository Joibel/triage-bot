package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/Joibel/triage-bot/internal/state"
)

// width is the layout width. Fixed rather than detected: terminal-size
// detection would mean another dependency for very little.
const width = 78

func (r *reviewer) printList(cfg *state.Config, items []*state.Item) {
	filterNote := ""
	if r.filter.active() {
		filterNote = fmt.Sprintf(" [filter: %s]", r.filter.describe())
	}
	fmt.Fprintf(r.out, "\n%s/%s - %d awaiting action%s\n\n", cfg.Org, cfg.Repo, len(items), filterNote)

	if len(items) == 0 {
		fmt.Fprintln(r.out, "  Nothing matches this filter - press f to change it.")
		fmt.Fprintln(r.out)
		return
	}

	fmt.Fprintf(r.out, "  %-3s %-40s %-24s %s\n", "#", "item", "verdict", "conf")

	for i, it := range items {
		kind := ""
		if it.Kind == "pr" {
			kind = " (pr)"
		}
		item := truncate(fmt.Sprintf("%d %s%s", it.Number, it.Title, kind), 40)
		verdict := truncate(fmt.Sprintf("%s/%s", it.Result.Recommendation, it.Result.Reason), 24)
		fmt.Fprintf(r.out, "  %-3d %-40s %-24s %4d\n", i+1, item, verdict, *it.Result.Confidence)
	}
	fmt.Fprintln(r.out)
}

func (r *reviewer) printDetail(cfg *state.Config, it *state.Item, pos, total int) {
	head := fmt.Sprintf("- %s/%s#%d ", cfg.Org, cfg.Repo, it.Number)
	tail := fmt.Sprintf(" (%d of %d)", pos, total)
	fmt.Fprintf(r.out, "\n%s%s%s\n", head, strings.Repeat("-", max(width-len(head)-len(tail), 0)), tail)

	fmt.Fprintf(r.out, "%s\n", it.Title)
	fmt.Fprintf(r.out, "%s / %s - confidence %d",
		it.Result.Recommendation, it.Result.Reason, *it.Result.Confidence)
	if it.Result.DuplicateOf != nil {
		fmt.Fprintf(r.out, " - duplicate of #%d", *it.Result.DuplicateOf)
	}
	if it.Result.FixedIn != "" {
		fmt.Fprintf(r.out, " - fixed in %s", it.Result.FixedIn)
	}
	fmt.Fprintf(r.out, "\n\nReasoning\n%s\n", indent(strings.TrimSpace(it.Result.Reasoning), "  "))

	// Flush-left and unwrapped, for the same reason as in `report`: this is the
	// text a maintainer pastes into GitHub.
	if c := strings.TrimSpace(it.Result.SuggestedComment); c != "" {
		fmt.Fprintf(r.out, "\nSuggested comment\n%s\n", unwrap(c))
	}
	if len(it.Result.SuggestedLabels) > 0 {
		fmt.Fprintf(r.out, "\nSuggested labels: %s\n", strings.Join(it.Result.SuggestedLabels, ", "))
	}
	for _, e := range it.Result.Evidence {
		fmt.Fprintf(r.out, "Evidence: %s\n", e)
	}
	fmt.Fprintln(r.out)
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
	//nolint:gosec,noctx // opener is a fixed name, url derives from config; see above on context
	if err := exec.Command(opener, url).Start(); err != nil {
		return fmt.Errorf("%s failed: %w", opener, err)
	}
	return nil
}
