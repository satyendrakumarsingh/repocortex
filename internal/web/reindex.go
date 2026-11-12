package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file is the one place the portal is allowed to cause a write, and the
// whole design is about making that exception impossible to trigger by
// accident.
//
// The portal's security posture rests on a single claim: pointing a browser at
// localhost cannot make RepoCortex do anything. Re-indexing is the first feature
// that would break that claim, because a page on the internet can make a
// victim's browser issue cross-site POSTs. So the capability is:
//
//  1. absent unless the operator asked for it — no Reindexer, no route;
//  2. refused unless the request proves it came from the portal's own page,
//     via a header a cross-site form cannot set and a preflight this server
//     never approves;
//  3. refused for any Origin that is not loopback, even with the header;
//  4. serialised, so a held-down button cannot start twenty indexers.
//
// Point 2 is what actually stops CSRF. A cross-site <form> can POST but cannot
// add headers; fetch() can add headers but is then preflighted, and this server
// answers no OPTIONS and sends no Access-Control-Allow-* — so the preflight
// fails and the real request is never sent.

// IndexStateResponse is what /api/indexstate serves.
//
// Typed rather than a map because the portal freezes its API surface in
// testdata/api-contract.golden: a map payload is invisible to that check, and an
// endpoint nothing checks is one that can be renamed out from under the page
// without a single test failing.
type IndexStateResponse struct {
	Meta Meta `json:"meta"`

	// Refresh describes whether this portal may rebuild the index.
	Refresh RefreshState `json:"refresh"`

	// CommitHook reports whether a post-commit hook keeps the index current.
	CommitHook CommitHookState `json:"commit_hook"`
}

// RefreshState is the refresh capability as the page sees it.
type RefreshState struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`

	// Header names the request header a refresh must carry. Sent to the page so
	// the anti-CSRF contract lives in one place rather than as a literal
	// duplicated in the JavaScript.
	Header string `json:"header,omitempty"`

	// Detail and Command are populated when refresh is disabled: a portal that
	// cannot do the thing must say what can.
	Detail  string `json:"detail,omitempty"`
	Command string `json:"command,omitempty"`

	Last *RefreshAttempt `json:"last,omitempty"`
}

// RefreshAttempt records how the previous refresh ended.
type RefreshAttempt struct {
	AtUnix int64          `json:"at_unix"`
	Result *ReindexResult `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// CommitHookState reports the post-commit hook without asserting more than was
