// Package beads wraps the bd CLI.
//
// triage-bot only ever opens beads and reads their state; it never actions
// them. The one exception is reopening a bead whose completion template did not
// validate, which is a correction to our own request rather than triage work.
package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Bead is the subset of bd's JSON output triage-bot uses.
//
// CloseReason carries the completion template. It is returned by both
// `bd show --json` and `bd query --json`, so reconciling a batch of closed
// beads is a single call rather than an N+1.
type Bead struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	CloseReason string   `json:"close_reason"`
	ExternalRef string   `json:"external_ref"`
	Labels      []string `json:"labels"`
	Notes       string   `json:"notes"`
}

// Closed reports whether the bead is in bd's closed status.
func (b Bead) Closed() bool { return b.Status == "closed" }

// CreateRequest describes a triage bead to open.
type CreateRequest struct {
	Title string
	Body  string
	Label string
	// ExternalRef ties the bead back to its GitHub item ("gh-8123") so the
	// mapping survives loss of the status file.
	ExternalRef string
}

// Client is the bd surface triage-bot needs. It is an interface so the sync
// logic can be tested without a database.
type Client interface {
	// Create opens a bead and returns its ID.
	Create(ctx context.Context, req CreateRequest) (string, error)
	// Query runs a bd query expression and returns the matching beads.
	Query(ctx context.Context, expr string) ([]Bead, error)
	// Note appends to a bead's notes. Notes are visible in `bd show`, which is
	// how validation errors reach the agent: `bd reopen --reason` text is
	// recorded only as an event and is not shown anywhere the agent looks.
	Note(ctx context.Context, id, text string) error
	// Reopen returns a bead to open status, for the audit trail.
	Reopen(ctx context.Context, id, reason string) error
}

// CLI is a Client backed by the bd binary.
type CLI struct {
	// Bin is the bd executable; empty means "bd" on PATH.
	Bin string
	// Dir is the working directory, which is how bd discovers its database.
	// Empty means the current process's directory.
	Dir string
}

var _ Client = (*CLI)(nil)

func (c *CLI) bin() string {
	if c.Bin == "" {
		return "bd"
	}
	return c.Bin
}

// run executes bd and returns stdout. stderr is folded into the error, since
// bd reports failures there.
func (c *CLI) run(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin(), args...) //nolint:gosec // args are constructed here, not user-supplied
	cmd.Dir = c.Dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("bd %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}

// Available checks that bd is runnable and has a database, so a misconfigured
// deployment fails at startup rather than mid-tick.
func (c *CLI) Available(ctx context.Context) error {
	if _, err := c.run(ctx, "", "where"); err != nil {
		return fmt.Errorf("bd is not usable (is it on PATH, and has `bd init` been run?): %w", err)
	}
	return nil
}

// Create opens a bead and returns its ID.
func (c *CLI) Create(ctx context.Context, req CreateRequest) (string, error) {
	args := []string{"create", req.Title, "--type", "task", "--silent", "--body-file", "-"}
	if req.Label != "" {
		args = append(args, "--label", req.Label)
	}
	if req.ExternalRef != "" {
		args = append(args, "--external-ref", req.ExternalRef)
	}

	out, err := c.run(ctx, req.Body, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", fmt.Errorf("bd create returned no issue ID")
	}
	return id, nil
}

// Query runs a bd query expression and returns every matching bead.
func (c *CLI) Query(ctx context.Context, expr string) ([]Bead, error) {
	out, err := c.run(ctx, "", queryArgs(expr)...)
	if err != nil {
		return nil, err
	}
	return decodeBeads(out)
}

// queryArgs builds the bd invocation for a query.
//
// The explicit --limit 0 matters: bd defaults to 50 results and says nothing
// when it truncates. Reconcile matches queued items against closed beads, so a
// silently-dropped bead would leave its item queued forever, holding a work
// slot that never frees. The cap is invisible until the backlog of closed beads
// grows past it, at which point the bot quietly stops making progress.
func queryArgs(expr string) []string {
	return []string{"query", expr, "--json", "--limit", "0"}
}

// Note appends to a bead's notes.
func (c *CLI) Note(ctx context.Context, id, text string) error {
	_, err := c.run(ctx, text, "note", id, "--stdin")
	return err
}

// Reopen returns a bead to open status.
func (c *CLI) Reopen(ctx context.Context, id, reason string) error {
	_, err := c.run(ctx, "", "reopen", id, "--reason", reason)
	return err
}

// decodeBeads parses bd's JSON array output, tolerating the empty output some
// commands produce instead of "[]".
func decodeBeads(out string) ([]Bead, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	var beads []Bead
	if err := json.Unmarshal([]byte(trimmed), &beads); err != nil {
		return nil, fmt.Errorf("failed to parse bd JSON output: %w", err)
	}
	return beads, nil
}
