package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/github/githubtest"
	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

const (
	ghsa = "GHSA-aaaa-bbbb-cccc"
	fork = "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc"
)

// connect starts the server over an in-memory transport, on a fresh store and
// a fake GitHub holding made-up items, and returns a client session.
func connect(t *testing.T) (*mcp.ClientSession, *service.Service) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.Update(ctx, func(tx *store.Tx) error {
		s := store.DefaultSettings()
		s.Org, s.Repo = "argoproj", "argo-workflows"
		return tx.SaveSettings(ctx, s)
	}))

	gh := githubtest.New(
		&github.Item{Number: 100, Kind: triage.KindIssue, Title: "Widgets wobble", State: "open", Open: true},
		&github.Item{Number: 200, Kind: triage.KindPR, Title: "Stop the wobble", State: "merged"},
	)
	gh.SetAdvisories(github.Advisory{GHSA: ghsa, Summary: "Made-up leak", State: "triage", Severity: "medium", PrivateFork: fork})
	gh.Pulls[fork] = []github.Pull{{Repo: fork, Number: 3, Title: "Fix the made-up leak", State: "merged"}}

	svc, err := service.New(ctx, st, gh)
	require.NoError(t, err)

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := New(svc).Connect(ctx, serverT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs, svc
}

// call invokes a tool and decodes its structured result into out, failing the
// test if the tool reported an error. out is zeroed first, so a reused
// variable cannot keep fields from an earlier call.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, res.IsError, "%s failed: %s", name, text(res))
	b, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	reflect.ValueOf(out).Elem().SetZero()
	require.NoError(t, json.Unmarshal(b, out))
}

// callErr invokes a tool that should fail, and returns what it said.
func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.True(t, res.IsError, "%s should have failed", name)
	return text(res)
}

func text(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestToolsAreListed(t *testing.T) {
	t.Parallel()

	cs, _ := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"show", "search", "add_note", "link", "advisories", "set_disposition"}, names)
	assert.Contains(t, cs.InitializeResult().Instructions, "never repeat them")
}

// A session may add notes without asking; they are recorded as claude's, and
// an item it has not seen is brought in from GitHub.
func TestAddNoteAndShow(t *testing.T) {
	t.Parallel()

	cs, _ := connect(t)
	var added addNoteOutput
	call(t, cs, "add_note", map[string]any{"ref": "#100", "text": "reproduced on main today", "tags": []string{"Repro"}}, &added)
	assert.Equal(t, "#100", added.Item.Ref)
	assert.Equal(t, "Widgets wobble", added.Item.Title)
	assert.Equal(t, "claude", added.Note.Author)
	assert.Equal(t, []string{"repro"}, added.Note.Tags)

	var d detailView
	call(t, cs, "show", map[string]any{"ref": "https://github.com/argoproj/argo-workflows/issues/100"}, &d)
	require.Len(t, d.Notes, 1)
	assert.Equal(t, "reproduced on main today", d.Notes[0].Body)
	assert.Empty(t, d.Item.TriageState, "a noted item is not queued for triage")
	assert.NotNil(t, d.History, "lists are empty, not null")
}

func TestLinkAndAdvisories(t *testing.T) {
	t.Parallel()

	cs, _ := connect(t)
	var l linkOutput
	call(t, cs, "link", map[string]any{"from": "#100", "type": "advisory_for", "to": ghsa}, &l)
	assert.True(t, l.Added)
	assert.Equal(t, ghsa, l.From.Ref, "the advisory goes first")
	assert.True(t, l.To.Sensitive, "what an advisory is about is sensitive")

	call(t, cs, "link", map[string]any{"from": ghsa, "type": "fix_in_fork", "to": "argo-workflows-ghsa-aaaa-bbbb-cccc#3"}, &l)
	assert.Equal(t, fork+"#3", l.To.Ref)

	var out advisoriesOutput
	call(t, cs, "advisories", map[string]any{}, &out)
	require.Len(t, out.Advisories, 1)
	a := out.Advisories[0]
	assert.Equal(t, ghsa, a.Item.Ref)
	assert.Equal(t, "medium", a.Advisory.Severity)
	assert.Equal(t, fork, a.Advisory.PrivateFork)
	relations := make([]string, 0, len(a.Links))
	for _, l := range a.Links {
		relations = append(relations, l.Relation+" "+l.Item.Ref)
	}
	assert.ElementsMatch(t, []string{"advisory_for #100", "fix_in_fork " + fork + "#3"}, relations)

	assert.Contains(t, callErr(t, cs, "link", map[string]any{"from": "#100", "type": "blocks", "to": "#200"}),
		"no such link type")
}