// checked. Checked=false means "not looked at", which is not the same as "not
// installed" and must not render as one.
type CommitHookState struct {
	Checked   bool   `json:"checked"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Command   string `json:"command,omitempty"`
}

// ReindexHeader must be present on any refresh request. Its value is not a
// secret and does not need to be: what matters is that setting a custom header
// forces a CORS preflight this server refuses.
const ReindexHeader = "X-RepoCortex-Reindex"

// Reindexer rebuilds the index. Declared here, at the consumer, so the portal
// depends on the capability and not on internal/pipeline (CLAUDE.md §1, §3 DIP).
type Reindexer interface {
	// Reindex rebuilds and re-resolves, honouring cancellation.
	Reindex(ctx context.Context) (ReindexResult, error)
}

// ReindexResult is what the portal reports back to the page.
type ReindexResult struct {
	Revision    int64  `json:"revision"`
	GitRevision string `json:"git_revision,omitempty"`
	Files       int    `json:"files"`
	Symbols     int    `json:"symbols"`
	Edges       int    `json:"edges"`
	DurationMS  int64  `json:"duration_ms"`
}

// ReindexFunc adapts a function to the Reindexer port.
type ReindexFunc func(ctx context.Context) (ReindexResult, error)

// Reindex implements Reindexer.
func (f ReindexFunc) Reindex(ctx context.Context) (ReindexResult, error) { return f(ctx) }

// WithReindexer enables the refresh endpoint.
//
// Off by default, and deliberately so: it is the only route that writes, and a
// portal that can rebuild an index is a portal a hostile page would like to
// reach. `repocortex ui --allow-refresh` is the operator saying yes.
func WithReindexer(r Reindexer) Option {
	return func(s *Server) { s.reindexer = r }
}

// WithReindexTimeout bounds a single refresh. Zero keeps the default.
func WithReindexTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.reindexTimeout = d
		}
	}
}

// DefaultReindexTimeout bounds one refresh. Indexing a large repository is
// measured in seconds, not minutes; a run that exceeds this has gone wrong and
// holding the request open teaches a reader the portal has hung.
const DefaultReindexTimeout = 10 * time.Minute

// reindexGate serialises refreshes and remembers the last outcome, so a second
// click reports "already running" instead of starting a competing indexer over
// the same SQLite file.
type reindexGate struct {
	mu      sync.Mutex
	running bool
	last    *reindexRecord
}

type reindexRecord struct {
	At      time.Time
	Result  ReindexResult
	Err     string
	Started time.Time
}

// begin claims the gate. Reports false when a refresh is already in flight.
func (g *reindexGate) begin(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	g.last = &reindexRecord{Started: now}
	return true
}

func (g *reindexGate) finish(now time.Time, res ReindexResult, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running = false
	rec := &reindexRecord{At: now, Result: res}
	if g.last != nil {
		rec.Started = g.last.Started
	}
	if err != nil {
		rec.Err = err.Error()
	}
	g.last = rec
}

func (g *reindexGate) snapshot() (running bool, last *reindexRecord) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running, g.last
}

// handleReindex rebuilds the index and answers with what changed.
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if s.reindexer == nil {
		// Unreachable while the route is only registered when enabled, but a
		// nil dereference here would be a crash in the one handler that writes.
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "refresh is not enabled on this portal"})
		return
	}
	if msg, ok := s.checkReindexOrigin(r); !ok {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": msg})
		return
	}
	if !s.reindexes.begin(s.now()) {
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"status": "already_running",
			"detail": "a refresh is already in progress; this one was not started so two indexers cannot write the same file",
		})
		return
	}

	// Deliberately detached from the request's context. A reader who clicks
	// refresh and then closes the tab has still asked for a rebuild, and
	// aborting mid-pass would mean the visible index silently stays old. The
	// work is safe to finish unattended because a run is only visible once
	// committed, and the gate above stops a second one starting meanwhile.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.reindexTimeout)
	defer cancel()

	start := s.now()
	res, err := s.reindexer.Reindex(ctx)
	s.reindexes.finish(s.now(), res, err)

	if err != nil {
		s.log.Error("web: reindex", "error", err)
		status := http.StatusInternalServerError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			status = http.StatusGatewayTimeout
		}
		s.writeJSON(w, status, map[string]any{
			"error": fmt.Sprintf("the refresh failed and the index was left as it was: %v", err),
		})
		return
	}
	if res.DurationMS == 0 {
		res.DurationMS = s.now().Sub(start).Milliseconds()
	}
	// The cached structural map describes the run that just became stale.
	s.mu.Lock()
	s.cached, s.cachedID = nil, 0
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"result": res,
	})
}

// checkReindexOrigin applies the anti-CSRF rules. Returns the refusal to show
// when it fails — an explanation, never a bare 403.
func (s *Server) checkReindexOrigin(r *http.Request) (string, bool) {
	if r.Header.Get(ReindexHeader) == "" {
		return "refused: a refresh must carry the " + ReindexHeader + " header, which a cross-site request cannot set. " +
			"If you are calling this by hand, add it.", false
	}
	// An Origin from another site is refused even with the header, which covers
	// any future browser that stops preflighting.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !isLoopbackHost(u.Host) {
			return "refused: this refresh came from " + origin + ", which is not this portal.", false
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" &&
		!strings.EqualFold(site, "same-origin") && !strings.EqualFold(site, "none") {
		return "refused: the browser reported this request as " + site + " rather than same-origin.", false
	}
	return "", true
}

// reindexState describes the refresh capability for the page: whether it exists
// at all, whether one is running, and how the last one went. Read-only.
func (s *Server) reindexState() RefreshState {
	if s.reindexer == nil {
		return RefreshState{
			Enabled: false,
			Detail: "This portal cannot rebuild the index. It is read-only by default; " +
				"start it with --allow-refresh to enable this, or run the command yourself.",
			Command: "repocortex index && repocortex resolve",
		}
	}
	running, last := s.reindexes.snapshot()
	out := RefreshState{Enabled: true, Running: running, Header: ReindexHeader}
	if last != nil && !last.At.IsZero() {
		attempt := &RefreshAttempt{AtUnix: last.At.Unix()}
		if last.Err != "" {
			attempt.Error = last.Err
		} else {
			res := last.Result
			attempt.Result = &res
		}
		out.Last = attempt
	}
	return out
}

// methodAllowed decides whether a request's method may reach the mux at all.
//
// The rule is not "GET plus POST": it is "GET, plus exactly one POST path, and
// only while a Reindexer is configured". Written as one predicate so the
// read-only guarantee is a single readable expression rather than a condition
// spread across the middleware.
func (s *Server) methodAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	return r.Method == http.MethodPost && s.reindexer != nil && r.URL.Path == "/api/reindex"
}

// handleIndexState answers what the page needs to describe the index it shows:
// when it was built, whether a commit hook keeps it current, and whether this
// portal may refresh it.
//
// It exists because "is what I am reading current?" was answerable only by
// reading a relative age in the corner, and a reader who has just committed
// wants a yes or a no.
func (s *Server) handleIndexState(w http.ResponseWriter, r *http.Request) {
	resp := IndexStateResponse{
		Refresh:    s.reindexState(),
		CommitHook: s.commitHookState(),
	}
	meta, _, _, err := s.run(r.Context())
	if err != nil {
		// Provenance is the nice-to-have here; refresh and hook state are what a
		// reader acts on, so answer with those rather than failing the request.
		s.log.Debug("web: index state meta", "error", err)
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Meta = meta
	s.writeJSON(w, http.StatusOK, resp)
}

// commitHookState reports the hook, distinguishing "not installed" from "not
// checked". A portal started without a reporter knows nothing about the hook,
// and rendering that as "not installed" would be an invented fact.
func (s *Server) commitHookState() CommitHookState {
	if s.hooks == nil {
		return CommitHookState{
			Checked: false,
			Detail:  "Not checked: this portal was started without a hook reporter.",
		}
	}
	st := CommitHookState{Checked: true}
	// The reporter is a port supplying the CLI's own value. Read it structurally
	// via JSON so this package never imports internal/cli: an adapter may depend
	// on the domain, never the reverse (CLAUDE.md §1).
	if raw, err := json.Marshal(s.hooks.CommitHookState(s.deps.Config.RepoRoot)); err == nil {
		var probe struct {
			Installed bool   `json:"installed"`
			Path      string `json:"path"`
		}
		if json.Unmarshal(raw, &probe) == nil {
			st.Installed, st.Path = probe.Installed, probe.Path
		}
	}
	if st.Installed {
		st.Detail = "A post-commit hook rebuilds this index after every commit on this machine."
	} else {
		st.Detail = "No commit hook is installed, so this index is only as current as the last time it was built."
		st.Command = "repocortex hooks install"
	}
	return st
}

// HookReporter tells the portal whether a commit hook keeps this index current.
// Declared here, at the consumer: the portal renders the fact and never installs
// anything itself (installing is a CLI action, and a page must not write to a
// repository's .git directory).
type HookReporter interface {
	CommitHookState(root string) any
}

// HookReporterFunc adapts a function to HookReporter.
type HookReporterFunc func(root string) any

// CommitHookState implements HookReporter.
func (f HookReporterFunc) CommitHookState(root string) any { return f(root) }

// WithHookReporter supplies the commit-hook status source.
func WithHookReporter(h HookReporter) Option {
	return func(s *Server) { s.hooks = h }
}
