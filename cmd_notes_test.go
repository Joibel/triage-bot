package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/github/githubtest"
	"github.com/Joibel/triage-bot/internal/store"
	"github.com/Joibel/triage-bot/internal/triage"
)

const (
	testFork = "argoproj/argo-workflows-ghsa-aaaa-bbbb-cccc"
	testGHSA = "GHSA-aaaa-bbbb-cccc"
)

// cli runs commands against one database and a fake GitHub holding made-up
// items.
type cli struct {
	db string
	gh *githubtest.Fake
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	db := filepath.Join(t.TempDir(), "triage.db")
	gh := githubtest.New(
		&github.Item{Number: 100, Kind: triage.KindIssue, Title: "Widgets wobble", State: "open", Open: true},
		&github.Item{Number: 200, Kind: triage.KindPR, Title: "Stop the wobble", State: "merged"},
	)
	gh.SetAdvisories(github.Advisory{GHSA: testGHSA, Summary: "Made-up leak", State: "draft", PrivateFork: testFork})
	gh.Pulls[testFork] = []github.Pull{{Repo: testFork, Number: 1, Title: "Fix the made-up leak", State: "merged"}}

	c := &cli{db: db, gh: gh}
	c.run(t, "", "init", "--org", "argoproj", "--repo", "argo-workflows")
	return c
}

// run executes one command and returns what it wrote. Commands that print
// with fmt.Printf write to the real stdout, so that is captured too.
func (c *cli) run(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, err := c.try(t, stdin, args...)
	require.NoError(t, err, out)
	return out
}

func (c *cli) try(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	o := &opts{gitHub: func(store.Settings) github.Client { return c.gh }}
	root := newRoot(o)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--db", c.db}, args...))

	var err error
	stdout := captureStdout(t, func() { err = root.ExecuteContext(context.Background()) })
	if err != nil {
		return buf.String() + stdout, fmt.Errorf("triage-bot %s: %w", strings.Join(args, " "), err)
	}
	return buf.String() + stdout, nil
}

func TestNoteLinkShowSearch(t *testing.T) {
	c := newCLI(t)

	assert.Contains(t, c.run(t, "", "note", "#100", "needs", "a", "backport", "-t", "backport"),
		"Noted on #100 [backport]")
	assert.Contains(t, c.run(t, "remembered from stdin\n", "note", "100", "-"), "Noted on #100")
	assert.Contains(t, c.run(t, "", "link", "#200", "fixes", "#100"), "Linked #200 fixes #100")
	assert.Contains(t, c.run(t, "", "link", "#200", "fixes", "#100"), "Already linked: #200 fixes #100")

	show := c.run(t, "", "show", "https://github.com/argoproj/argo-workflows/issues/100")
	assert.Contains(t, show, "#100  issue  open")
	assert.Contains(t, show, "Widgets wobble")
	assert.Contains(t, show, "fixed_by")
	assert.Contains(t, show, "Stop the wobble")
	assert.Contains(t, show, "needs a backport")
	assert.Contains(t, show, "remembered from stdin")
	assert.NotContains(t, show, "Triage:", "a noted item is not in the triage pipeline")

	notes := c.run(t, "", "notes", "#100")
	assert.Contains(t, notes, "human [backport]")

	assert.Contains(t, c.run(t, "", "search", "backport"), "#100")
	assert.Contains(t, c.run(t, "", "search", "--tag", "backport"), "needs a backport")
	assert.Contains(t, c.run(t, "", "search", "wobble"), "title")
	assert.Contains(t, c.run(t, "", "search", "nothing-like-this"), "Nothing found.")
}

func TestAdvisoryNotesAndLinksAreSensitive(t *testing.T) {
	c := newCLI(t)

	c.run(t, "", "note", testGHSA, "fix agreed with the reporter", "-t", "security")
	assert.Contains(t, c.run(t, "", "link", "#100", "advisory_for", testGHSA),
		"Linked "+testGHSA+" advisory_for #100", "the advisory goes first")
	c.run(t, "", "link", testGHSA, "fix_in_fork", "argo-workflows-ghsa-aaaa-bbbb-cccc#1")

	show := c.run(t, "", "show", "#100")
	assert.Contains(t, show, "[sensitive]", "an item an advisory is about is sensitive")
	assert.Contains(t, show, "has_advisory")

	advisories := c.run(t, "", "advisories")
	assert.Contains(t, advisories, testGHSA+"  draft")
	assert.Contains(t, advisories, "private fork "+testFork)
	assert.Contains(t, advisories, "fix_in_fork")
	assert.Contains(t, advisories, "Fix the made-up leak")

	assert.Contains(t, c.run(t, "", "search", "--public", "reporter"), "Nothing found.")
	assert.Contains(t, c.run(t, "", "search", "--sensitive", "reporter"), testGHSA)
}

// A bulk export leaves out anything sensitive: here a verdict on an issue an
// advisory is about.
func TestReportJSONLeavesOutSensitiveItems(t *testing.T) {
	c := newCLI(t)
	seedTriaged(t, c.db, 100)
	seedTriaged(t, c.db, 300)
	c.run(t, "", "link", testGHSA, "advisory_for", "#100")

	var got []map[string]any
	require.NoError(t, json.Unmarshal([]byte(c.run(t, "", "report", "--json")), &got))
	require.Len(t, got, 1)
	assert.EqualValues(t, 300, got[0]["Number"], "only the public verdict is exported")

	// The export keeps the shape the status file's items encoded to.
	for _, key := range []string{"Number", "Kind", "Title", "State", "BeadID", "TriagedAt", "Result", "Human"} {
		assert.Contains(t, got[0], key)
	}
	human, ok := got[0]["Human"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "pending", human["State"])
}

func TestLinkRefusesAnUnknownType(t *testing.T) {
	c := newCLI(t)
	out, err := c.try(t, "", "link", "#100", "blocks", "#200")
	require.Error(t, err)
	assert.Contains(t, out, "no such link type")
}

// seedTriaged records a pending verdict on a main-repository item.
func seedTriaged(t *testing.T, db string, number int) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	require.NoError(t, st.Update(ctx, func(tx *store.Tx) error {
		id, err := tx.InsertItem(ctx, &store.Item{
			Repo: "argoproj/argo-workflows", Kind: triage.KindIssue, Number: number, State: store.Queued, BeadID: "tb-1",
		})
		if err != nil {
			return err
		}
		return tx.RecordVerdict(ctx, id, "tb-1", &triage.Result{
			Recommendation: triage.Close, Reason: triage.Stale, Confidence: new(70), Reasoning: "dormant",
		}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	}))
}

// captureStdout runs fn with os.Stdout redirected, and returns what it wrote.
// Tests using it must not run in parallel, since stdout is process-wide.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done
}
