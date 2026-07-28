package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Joibel/triage-bot/internal/state"
)

// reviewer walks the pending queue interactively.
//
// Input, output and the two side effects (clipboard, browser) are injected so
// the whole loop can be driven by a scripted stdin in tests, with no terminal
// and nothing touching the real clipboard.
type reviewer struct {
	path string
	in   *bufio.Scanner
	out  io.Writer
	copy func(context.Context, string) error
	open func(string) error
}

func reviewCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "review",
		Short: "Walk the pending verdicts one at a time and record what you did",
		Long: "Shows the queue as a list, opens one item at a time, and records your\n" +
			"decision without leaving the session.\n\n" +
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

		items := pendingItems(cfg, false)
		if len(items) == 0 {
			fmt.Fprintln(r.out, "Nothing awaiting action.")
			return nil
		}

		r.printList(cfg, items)

		line, ok := r.prompt(fmt.Sprintf("[1-%d] open  [q]uit > ", len(items)))
		if !ok || line == "q" {
			return nil
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
