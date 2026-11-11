// Package web serves RepoCortex's local, read-only portal over an indexed
// repository.
//
// # Why this exists as a screen and not just as JSON
//
// Every competing "code graph" product visualises an LLM-asserted graph, in
// which each edge is an unverifiable assertion: the picture is the claim, and
// there is nothing behind it to check. RepoCortex's graph is the only one where
// every edge carries EVIDENCE (a file and a line) and a MEASURED confidence
// (docs/accuracy.md adjudicates 30,975 heuristic edges against compiler-backed
// indexes). A portal that rendered all edges identically would throw away the
// single property that distinguishes this product, so confidence is not a
// filter tucked into a settings panel here — it is the visual language of every
// view, and it is served as data (see Legend) so the client cannot invent its
// own mapping.
//
// # The rule this package exists to enforce
//
// NOTHING IS EVER SILENTLY OMITTED. Every response embeds an Omissions block
// stating what the view left out and why, and it is impossible to serialise a
// response without one: Omissions.MarshalJSON always emits the fields and
// always recomputes `complete` from the recorded omissions, so a handler that
// forgets to fill it in publishes `complete: true` only when it really did
// return everything. This is the same contract capability.NewCompleteness
// enforces for context packs and impact reports, restated at the HTTP boundary
// because that is where a truncation is easiest to hide.
//
// Every list this package shortens goes through capped, which records the
// remainder. That is a convention, not a compiler rule; the tests assert the
// counts rather than the convention.
//
// # Read-only, and local
//
// This package performs no writes. Its Registry port exposes no writer, the CLI
// opens the database read-only, and every route is GET. It also refuses
// non-loopback Host headers by default, because a page on the public internet
// can otherwise point a browser at http://localhost:7777 and read the whole
// structure of a private codebase (DNS rebinding). That guard is relaxed only
// when the operator explicitly bound to a non-loopback address.
//
// # What it does not do
//
// It does not traverse, rank, score or resolve anything itself. Blast radius is
// capability/impact, covering tests are capability/tests, governance is
// capability/architecture. A second traversal here would be a second place for
// "reports depth-3 reachability as high confidence" to be reintroduced.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ---------------------------------------------------------------- ports
//
// Declared here, at the consumer, and split by intent rather than gathered into
// one twelve-method facade (CLAUDE.md §1, §3 ISP). The split is not decoration:
// BulkReader is the expensive one — three whole-run table scans — and keeping it
// nameable is what let structuralIndex be built once per run behind a cache
// instead of per request.

// RunReader resolves which index run a reader may see.
type RunReader interface {
	CurrentRun(ctx context.Context) (*model.IndexRun, error)
}

// SummaryReader supplies the run-level totals behind the overview's headline
// numbers. All three are aggregate queries, cheap enough to run per request.
type SummaryReader interface {
	Counts(ctx context.Context, runID int64) (registry.Counts, error)
	LanguageCounts(ctx context.Context, runID int64) (map[string]int, error)
	ResolutionStats(ctx context.Context, runID int64) (registry.ResolutionStats, error)
}

// BulkReader supplies the whole run's structure.
//
// Deliberately its own interface: these three calls materialise every file,
// symbol and edge in the run, which is what the graph explorer and the blind-spot
// matrix genuinely need and what nothing else should reach for casually. The
// results are folded into a compact structuralIndex and the raw rows dropped —
// see index.go for the measured reason.
//
// Structurally identical to architecture.Registry on purpose, so the governance
// handler passes its Registry straight through rather than re-reading.
type BulkReader interface {
	Files(ctx context.Context, runID int64) ([]model.File, error)
	Symbols(ctx context.Context, runID int64) ([]model.Symbol, error)
	Edges(ctx context.Context, runID int64) ([]model.Edge, error)
}

// SurfaceReader supplies the externally visible surfaces of the repository:
// what it exposes to callers and what it touches in a database.
type SurfaceReader interface {
	EndpointReport(ctx context.Context, runID int64) ([]registry.Endpoint, error)
	TableUsageReport(ctx context.Context, runID int64) ([]registry.TableUsage, error)
}

// Registry is the composed read port. Nothing here writes, and there is no
// method on it that could.
type Registry interface {
	RunReader
	SummaryReader
	BulkReader
	SurfaceReader
}

// Compile-time proof the production adapter satisfies the port. If a registry
// signature changes, this file stops building rather than the wiring failing at
// request time.
var _ Registry = (*registry.Registry)(nil)

