package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// refHelp explains the forms an item reference takes.
const refHelp = "A ref is #123 or 123 (argo-workflows), a GitHub URL, a GHSA id, or\n" +
	"fork#N for a pull request in an advisory's private fork."

func noteCmd(o *opts) *cobra.Command {
	var tags []string

	cmd := &cobra.Command{
		Use:   "note <ref> <text...>",
		Short: "Add a note to an item",
		Long: "Records a timestamped note on an issue, pull request or advisory. Text\n" +
			"of - is read from stdin. An item not yet in the database is looked up on\n" +
			"GitHub and added, without joining the triage queue.\n\n" + refHelp,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args[1:], " ")
			if text == "-" {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("failed to read the note: %w", err)
				}
				text = string(b)
			}
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				ref, err := svc.ParseRef(args[0])
				if err != nil {
					return err
				}
				it, n, err := svc.AddNote(ctx, ref, text, tags, store.AuthorHuman)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Noted on %s%s\n", it.Ref(svc.MainRepo), tagSuffix(n.Tags))
				return nil
			})
		},
	}

	cmd.Flags().StringSliceVarP(&tags, "tag", "t", nil, "Tag the note, e.g. security, backport, customer, blocked (repeatable)")
	return cmd
}

func notesCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "notes <ref>",
		Short: "List an item's notes",
		Long:  "Lists the notes on one item, oldest first.\n\n" + refHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				ref, err := svc.ParseRef(args[0])
				if err != nil {
					return err
				}
				d, err := svc.Show(ctx, ref)
				if err != nil {
					return err
				}
				w := cmd.OutOrStdout()
				if len(d.Notes) == 0 {
					fmt.Fprintf(w, "No notes on %s.\n", d.Item.Ref(svc.MainRepo))
					return nil
				}
				printNotes(w, d.Notes)
				return nil
			})
		},
	}
}

func linkCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "link <ref> <type> <ref>",
		Short: "Link two items",
		Long: "Records that the first item relates to the second: \"link #9000 fixes #8123\".\n" +
			"Types: duplicate_of, fixes, advisory_for, fix_in_fork, backport_of, related.\n" +
			"advisory_for and fix_in_fork start at the advisory, whichever way round\n" +
			"they are given, and make the other item sensitive. An item not yet in the\n" +
			"database is looked up on GitHub and added.\n\n" + refHelp,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			t := store.LinkType(args[1])
			if !slices.Contains(store.LinkTypes(), t) {
				return fmt.Errorf("no such link type %q: choose from %s", t, joinValues(store.LinkTypes()))
			}
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				from, err := svc.ParseRef(args[0])
				if err != nil {
					return err
				}
				to, err := svc.ParseRef(args[2])
				if err != nil {
					return err
				}
				l, err := svc.Link(ctx, from, t, to, store.AuthorHuman)
				if err != nil {
					return err
				}
				verb := "Linked"
				if !l.Added {
					verb = "Already linked:"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s %s\n", verb, l.From.Ref(svc.MainRepo), t, l.To.Ref(svc.MainRepo))
				return nil
			})
		},
	}
}

func showCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "show <ref>",
		Short: "Show everything known about an item",
		Long: "Shows an item with its triage state, current verdict and earlier ones, the\n" +
			"decisions on them, its notes, and its links in both directions.\n\n" + refHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				ref, err := svc.ParseRef(args[0])
				if err != nil {
					return err
				}
				d, err := svc.Show(ctx, ref)
				if err != nil {
					return err
				}
				printShow(cmd.OutOrStdout(), svc.MainRepo, d)
				return nil
			})
		},
	}
}

