package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Joibel/triage-bot/internal/state"
	"github.com/Joibel/triage-bot/internal/triage"
)

// reviewer walks the pending queue interactively.
//
// Input, output and the two side effects (clipboard, browser) are injected so
// the whole loop can be driven by a scripted stdin in tests, with no terminal
// and nothing touching the real clipboard.
type reviewer struct {
	path   string
	in     *bufio.Scanner
	out    io.Writer
	copy   func(context.Context, string) error
	open   func(string) error
	filter reviewFilter
}

// reviewFilter narrows the queue to one kind, recommendation and/or reason. The
// zero value matches everything.
type reviewFilter struct {
	kind   triage.Kind
	rec    triage.Recommendation
	reason triage.Reason
}

func (f reviewFilter) active() bool { return f.kind != "" || f.rec != "" || f.reason != "" }

// apply keeps only the items the filter matches.
func (f reviewFilter) apply(items []*state.Item) []*state.Item {
	if !f.active() {
		return items
	}
	var out []*state.Item
	for _, it := range items {
		if f.matches(it) {
			out = append(out, it)
		}
	}
	return out
}

// matches reports whether one item satisfies every field the filter sets.
func (f reviewFilter) matches(it *state.Item) bool {
	if f.kind != "" && it.Kind != f.kind {
		return false
	}
	if f.rec == "" && f.reason == "" {
		return true
	}
	// An item without a verdict cannot match a recommendation or reason.
	if it.Result == nil {
		return false
	}
	return (f.rec == "" || it.Result.Recommendation == f.rec) &&
		(f.reason == "" || it.Result.Reason == f.reason)
}

// describe renders the active filter for the list header.
func (f reviewFilter) describe() string {
	var parts []string
	if f.kind != "" {
		parts = append(parts, string(f.kind))
	}
	switch {
	case f.reason != "":
		parts = append(parts, fmt.Sprintf("%s/%s", f.rec, f.reason))
	case f.rec != "":
		parts = append(parts, string(f.rec))
	}
	return strings.Join(parts, " ")
}

// parseFilter resolves a whitespace-separated line of tokens to a filter. Each
// token is a kind, a recommendation (narrowing to that group) or a reason
// (narrowing to the one pair it belongs to), in any order; an empty line clears
// the filter. A reason carries its recommendation, so one token still suffices
// to name a verdict filter.
func parseFilter(line string) (reviewFilter, error) {
	var f reviewFilter
	for token := range strings.FieldsSeq(line) {
		next, err := parseFilterToken(token)
		if err != nil {
			return reviewFilter{}, err
		}
		if f, err = f.merge(next); err != nil {
			return reviewFilter{}, err
		}
	}
	return f, nil
}

// parseFilterToken resolves one token to the single field it names.
func parseFilterToken(token string) (reviewFilter, error) {
	if slices.Contains(triage.Kinds(), triage.Kind(token)) {
		return reviewFilter{kind: triage.Kind(token)}, nil
	}
	if slices.Contains(triage.Recommendations(), triage.Recommendation(token)) {
		return reviewFilter{rec: triage.Recommendation(token)}, nil
	}
	if owner, ok := triage.RecommendationFor(triage.Reason(token)); ok {
		return reviewFilter{rec: owner, reason: triage.Reason(token)}, nil
	}
	return reviewFilter{}, fmt.Errorf("no such kind, recommendation or reason: %q", token)
}