// ---------------------------------------------------------------- server

// Deps is what the server is given instead of a database handle.
type Deps struct {
	// Registry is the bulk/summary read port.
	Registry Registry

	// Caps is the capability layer's own dependency bundle, carrying the
	// memoised symbol index. Blast radius and covering tests are answered
	// through it, never re-implemented here.
	Caps capability.Deps

	// Config carries the declared architecture model and the project name.
	Config config.Config

	// Packs is the read surface internal/context needs to assemble a context
	// pack, which is what the contract and verify screens are compiled from.
	//
	// Optional, and separate from Registry on purpose: it is a WIDER surface
	// (full-text file search and file excerpting) than any other screen needs,
	// and a caller that only wants the four graph screens should not have to
	// supply it. When it is nil the two task screens fall back to Registry if
	// that value happens to satisfy PackReader, and otherwise answer with
	// guidance naming the missing wiring rather than an empty brief.
	Packs PackReader

	// ConfigKeys and Dependencies are the two narrow read ports the ConfigMind
	// and DepMind screens need.
	//
	// Optional, and kept OUT of the composed Registry above on purpose: each is
	// a single method serving a single screen, and widening the port every
	// implementation of Registry must satisfy in order to add a screen is the
	// ISP violation CLAUDE.md §3 names. When one is nil the screen falls back
	// to Registry if that value happens to satisfy the port — the production
	// registry does — and otherwise answers with guidance naming the missing
	// wiring rather than an empty inventory that reads like a repository with
	// no configuration and no dependencies. Same shape as Packs, for the same
	// reason.
	ConfigKeys   ConfigReader
	Dependencies DependencyReader

	// Source is the code map's read of a file's indexed text (source.go).
	// Optional for the same reason as Packs: when nil the Code view falls back
	// to Packs or Registry if either satisfies it — the production registry
	// does — and otherwise answers with guidance rather than an empty file.
	Source SourceReader

	// IndexPath is shown in the portal so a reader always knows which index
	// they are looking at. Displayed, never opened by this package.
	IndexPath string
}

// WithConfigReader injects the configuration read port explicitly, for a caller
// whose Registry does not satisfy it.
func WithConfigReader(cr ConfigReader) Option {
	return func(s *Server) {
		if cr != nil {
			s.deps.ConfigKeys = cr
		}
	}
}

// WithDependencyReader injects the dependency read port explicitly, for a
// caller whose Registry does not satisfy it.
func WithDependencyReader(dr DependencyReader) Option {
	return func(s *Server) {
		if dr != nil {
			s.deps.Dependencies = dr
		}
	}
}

// Option configures a Server (CLAUDE.md §2, functional options).
type Option func(*Server)

// WithLogger sets the request logger. Defaults to a discarding logger so a
// Server is usable in a test without wiring one.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.log = l
		}
	}
}

// WithAllowRemoteHosts disables the loopback Host check.
//
// Set only when the operator has deliberately bound to a non-loopback address.
// The guard's whole purpose is that a page on the public internet cannot use a
// victim's browser to read a private repository's structure off localhost, so it
// is on unless the deployment has already given that up.
func WithAllowRemoteHosts(allow bool) Option {
	return func(s *Server) { s.allowRemoteHosts = allow }
}

// WithMaxNodes bounds how many symbol-level nodes the graph explorer will
// return in one response. Whatever it excludes is stated, never dropped.
func WithMaxNodes(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.maxNodes = n
		}
	}
}

// WithCycleBudget bounds cycle enumeration on the metrics screen.
//
// Injectable because the bound is the behaviour: a budget no test can reach is
// a budget no test can prove discloses itself, and the disclosure is the half
// that shipped broken. Defaults to metrics.DefaultCycleBudget.
func WithCycleBudget(b metrics.CycleBudget) Option {
	return func(s *Server) {
		if b.MaxLen >= 2 {
			s.cycleBudget = b
		}
	}
}

// WithClock injects the clock, so index-freshness assertions are deterministic.
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

// Default bounds. Small enough that a response stays interactive, large enough
// that a real repository's component graph fits without truncation.
const (
	// DefaultMaxNodes bounds symbol-level graph responses.
	DefaultMaxNodes = 1500

	// DefaultMaxEdges bounds symbol-level graph responses after aggregation.
	DefaultMaxEdges = 4000

	// maxRows bounds every listing (impacted symbols, tests, violations) in a
	// single response body.
	maxRows = 500
)

