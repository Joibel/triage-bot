// Command triage-bot drives AI-assisted stale triage of a GitHub repository's
// most neglected open issues and pull requests, and keeps what we know about
// its items: verdicts, decisions, notes, links and security advisories.
//
// It opens a bounded number of beads for assessment, parses the completion
// template each closed bead carries, and records the verdict in a local SQLite
// database for a human to action. It never writes to GitHub.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/buildinfo"
	"github.com/Joibel/triage-bot/internal/engine"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/service"
	"github.com/Joibel/triage-bot/internal/store"
)

// opts holds the flags every subcommand shares. It is threaded through command
// constructors rather than kept in package globals.
type opts struct {
	db       string
	beadsDir string
	verbose  bool

	// gitHub, when set, replaces the real GitHub client; tests use it.
	gitHub func(store.Settings) github.Client
}

func main() {
	if err := newRoot(&opts{}).Execute(); err != nil {
		os.Exit(1)
	}
}

// newRoot builds the command tree.
func newRoot(o *opts) *cobra.Command {
	root := &cobra.Command{
		Use:   "triage-bot",
		Short: "AI-assisted stale triage for GitHub issues and pull requests",
		Long: "triage-bot works a repository's backlog least-recently-active first,\n" +
			"opening beads for an AI agent to assess and recording the verdicts in a local\n" +
			"database for a human to action. The same database keeps notes, links between\n" +
			"items, and the repository's security advisories. It reads GitHub but never\n" +
			"writes to it.",
		SilenceUsage: true,
		Version:      fmt.Sprintf("%s (%s, built %s)", buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime),
	}

	// An unresolvable home directory leaves no default, and every command
	// then says --db is needed.
	defaultDB, _ := store.DefaultPath()
	root.PersistentFlags().StringVar(&o.db, "db", defaultDB, "Path to the SQLite database")
	root.PersistentFlags().StringVar(&o.beadsDir, "beads-dir", "", "Directory holding the beads database (default: current directory)")
	root.PersistentFlags().BoolVarP(&o.verbose, "verbose", "v", false, "Verbose logging")

	root.AddCommand(
		initCmd(o),
		configCmd(o),
		syncCmd(o),
		daemonCmd(o),
		statusCmd(o),
		reportCmd(o),
		reviewCmd(o),
		ackCmd(o),
		retriageCmd(o),
		reparseCmd(o),
		noteCmd(o),
		notesCmd(o),
		linkCmd(o),
		showCmd(o),
		searchCmd(o),
		advisoriesCmd(o),
		mcpCmd(o),
	)
	return root
}

// logger builds the process logger. Logs go to stderr so stdout stays clean
// for report output that may be piped.
func (o *opts) logger() *slog.Logger {
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// openStore opens the database, which must already exist: only init creates
// one, so a mistyped --db fails rather than starting empty.
func (o *opts) openStore(ctx context.Context) (*store.Store, error) {
	if o.db == "" {
		return nil, errors.New("no database path: pass --db")
	}
	if _, err := os.Stat(o.db); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no database at %s\n\n"+
				"Run `triage-bot init --org ORG --repo REPO` to start one", o.db)
		}
		return nil, fmt.Errorf("failed to read %s: %w", o.db, err)
	}
	st, err := store.Open(ctx, o.db)
	if err != nil {
		return nil, fmt.Errorf("failed to open the database: %w", err)
	}
	return st, nil
}

// withStore opens the database for the length of fn.
func (o *opts) withStore(ctx context.Context, fn func(*store.Store) error) error {
	st, err := o.openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return fn(st)
}

// settings reads the settings, refusing a database with no repository.
func settings(ctx context.Context, st *store.Store) (store.Settings, error) {
	var s store.Settings
	if err := st.View(ctx, func(tx *store.Tx) error {
		var err error
		s, err = tx.Settings(ctx)
		return err
	}); err != nil {
		return store.Settings{}, fmt.Errorf("failed to read settings: %w", err)
	}
	if s.Org == "" || s.Repo == "" {
		return store.Settings{}, fmt.Errorf("%s does not name an org and repo: run `triage-bot init`", st.Path)
	}
	return s, nil
}

// githubClient builds the read-only GitHub client for the configured
// repository. A missing token still works against public repos, but the
// unauthenticated rate limit makes that useful only for experiments.
//
//nolint:ireturn // the real client, or a test's fake
func (o *opts) githubClient(s store.Settings) github.Client {
	if o.gitHub != nil {
		return o.gitHub(s)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	return github.New(s.Org, s.Repo, token)
}

// newService wires the operations the CLI shares with the MCP server.
func (o *opts) newService(ctx context.Context, st *store.Store) (*service.Service, error) {
	s, err := settings(ctx, st)
	if err != nil {
		return nil, err
	}
	svc, err := service.New(ctx, st, o.githubClient(s))
	if err != nil {
		return nil, fmt.Errorf("failed to start: %w", err)
	}
	return svc, nil
}

// withService opens the database and a service on it for the length of fn.
func (o *opts) withService(ctx context.Context, fn func(*service.Service) error) error {
	return o.withStore(ctx, func(st *store.Store) error {
		svc, err := o.newService(ctx, st)
		if err != nil {
			return err
		}
		return fn(svc)
	})
}

// newEngine wires an engine to the database, which is where org, repo and
// all operational settings live.
func (o *opts) newEngine(ctx context.Context, st *store.Store) (*engine.Engine, error) {
	s, err := settings(ctx, st)
	if err != nil {
		return nil, err
	}
	return &engine.Engine{
		Store:  st,
		GitHub: o.githubClient(s),
		Beads:  &beads.CLI{Dir: o.beadsDir},
		Log:    o.logger(),
	}, nil
}