// merge folds one token's filter into the filter built so far. A token that
// contradicts an earlier one is refused rather than allowed to win: "issue pr"
// and "close still_wanted" are mistakes, and silently honouring the last of them
// would show a list the user did not ask for.
func (f reviewFilter) merge(g reviewFilter) (reviewFilter, error) {
	if f.kind != "" && g.kind != "" && f.kind != g.kind {
		return reviewFilter{}, fmt.Errorf("cannot filter on both %s and %s", f.kind, g.kind)
	}
	if f.rec != "" && g.rec != "" && f.rec != g.rec {
		return reviewFilter{}, fmt.Errorf("cannot filter on both %s and %s", f.rec, g.rec)
	}
	if f.reason != "" && g.reason != "" && f.reason != g.reason {
		return reviewFilter{}, fmt.Errorf("cannot filter on both %s and %s", f.reason, g.reason)
	}
	return reviewFilter{
		kind:   cmp.Or(g.kind, f.kind),
		rec:    cmp.Or(g.rec, f.rec),
		reason: cmp.Or(g.reason, f.reason),
	}, nil
}

// joinValues renders a list of string-like enum values for a hint line.
func joinValues[T ~string](vals []T) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}

func reviewCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "review",
		Short: "Walk the pending verdicts one at a time and record what you did",
		Long: "Shows the queue as a list, opens one item at a time, and records your\n" +
			"decision without leaving the session. Press f to filter the list down to\n" +
			"one kind, recommendation or reason, or a combination such as `pr close`.\n\n" +
			"`report` remains the non-interactive view, and is what to use in a pipe.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Refuse early rather than sit waiting on input that will never
			// come. Note a character-device check is not enough: /dev/null is
			// one, and would sail past it.
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return errors.New("review needs a terminal; use `triage-bot report` or `report --json` in a pipe")
			}
			r := &reviewer{
				path: o.statusFile,
				in:   bufio.NewScanner(os.Stdin),
				out:  os.Stdout,
				copy: copyToClipboard,
				open: openInBrowser,
			}
			return r.run(cmd.Context())
		},
	}
}

// run is the outer loop: show the list, open what is chosen, repeat.
//
// The config is re-read on every pass rather than held, so the list reflects
// decisions just made and anything the daemon changed underneath.
func (r *reviewer) run(ctx context.Context) error {
	for {
		cfg, err := state.Load(r.path)
		if err != nil {
			return fmt.Errorf("failed to read status file: %w", err)
		}

		items := r.filter.apply(pendingItems(cfg, false))

		// An empty queue with no filter is the end of the work. Under a filter
		// it is not: the user must still be able to change or clear it, so the
		// prompt is shown rather than returning.
		if len(items) == 0 && !r.filter.active() {
			fmt.Fprintln(r.out, "Nothing awaiting action.")
			return nil
		}

		r.printList(cfg, items)

		line, ok := r.prompt(listPrompt(len(items)))
		if !ok || line == "q" {
			return nil
		}
		if line == "f" {
			r.editFilter()
			continue
		}

		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(items) {
			fmt.Fprintf(r.out, "\nNot a listed number: %q\n", line)
			continue
		}

		if quit := r.detail(ctx, cfg, items, n-1); quit {
			return nil
		}
	}
}

// listPrompt offers a number to open only when the list has something in it;
// under a filter that matches nothing, filtering and quitting are the only moves.
func listPrompt(n int) string {
	if n == 0 {
		return "[f]ilter  [q]uit > "
	}
	return fmt.Sprintf("[1-%d] open  [f]ilter  [q]uit > ", n)
}

// editFilter reads a line of tokens and sets the filter from it. An empty line
// clears the filter; an unknown or contradictory token is reported and leaves
// the current filter in place.
func (r *reviewer) editFilter() {
	fmt.Fprintf(r.out, "\nKinds: %s\n", joinValues(triage.Kinds()))
	fmt.Fprintf(r.out, "Recommendations: %s\n", joinValues(triage.Recommendations()))
	fmt.Fprintf(r.out, "Reasons: %s\n", joinValues(triage.Reasons()))

	line, ok := r.prompt("filter by kind, recommendation or reason (empty clears) > ")
	if !ok {
		return
	}
	f, err := parseFilter(line)
	if err != nil {
		fmt.Fprintf(r.out, "\n%v\n", err)
		return
	}
	r.filter = f
}

