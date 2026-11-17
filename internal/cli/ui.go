package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/portal"
	"github.com/satyendrakumarsingh/repocortex/internal/restapi"

	"github.com/spf13/cobra"
)

// `repocortex ui` — the local portal.
//
// # Why it binds to loopback and says so
//
// This server renders the entire structure of a codebase: every symbol, every
// file path, every architectural boundary. Defaulting to 0.0.0.0 would publish
// that to every host on the network the moment someone ran it on a shared
// machine or inside a container with a mapped port, and the reader would have no
// signal that it had happened. So the default is 127.0.0.1, the bound address is
// printed on every start, and choosing anything else requires typing it — at
// which point the command says plainly what has been given up.
//
// The portal has no login, so it is served on loopback ONLY: --host accepts a
// loopback address and nothing else. The server-side guard (internal/portal) is
// the second half of the same decision: it refuses requests whose Host header
// is not a loopback name, which is what stops a public web page from using a
// reader's own browser to fetch this API off their laptop.

const (
	// defaultUIPort is a fixed default so the URL is memorable across restarts
	// and a bookmark keeps working. --port 0 asks the OS for a free one.
	defaultUIPort = 7777

	// defaultUIHost is loopback. See the comment above.
	defaultUIHost = "127.0.0.1"

	// uiShutdownGrace bounds how long a Ctrl-C waits for in-flight reads.
	uiShutdownGrace = 3 * time.Second
)