func TestSearch(t *testing.T) {
	t.Parallel()

	cs, _ := connect(t)
	var added addNoteOutput
	call(t, cs, "add_note", map[string]any{"ref": ghsa, "text": "embargo lifts after the release", "tags": []string{"security"}}, &added)
	call(t, cs, "add_note", map[string]any{"ref": "#100", "text": "release blocker for 3.7"}, &added)

	var out searchOutput
	call(t, cs, "search", map[string]any{"query": "release"}, &out)
	assert.Len(t, out.Hits, 2)

	call(t, cs, "search", map[string]any{"query": "release", "sensitive": "exclude"}, &out)
	require.Len(t, out.Hits, 1)
	assert.Equal(t, "#100", out.Hits[0].Item.Ref)
	require.NotNil(t, out.Hits[0].Note)
	assert.Contains(t, out.Hits[0].Snippet, "[release]")

	call(t, cs, "search", map[string]any{"tag": "security"}, &out)
	require.Len(t, out.Hits, 1)
	assert.Equal(t, ghsa, out.Hits[0].Item.Ref)

	call(t, cs, "search", map[string]any{"query": "wobble"}, &out)
	require.Len(t, out.Hits, 1)
	assert.Nil(t, out.Hits[0].Note, "a title match carries no note")

	assert.Contains(t, callErr(t, cs, "search", map[string]any{"query": "x", "sensitive": "maybe"}), "only or exclude")
}

func TestSetDisposition(t *testing.T) {
	t.Parallel()

	cs, svc := connect(t)
	ctx := context.Background()
	require.NoError(t, svc.Store.Update(ctx, func(tx *store.Tx) error {
		id, err := tx.InsertItem(ctx, &store.Item{
			Repo: "argoproj/argo-workflows", Kind: triage.KindIssue, Number: 100, State: store.Queued, BeadID: "tb-1",
		})
		if err != nil {
			return err
		}
		return tx.RecordVerdict(ctx, id, "tb-1", &triage.Result{
			Recommendation: triage.Close, Reason: triage.Stale, Confidence: new(70), Reasoning: "dormant",
		}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	}))

	assert.Contains(t, callErr(t, cs, "set_disposition", map[string]any{"ref": "#100", "disposition": "rejected"}),
		"needs a note")
	assert.Contains(t, callErr(t, cs, "set_disposition", map[string]any{"ref": "#100", "disposition": "done"}),
		"applied, deferred, rejected or pending")

	var it itemView
	call(t, cs, "set_disposition", map[string]any{"ref": "#100", "disposition": "deferred", "note": "after 3.7"}, &it)
	require.NotNil(t, it.Verdict)
	assert.Equal(t, "deferred", it.Verdict.Disposition.State)
	assert.Equal(t, "claude", it.Verdict.Disposition.Author)

	call(t, cs, "set_disposition", map[string]any{"ref": "#100", "disposition": "rejected", "note": "still happens"}, &it)
	assert.Equal(t, "untriaged", it.TriageState)
	assert.Equal(t, "still happens", it.CarryNote)

	assert.Contains(t, callErr(t, cs, "set_disposition", map[string]any{"ref": "#100", "disposition": "applied"}),
		"not triaged")
}

func TestShowReportsAnUnknownItem(t *testing.T) {
	t.Parallel()

	cs, _ := connect(t)
	assert.Contains(t, callErr(t, cs, "show", map[string]any{"ref": "#4242"}), "#4242 is not in the store")
	assert.Contains(t, callErr(t, cs, "show", map[string]any{"ref": "kubernetes/kubernetes#1"}), "bad reference")
}