// Server is the portal's HTTP surface.
//
// Safe for concurrent use. The only mutable state is the structural-index
// cache, guarded by mu and keyed by run id — an index swap invalidates it
// automatically rather than serving a run CommitRun has already retired.
type Server struct {
	deps Deps
	log  *slog.Logger
	now  func() time.Time

	allowRemoteHosts bool
	maxNodes         int
	cycleBudget      metrics.CycleBudget

	// reindexer is nil unless the operator passed --allow-refresh. Nil means
	// the POST route is never registered, so the portal stays GET-only.
	reindexer      Reindexer
	reindexTimeout time.Duration
	reindexes      reindexGate

	// hooks reports whether a commit hook keeps the index current. Nil is
	// "unknown", never "not installed".
	hooks HookReporter

	// assembler owns the context-pack catalogue cache (CLAUDE.md §4.2: the
	// cache belongs to a struct, not to a package). One per server, so a
	// portal answering repeated contract requests reuses the catalogue the way
	// the MCP server does, instead of rebuilding it per request the way a CLI
	// invocation would.
	assembler *devctx.Assembler

	mu       sync.RWMutex
	cachedID int64
	cached   *structuralIndex

	mux *http.ServeMux
}

// New builds the portal server.
func New(deps Deps, opts ...Option) (*Server, error) {
	if deps.Registry == nil {
		return nil, errors.New("web: Deps.Registry is nil")
	}
	if err := deps.Caps.Validate(); err != nil {
		return nil, fmt.Errorf("web: capability dependencies: %w", err)
	}
	s := &Server{
		deps:           deps,
		log:            slog.New(slog.DiscardHandler),
		now:            time.Now,
		maxNodes:       DefaultMaxNodes,
		cycleBudget:    metrics.DefaultCycleBudget(),
		assembler:      devctx.NewAssembler(),
		reindexTimeout: DefaultReindexTimeout,
	}
	for _, o := range opts {
		o(s)
	}
	s.mux = s.routes()
	return s, nil
}

// Handler returns the portal's http.Handler, wrapped in the cross-cutting
// middleware (CLAUDE.md §2, decorator): method allow-list, loopback Host guard,
// no-store caching and a conservative content policy.
func (s *Server) Handler() http.Handler {
	return s.withGuards(s.mux)
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// The seven original screens, the two supporting endpoints they need, and
	// the four pillar screens registered below them.
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/graph", s.handleGraph)
	mux.HandleFunc("GET /api/blast", s.handleBlast)
	mux.HandleFunc("GET /api/governance", s.handleGovernance)
	mux.HandleFunc("GET /api/crossstack", s.handleCrossStack)
	mux.HandleFunc("GET /api/contract", s.handleContract)
	mux.HandleFunc("GET /api/verify", s.handleVerify)
	mux.HandleFunc("GET /api/symbols", s.handleSymbols)
	mux.HandleFunc("GET /api/source", s.handleSource)
	mux.HandleFunc("GET /api/indexstate", s.handleIndexState)

	// The four pillar screens that had a CLI command and no portal endpoint.
	// Each reads exactly what its command reads; see dbmind.go, configmind.go,
	// depmind.go and testmind.go for which call, and why it is not recomputed.
	mux.HandleFunc("GET /api/db", s.handleDB)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/deps", s.handleDeps)
	mux.HandleFunc("GET /api/tests", s.handleTests)

	// The metrics screen is registered here and NOT with the pillar screens,
	// because it does not share the property that groups them: every pillar
	// screen is empty until the reader does a second thing (import a compiler
	// index, index twice, export a trace, write a bench case). Centrality is
	// computed over the call graph any ordinary index already holds.
	mux.HandleFunc("GET /api/metrics", s.handleMetrics)

	// Event topology, blindspot detection, and refactoring planner endpoints.
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/missed", s.handleMissed)
	mux.HandleFunc("GET /api/planchange", s.handlePlanChange)
	mux.HandleFunc("GET /api/agent", s.handleAgent)

	// The remaining pillars — accuracy, fact diff, ownership, trace, retrieval
	// score and the federated estate. Registered from one function so the set
	// stays legible as it grows past a dozen routes.
	s.registerPillarRoutes(mux)

	// The only write in the portal, and only when the operator enabled it.
	// Registering conditionally means "read-only" is enforced by the absence of
	// a route, not by a check inside a handler someone could later reorder.
	if s.reindexer != nil {
		mux.HandleFunc("POST /api/reindex", s.handleReindex)
	}

	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Unreachable: the embed directive fixes the tree at build time. Panic
		// would violate CLAUDE.md §4.3, so serve an explanation instead.
		mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "web: embedded assets unavailable: "+err.Error(), http.StatusInternalServerError)
		})
		return mux
	}
	files := http.FileServerFS(sub)
	mux.HandleFunc("GET /assets/", s.serveAssets(files))
	mux.HandleFunc("GET /{$}", s.serveIndex(sub))
	// Any other path is the SPA shell too, so a deep link (#graph) survives a
	// reload. It is still a 404 for anything under /api that does not exist,
	// because those are handled by more specific patterns above.
	mux.HandleFunc("GET /", s.serveIndex(sub))
	return mux
}

