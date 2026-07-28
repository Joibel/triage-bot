// Command triage-bot drives AI-assisted stale triage of a GitHub repository's
// oldest open issues and pull requests.
//
// It opens a bounded number of beads for assessment, parses the completion
// template each closed bead carries, and records the verdict in a YAML status
// file for a human to action. It never writes to GitHub.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/beads"
	"github.com/Joibel/triage-bot/internal/buildinfo"
	"github.com/Joibel/triage-bot/internal/engine"
	"github.com/Joibel/triage-bot/internal/github"
	"github.com/Joibel/triage-bot/internal/state"
)

// opts holds the flags every subcommand shares. It is threaded through command
// constructors rather than kept in package globals.
type opts struct {
	statusFile string
	beadsDir   string
	verbose    bool
}

func main() {
	o := &opts{}

	root := &cobra.Command{
		Use:   "triage-bot",
		Short: "AI-assisted stale triage for GitHub issues and pull requests",
		Long: "triage-bot works a repository's backlog oldest-first, opening beads for an\n" +
			"AI agent to assess and recording the verdicts in a status file for a human to\n" +
			"action. It reads GitHub but never writes to it.",
		SilenceUsage: true,
		Version:      fmt.Sprintf("%s (%s, built %s)", buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime),
	}

	root.PersistentFlags().StringVarP(&o.statusFile, "file", "f", "triage-bot.yaml", "Path to the status file")
	root.PersistentFlags().StringVar(&o.beadsDir, "beads-dir", "", "Directory holding the beads database (default: current directory)")
	root.PersistentFlags().BoolVarP(&o.verbose, "verbose", "v", false, "Verbose logging")

	root.AddCommand(
		initCmd(o),
		syncCmd(o),
		daemonCmd(o),
		statusCmd(o),
		reportCmd(o),
		ackCmd(o),
		retriageCmd(o),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
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

// newEngine wires an engine from the status file, which is where org, repo and
// all operational settings live.
func (o *opts) newEngine() (*engine.Engine, error) {
	cfg, err := state.Load(o.statusFile)
	if err != nil {
		return nil, fmt.Errorf("%w\n\nRun `triage-bot init --org ORG --repo REPO` to create one", err)
	}
	if cfg.Org == "" || cfg.Repo == "" {
		return nil, fmt.Errorf("%s does not name an org and repo", o.statusFile)
	}

	// A missing token still works against public repos, but the
	// unauthenticated rate limit makes that useful only for experiments.
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}

	return &engine.Engine{
		Path:   o.statusFile,
		GitHub: github.New(cfg.Org, cfg.Repo, token),
		Beads:  &beads.CLI{Dir: o.beadsDir},
		Log:    o.logger(),
	}, nil
}