// detail shows one item and handles the action taken on it. It reports whether
// the user asked to quit; every other action returns to the list.
func (r *reviewer) detail(ctx context.Context, cfg *state.Config, items []*state.Item, idx int) bool {
	item := items[idx]

	for {
		r.printDetail(cfg, item, idx+1, len(items))

		line, ok := r.prompt("[a]pplied [r]ejected [d]efer [c]opy [o]pen [b]ack [q]uit > ")
		if !ok || line == "q" {
			return true
		}

		switch line {
		case "", "b":
			return false
		case "c":
			r.clipboard(ctx, item)
		case "o":
			r.browser(cfg, item)
		case "a", "d", "r":
			if r.act(item, line) {
				return false
			}
		default:
			fmt.Fprintf(r.out, "\nUnrecognised: %q\n", line)
		}
	}
}

// act records a decision. It reports whether the decision was made; a cancelled
// rejection leaves the caller on the item.
func (r *reviewer) act(item *state.Item, key string) bool {
	outcome := map[string]state.HumanState{
		"a": state.Applied,
		"d": state.Deferred,
		"r": state.Rejected,
	}[key]

	var note string
	if outcome == state.Rejected {
		// Same rule as `ack --rejected`: the note is what stops the next agent
		// repeating the rejected conclusion, so a rejection without one is
		// worse than no rejection.
		n, ok := r.prompt("Why was it wrong? (empty cancels) > ")
		if !ok || n == "" {
			fmt.Fprintln(r.out, "Cancelled.")
			return false
		}
		note = n
	}

	if err := r.record(item.Number, outcome, note); err != nil {
		fmt.Fprintf(r.out, "\n%v\n", err)
		return false
	}
	fmt.Fprintf(r.out, "\n#%d %s\n\n", item.Number, ackMessages[outcome])
	return true
}

// record applies the decision under the writer lock, sharing applyOutcome with
// the ack command so the two cannot drift on what a rejection means.
func (r *reviewer) record(number int, outcome state.HumanState, note string) error {
	if err := state.Update(r.path, func(c *state.Config) error {
		item := c.Item(number)
		if item == nil {
			return fmt.Errorf("#%d is no longer tracked", number)
		}
		// Reloaded under the lock, so this catches a daemon requeue or another
		// shell acking the same item while it was on screen.
		if item.State != state.Triaged {
			return fmt.Errorf("#%d is now %s, not triaged - leaving it alone", number, item.State)
		}
		applyOutcome(item, outcome, note)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to record decision: %w", err)
	}
	return nil
}

func (r *reviewer) clipboard(ctx context.Context, item *state.Item) {
	comment := strings.TrimSpace(item.Result.SuggestedComment)
	if comment == "" {
		fmt.Fprintln(r.out, "\nNo suggested comment on this item.")
		return
	}
	if err := r.copy(ctx, unwrap(comment)); err != nil {
		fmt.Fprintf(r.out, "\nCould not copy: %v\n", err)
		return
	}
	fmt.Fprintln(r.out, "\nSuggested comment copied to clipboard.")
}

func (r *reviewer) browser(cfg *state.Config, item *state.Item) {
	if err := r.open(itemURL(cfg, item)); err != nil {
		fmt.Fprintf(r.out, "\nCould not open a browser: %v\n", err)
	}
}

// prompt writes a prompt and reads one line. The second result is false at end
// of input, which is treated as a quit rather than an error.
func (r *reviewer) prompt(p string) (string, bool) {
	fmt.Fprint(r.out, p)
	if !r.in.Scan() {
		fmt.Fprintln(r.out)
		return "", false
	}
	return strings.TrimSpace(r.in.Text()), true
}

// itemURL derives the GitHub URL. The status file stores no URL and does not
// need to: GitHub redirects /issues/N to /pull/N, so one form covers both kinds.
func itemURL(cfg *state.Config, item *state.Item) string {
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", cfg.Org, cfg.Repo, item.Number)
}