func searchCmd(o *opts) *cobra.Command {
	var (
		kind, tag, state  string
		sensitive, public bool
		limit             int
	)

	cmd := &cobra.Command{
		Use:   "search [words...]",
		Short: "Search notes and item titles",
		Long: "Finds notes and item titles containing every word; a trailing * matches a\n" +
			"prefix. With no words, lists notes matching the filters, newest first.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if sensitive && public {
				return errors.New("choose one of --sensitive or --public")
			}
			q := store.SearchQuery{
				Text: strings.Join(args, " "), Kind: triage.Kind(kind), Tag: tag, State: state, Limit: limit,
			}
			switch {
			case sensitive:
				q.Sensitive = new(true)
			case public:
				q.Sensitive = new(false)
			}
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				hits, err := svc.Search(ctx, q)
				if err != nil {
					return err
				}
				printHits(cmd.OutOrStdout(), svc.MainRepo, hits)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", "Only issue, pr or advisory")
	cmd.Flags().StringVar(&tag, "tag", "", "Only notes with this tag")
	cmd.Flags().StringVar(&state, "state", "", "Only items in this triage or GitHub state (e.g. triaged, open, draft)")
	cmd.Flags().BoolVar(&sensitive, "sensitive", false, "Only sensitive items and their notes")
	cmd.Flags().BoolVar(&public, "public", false, "Leave out sensitive items and their notes")
	cmd.Flags().IntVar(&limit, "limit", 0, "Most results to show (default 50)")
	return cmd
}

func advisoriesCmd(o *opts) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "advisories",
		Short: "List open security advisories with their fork PRs and links",
		Long: "Lists the advisories the last sync found in the triage, draft or published\n" +
			"state, newest first, each with its private fork's pull requests and\n" +
			"anything else linked to it. `sync --only advisories` refreshes them.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			return o.withService(ctx, func(svc *service.Service) error {
				advisories, err := svc.Advisories(ctx, all)
				if err != nil {
					return err
				}
				printAdvisories(cmd.OutOrStdout(), svc.MainRepo, advisories)
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "Include closed and withdrawn advisories")
	return cmd
}

const dateTime = "2006-01-02 15:04"

func tagSuffix(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return " [" + strings.Join(tags, ", ") + "]"
}

// headline is an item's one-line identity: ref, kind, GitHub state, and
// whether it is sensitive.
func headline(mainRepo string, it *store.Item) string {
	parts := []string{it.Ref(mainRepo), string(it.Kind)}
	if it.GHState != "" {
		parts = append(parts, it.GHState)
	}
	if it.Sensitive {
		parts = append(parts, "[sensitive]")
	}
	return strings.Join(parts, "  ")
}

func printShow(w io.Writer, mainRepo string, d *service.Detail) {
	it := &d.Item
	fmt.Fprintln(w, headline(mainRepo, it))
	if it.Title != "" {
		fmt.Fprintln(w, it.Title)
	}
	fmt.Fprintln(w, it.HTMLURL())

	if a := d.Advisory; a != nil {
		var parts []string
		for _, kv := range [][2]string{{"severity", a.Severity}, {"CVE", a.CVE}, {"private fork", a.PrivateFork}} {
			if kv[1] != "" {
				parts = append(parts, kv[0]+" "+kv[1])
			}
		}
		if len(parts) > 0 {
			fmt.Fprintln(w, strings.Join(parts, ", "))
		}
	}

	printTriage(w, it, d.History)

	if len(d.Links) > 0 {
		fmt.Fprintln(w, "\nLinks")
		printLinks(w, mainRepo, d.Links)
	}
	if len(d.Notes) > 0 {
		fmt.Fprintln(w, "\nNotes")
		printNotes(w, d.Notes)
	}
}

