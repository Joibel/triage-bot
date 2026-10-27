package main

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Joibel/triage-bot/internal/mcpserver"
	"github.com/Joibel/triage-bot/internal/service"
)

func mcpCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the database to Claude Code over stdio MCP",
		Long: "Runs an MCP server on stdin and stdout for a Claude Code session on this\n" +
			"machine, with tools show, search, add_note, link, advisories and\n" +
			"set_disposition. Notes, links and decisions it records name claude as\n" +
			"their author. It returns sensitive items too: it is local, and serves\n" +
			"only this machine.\n\n" +
			"Register it once (see the README):\n\n" +
			"  claude mcp add -s user triage-bot -- triage-bot mcp --db PATH\n\n" +
			"Adding a note to an item not yet in the database looks it up on GitHub\n" +
			"with GITHUB_TOKEN or GH_TOKEN; advisories and private forks need the\n" +
			"maintainer's own token.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return o.withService(ctx, func(svc *service.Service) error { return mcpserver.Run(ctx, svc) })
		},
	}
}
