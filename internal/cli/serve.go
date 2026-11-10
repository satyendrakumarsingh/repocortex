package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func newServeCmd(g *Global) *cobra.Command {
	var listTools bool

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the index to AI agents over MCP",
		Long: `Runs an MCP server on stdio, exposing the index to a coding agent.

Register with Claude Code:

    claude mcp add cortex -- cortex --project /path/to/repo serve

The exposed tool set is governed by mcp.profile in .cortex.yml
(minimal | standard | full). The default is deliberately small: every tool's
schema is injected into the agent's context on every request, so a large tool
surface spends the very context this tool exists to save.

Responses are bounded and never contain whole files. Repository content is
labelled as untrusted evidence, because indexed source may have been written by
someone other than you.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := g.Config()

			dbPath := g.DBPath()
			if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("no index at %s — run 'cortex init' and 'cortex index' first", dbPath)
			}

			// Read-only: an agent session must never be able to mutate the
			// index it is querying.
			db, err := openReadOnly(ctx, dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			srv, err := devmcp.New(devmcp.Deps{
				Registry: registry.New(db),
				Graph:    graph.New(db, graph.WithCSR()),
				Config:   cfg,
			})
			if err != nil {
				return err
			}

			if listTools {
				if g.JSON {
					return emitJSON(cmd.OutOrStdout(), map[string]any{
						"profile": string(cfg.MCPProfile),
						"tools":   srv.Tools(),
						"version": devmcp.Version,
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "MCP profile %q exposes %d tool(s):\n",
					cfg.MCPProfile, len(srv.Tools()))
				for _, t := range srv.Tools() {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", t)
				}
				return nil
			}

			// Logs go to stderr; stdout is the MCP transport and any stray
			// write to it corrupts the protocol stream.
			g.Log().Info("serving MCP on stdio",
				"project", cfg.ProjectName,
				"profile", string(cfg.MCPProfile),
				"tools", len(srv.Tools()))

			// IOTransport rather than StdioTransport, so the client's stream
			// passes through a filter first. The SDK treats an unparseable line
			// as a fatal transport error, which meant one stray line from a
			// client — a debug print, a truncated write — ended the session and
			// every request after it went unanswered. See jsonLineFilter.
			transport := &sdk.IOTransport{
				Reader: io.NopCloser(newJSONLineFilter(os.Stdin, g.Log())),
				Writer: nopWriteCloser{os.Stdout},
			}
			if err := srv.Run(ctx, transport); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				// A client hanging up is how an MCP session ENDS, not how it
				// fails. Reporting EOF as an error meant `cortex serve` exited
				// 1 on every normal disconnect, which turns a supervisor's
				// restart-on-failure into a restart loop and makes a real
				// failure indistinguishable from a client closing its pipe.
				if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF") {
					g.Log().Info("MCP client disconnected; session ended")
					return nil
				}
				return fmt.Errorf("mcp server: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&listTools, "list-tools", false, "print the exposed tools and exit")
	return cmd
}

// nopWriteCloser adapts stdout to the io.WriteCloser IOTransport wants without
// letting the SDK close the process's own stdout on shutdown.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