func newUICmd(g *Global) *cobra.Command {
	var (
		port         int
		host         string
		printOnly    bool
		allowRefresh bool
		next         bool
		maxNodes     int
	)

	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Serve a local, read-only portal over the index",
		Long: `Starts a local web portal over the indexed repository.

Five sections, built around what you came to find out: Home (ask a question,
and the state of the index), Explore (routes, tables and layout), Change (plan
a change safely), Health (hotspots, unused code, blind spots) and Agents
(connect an agent, and what each tool costs). Every function, type, file,
route and table has its own page: what uses it, what a change to it can reach,
and the tests that cover it.

The portal asks the index the same questions an agent does, through the same
tools: it has no data routes of its own, and every figure on every page is a
call to the tool endpoint under /api/v1. A page and an agent cannot disagree.

The portal is READ-ONLY. It opens the index read-only, the tools it calls are
read-only, and it serves no route that writes.

It is reachable from this machine only. It binds to 127.0.0.1, accepts only a
loopback address for --host, refuses requests whose Host header is not a
loopback name, and refuses cross-origin requests: it renders the structure of
your codebase and has no login, so a page on the internet must not be able to
point your own browser at http://localhost:7777 and read it.

Everything is served from the binary. There is no npm, no build step and no CDN:
the assets are embedded, and the page's Content-Security-Policy forbids loading
anything from another origin, so it works offline on a machine with no Node.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if allowRefresh {
				return errors.New("--allow-refresh was removed with the previous portal: the portal has no route " +
					"that writes. Run 'repocortex index' to rebuild, or 'repocortex watch' to keep the index current")
			}
			return runUI(cmd, g, uiOptions{Host: host, Port: port, PrintOnly: printOnly})
		},
	}

	cmd.Flags().IntVar(&port, "port", defaultUIPort, "port to listen on (0 picks a free one)")
	cmd.Flags().StringVar(&host, "host", defaultUIHost,
		"loopback address to bind (127.0.0.1, ::1 or localhost); the portal has no login and is never served beyond this machine")
	cmd.Flags().BoolVar(&printOnly, "print-url", false,
		"print the URL that would be served and exit, without listening")

	// Flags of the previous portal, kept so existing scripts and shortcuts do
	// not fail on an unknown flag. --next selected the portal that is now the
	// only one; --max-nodes bounded a graph screen that no longer exists.
	cmd.Flags().BoolVar(&next, "next", false, "")
	_ = cmd.Flags().MarkDeprecated("next", "the redesigned portal is now the only one; drop --next")
	cmd.Flags().IntVar(&maxNodes, "max-nodes", 0, "")
	_ = cmd.Flags().MarkDeprecated("max-nodes", "it bounded a screen of the previous portal and has no effect")
	cmd.Flags().BoolVar(&allowRefresh, "allow-refresh", false, "")
	_ = cmd.Flags().MarkHidden("allow-refresh")
	return cmd
}

type uiOptions struct {
	Host      string
	Port      int
	PrintOnly bool
}

func runUI(cmd *cobra.Command, g *Global, opts uiOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	if opts.Host == "" {
		opts.Host = defaultUIHost
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return fmt.Errorf("--port %d: want 0-65535", opts.Port)
	}
	loopback := isLoopbackAddr(opts.Host)

	if opts.PrintOnly {
		fmt.Fprintf(out, "http://%s/\n", net.JoinHostPort(opts.Host, fmt.Sprint(opts.Port)))
		return nil
	}

	// The portal has no login and calls the tool endpoint without a token, so
	// it is never served beyond this machine.
	if !loopback {
		return fmt.Errorf("--host %s: the portal is served on a loopback address only (127.0.0.1, ::1 or localhost); "+
			"it renders the structure of your codebase and has no login", opts.Host)
	}

	dbPath := g.DBPath()
	handler, closePortal, err := newPortal(ctx, g)
	if err != nil {
		return err
	}
	defer closePortal()

	addr := net.JoinHostPort(opts.Host, fmt.Sprint(opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	// The listener owns the real port, which is not opts.Port when it was 0.
	// Printing the requested one would hand the reader a URL that does not work.
	shown := displayURL(opts.Host, ln.Addr())

	if g.JSON {
		if err := emitJSON(out, map[string]any{
			"url":        shown,
			"address":    ln.Addr().String(),
			"read_only":  true,
			"loopback":   loopback,
			"index_path": dbPath,
		}); err != nil {
			_ = ln.Close()
			return err
		}
	} else {
		fmt.Fprintf(out, "RepoCortex portal: %s\n", shown)
		fmt.Fprintf(out, "  index      %s (opened read-only; this portal never writes)\n", dbPath)
		fmt.Fprintln(out, "  binding    loopback only — not reachable from other machines.")
		fmt.Fprintln(out, "             Requests whose Host header is not a loopback address are refused, so a")
		fmt.Fprintln(out, "             page on the internet cannot use your browser to read this index.")
		fmt.Fprintln(out, "\nPress Ctrl-C to stop.")
	}

	httpSrv := &http.Server{
		Handler: handler,
		// A local server still gets scanned, and an idle half-open connection
		// should not be able to hold a slot forever (CLAUDE.md §4.4).
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}

	// Shutdown is driven by the command's context, which the root wires to the
	// signal handler. Serve returns ErrServerClosed on a clean stop, which is
	// how a server ENDS rather than how it fails — reporting it as an error
	// would make `repocortex ui` exit non-zero on every Ctrl-C.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutCtx, cancel := stdctx.WithTimeout(stdctx.Background(), uiShutdownGrace)
		defer cancel()
		if err := httpSrv.Shutdown(shutCtx); err != nil {
			g.Log().Debug("portal shutdown", "error", err)
		}
	}()

	g.Log().Info("serving the portal", "url", shown, "loopback", loopback)
	err = httpSrv.Serve(ln)
	<-stopped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// newPortal assembles the portal: its embedded page and assets, plus the tool
// endpoint over the read-only index.
//
// It uses the `full` profile whatever .repocortex.yml says. A profile exists to
// keep tool schemas out of a model's context; a web page pays nothing for a
// tool it does not call, and the Health section needs the full-profile tools.
func newPortal(ctx stdctx.Context, g *Global) (http.Handler, func(), error) {
	srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{Profile: config.MCPFull})
	if err != nil {
		return nil, nil, err
	}
	inv, err := srv.NewInvoker(ctx, "portal")
	if err != nil {
		closeServer()
		return nil, nil, err
	}
	api := restapi.Handler(inv, restapi.Info{
		Project: g.Config().ProjectName,
		Version: Version,
		Profile: string(config.MCPFull),
	})
	return portal.Handler(api), func() {
		_ = inv.Close()
		closeServer()
	}, nil
}

// displayURL turns a listener address into something a reader can paste.
//
// A wildcard bind reports 0.0.0.0 or ::, neither of which is a URL anyone should
// paste into a browser, so those become localhost while the real bound address
// is still printed beside it.
func displayURL(requestedHost string, addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return "http://" + addr.String() + "/"
	}
	host := requestedHost
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "localhost"
	}
	return fmt.Sprintf("http://%s/", net.JoinHostPort(host, fmt.Sprint(tcp.Port)))
}

// isLoopbackAddr reports whether binding to host keeps the portal on this
// machine. An unresolvable name is treated as NOT loopback: the safe assumption
// when the answer is unknown is the one that keeps the Host guard switched on.
func isLoopbackAddr(host string) bool {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	if h == "" {
		return true // empty binds every interface in Go, but our default is loopback
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