func (s *Server) serveIndex(sub fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "no such endpoint: " + r.URL.Path,
				"endpoints": []string{
					"/api/health", "/api/overview", "/api/graph",
					"/api/blast", "/api/governance", "/api/crossstack",
					"/api/contract", "/api/verify", "/api/symbols", "/api/source", "/api/indexstate",
					"/api/db", "/api/config", "/api/deps", "/api/tests",
					"/api/metrics",
					"/api/accuracy", "/api/diff", "/api/who", "/api/trace",
					"/api/bench", "/api/system",
					"/api/events", "/api/missed", "/api/planchange",
					"/api/agent",
				},
			})
			return
		}
		body, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "web: read index.html: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}
}

// withGuards applies the cross-cutting concerns every route needs.
func (s *Server) withGuards(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read-only means read-only at the protocol level too. Anything that is
		// not a read is refused before it reaches a handler.
		if !s.methodAllowed(r) {
			allow := "GET, HEAD"
			if s.reindexer != nil && r.URL.Path == "/api/reindex" {
				allow = "POST"
			}
			w.Header().Set("Allow", allow)
			http.Error(w, "the portal is read-only; only GET and HEAD are served", http.StatusMethodNotAllowed)
			return
		}
		if !s.allowRemoteHosts && !isLoopbackHost(r.Host) {
			// DNS rebinding: a public page resolves its own name to 127.0.0.1
			// and reads this API with the victim's browser. The Host header is
			// the one field that attack cannot forge into a loopback name while
			// still reaching a loopback socket.
			http.Error(w,
				"refused: this portal is bound to localhost and only answers requests whose Host is a "+
					"loopback address. Reaching it from another machine exposes the whole structure of this "+
					"repository; start it with an explicit --host if that is what you intend.",
				http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// Self-contained by construction: no CDN, no remote font, no analytics.
		// Saying so in a header means a mistake later fails visibly.
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether a Host header names this machine.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") || strings.HasSuffix(strings.ToLower(name), ".localhost") {
		return true
	}
	if ip := net.ParseIP(name); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ---------------------------------------------------------------- responses

// writeJSON encodes v. Errors after the first byte cannot be reported to the
// client, so they are logged rather than discarded (CLAUDE.md §4.3).
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		s.log.Error("web: encode response", "error", err)
		http.Error(w, `{"error":"internal encoding failure"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		s.log.Debug("web: write response", "error", err)
	}
}

// Guidance is what a view returns instead of data when there is nothing to
// show yet.
//
// A portal that answers an unindexed repository with a 500 teaches the reader
// that the tool is broken, when in fact they have not run it yet. So the
// not-indexed case is a normal 200 answer carrying the command that fixes it:
// the server is healthy, the index is absent, and those are different facts.
type Guidance struct {
	Title  string   `json:"title"`
	Detail string   `json:"detail"`
	Steps  []string `json:"steps"`
}

// Meta is the header every screen response carries: which index, how fresh, and
// how much of it is trustworthy.
type Meta struct {
	Project   string `json:"project"`
	IndexPath string `json:"index_path"`

	// ReadOnly states whether this portal can cause a write. It is FALSE when
	// the operator passed --allow-refresh, because a portal that can rebuild the
	// index is not read-only and must not say it is — a reader who trusts this
	// field is deciding whether it is safe to leave the portal running.
	//
	// It is not a claim about the database handle, which is opened read-only in
	// every configuration; a refresh runs the indexer, which opens its own.
	ReadOnly bool `json:"read_only"`

	Indexed bool      `json:"indexed"`
	Run     *RunMeta  `json:"run,omitempty"`
	Help    *Guidance `json:"guidance,omitempty"`

	// Health is the run's own verdict on itself, promoted out of Run.Status.
	//
	// # Why this is a required field and not a status string
	//
	// The API has always sent `run.status: "degraded"`. Every screen received
	// it. Nothing rendered it: the freshness card printed `current` and
	// `complete` badges from other fields, the status string reached only a
	// footer line, and grep for "degraded" in the client returned nothing. A
	// degraded run therefore presented as a healthy one everywhere a reader
	// actually looks, on every screen, for as long as the portal has existed.
	//
	// A bare status string invites that: it is one word among twenty in a meta
	// block, easy to pass over and easy to forget. This is a structured verdict
	// with a severity, a headline written for a reader rather than a machine,
	// and the specific numbers behind it — a shape a client cannot render as
	// "everything is fine" without deliberately dropping it, and one the
	// contract test can require every screen to display.
	//
	// Never omitempty. "This view does not say" and "this run is healthy" are
	// different claims and the whole package rests on the difference.
	Health IndexHealth `json:"index_health"`
}

// HealthSeverity is how loudly a client must render the verdict.
type HealthSeverity string

const (
	// HealthOK: the run completed and accounted for everything it discovered.
	HealthOK HealthSeverity = "ok"

	// HealthWarn: the run committed, but part of the tree is missing from it.
	// Every answer drawn from it is a lower bound by an amount this states.
	HealthWarn HealthSeverity = "warn"

	// HealthError: the run itself reports failure, or there is no run.
	HealthError HealthSeverity = "error"
)

// HealthReason is one specific, counted thing wrong with the run.
type HealthReason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`

	// Count sizes the problem. 0 means unquantified, matching the convention
	// Omission.Count uses.
	Count int64 `json:"count"`

	// Remedy names the command that addresses it.
	Remedy string `json:"remedy,omitempty"`
}

// IndexHealth is the verdict a reader must see before trusting any number on
// any screen.
type IndexHealth struct {
	// Status is the run's own recorded status, verbatim, so a reader comparing
	// the portal against `cortex status` sees the same word.
	Status string `json:"status"`

	Severity HealthSeverity `json:"severity"`

	// Headline is the one sentence a reader needs. Written here, once, so seven
	// screens cannot phrase the same verdict seven ways.
	Headline string `json:"headline"`

	// Reasons is why. Empty only when Severity is ok.
	Reasons []HealthReason `json:"reasons"`

	// Trustworthy is the single boolean a client can gate a banner on. False
	// whenever Severity is not ok — stated as its own field because a client
	// comparing strings can get a comparison wrong and a client reading a
	// boolean cannot.
	Trustworthy bool `json:"trustworthy"`
}

// healthFor derives the verdict from the committed run.
//
// One place. The CLI's status word, the freshness card's badges and this
// headline all describe the same run, and the way they came to disagree was by
// each deriving its own view of it.
func healthFor(run *model.IndexRun, ageSeconds int64) IndexHealth {
	h := IndexHealth{
		Status: string(run.Status), Severity: HealthOK, Reasons: []HealthReason{},
		Headline:    "This index completed and accounted for every file it discovered.",
		Trustworthy: true,
	}
	worsen := func(sev HealthSeverity) {
		if sev == HealthError || (sev == HealthWarn && h.Severity == HealthOK) {
			h.Severity = sev
		}
	}

	switch strings.ToLower(string(run.Status)) {
	case "failed", "error":
		worsen(HealthError)
		h.Reasons = append(h.Reasons, HealthReason{
			Code: "run_failed",
			Detail: "The indexing run recorded itself as failed. What was committed is whatever it " +
				"managed before it stopped, and nothing here says which part that is.",
			Remedy: "cortex index",
		})
	case "degraded", "partial":
		worsen(HealthWarn)
		h.Reasons = append(h.Reasons, HealthReason{
			Code: "run_degraded",
			Detail: "The indexing run recorded itself as degraded: it committed, but not everything it " +
				"set out to do succeeded. Every count on every screen is a LOWER BOUND.",
			Remedy: "cortex doctor",
		})
	}
	if !run.Complete() {
		worsen(HealthWarn)
		h.Reasons = append(h.Reasons, HealthReason{
			Code: "index_incomplete",
			Detail: "The index state is " + string(run.IndexState) + " — the pass that binds references " +
				"into edges has not finished, so relationship answers are partial by construction.",
			Remedy: "cortex resolve",
		})
	}
	if run.FilesUnaccounted > 0 {
		worsen(HealthWarn)
		h.Reasons = append(h.Reasons, HealthReason{
			Code:  "files_unaccounted",
			Count: run.FilesUnaccounted,
			Detail: fmt.Sprintf("%d file(s) were discovered and never accounted for. They hold symbols "+
				"nothing on any screen can mention, and an empty answer about them is not evidence of "+
				"absence.", run.FilesUnaccounted),
			Remedy: "cortex doctor --verify-index",
		})
	}
	if run.ParseFailures > 0 {
		worsen(HealthWarn)
		h.Reasons = append(h.Reasons, HealthReason{
			Code:  "parse_failures",
			Count: run.ParseFailures,
			Detail: fmt.Sprintf("%d file(s) failed to parse. Nothing was extracted from them, so every "+
				"edge that ran through one is missing rather than absent.", run.ParseFailures),
			Remedy: "cortex doctor",
		})
	}
	if ageSeconds > staleAfterSeconds {
		worsen(HealthWarn)
		h.Reasons = append(h.Reasons, HealthReason{
			Code:  "stale",
			Count: ageSeconds / 86400,
			Detail: "This index is more than a day old, so it describes the tree as it was, not as it " +
				"is. Anything changed since is invisible here.",
			Remedy: "cortex index",
		})
	}

	switch h.Severity {
	case HealthError:
		h.Trustworthy = false
		h.Headline = "This index did not complete. Do not act on anything here without re-indexing."
	case HealthWarn:
		h.Trustworthy = false
		h.Headline = fmt.Sprintf("This index is %s: %d problem(s) mean every count on every screen is a "+
			"lower bound, not a total.", h.Status, len(h.Reasons))
	}
	return h
}

// unindexedHealth is the verdict when there is no committed run at all. An
// error, not an absence: a screen rendering empty arrays with no verdict looks
// like a repository with nothing in it.
func unindexedHealth() IndexHealth {
	return IndexHealth{
		Status: "none", Severity: HealthError, Trustworthy: false,
		Headline: "There is no committed index, so every view on this portal is empty for that reason " +
			"and no other.",
		Reasons: []HealthReason{{
			Code:   "no_committed_run",
			Detail: "No indexing pass has been committed. Readers only ever see a committed run.",
			Remedy: "cortex index",
		}},
	}
}

// RunMeta describes the committed run being served.
type RunMeta struct {
	ID           int64  `json:"run_id"`
	Status       string `json:"status"`
	State        string `json:"index_state"`
	Complete     bool   `json:"complete"`
	GitRevision  string `json:"git_revision,omitempty"`
	AccuracyMode string `json:"accuracy_mode,omitempty"`

	StartedUnix  int64 `json:"started_unix"`
	FinishedUnix int64 `json:"finished_unix,omitempty"`
	AgeSeconds   int64 `json:"age_seconds"`

	FilesDiscovered  int64 `json:"files_discovered"`
	FilesIndexed     int64 `json:"files_indexed"`
	FilesUnaccounted int64 `json:"files_unaccounted"`
	ParseFailures    int64 `json:"parse_failures"`
}

// notIndexed is the shared "there is nothing here yet" answer.
func (s *Server) notIndexed(detail string) Meta {
	return Meta{
		Project:   s.deps.Config.ProjectName,
		IndexPath: s.deps.IndexPath,
		ReadOnly:  s.reindexer == nil,
		Indexed:   false,
		Health:    unindexedHealth(),
		Help: &Guidance{
			Title:  "This repository has no committed index yet",
			Detail: detail,
			Steps: []string{
				"cortex init    — write .cortex.yml and create the index",
				"cortex index   — parse the tree and extract symbols",
				"cortex resolve — bind references into edges, which is what every view here reads",
			},
		},
	}
}

// run resolves the committed run and builds the Meta header for it.
//
// The (Meta, bool) shape is deliberate: false means the caller must return the
// Meta as-is, with guidance, rather than continue. registry.ErrNoCommittedRun
// is a normal state, not a fault.
func (s *Server) run(ctx context.Context) (Meta, *model.IndexRun, bool, error) {
	cur, err := s.deps.Registry.CurrentRun(ctx)
	if errors.Is(err, registry.ErrNoCommittedRun) {
		return s.notIndexed("The store exists but no indexing pass has been committed, " +
			"so there is no graph to show. Readers only ever see a committed run."), nil, false, nil
	}
	if err != nil {
		return Meta{}, nil, false, err
	}

	finished := cur.FinishedUnix
	if finished == 0 {
		finished = cur.StartedUnix
	}
	meta := Meta{
		Project:   s.deps.Config.ProjectName,
		IndexPath: s.deps.IndexPath,
		ReadOnly:  s.reindexer == nil,
		Indexed:   true,
		Run: &RunMeta{
			ID:               cur.ID,
			Status:           string(cur.Status),
			State:            string(cur.IndexState),
			Complete:         cur.Complete(),
			GitRevision:      cur.GitRevision,
			AccuracyMode:     cur.AccuracyMode,
			StartedUnix:      cur.StartedUnix,
			FinishedUnix:     cur.FinishedUnix,
			AgeSeconds:       s.now().Unix() - finished,
			FilesDiscovered:  cur.FilesDiscovered,
			FilesIndexed:     cur.FilesIndexed,
			FilesUnaccounted: cur.FilesUnaccounted,
			ParseFailures:    cur.ParseFailures,
		},
	}
	meta.Health = healthFor(cur, meta.Run.AgeSeconds)
	return meta, cur, true, nil
}

// fail turns an unexpected error into a response that says what broke without
// leaking a driver message into the browser.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		// The reader navigated away. Nothing to report and nobody to report to.
		s.log.Debug("web: request cancelled", "path", r.URL.Path)
		return
	}
	s.log.Error("web: request failed", "path", r.URL.Path, "error", err)
	s.writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": err.Error(),
		"guidance": Guidance{
			Title:  "The portal could not answer this view",
			Detail: "The index may be from an older schema, or partially written.",
			Steps: []string{
				"cortex doctor — check the index for the fault above",
				"cortex index  — rebuild it if doctor reports damage",
			},
		},
	})
}

