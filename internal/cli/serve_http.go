package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/restapi"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpTokenEnv names the environment variable that supplies the bearer token
// for `repocortex serve --http`. When it is unset a random token is generated for
// the life of the process.
const mcpTokenEnv = "REPOCORTEX_MCP_TOKEN" //nolint:gosec // G101: the name of an environment variable, not a credential

// mcpPath is where the Streamable HTTP endpoint is mounted. A client that
// cannot send an Authorization header uses mcpPath + "/" + token instead.
const mcpPath = "/mcp"

// minTokenLen keeps a hand-picked REPOCORTEX_MCP_TOKEN from being guessable.
const minTokenLen = 24

// httpServeOptions configures the HTTP transport.
type httpServeOptions struct {
	Addr         string   // host:port to listen on
	AllowRemote  bool     // permit a non-loopback listen address
	AllowedHosts []string // extra Host header values accepted, for a tunnel or proxy
	Token        string   // bearer token; required
}

// checkListenAddr refuses to listen beyond this machine unless the caller
// asked for it explicitly. The index describes private source code, and an
// accidental 0.0.0.0 would publish it to the local network.
func checkListenAddr(addr string, allowRemote bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--http %q: want host:port, such as 127.0.0.1:7788", addr)
	}
	if port == "" {
		return fmt.Errorf("--http %q: missing port", addr)
	}
	if isLoopbackHost(host) || allowRemote {
		return nil
	}
	return fmt.Errorf("--http %q listens beyond this machine; use 127.0.0.1, or add --allow-remote if that is intended", addr)
}

// isLoopbackHost reports whether host names this machine only. An empty host
// means every interface, so it is not loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// resolveToken returns the configured token, or a fresh random one. generated
// tells the caller to show the new token to the user once.
func resolveToken() (token string, generated bool, err error) {
	if v := strings.TrimSpace(os.Getenv(mcpTokenEnv)); v != "" {
		if len(v) < minTokenLen {
			return "", false, fmt.Errorf("%s is %d characters; use at least %d", mcpTokenEnv, len(v), minTokenLen)
		}
		return v, false, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), true, nil
}

// newHTTPMCPHandler wraps the SDK's Streamable HTTP handler with the checks a
// network-facing server needs and the SDK does not do on its own:
//
//   - a bearer token on every request, compared in constant time;
//   - a Host allowlist, which blocks DNS rebinding; loopback names are always
//     allowed, AllowedHosts adds a tunnel or proxy hostname;
//   - an Origin check, so a web page in the user's browser cannot drive the
//     server, as the MCP transport specification requires.
//
// The token may also be the last path segment (/mcp/<token>), for clients
// that take a URL but cannot set headers.
func newHTTPMCPHandler(srv *sdk.Server, opts httpServeOptions, logger *slog.Logger) http.Handler {
	return newHTTPHandler(srv, nil, opts, logger)
}

// healthPath answers liveness probes. It is the one route served without a
// token, and it says nothing but that the process is up: no project name, no
// version, no counts. A supervisor needs to know the server is alive without
// being handed the credential that reads the index.
const healthPath = "/healthz"

// newHTTPHandler is newHTTPMCPHandler plus the REST API.
//
// api is mounted at restapi.BasePath behind the SAME guard as the MCP
// endpoint: one Host allowlist, one Origin check, one token. Two front ends
// with two notions of who may call them is how one of them ends up open.
//
// The REST routes take the token only as a bearer header. The path-token form
// exists for MCP clients that accept nothing but a URL; an HTTP client that
// can make a REST call can set a header, and a token in a path is a token in
// every access log and proxy between the caller and here.
func newHTTPHandler(srv *sdk.Server, api http.Handler, opts httpServeOptions, logger *slog.Logger) http.Handler {
	allowed := map[string]bool{}
	for _, h := range opts.AllowedHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			allowed[h] = true
		}
	}
	hostOK := func(hostport string) bool {
		host := hostport
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			host = h
		}
		host = strings.ToLower(host)
		return isLoopbackHost(host) || allowed[host] || allowed[strings.ToLower(hostport)]
	}

	mcp := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{
		Logger: logger,
		// The Host check below covers DNS rebinding for loopback and also
		// admits the hostnames the user allowed, which the SDK's built-in
		// check would reject.
		DisableLocalhostProtection: true,
	})
	want := []byte(opts.Token)
	tokenOK := func(got string) bool {
		return got != "" && subtle.ConstantTimeCompare([]byte(got), want) == 1
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostOK(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
			if !hostOK(o) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}

		path := r.URL.Path
		bearerOK := func() bool {
			bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			return ok && tokenOK(strings.TrimSpace(bearer))
		}

		var next http.Handler
		authorized := false
		switch {
		case path == healthPath:
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
			return
		case path == mcpPath:
			next, authorized = mcp, bearerOK()
		case strings.HasPrefix(path, mcpPath+"/"):
			next, authorized = mcp, tokenOK(strings.TrimPrefix(path, mcpPath+"/"))
		case api != nil && (path == restapi.BasePath || strings.HasPrefix(path, restapi.BasePath+"/")):
			next, authorized = api, bearerOK()
		default:
			http.NotFound(w, r)
			return
		}
		if !authorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="repocortex"`)
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeTokenFile stores a generated token where only the user can read it,
// rather than printing it: a token on stderr ends up in terminal scrollback,
// CI logs and log collectors. The .repocortex directory is git-ignored.
func writeTokenFile(path, token string) error {
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	// WriteFile keeps the mode of an existing file; tighten it in case an
	// older copy was more open.
	return os.Chmod(path, 0o600)
}

// serveHTTP runs the MCP server over Streamable HTTP until ctx ends. tokenFrom
// tells the user where the token is, never the token itself.
func serveHTTP(ctx context.Context, srv *sdk.Server, api http.Handler, opts httpServeOptions, logger *slog.Logger, out io.Writer, tokenFrom string) error {
	if err := checkListenAddr(opts.Addr, opts.AllowRemote); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.Addr, err)
	}
	hs := &http.Server{
		Handler:           newHTTPHandler(srv, api, opts, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	base := "http://" + ln.Addr().String()
	endpoint := base + mcpPath
	_, _ = fmt.Fprintf(out, "MCP over HTTP: %s\n", endpoint)
	if api != nil {
		_, _ = fmt.Fprintf(out, "REST API:      %s%s  (OpenAPI: %s%s/openapi.json)\n",
			base, restapi.BasePath, base, restapi.BasePath)
	}
	_, _ = fmt.Fprintf(out, "Token: %s\n", tokenFrom)
	_, _ = fmt.Fprintf(out, "Send it as 'Authorization: Bearer <token>', or use %s/<token> if an MCP client cannot set headers.\n", endpoint)

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server: %w", err)
	}
}
