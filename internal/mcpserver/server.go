// Package mcpserver serves triage-bot's store to Claude Code sessions on this
// machine, over stdio MCP.
//
// It is local and returns everything, sensitive items included: it serves
// sessions on this machine, which is trusted with them. Notes and links a
// session adds are recorded with claude as their author.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Joibel/triage-bot/internal/buildinfo"
	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

// instructions tell a session what this server is and the rule that matters.
const instructions = "triage-bot's notes on argoproj/argo-workflows: triage verdicts and the maintainer's " +
	"decisions on them, free notes, typed links between issues, pull requests and security advisories, " +
	"and the open advisories with their private-fork pull requests.\n\n" +
	"A ref is #123 or 123, a GitHub URL, a GHSA id, or fork#N for a pull request in an advisory's " +
	"private fork.\n\n" +
	"You may add notes and links without asking: record what you learn about an item as you learn it. " +
	"Items marked sensitive - advisories, private-fork pull requests, anything an advisory is about - and " +
	"every note on them are security-sensitive: never repeat them in a GitHub comment, issue, pull request, " +
	"commit message or anywhere else that leaves this machine."

// New builds the server over a service.
func New(svc *service.Service) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "triage-bot", Version: buildinfo.Version},
		&mcp.ServerOptions{Instructions: instructions})
	t := &tools{svc: svc}

	mcp.AddTool(s, &mcp.Tool{
		Name:         "show",
		OutputSchema: object(),
		Description:  "Everything known about one item: triage state, current and earlier verdicts, decisions, notes, and links both ways.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.show)
	mcp.AddTool(s, &mcp.Tool{
		Name:         "verdicts",
		OutputSchema: object(),
		Description:  verdictsDescription(),
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.verdicts)
	mcp.AddTool(s, &mcp.Tool{
		Name:         "search",
		OutputSchema: object(),
		Description: "Full-text search over notes and item titles. Every word must match; a trailing * matches a prefix. " +
			"With no query, lists notes matching the filters, newest first. For triage verdicts by recommendation " +
			"or reason, use verdicts.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.search)
	mcp.AddTool(s, &mcp.Tool{
		Name:         "add_note",
		OutputSchema: object(),
		Description: "Add a note to an issue, pull request or advisory, recorded as written by claude. An item not yet " +
			"known is looked up on GitHub and added, without being queued for triage.",
	}, t.addNote)
	mcp.AddTool(s, &mcp.Tool{
		Name:         "link",
		OutputSchema: object(),
		Description: "Record that one item relates to another: \"from <type> to\". Types: duplicate_of, fixes, " +
			"advisory_for, fix_in_fork, backport_of, related. advisory_for and fix_in_fork start at the advisory " +
			"whichever way round they are given, and make the other item sensitive. Adding an existing link is a no-op.",
	}, t.link)
	mcp.AddTool(s, &mcp.Tool{
		Name:         "advisories",
		OutputSchema: object(),
		Description:  "The security advisories in the triage, draft or published state, newest first, with their private-fork pull requests and other links.",
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, t.advisories)
	mcp.AddTool(s, &mcp.Tool{
		Name:         "set_disposition",
		OutputSchema: object(),
		Description: "Record the maintainer's decision on an item's current triage verdict: applied (actioned on GitHub), " +
			"deferred (later, without requeueing), rejected (wrong; requeues the item for triage, and needs a note " +
			"saying why, which the next agent is shown) or pending (undecided again). Only for triaged items.",
	}, t.setDisposition)
	return s
}

// object is the output schema every tool declares. The results are typed, but
// their inferred schemas repeat the item view in every tool and come to some
// 20 KB, which every session would carry in its context just to list the
// tools. The field names in the results speak for themselves.
func object() *jsonschema.Schema { return &jsonschema.Schema{Type: "object"} }

// Run serves a session on stdin and stdout until it ends or ctx is done.
func Run(ctx context.Context, svc *service.Service) error {
	if err := New(svc).Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("MCP server stopped: %w", err)
	}
	return nil
}

type tools struct {
	svc *service.Service
}

type refInput struct {
	Ref string `json:"ref" jsonschema:"the item: #123, a GitHub URL, a GHSA id, or fork#N"`
}

func (t *tools) show(ctx context.Context, _ *mcp.CallToolRequest, in refInput) (*mcp.CallToolResult, detailView, error) {
	ref, err := t.svc.ParseRef(in.Ref)
	if err != nil {
		return nil, detailView{}, err
	}
	d, err := t.svc.Show(ctx, ref)
	if err != nil {
		return nil, detailView{}, err
	}
	return nil, viewDetail(t.svc.MainRepo, d), nil
}

// verdictsDescription describes the verdicts tool, listing the
// recommendations and their reasons from the pair table, so a reason added
// there reaches sessions too.
func verdictsDescription() string {
	var b strings.Builder
	b.WriteString("Triage verdicts, filtered, most confident first. By default only those still awaiting the " +
		"maintainer's decision. Recommendations and their reasons:")
	for _, rec := range triage.Table {
		reasons := make([]string, 0, len(rec.Reasons))
		for _, r := range rec.Reasons {
			reasons = append(reasons, string(r.Reason))
		}
		fmt.Fprintf(&b, " %s (%s);", rec.Recommendation, strings.Join(reasons, ", "))
	}
	return strings.TrimSuffix(b.String(), ";") + "."
}

type verdictsInput struct {
	Recommendation string `json:"recommendation,omitempty" jsonschema:"only this recommendation"`
	Reason         string `json:"reason,omitempty" jsonschema:"only this reason, e.g. good_first_issue"`
	Kind           string `json:"kind,omitempty" jsonschema:"only issue or pr"`
	Disposition    string `json:"disposition,omitempty" jsonschema:"the maintainer's decision: pending (the default), applied, deferred, rejected or any"`
	Sensitive      string `json:"sensitive,omitempty" jsonschema:"only to keep just sensitive items, exclude to leave them out"`
	Limit          int    `json:"limit,omitempty" jsonschema:"most results, default 20, at most 200"`
}

type verdictsOutput struct {
	Verdicts []itemView `json:"verdicts" jsonschema:"items with their verdict in force"`
}

func (t *tools) verdicts(ctx context.Context, _ *mcp.CallToolRequest, in verdictsInput) (*mcp.CallToolResult, verdictsOutput, error) {
	q, err := verdictQuery(in)
	if err != nil {
		return nil, verdictsOutput{}, err
	}
	items, err := t.svc.FindVerdicts(ctx, q)
	if err != nil {
		return nil, verdictsOutput{}, err
	}
	out := verdictsOutput{Verdicts: make([]itemView, 0, len(items))}
	for _, it := range items {
		out.Verdicts = append(out.Verdicts, viewItem(t.svc.MainRepo, it))
	}
	return nil, out, nil
}

// verdictQuery checks the verdicts tool's filters against the pair table.
func verdictQuery(in verdictsInput) (store.VerdictQuery, error) {
	q := store.VerdictQuery{
		Recommendation: triage.Recommendation(in.Recommendation), Reason: triage.Reason(in.Reason),
		Kind: triage.Kind(in.Kind), Disposition: store.DispositionState(in.Disposition), Limit: in.Limit,
	}
	if q.Recommendation != "" && !slices.Contains(triage.Recommendations(), q.Recommendation) {
		return q, fmt.Errorf("no such recommendation %q", in.Recommendation)
	}
	if q.Reason != "" {
		rec, ok := triage.RecommendationFor(q.Reason)
		if !ok {
			return q, fmt.Errorf("no such reason %q", in.Reason)
		}
		if q.Recommendation != "" && rec != q.Recommendation {
			return q, fmt.Errorf("%s is a reason for %s, not %s", q.Reason, rec, q.Recommendation)
		}
	}
	if q.Kind != "" && !slices.Contains(triage.Kinds(), q.Kind) {
		return q, fmt.Errorf("kind is issue or pr, not %q", in.Kind)
	}
	switch q.Disposition {
	case "", store.Pending, store.Applied, store.Deferred, store.Rejected, store.AnyDisposition:
	default:
		return q, fmt.Errorf("disposition is pending, applied, deferred, rejected or any, not %q", in.Disposition)
	}
	switch in.Sensitive {
	case "":
	case "only":
		q.Sensitive = new(true)
	case "exclude":
		q.Sensitive = new(false)
	default:
		return q, fmt.Errorf("sensitive is only or exclude, not %q", in.Sensitive)
	}
	return q, nil
}

type searchInput struct {
	Query     string `json:"query,omitempty" jsonschema:"words to find; empty lists notes by the filters alone"`
	Kind      string `json:"kind,omitempty" jsonschema:"only issue, pr or advisory"`
	Tag       string `json:"tag,omitempty" jsonschema:"only notes with this tag"`
	State     string `json:"state,omitempty" jsonschema:"only items in this triage or GitHub state, e.g. triaged, open, draft"`
	Sensitive string `json:"sensitive,omitempty" jsonschema:"only to keep just sensitive items, exclude to leave them out"`
	Limit     int    `json:"limit,omitempty" jsonschema:"most results, default 50"`
}

type hitView struct {
	Item    itemView  `json:"item"`
	Note    *noteView `json:"note,omitempty" jsonschema:"the matching note; absent when the title matched"`
	Snippet string    `json:"snippet" jsonschema:"the match in context, matched words in [brackets]"`
}

type searchOutput struct {
	Hits []hitView `json:"hits"`
}

func (t *tools) search(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	q := store.SearchQuery{Text: in.Query, Kind: triage.Kind(in.Kind), Tag: in.Tag, State: in.State, Limit: in.Limit}
	switch in.Sensitive {
	case "":
	case "only":
		q.Sensitive = new(true)
	case "exclude":
		q.Sensitive = new(false)
	default:
		return nil, searchOutput{}, fmt.Errorf("sensitive is only or exclude, not %q", in.Sensitive)
	}
	hits, err := t.svc.Search(ctx, q)
	if err != nil {
		return nil, searchOutput{}, err
	}
	out := searchOutput{Hits: make([]hitView, 0, len(hits))}
	for _, h := range hits {
		hv := hitView{Item: viewItem(t.svc.MainRepo, &h.Item), Snippet: h.Snippet}
		if h.Note != nil {
			n := viewNote(*h.Note)
			hv.Note = &n
		}
		out.Hits = append(out.Hits, hv)
	}
	return nil, out, nil
}

type addNoteInput struct {
	Ref  string   `json:"ref" jsonschema:"the item: #123, a GitHub URL, a GHSA id, or fork#N"`
	Text string   `json:"text" jsonschema:"the note"`
	Tags []string `json:"tags,omitempty" jsonschema:"e.g. security, backport, customer, blocked"`
}

type addNoteOutput struct {
	Item itemView `json:"item"`
	Note noteView `json:"note"`
}

func (t *tools) addNote(ctx context.Context, _ *mcp.CallToolRequest, in addNoteInput) (*mcp.CallToolResult, addNoteOutput, error) {
	ref, err := t.svc.ParseRef(in.Ref)
	if err != nil {
		return nil, addNoteOutput{}, err
	}
	it, n, err := t.svc.AddNote(ctx, ref, in.Text, in.Tags, store.AuthorClaude)
	if err != nil {
		return nil, addNoteOutput{}, err
	}
	return nil, addNoteOutput{Item: viewItem(t.svc.MainRepo, it), Note: viewNote(*n)}, nil
}

type linkInput struct {
	From string `json:"from" jsonschema:"the item the link starts at"`
	Type string `json:"type" jsonschema:"duplicate_of, fixes, advisory_for, fix_in_fork, backport_of or related"`
	To   string `json:"to" jsonschema:"the item the link points at"`
}

type linkOutput struct {
	From  itemView `json:"from"`
	Type  string   `json:"type"`
	To    itemView `json:"to"`
	Added bool     `json:"added" jsonschema:"false when the link was already there"`
}

func (t *tools) link(ctx context.Context, _ *mcp.CallToolRequest, in linkInput) (*mcp.CallToolResult, linkOutput, error) {
	lt := store.LinkType(in.Type)
	if !slices.Contains(store.LinkTypes(), lt) {
		return nil, linkOutput{}, fmt.Errorf("no such link type %q", in.Type)
	}
	from, err := t.svc.ParseRef(in.From)
	if err != nil {
		return nil, linkOutput{}, err
	}
	to, err := t.svc.ParseRef(in.To)
	if err != nil {
		return nil, linkOutput{}, err
	}
	l, err := t.svc.Link(ctx, from, lt, to, store.AuthorClaude)
	if err != nil {
		return nil, linkOutput{}, err
	}
	return nil, linkOutput{
		From: viewItem(t.svc.MainRepo, &l.From), Type: in.Type, To: viewItem(t.svc.MainRepo, &l.To), Added: l.Added,
	}, nil
}

type advisoriesInput struct {
	All bool `json:"all,omitempty" jsonschema:"include closed and withdrawn advisories"`
}

type advisoryEntry struct {
	Item     itemView     `json:"item"`
	Advisory advisoryView `json:"advisory"`
	Links    []linkView   `json:"links" jsonschema:"private-fork pull requests (fix_in_fork) and anything else linked"`
}

type advisoriesOutput struct {
	Advisories []advisoryEntry `json:"advisories"`
}

func (t *tools) advisories(ctx context.Context, _ *mcp.CallToolRequest, in advisoriesInput) (*mcp.CallToolResult, advisoriesOutput, error) {
	advisories, err := t.svc.Advisories(ctx, in.All)
	if err != nil {
		return nil, advisoriesOutput{}, err
	}
	out := advisoriesOutput{Advisories: make([]advisoryEntry, 0, len(advisories))}
	for _, a := range advisories {
		out.Advisories = append(out.Advisories, advisoryEntry{
			Item:     viewItem(t.svc.MainRepo, &a.Item),
			Advisory: *viewAdvisory(&a.Advisory),
			Links:    viewLinks(t.svc.MainRepo, a.Links),
		})
	}
	return nil, out, nil
}

type dispositionInput struct {
	Ref         string `json:"ref" jsonschema:"the triaged item: #123 or its URL"`
	Disposition string `json:"disposition" jsonschema:"applied, deferred, rejected or pending"`
	Note        string `json:"note,omitempty" jsonschema:"why; required for rejected, and shown to the next triage agent"`
}

func (t *tools) setDisposition(ctx context.Context, _ *mcp.CallToolRequest, in dispositionInput) (*mcp.CallToolResult, itemView, error) {
	state := store.DispositionState(in.Disposition)
	if !slices.Contains([]store.DispositionState{store.Applied, store.Deferred, store.Rejected, store.Pending}, state) {
		return nil, itemView{}, fmt.Errorf("disposition is applied, deferred, rejected or pending, not %q", in.Disposition)
	}
	ref, err := t.svc.ParseRef(in.Ref)
	if err != nil {
		return nil, itemView{}, err
	}
	it, err := t.svc.Decide(ctx, ref, state, in.Note, store.AuthorClaude)
	if err != nil {
		return nil, itemView{}, err
	}
	return nil, viewItem(t.svc.MainRepo, it), nil
}
