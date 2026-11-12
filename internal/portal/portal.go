// Package portal serves the next RepoCortex portal: a small web application
// that asks the index the same questions an agent does, through the same tools.
//
// # Why it is a separate package from internal/web
//
// internal/web is the current portal: twenty-three report screens over its own
// JSON routes. This one is built differently on purpose (see
// docs/design/PORTAL_UX_PLAN.md): five sections, a page per symbol, and NO
// routes of its own. Every figure on every page comes from a tool call to the
// REST surface in internal/restapi, which in turn calls the MCP server
// in-process. So a page and an agent cannot disagree about an answer, because
// there is one implementation of each answer, and "ask your agent the same
// thing" beside a table is literally true.
//
// The two portals live side by side until this one covers every screen of the
// other; `repocortex ui --next` serves this one.
//
// # Security
//
// The portal renders the structure of a private codebase and has no login, so
// it is reachable from this machine only, and three checks keep a web page in
// the user's own browser from reading it:
//
//   - the Host header must name this machine, which defeats DNS rebinding;
//   - a request carrying an Origin must carry this server's own, which stops a
//     cross-site page from calling the tool endpoint with a simple POST;
//   - the Content-Security-Policy allows scripts, styles and connections from
//     this origin only, so the page cannot be made to load or send anything
//     elsewhere.
//
// Nothing here writes. The tools are read-only, and the index is opened
// read-only by the caller.
package portal

import (
	"embed"
	"io/fs"
	"net"
	"net/http"
	"strings"
)

//go:embed assets
var assets embed.FS

// APIPrefix is where the tool endpoint is mounted. It must match
// restapi.BasePath; it is restated here so this package needs no import of it.
const APIPrefix = "/api/v1"

// Handler serves the portal's page and assets, and forwards APIPrefix to api.
func Handler(api http.Handler) http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// The directory is embedded at build time; its absence is a build
		// defect, not a runtime condition.
		panic("portal: embedded assets are missing: " + err.Error())
	}
	static := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.Handle(APIPrefix, api)
	mux.Handle(APIPrefix+"/", api)
	// No method on this pattern: "GET /" would conflict with the method-less
	// API patterns above, which the mux refuses at registration.
	mux.HandleFunc("/", serveAssets(static))
	return guard(mux)
}

// serveAssets serves the embedded page and its files. It is a named handler,
// not the file server passed straight to the mux, so the route has a symbol
// that `repocortex blindspots` and a reader can both follow.
func serveAssets(files http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the portal's pages are read-only", http.StatusMethodNotAllowed)
			return
		}
		files.ServeHTTP(w, r)
	}
}

// guard applies the checks described in the package comment.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "this portal answers only on this machine", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			http.Error(w, "cross-origin requests are refused", http.StatusForbidden)
			return
		}

		h := w.Header()
		// No inline script and no inline style: the page sets dimensions
		// through the DOM, which this policy permits and an injected
		// attribute would not be.
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// The page describes private source. Nothing may keep a copy.
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// loopbackHost reports whether a Host header names this machine.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameOrigin reports whether origin is this server's own, as named by host.
func sameOrigin(origin, host string) bool {
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "https://")
	}
	return ok && strings.EqualFold(rest, host)
}
