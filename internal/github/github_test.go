package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/triage"
)

// request is one call the client made to the fake GitHub.
type request struct {
	path  string
	query url.Values
}

// serve stands up a fake GitHub and points a client at it. It records the
// requests the client sent, so tests can assert on how they were built.
func serve(t *testing.T, handler http.HandlerFunc) (*API, *[]request) {
	t.Helper()

	var seen []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, request{path: r.URL.Path, query: r.URL.Query()})
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	api := New("argoproj", "argo-workflows", "")
	base, err := url.Parse(srv.URL + "/")
	require.NoError(t, err)
	api.client.BaseURL = base
	return api, &seen
}

// collect is a yield that gathers every item and never asks to stop.
func collect(out *[]Item) func(Item) bool {
	return func(it Item) bool {
		*out = append(*out, it)
		return true
	}
}

// Discovery must use the issues-list endpoint, not search: search now rejects
// any query that does not commit to is:issue or is:pull-request, and we need
// both kinds from one call.
func TestListUpdatedSinceUsesListEndpointNotSearch(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})

	var got []Item
	since := time.Date(2023, 4, 11, 9, 12, 0, 0, time.UTC)
	require.NoError(t, api.ListUpdatedSince(context.Background(), &since, collect(&got)))

	require.Len(t, *seen, 1)
	r := (*seen)[0]
	assert.Equal(t, "/repos/argoproj/argo-workflows/issues", r.path,
		"must not use /search/issues, which requires is:issue or is:pull-request")
	assert.Equal(t, "open", r.query.Get("state"))
	assert.Equal(t, "updated", r.query.Get("sort"), "stale triage orders by last activity, not creation")
	assert.Equal(t, "asc", r.query.Get("direction"), "least recently updated first is the whole point")
	assert.Equal(t, "2023-04-11T09:12:00Z", r.query.Get("since"), "the cursor bound is what makes discovery incremental")
}

// With no cursor the request must not carry a since bound at all.
func TestListUpdatedSinceWithoutCursor(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})

	var got []Item
	require.NoError(t, api.ListUpdatedSince(context.Background(), nil, collect(&got)))
	assert.Empty(t, (*seen)[0].query.Get("since"))
}

// GitHub models pull requests as issues carrying a pull_request block, which is
// how one query covers both kinds. Getting this wrong would apply issue-only
// triage reasons to PRs.
func TestConvertDistinguishesIssuesFromPRs(t *testing.T) {
	t.Parallel()

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[
			{"number":8123,"title":"a bug","state":"open","comments":7,
			 "created_at":"2022-03-14T10:22:11Z","updated_at":"2022-09-02T14:03:00Z",
			 "user":{"login":"reporter"},
			 "labels":[{"name":"area/controller"},{"name":"type/bug"}],
			 "html_url":"https://github.com/argoproj/argo-workflows/issues/8123"},
			{"number":9500,"title":"a change","state":"open","comments":2,
			 "created_at":"2022-04-01T10:00:00Z","updated_at":"2022-04-02T10:00:00Z",
			 "user":{"login":"contributor"},
			 "pull_request":{"url":"https://api.github.com/repos/o/r/pulls/9500"},
			 "html_url":"https://github.com/argoproj/argo-workflows/pull/9500"}
		]`)
	})

	var items []Item
	require.NoError(t, api.ListUpdatedSince(context.Background(), nil, collect(&items)))
	require.Len(t, items, 2)

	assert.Equal(t, 8123, items[0].Number)
	assert.Equal(t, triage.KindIssue, items[0].Kind)
	assert.Equal(t, "reporter", items[0].Author)
	assert.Equal(t, []string{"area/controller", "type/bug"}, items[0].Labels)
	assert.Equal(t, 7, items[0].Comments)
	assert.True(t, items[0].Open)
	assert.Equal(t, time.Date(2022, 3, 14, 10, 22, 11, 0, time.UTC), items[0].CreatedAt)

	assert.Equal(t, triage.KindPR, items[1].Kind, "a pull_request block makes it a PR")
}

// A caller that stops walking must stop the paging with it, so discovery never
// pulls more of the backlog than the queue needs.
func TestListUpdatedSinceStopsWhenTheCallerDoes(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://example.com/?page=2>; rel="next"`)
		fmt.Fprint(w, `[
			{"number":1,"state":"open","created_at":"2022-01-01T00:00:00Z"},
			{"number":2,"state":"open","created_at":"2022-01-02T00:00:00Z"},
			{"number":3,"state":"open","created_at":"2022-01-03T00:00:00Z"}
		]`)
	})

	var items []Item
	require.NoError(t, api.ListUpdatedSince(context.Background(), nil, func(it Item) bool {
		items = append(items, it)
		return len(items) < 2
	}))

	assert.Len(t, items, 2, "yield must not be called again after it returns false")
	assert.Len(t, *seen, 1, "a next-page link must not be followed once the caller has stopped")
}

// A caller that keeps walking must be paged through to the end, which is what
// lets it step over a block of items sharing one updated_at that is larger than
// a single page.
func TestListUpdatedSinceFollowsPagesUntilTheCallerStops(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<https://example.com/?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"number":1,"state":"open"},{"number":2,"state":"open"}]`)
			return
		}
		fmt.Fprint(w, `[{"number":3,"state":"open"}]`)
	})

	var items []Item
	require.NoError(t, api.ListUpdatedSince(context.Background(), nil, collect(&items)))

	assert.Len(t, *seen, 2, "the second page must be fetched")
	require.Len(t, items, 3)
	assert.Equal(t, 3, items[2].Number, "items from later pages must reach the caller")
}

