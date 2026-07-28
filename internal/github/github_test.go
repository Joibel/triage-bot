package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// Discovery must use the issues-list endpoint, not search: search now rejects
// any query that does not commit to is:issue or is:pull-request, and we need
// both kinds from one call.
func TestListUpdatedSinceUsesListEndpointNotSearch(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})

	since := time.Date(2023, 4, 11, 9, 12, 0, 0, time.UTC)
	_, err := api.ListUpdatedSince(context.Background(), &since, 10)
	require.NoError(t, err)

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

	_, err := api.ListUpdatedSince(context.Background(), nil, 10)
	require.NoError(t, err)
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

	items, err := api.ListUpdatedSince(context.Background(), nil, 10)
	require.NoError(t, err)
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

// The limit must be honoured across pages, so discovery never pulls more of the
// backlog than the queue needs.
func TestListUpdatedSinceHonoursLimit(t *testing.T) {
	t.Parallel()

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://example.com/?page=2>; rel="next"`)
		fmt.Fprint(w, `[
			{"number":1,"state":"open","created_at":"2022-01-01T00:00:00Z"},
			{"number":2,"state":"open","created_at":"2022-01-02T00:00:00Z"},
			{"number":3,"state":"open","created_at":"2022-01-03T00:00:00Z"}
		]`)
	})

	items, err := api.ListUpdatedSince(context.Background(), nil, 2)
	require.NoError(t, err)
	assert.Len(t, items, 2)
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
