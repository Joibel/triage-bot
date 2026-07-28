// Package github provides the read-only GitHub access triage-bot needs.
//
// There are deliberately no write methods in this package. triage-bot never
// comments, labels, or closes anything: every recommendation is delivered to a
// human, who decides and acts. Keeping writes absent from the client makes that
// guarantee structural rather than a matter of discipline.
package github

import (
	"context"
	"fmt"
	"net/http"
	"time"

	gh "github.com/google/go-github/v75/github"

	"github.com/Joibel/triage-bot/internal/triage"
)

// Item is the metadata triage-bot holds about a GitHub issue or pull request.
// The body and comments are deliberately absent: beads carry a pointer plus
// this metadata, and the actioning agent fetches the content itself.
type Item struct {
	Number    int
	Kind      triage.Kind
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Labels    []string
	Comments  int
	Author    string
	URL       string
	Open      bool
}

// Client is the read-only GitHub surface triage-bot needs, as an interface so
// sync can be tested without network access.
type Client interface {
	// ListUpdatedSince returns open items last updated at or after since,
	// least-recently-updated first, up to limit. A nil since means "from the
	// beginning of the repo".
	ListUpdatedSince(ctx context.Context, since *time.Time, limit int) ([]Item, error)
	// Get fetches one item by number, whatever its state. Used to confirm an
	// item is still open immediately before spending a bead on it.
	Get(ctx context.Context, number int) (*Item, error)
}

// API is a Client backed by the GitHub REST API.
type API struct {
	client *gh.Client
	org    string
	repo   string
}

var _ Client = (*API)(nil)

// New builds a client for one repository. token may be empty, but the
// unauthenticated rate limit makes that useful only for experiments.
func New(org, repo, token string) *API {
	c := gh.NewClient(&http.Client{Timeout: 30 * time.Second})
	if token != "" {
		c = c.WithAuthToken(token)
	}
	return &API{client: c, org: org, repo: repo}
}

// pageSize is GitHub's maximum results per page.
const pageSize = 100

// ListUpdatedSince returns open items last updated at or after since,
// least-recently-updated first.
//
// Ordering by last activity rather than creation date is what makes this stale
// triage: a 2018 issue people still discuss is not neglected, whereas a 2023
// issue nobody has touched since is. Sorting ascending means genuinely dormant
// items come first and actively-discussed ones are never reached until the
// backlog is exhausted.
//
// This uses the issues-list endpoint rather than the search API. Search would
// express the same thing, but it now rejects any query that does not commit to
// is:issue or is:pull-request, which would mean two queries and a merge to
// cover both kinds. The list endpoint's `since` filters on updated_at, which is
// exactly the cursor bound we want, and it returns issues and pull requests
// together. It also has a far higher rate limit than search (5000/hr against
// 30/min), no 1000-result ceiling, and no search-index lag.
//
// Discovery stays incremental either way: we never re-page the part of the
// backlog we already hold.
func (a *API) ListUpdatedSince(ctx context.Context, since *time.Time, limit int) ([]Item, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "updated",
		Direction:   "asc",
		ListOptions: gh.ListOptions{PerPage: pageSize},
	}
	if since != nil {
		opts.Since = since.UTC()
	}

	var out []Item
	for {
		if remaining := limit - len(out); remaining < opts.ListOptions.PerPage {
			opts.ListOptions.PerPage = max(remaining, 1)
		}

		issues, resp, err := a.client.Issues.ListByRepo(ctx, a.org, a.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list %s/%s issues: %w", a.org, a.repo, err)
		}
		for _, issue := range issues {
			out = append(out, convert(issue))
			if len(out) >= limit {
				return out, nil
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

// Get fetches one item by number, whatever its state.
func (a *API) Get(ctx context.Context, number int) (*Item, error) {
	issue, _, err := a.client.Issues.Get(ctx, a.org, a.repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s/%s#%d: %w", a.org, a.repo, number, err)
	}
	item := convert(issue)
	return &item, nil
}

// convert maps go-github's issue representation onto our own. GitHub models
// pull requests as issues carrying a PullRequestLinks block, which is how one
// query covers both kinds.
func convert(issue *gh.Issue) Item {
	kind := triage.KindIssue
	if issue.IsPullRequest() {
		kind = triage.KindPR
	}

	labels := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, l.GetName())
	}

	return Item{
		Number:    issue.GetNumber(),
		Kind:      kind,
		Title:     issue.GetTitle(),
		CreatedAt: issue.GetCreatedAt().Time,
		UpdatedAt: issue.GetUpdatedAt().Time,
		Labels:    labels,
		Comments:  issue.GetComments(),
		Author:    issue.GetUser().GetLogin(),
		URL:       issue.GetHTMLURL(),
		Open:      issue.GetState() == "open",
	}
}