// A closed item must report Open false: this is the check that stops a triage
// bead being spent on something a human already dealt with.
func TestGetReportsClosedState(t *testing.T) {
	t.Parallel()

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":8123,"title":"gone","state":"closed",
			"created_at":"2022-03-14T10:22:11Z","updated_at":"2023-01-01T00:00:00Z"}`)
	})

	item, err := api.Get(context.Background(), 8123)
	require.NoError(t, err)
	assert.False(t, item.Open)
	assert.Equal(t, 8123, item.Number)
}

func TestGetPropagatesErrors(t *testing.T) {
	t.Parallel()

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})

	_, err := api.Get(context.Background(), 404)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "argo-workflows#404")
}

// The client must stay read-only. Every method on it reads; a new one that
// does anything else has to get past this list first.
func TestClientHasNoWriteMethods(t *testing.T) {
	t.Parallel()

	for m := range reflect.TypeFor[*API]().Methods() {
		name := m.Name
		assert.True(t, strings.HasPrefix(name, "Get") || strings.HasPrefix(name, "List"),
			"%s: the GitHub client only reads", name)
	}
}

// One listing, in every state, cursor-paged: closed advisories come back too,
// which is how the sync learns that one it holds has closed.
func TestListAdvisoriesPagesThroughEveryState(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			w.Header().Set("Link", `<https://example.com/?after=cursor1>; rel="next"`)
			fmt.Fprint(w, `[{"ghsa_id":"GHSA-aaaa-bbbb-cccc","cve_id":"CVE-2026-0001","summary":"Made-up leak",
				"severity":"high","state":"draft","html_url":"https://github.com/argoproj/argo-workflows/security/advisories/GHSA-aaaa-bbbb-cccc",
				"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z",
				"private_fork":{"full_name":"argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc"}}]`)
			return
		}
		fmt.Fprint(w, `[{"ghsa_id":"GHSA-dddd-eeee-ffff","state":"closed"}]`)
	})

	got, err := api.ListAdvisories(context.Background())
	require.NoError(t, err)

	require.Len(t, *seen, 2, "the second page must be fetched")
	assert.Equal(t, "/repos/argoproj/argo-workflows/security-advisories", (*seen)[0].path)
	assert.Empty(t, (*seen)[0].query.Get("state"), "every state is listed, not just open ones")
	assert.Equal(t, "cursor1", (*seen)[1].query.Get("after"))

	require.Len(t, got, 2)
	assert.Equal(t, Advisory{
		GHSA: "GHSA-aaaa-bbbb-cccc", CVE: "CVE-2026-0001", Summary: "Made-up leak", Severity: "high", State: "draft",
		URL:         "https://github.com/argoproj/argo-workflows/security/advisories/GHSA-aaaa-bbbb-cccc",
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		PrivateFork: "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc",
	}, got[0])
	assert.Equal(t, "closed", got[1].State)
}

// A token that cannot read advisories must be told apart from a failure, so
// the sync can skip quietly and leave triage running.
func TestNoAccessIsRecognisable(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"no"}`)
			})

			_, err := api.ListAdvisories(context.Background())
			require.ErrorIs(t, err, ErrNoAccess)
			_, err = api.ListPulls(context.Background(), "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc")
			require.ErrorIs(t, err, ErrNoAccess)
		})
	}

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"boom"}`)
	})
	_, err := api.ListAdvisories(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoAccess, "a server error is a failure, not a refusal")
}

func TestListPullsReadsEveryStateAndMerges(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[
			{"number":1,"title":"Fix the leak","state":"closed","merged_at":"2026-01-03T00:00:00Z"},
			{"number":2,"title":"Backport","state":"open"}
		]`)
	})

	got, err := api.ListPulls(context.Background(), "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc")
	require.NoError(t, err)

	assert.Equal(t, "/repos/argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc/pulls", (*seen)[0].path)
	assert.Equal(t, "all", (*seen)[0].query.Get("state"))
	require.Len(t, got, 2)
	assert.Equal(t, "merged", got[0].State)
	assert.Equal(t, "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc", got[0].Repo)
	assert.Equal(t, "open", got[1].State)
}

func TestGetAdvisory(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ghsa_id":"GHSA-aaaa-bbbb-cccc","summary":"Made-up leak","state":"triage"}`)
	})

	got, err := api.GetAdvisory(context.Background(), "GHSA-aaaa-bbbb-cccc")
	require.NoError(t, err)
	assert.Equal(t, "/repos/argoproj/argo-workflows/security-advisories/GHSA-aaaa-bbbb-cccc", (*seen)[0].path)
	assert.Equal(t, "Made-up leak", got.Summary)
}

// A merged pull request reached through the issues endpoint reports merged,
// not merely closed.
func TestGetReportsMerged(t *testing.T) {
	t.Parallel()

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":9000,"state":"closed","pull_request":{"merged_at":"2026-01-01T00:00:00Z"}}`)
	})

	item, err := api.Get(context.Background(), 9000)
	require.NoError(t, err)
	assert.Equal(t, "merged", item.State)
	assert.False(t, item.Open)
}
