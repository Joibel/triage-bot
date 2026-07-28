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

// serve stands up a fake GitHub and points a client at it. It records the query
// strings the client sent, so tests can assert on how the request was built.
func serve(t *testing.T, handler http.HandlerFunc) (*API, *[]url.Values) {
	t.Helper()

	var seen []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query())
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

// The search query must scope to the repo, restrict to open items, and express
// the cursor bound - that last part is what makes discovery incremental.
func TestListCreatedSinceBuildsQuery(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	})

	since := time.Date(2023, 4, 11, 9, 12, 0, 0, time.UTC)
	_, err := api.ListCreatedSince(context.Background(), &since, 10)
	require.NoError(t, err)

	require.Len(t, *seen, 1)
	q := (*seen)[0]
	assert.Equal(t, "repo:argoproj/argo-workflows is:open created:>=2023-04-11T09:12:00Z", q.Get("q"))
	assert.Equal(t, "created", q.Get("sort"))
	assert.Equal(t, "asc", q.Get("order"), "oldest first is the whole point")
}

// With no cursor the query must not carry a created bound at all.
func TestListCreatedSinceWithoutCursor(t *testing.T) {
	t.Parallel()

	api, seen := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":0,"items":[]}`)
	})

	_, err := api.ListCreatedSince(context.Background(), nil, 10)
	require.NoError(t, err)
	assert.Equal(t, "repo:argoproj/argo-workflows is:open", (*seen)[0].Get("q"))
}

// GitHub models pull requests as issues carrying a pull_request block, which is
// how one query covers both kinds. Getting this wrong would apply issue-only
// triage reasons to PRs.
func TestConvertDistinguishesIssuesFromPRs(t *testing.T) {
	t.Parallel()

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":2,"items":[
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
		]}`)
	})

	items, err := api.ListCreatedSince(context.Background(), nil, 10)
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
func TestListCreatedSinceHonoursLimit(t *testing.T) {
	t.Parallel()

	api, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://example.com/?page=2>; rel="next"`)
		fmt.Fprint(w, `{"total_count":100,"items":[
			{"number":1,"state":"open","created_at":"2022-01-01T00:00:00Z"},
			{"number":2,"state":"open","created_at":"2022-01-02T00:00:00Z"},
			{"number":3,"state":"open","created_at":"2022-01-03T00:00:00Z"}
		]}`)
	})

	items, err := api.ListCreatedSince(context.Background(), nil, 2)
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
