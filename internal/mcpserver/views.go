package mcpserver

import (
	"time"

	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
)

// The views are what the tools return: the store's types, flattened for a
// reader that has never seen them, with refs in the form the tools accept.

type itemView struct {
	Ref         string       `json:"ref" jsonschema:"the item's reference, usable as a ref in any tool"`
	Kind        string       `json:"kind" jsonschema:"issue, pr or advisory"`
	Title       string       `json:"title"`
	URL         string       `json:"url"`
	GitHubState string       `json:"github_state,omitempty" jsonschema:"open, closed or merged; an advisory's triage, draft, published, closed or withdrawn"`
	Sensitive   bool         `json:"sensitive" jsonschema:"true for advisories, private-fork PRs and what an advisory is about: never repeat these anywhere public"`
	TriageState string       `json:"triage_state,omitempty" jsonschema:"untriaged, queued, triaged, needs_human or closed_upstream; empty when not in triage"`
	BeadID      string       `json:"bead_id,omitempty"`
	CarryNote   string       `json:"carry_note,omitempty" jsonschema:"what the next triage agent is told, after a rejection or retriage"`
	Verdict     *verdictView `json:"verdict,omitempty" jsonschema:"the verdict in force, while triaged"`
}

type verdictView struct {
	Recommendation   string          `json:"recommendation"`
	Reason           string          `json:"reason"`
	Confidence       *int            `json:"confidence,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	SuggestedComment string          `json:"suggested_comment,omitempty"`
	SuggestedLabels  []string        `json:"suggested_labels,omitempty"`
	Evidence         []string        `json:"evidence,omitempty"`
	DuplicateOf      *int            `json:"duplicate_of,omitempty"`
	FixedIn          string          `json:"fixed_in,omitempty"`
	BeadID           string          `json:"bead_id,omitempty"`
	TriagedAt        *time.Time      `json:"triaged_at,omitempty"`
	Disposition      dispositionView `json:"disposition" jsonschema:"the maintainer's latest decision on this verdict"`
}

type dispositionView struct {
	State  string     `json:"state" jsonschema:"pending, applied, rejected or deferred"`
	Note   string     `json:"note,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	Author string     `json:"author,omitempty" jsonschema:"human or claude"`
}

type noteView struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	Tags      []string  `json:"tags,omitempty"`
	Author    string    `json:"author" jsonschema:"human or claude"`
	CreatedAt time.Time `json:"created_at"`
}

type linkView struct {
	Relation  string    `json:"relation" jsonschema:"the link read from this item's side, e.g. fixes or fixed_by"`
	Item      itemView  `json:"item" jsonschema:"the item at the other end"`
	Author    string    `json:"author" jsonschema:"human, claude or triage-bot"`
	CreatedAt time.Time `json:"created_at"`
}

type advisoryView struct {
	Severity    string `json:"severity,omitempty"`
	CVE         string `json:"cve,omitempty"`
	PrivateFork string `json:"private_fork,omitempty" jsonschema:"owner/name of the advisory's temporary private fork"`
}

type detailView struct {
	Item     itemView      `json:"item"`
	Advisory *advisoryView `json:"advisory,omitempty"`
	History  []verdictView `json:"history" jsonschema:"every verdict, oldest first; the last is current while triaged"`
	Notes    []noteView    `json:"notes"`
	Links    []linkView    `json:"links" jsonschema:"links in both directions"`
}

func viewItem(mainRepo string, it *store.Item) itemView {
	v := itemView{
		Ref:         it.Ref(mainRepo),
		Kind:        string(it.Kind),
		Title:       it.Title,
		URL:         it.HTMLURL(),
		GitHubState: it.GHState,
		Sensitive:   it.Sensitive,
		TriageState: string(it.State),
		BeadID:      it.BeadID,
		CarryNote:   it.CarryNote,
	}
	if it.Verdict != nil {
		vv := viewVerdict(*it.Verdict)
		v.Verdict = &vv
	}
	return v
}

func viewVerdict(v store.Verdict) verdictView {
	r := v.Result
	return verdictView{
		Recommendation:   string(r.Recommendation),
		Reason:           string(r.Reason),
		Confidence:       r.Confidence,
		Reasoning:        r.Reasoning,
		SuggestedComment: r.SuggestedComment,
		SuggestedLabels:  r.SuggestedLabels,
		Evidence:         r.Evidence,
		DuplicateOf:      r.DuplicateOf,
		FixedIn:          r.FixedIn,
		BeadID:           v.BeadID,
		TriagedAt:        v.TriagedAt,
		Disposition: dispositionView{
			State:  string(v.Disposition.State),
			Note:   v.Disposition.Note,
			At:     v.Disposition.At,
			Author: string(v.Disposition.Author),
		},
	}
}

func viewNote(n store.Note) noteView {
	return noteView{ID: n.ID, Body: n.Body, Tags: n.Tags, Author: string(n.Author), CreatedAt: n.CreatedAt}
}

func viewLinks(mainRepo string, links []store.Link) []linkView {
	out := make([]linkView, 0, len(links))
	for _, l := range links {
		out = append(out, linkView{
			Relation:  l.Label(),
			Item:      viewItem(mainRepo, &l.Other),
			Author:    string(l.Author),
			CreatedAt: l.CreatedAt,
		})
	}
	return out
}

func viewAdvisory(a *store.Advisory) *advisoryView {
	if a == nil {
		return nil
	}
	return &advisoryView{Severity: a.Severity, CVE: a.CVE, PrivateFork: a.PrivateFork}
}

func viewDetail(mainRepo string, d *service.Detail) detailView {
	out := detailView{
		Item:     viewItem(mainRepo, &d.Item),
		Advisory: viewAdvisory(d.Advisory),
		History:  make([]verdictView, 0, len(d.History)),
		Notes:    make([]noteView, 0, len(d.Notes)),
		Links:    viewLinks(mainRepo, d.Links),
	}
	for _, v := range d.History {
		out.History = append(out.History, viewVerdict(v))
	}
	for _, n := range d.Notes {
		out.Notes = append(out.Notes, viewNote(n))
	}
	return out
}