// printTriage shows where the item is in triage, its current verdict in full,
// and its earlier verdicts one line each.
func printTriage(w io.Writer, it *store.Item, history []store.Verdict) {
	if it.State != "" {
		fmt.Fprintf(w, "\nTriage: %s", it.State)
		if it.BeadID != "" {
			fmt.Fprintf(w, ", bead %s", it.BeadID)
		}
		if it.Attempts > 0 {
			fmt.Fprintf(w, ", %d invalid templates", it.Attempts)
		}
		fmt.Fprintln(w)
		if it.CarryNote != "" {
			fmt.Fprintf(w, "For the next agent:\n%s\n", indent(it.CarryNote, "  "))
		}
	}

	earlier := history
	if v := it.Verdict; v != nil {
		// The current verdict is the newest; the rest are history.
		if n := len(history); n > 0 && history[n-1].ID == v.ID {
			earlier = history[:n-1]
		}
		r := v.Result
		fmt.Fprintf(w, "\nVerdict%s: %s/%s, confidence %s - %s\n",
			onDate(v.TriagedAt), r.Recommendation, r.Reason, confidence(r.Confidence), describeDisposition(v.Disposition))
		if r.DuplicateOf != nil {
			fmt.Fprintf(w, "Duplicate of #%d\n", *r.DuplicateOf)
		}
		if r.FixedIn != "" {
			fmt.Fprintf(w, "Fixed in %s\n", r.FixedIn)
		}
		fmt.Fprintln(w, indent(strings.TrimSpace(r.Reasoning), "  "))
		if c := strings.TrimSpace(r.SuggestedComment); c != "" {
			fmt.Fprintf(w, "Suggested comment\n%s\n", unwrap(c))
		}
	}
	if len(earlier) > 0 {
		fmt.Fprintln(w, "\nEarlier verdicts")
		for _, v := range earlier {
			fmt.Fprintf(w, "  %s  %s/%s  %s\n", day(v.TriagedAt), v.Result.Recommendation, v.Result.Reason,
				describeDisposition(v.Disposition))
		}
	}
}

func describeDisposition(d store.Disposition) string {
	s := string(d.State)
	if d.Note != "" {
		s += ": " + d.Note
	}
	return s
}

func confidence(c *int) string {
	if c == nil {
		return "?"
	}
	return fmt.Sprint(*c)
}

func onDate(t *time.Time) string {
	if s := day(t); s != "" {
		return " (" + s + ")"
	}
	return ""
}

// day formats an optional time as a date, or nothing.
func day(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

func printLinks(w io.Writer, mainRepo string, links []store.Link) {
	for _, l := range links {
		fmt.Fprintf(w, "  %-14s %s  %s\n", l.Label(), headline(mainRepo, &l.Other), l.Other.Title)
	}
}

func printNotes(w io.Writer, notes []store.Note) {
	for _, n := range notes {
		fmt.Fprintf(w, "  %s  %s%s\n%s\n", n.CreatedAt.Local().Format(dateTime), n.Author, tagSuffix(n.Tags),
			indent(n.Body, "    "))
	}
}

func printHits(w io.Writer, mainRepo string, hits []store.Hit) {
	if len(hits) == 0 {
		fmt.Fprintln(w, "Nothing found.")
		return
	}
	for _, h := range hits {
		if h.Note == nil {
			fmt.Fprintf(w, "%s  title\n  %s\n", headline(mainRepo, &h.Item), h.Snippet)
			continue
		}
		fmt.Fprintf(w, "%s  %s  %s%s\n  %s\n", headline(mainRepo, &h.Item),
			h.Note.CreatedAt.Local().Format(dateTime), h.Note.Author, tagSuffix(h.Note.Tags),
			strings.ReplaceAll(h.Snippet, "\n", " "))
	}
}

func printAdvisories(w io.Writer, mainRepo string, advisories []service.AdvisoryDetail) {
	if len(advisories) == 0 {
		fmt.Fprintln(w, "No open advisories. (`triage-bot sync --only advisories` fetches them.)")
		return
	}
	for _, a := range advisories {
		parts := []string{a.GHSA, a.GHState}
		for _, s := range []string{a.Severity, a.CVE} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		fmt.Fprintf(w, "\n%s\n  %s\n", strings.Join(parts, "  "), a.Title)
		if a.PrivateFork != "" {
			fmt.Fprintf(w, "  private fork %s\n", a.PrivateFork)
		}
		printLinks(w, mainRepo, a.Links)
	}
}
