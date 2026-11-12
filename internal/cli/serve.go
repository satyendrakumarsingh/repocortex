package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/restapi"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func newServeCmd(g *Global) *cobra.Command {
	var (
		listTools    bool
		httpAddr     string
		allowRemote  bool
		allowedHosts []string
		auditLog     string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the index to AI agents over MCP",
		Long: `Runs an MCP server on stdio, exposing the index to a coding agent.

Register with Claude Code:

    claude mcp add repocortex -- repocortex --project /path/to/repo serve

With --http it serves MCP over Streamable HTTP instead, for clients that only
accept a remote server (ChatGPT developer mode, Android Studio):

    repocortex serve --http 127.0.0.1:7788

The same listener serves the tools as plain HTTP and JSON at /api/v1, for
callers with no MCP client, with an OpenAPI document at /api/v1/openapi.json:

    curl -H "Authorization: Bearer $TOKEN" -d '{"name":"ParseConfig"}' \
         http://127.0.0.1:7788/api/v1/tools/find_callers

From a shell with no server at all, 'repocortex tool call' runs the same tools.

Every request needs the token from REPOCORTEX_MCP_TOKEN, or the one generated for\nthe run and written to .repocortex/mcp-token (readable only by you),
as 'Authorization: Bearer <token>' or as the last path segment, /mcp/<token>.
The server listens on loopback only unless --allow-remote is given, and accepts
Host and Origin values that name this machine or a host in --allowed-host, such
as the hostname of a tunnel you run.

The exposed tool set is governed by mcp.profile in .repocortex.yml
(minimal | standard | full). The default is deliberately small: every tool's
schema is injected into the agent's context on every request, so a large tool
surface spends the very context this tool exists to save.

Responses are bounded and never contain whole files. Repository content is
labelled as untrusted evidence, because indexed source may have been written by
someone other than you.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg := g.Config()
			if httpAddr == "" && (allowRemote || len(allowedHosts) > 0) {
				return errors.New("--allow-remote and --allowed-host apply only with --http")
			}

			srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{AuditPath: auditLog})
			if err != nil {
				return err
			}
			defer closeServer()
			dbPath := g.DBPath()

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

			if httpAddr != "" {
				token, generated, err := resolveToken()
				if err != nil {
					return err
				}
				tokenFrom := "from " + mcpTokenEnv
				if generated {
					tokenPath := filepath.Join(filepath.Dir(dbPath), "mcp-token")
					if err := writeTokenFile(tokenPath, token); err != nil {
						return err
					}
					tokenFrom = "new for this run, in " + tokenPath + " (set " + mcpTokenEnv + " to keep one)"
				}
				g.Log().Info("serving MCP over HTTP",
					"project", cfg.ProjectName,
					"profile", string(cfg.MCPProfile),
					"tools", len(srv.Tools()))
				// The REST API rides on the same listener. It calls the tools
				// through an in-process client, so it is the MCP surface in a
				// different envelope and not a second implementation.
				inv, err := srv.NewInvoker(ctx, "rest")
				if err != nil {
					return err
				}
				defer func() { _ = inv.Close() }()
				api := restapi.Handler(inv, restapi.Info{
					Project: cfg.ProjectName,
					Version: Version,
					Profile: string(cfg.MCPProfile),
				})
				return serveHTTP(ctx, srv.SDK(), api, httpServeOptions{
					Addr:         httpAddr,
					AllowRemote:  allowRemote,
					AllowedHosts: allowedHosts,
					Token:        token,
				}, g.Log(), cmd.ErrOrStderr(), tokenFrom)
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
				// fails. Reporting EOF as an error meant `repocortex serve` exited
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
	cmd.Flags().StringVar(&httpAddr, "http", "", "serve MCP over Streamable HTTP on host:port instead of stdio (e.g. 127.0.0.1:7788)")
	cmd.Flags().BoolVar(&allowRemote, "allow-remote", false, "with --http, allow listening on a non-loopback address")
	cmd.Flags().StringSliceVar(&allowedHosts, "allowed-host", nil, "with --http, also accept this Host/Origin hostname (repeatable), e.g. a tunnel's hostname")
	cmd.Flags().StringVar(&auditLog, "audit-log", "", "append one JSON line per tool call to this file (default: $"+auditLogEnv+"); records the call, never the answer")
	return cmd
}

// nopWriteCloser adapts stdout to the io.WriteCloser IOTransport wants without
// letting the SDK close the process's own stdout on shutdown.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
