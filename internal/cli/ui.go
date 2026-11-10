package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/web"

	"github.com/spf13/cobra"
)

// `cortex ui` — the local portal.
//
// # Why it binds to loopback and says so
//
// This server renders the entire structure of a codebase: every symbol, every
// file path, every architectural boundary. Defaulting to 0.0.0.0 would publish
// that to every host on the network the moment someone ran it on a shared
// machine or inside a container with a mapped port, and the reader would have no
// signal that it had happened. So the default is 127.0.0.1, the bound address is
// printed on every start, and choosing anything else requires typing it — at
// which point the command says plainly what has been given up. The server-side
// Host guard (web.WithAllowRemoteHosts) is the second half of the same
// decision: on a loopback bind it also refuses requests whose Host header is not
// a loopback name, which is what stops a public web page from using a reader's
// own browser to fetch this API off their laptop.

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
		maxNodes     int
		printOnly    bool
		allowRefresh bool
	)

	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Serve a local, read-only portal over the index",
		Long: `Starts a local web portal that visualises the indexed repository.

Twenty screens, grouped by what you're trying to find out. Index health opens
on its own, above everything else. Then: Understand the code (map of the
code, find a symbol, shape of the code); Before you change something (what
could break, task brief, check a change, who knows this code, what changed);
How it runs (APIs and what they touch, database tables, settings and secrets,
what actually ran, across repositories); Quality (test coverage, architecture
rules, dependencies, how accurate is this index); and For your AI agent
(connect an agent, retrieval quality).

The portal is READ-ONLY. It opens the index read-only, exposes only GET routes,
and never writes. --allow-refresh is the single, opt-in exception: it adds one
POST route that rebuilds the index, and nothing else changes.

It binds to 127.0.0.1 by default and refuses requests whose Host header is not a
loopback address. That is deliberate: this server renders the whole structure of
your codebase, and a page on the public internet can otherwise point your own
browser at http://localhost:7777 and read it. Pass --host to bind elsewhere; the
command will say what that gives up.

Everything is served from the binary. There is no npm, no build step and no CDN:
the assets are embedded, and the page's Content-Security-Policy forbids loading
anything from another origin, so it works offline on a machine with no Node.

Confidence is the visual language of every view. An edge a compiler proved is
drawn solid with a tick; one measured at 5.7-37.5% precision is drawn dashed;
one measured at 0.0-2.8% is off by default — and the count of what that hides is
stated in every response, never dropped in silence.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUI(cmd, g, uiOptions{
				Host: host, Port: port, MaxNodes: maxNodes, PrintOnly: printOnly,
				AllowRefresh: allowRefresh,
			})
		},
	}

	cmd.Flags().IntVar(&port, "port", defaultUIPort, "port to listen on (0 picks a free one)")
	cmd.Flags().StringVar(&host, "host", defaultUIHost,
		"address to bind (default loopback; anything else exposes your codebase's structure to the network)")
	cmd.Flags().IntVar(&maxNodes, "max-nodes", web.DefaultMaxNodes,
		"maximum symbol nodes in one graph response; whatever it excludes is stated, never dropped")
	cmd.Flags().BoolVar(&printOnly, "print-url", false,
		"print the URL that would be served and exit, without listening")
	cmd.Flags().BoolVar(&allowRefresh, "allow-refresh", false,
		"let the portal rebuild the index from its own page; without this the portal is "+
			"strictly read-only and serves no route that writes. Even when enabled the "+
			"refresh is refused for any non-loopback origin, and requires a header a "+
			"cross-site request cannot set.")
	return cmd
}

type uiOptions struct {
	Host      string
	Port      int
	MaxNodes  int
	PrintOnly bool

	// AllowRefresh adds the one route that writes. Default false, and the
	// default is the security decision: see internal/web/reindex.go.
	AllowRefresh bool
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

	dbPath := g.DBPath()
	srv, closePortal, err := newPortal(ctx, g, opts)
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
			"url":     shown,
			"address": ln.Addr().String(),
			// read_only describes this process's own handle on the index, which
			// stays read-only either way; allow_refresh is the separate fact
			// about whether the portal may ask the indexer to rebuild it.
			"read_only":     true,
			"allow_refresh": opts.AllowRefresh,
			"loopback":      loopback,
			"index_path":    dbPath,
		}); err != nil {
			_ = ln.Close()
			return err
		}
	} else {
		fmt.Fprintf(out, "RepoCortex portal: %s\n", shown)
		fmt.Fprintf(out, "  index      %s (opened read-only; this portal never writes)\n", dbPath)
		if loopback {
			fmt.Fprintln(out, "  binding    loopback only — not reachable from other machines.")
			fmt.Fprintln(out, "             Requests whose Host header is not a loopback address are refused, so a")
			fmt.Fprintln(out, "             page on the internet cannot use your browser to read this index.")
		} else {
			fmt.Fprintf(out, "  binding    %s — REACHABLE FROM OTHER MACHINES.\n", opts.Host)
			fmt.Fprintln(out, "             This serves the entire structure of your codebase — every symbol, file")
			fmt.Fprintln(out, "             path and architectural boundary — to anything that can reach this port,")
			fmt.Fprintln(out, "             with no authentication. The loopback Host guard is disabled on a")
			fmt.Fprintln(out, "             non-loopback bind because it cannot work there.")
		}
		if opts.AllowRefresh {
			fmt.Fprintln(out, "  refresh    ENABLED — the page can rebuild this index (POST /api/reindex).")
			fmt.Fprintln(out, "             This is the only route that writes. It is refused for any origin that")
			fmt.Fprintln(out, "             is not loopback, and it requires a header a cross-site request cannot")
			fmt.Fprintln(out, "             set. Drop --allow-refresh to go back to a portal with no write route.")
		}
		fmt.Fprintln(out, "\nPress Ctrl-C to stop.")
	}

	httpSrv := &http.Server{
		Handler: srv.Handler(),
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
	// would make `cortex ui` exit non-zero on every Ctrl-C.
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

// newPortal opens the index and assembles the portal server, returning a
// closer for the database handle.
//
// Split out of runUI because everything above the listener — which options the
// flags select, and above all whether a Reindexer exists at all — is the part
// worth asserting on, and a test that has to bind a real port to reach it
// cannot assert on the ABSENCE of a route.
//
// extra options are appended last and therefore win, which is the seam a test
// uses to put a stubbed pipeline behind a portal built by this exact code.
func newPortal(ctx stdctx.Context, g *Global, opts uiOptions, extra ...web.Option) (*web.Server, func(), error) {
	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		// Not a fault. The portal itself answers an unindexed repository with
		// guidance rather than an error, but there is no database to open at
		// all here, so the guidance has to come from the command.
		return nil, nil, fmt.Errorf("no index at %s — run 'cortex init' and 'cortex index' first", dbPath)
	}

	// Read-only at the file level too, not only by convention: a portal must
	// never be able to mutate the index it is describing. That stays true with
	// --allow-refresh: a refresh runs the indexer, which opens its own writable
	// handle, so the portal's own connection can still never write.
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	closeDB := func() { _ = db.Close() }

	reg := registry.New(db)
	webOpts := []web.Option{
		web.WithLogger(g.Log()),
		web.WithMaxNodes(opts.MaxNodes),
		web.WithAllowRemoteHosts(!isLoopbackAddr(opts.Host)),
		// "Is what I am reading current?" has two answers, and this is the
		// second one: whether a commit hook rebuilds the index without anyone
		// remembering to. The portal renders the fact and never installs
		// anything — writing into .git is a CLI action, so only the status
		// crosses the port.
		web.WithHookReporter(web.HookReporterFunc(func(root string) any {
			return CommitHookStatus(root)
		})),
	}
	if opts.AllowRefresh {
		// The only write the portal can cause, and only because the operator
		// typed the flag. Passing nothing here is what leaves the POST route
		// unregistered — read-only enforced by an absent route rather than by a
		// check somebody could later reorder.
		webOpts = append(webOpts, web.WithReindexer(newUIReindexer(g, reg)))
	}
	webOpts = append(webOpts, extra...)

	srv, err := web.New(web.Deps{
		Registry: reg,
		Caps:     capability.NewDeps(reg, graph.New(db, graph.WithCSR())),
		// The contract and verify screens compile a brief from a context pack,
		// which needs full-text file search and file excerpting on top of the
		// bulk reads the graph screens use. Passed explicitly rather than left
		// to a type assertion inside the portal, so the wiring is visible here.
		Packs:     reg,
		Config:    g.Config(),
		IndexPath: dbPath,
	}, webOpts...)
	if err != nil {
		closeDB()
		return nil, nil, err
	}
	return srv, closeDB, nil
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