// ---------------------------------------------------------------- parameters

func intParam(r *http.Request, name string, def, minV, maxV int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < minV {
		return minV
	}
	if n > maxV {
		return maxV
	}
	return n
}

// confidenceParam reads a floor from the query string.
//
// An unrecognised value falls back to the supplied default rather than erroring:
// a typo in a URL should not blank the screen, and the floor actually applied is
// echoed in every response so the reader can see what happened.
func confidenceParam(r *http.Request, name string, def model.Confidence) model.Confidence {
	c := model.Confidence(strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))))
	if !c.Valid() {
		return def
	}
	return c
}

// kindsParam reads a comma-separated edge-kind filter. Unknown kinds are
// ignored; an empty result means "no filter", which every consumer treats as
// all kinds.
func kindsParam(r *http.Request, name string) []model.EdgeKind {
	raw := r.URL.Query().Get(name)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []model.EdgeKind
	seen := map[model.EdgeKind]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		k := model.EdgeKind(strings.ToUpper(strings.TrimSpace(part)))
		if _, ok := knownEdgeKinds[k]; !ok {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// knownEdgeKinds is the closed vocabulary from pkg/model, restated as a set so
// a query string cannot inject a kind the graph has never heard of.
var knownEdgeKinds = func() map[model.EdgeKind]struct{} {
	all := []model.EdgeKind{
		model.EdgeContains, model.EdgeImports, model.EdgeReferences, model.EdgeCalls,
		model.EdgeExtends, model.EdgeImplements, model.EdgeDependsOn, model.EdgeExposes,
		model.EdgeReads, model.EdgeWrites, model.EdgePublishes, model.EdgeConsumes,
		model.EdgeTestedBy, model.EdgeConfigures, model.EdgeTracesTo, model.EdgeViolates,
		model.EdgeOwnedBy, model.EdgeChangedBy,
	}
	m := make(map[model.EdgeKind]struct{}, len(all))
	for _, k := range all {
		m[k] = struct{}{}
	}
	return m
}()

// serveAssets serves the embedded static files under /assets/. A named handler
// rather than an anonymous wrapper, so the route is attributed to the code that
// serves it.
func (s *Server) serveAssets(files http.Handler) http.HandlerFunc {
	return http.StripPrefix("/assets/", files).ServeHTTP
}
