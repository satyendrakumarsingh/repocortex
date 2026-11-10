package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	acquiregit "github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is the MCP surface version.
//
// Separate from the binary version because MCP tool schemas are CONTRACTS.
// An agent's behaviour depends on argument names and result shapes, so a
// breaking change needs a new tool name (tool.v2), not a silent edit. Spec §54.
const Version = "0.1.0"

// Deps are what the server needs to answer questions.
//
// An interface rather than a concrete registry so the tool layer can be tested
// without a database, and so a future backend can be swapped in without
// touching tool definitions.
type Deps struct {
	Registry *registry.Registry

	// Graph enables get_task_context. Optional: without it the server still
	// starts and answers symbol and search queries, which is the right
	// behaviour on an index whose references have never been resolved.
	Graph *graph.Graph

	Config config.Config
	Limits Limits
}

// Server wraps the SDK server with RepoCortex's tool set.
type Server struct {
	sdk    *sdk.Server
	deps   Deps
	tools  []string
	limits Limits

	// assembler owns the context engine's catalogue cache for this server's
	// lifetime. This is the one place the cache genuinely pays: an agent asks
	// several questions of one index in a session, and rebuilding the catalogue
	// per request cost 1.34s at 94,000 symbols. It is a field rather than a
	// package-level var because CLAUDE.md §4.2 forbids the latter, and because
	// "which index is this cache serving?" should be answerable from the type.
	assembler *devctx.Assembler

	// ledgers records, per live MCP session, which source ranges have already
	// been delivered to that conversation, so the next call can point back at
	// them instead of sending them again. See dedup.go; the state is
	// per-session and dies with the session, never persisted.
	ledgers *ledgerBook

	// capDeps carries the capability layer's memoised symbol index, built once
	// for the server's lifetime.
	//
	// It was being constructed PER REQUEST, which allocated a fresh empty cache
	// each time and re-ran LoadSymbolIndex — an unbounded scan of the whole
	// symbol table — on every impact_analysis and find_tests call. The
	// capability package's own doc says "long-lived callers, the MCP server
	// especially, should use NewDeps"; the server was the one caller ignoring
	// it.
	capDeps capability.Deps

	// runMeta memoises each committed run's GitRevision and StartedUnix by run
	// ID, so reply() can build Provenance WITHOUT a second registry read: the
	// row was already fetched once by currentRun() earlier in the very same
	// request. Keyed by ID (not a single cached value) so a reindex that
	// commits mid-session is reflected correctly instead of pinning the
	// server to whichever run happened to be current at startup.
	//
	// Bounded to the current run plus the one before it (runMetaOrder tracks
	// insertion order): this is an insert-only map on a long-lived server, and
	// CLAUDE.md §4.4 requires bounded buffers. Two entries are enough — a
	// response only ever needs the run current at the moment it was built —
	// and keeping one extra tolerates a reindex racing an in-flight request.
	runMetaMu    sync.RWMutex
	runMeta      map[int64]runMeta
	runMetaOrder []int64

	// headRepo is a git handle opened once for the server's lifetime, used to
	// read HEAD fresh on every request via HeadSHA.
	//
	// Info() on this package does a full worktree status scan (~15ms on a
	// synthetic 200-file repo per its own benchmarks) to compute Dirty, a
	// field Provenance never uses. HeadSHA skips that scan entirely — it
	// costs one ref read, not a tree walk — so there is no need to cache its
	// result: go-git re-reads refs from disk on every call, so calling it
	// per request means a `git checkout` made mid-session is reflected on
	// the very next tool call, not just after a restart. Open itself
	// (~1ms) is what is cached, since the repository path never changes for
	// a running server.
	headRepo *acquiregit.Repo
}

// runMeta is the slice of a committed run's data Provenance needs.
type runMeta struct {
	gitRevision string
	startedUnix int64
}

// New builds an MCP server exposing the tools allowed by the configured
// profile.
func New(deps Deps) (*Server, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("mcp: registry is required")
	}
	limits := deps.Limits
	if limits.MaxItems == 0 {
		limits = DefaultLimits()
	}

	impl := &sdk.Implementation{
		Name:    "cortex",
		Version: Version,
		Title:   "RepoCortex code intelligence",
	}

	// Opened once, here, rather than per request: Open is ~1ms and the
	// repository path never changes for the life of the server. A genuine
	// open failure (unreadable path, corrupt repository) is not fatal to the
	// server — it just means HEAD reads as unknown, same as a non-repository
	// — so the error is deliberately not propagated.
	headRepo, _ := acquiregit.Open(deps.Config.RepoRoot)

	s := &Server{
		sdk:       sdk.NewServer(impl, nil),
		deps:      deps,
		limits:    limits,
		assembler: devctx.NewAssembler(),
		ledgers:   newLedgerBook(),
		capDeps:   capability.NewDeps(deps.Registry, deps.Graph),
		runMeta:   make(map[int64]runMeta),
		headRepo:  headRepo,
	}

	if err := s.registerTools(); err != nil {
		return nil, err
	}
	s.registerTaskContext(profileTools(deps.Config.MCPProfile))
	s.registerCapabilities(profileTools(deps.Config.MCPProfile))
	return s, nil
}

// Tools lists the registered tool names. For `cortex serve --list-tools` and
// for tests asserting a profile's surface.
func (s *Server) Tools() []string { return append([]string(nil), s.tools...) }

// Run serves until the context is cancelled or the transport closes.
func (s *Server) Run(ctx context.Context, t sdk.Transport) error {
	return s.sdk.Run(ctx, t)
}

// SDK exposes the underlying server, for tests using in-memory transports.
func (s *Server) SDK() *sdk.Server { return s.sdk }

// profileTools maps a profile to the tools it exposes.
//
// THE TOOL COUNT IS A PERFORMANCE CONSTRAINT, NOT A PREFERENCE.
//
// Every exposed tool's schema is injected into the agent's context on EVERY
// request, and a long tool list measurably degrades tool selection. A server
// exposing forty tools would spend context on descriptions the agent never
// uses — which is precisely the waste RepoCortex exists to eliminate. Shipping a
// small default is therefore part of the product, not a limitation of it.
func profileTools(p config.MCPProfile) map[string]bool {
	minimal := []string{
		// First in the list because it is the tool an agent should reach for
		// first: one call instead of a dozen exploratory ones.
		"get_task_context",
		"get_project_overview",
		"search_code",
		"find_symbol",
		"get_symbol",
		"find_references",
		"find_callers",
	}
	// `find_callees` and `get_file_outline` used to be listed here and are not
	// registered anywhere — no such tools exist. The names were inert, so
	// `standard` and `full` silently exposed an identical set and the profile
	// promised a surface it did not have. Removed rather than implemented,
	// because the argument above says the default surface should be small and
	// inventing two tools to justify a stale list is the wrong direction.
	//
	// TestProfilesOnlyNameRegisteredTools now fails the build on a repeat.
	standard := append(append([]string{}, minimal...),
		"find_tests",
		"impact_analysis",
		// Kept out of `minimal` because get_project_overview already states the
		// figure once, and minimal's whole argument is that seven schemas is
		// the budget. It joins `standard` with impact_analysis because that is
		// the profile where an agent starts reading relationship results, and a
		// short result it cannot re-check against the index's own coverage is
		// the one an agent over-reads.
		"resolution_coverage",
		"plan_rename",
		"find_api_endpoints",
		"find_db_access",
		"get_config_key",
		"get_file_outline",
		"minimize_diff",
		"plan_change",
		"detect_blindspots",
	)

	set := func(names []string) map[string]bool {
		m := make(map[string]bool, len(names))
		for _, n := range names {
			m[n] = true
		}
		return m
	}

	switch p {
	case config.MCPStandard:
		return set(standard)
	case config.MCPFull:
		return nil // nil means "no filter"
	default:
		return set(minimal)
	}
}

// addTool registers a tool if the active profile allows it.
func addTool[In, Out any](s *Server, allowed map[string]bool, t *sdk.Tool, h sdk.ToolHandlerFor[In, Out]) {
	if allowed != nil && !allowed[t.Name] {
		return
	}
	sdk.AddTool(s.sdk, t, h)
	s.tools = append(s.tools, t.Name)
}

// ------------------------------------------------------------- isError policy
//
// AN isError RESULT IS A STATEMENT ABOUT THE TOOL, NOT ABOUT THE ANSWER.
//
// The host surfaces isError to the model as "this call failed". A model that
// sees a tool fail early concludes the toolset is broken and STOPS CALLING IT
// FOR THE REST OF THE SESSION — measured repeatedly, and the cost is not the
// one bad answer, it is every answer RepoCortex would have given afterwards. A
// server that flags "you have not indexed yet" as a failure has therefore
// disabled itself over a condition the user fixes with one command.
//
// So isError is reserved for exactly two things:
//
//   - SECURITY REFUSALS — RepoCortex declined to do what was asked.
//   - GENUINE MALFUNCTION — storage failed, a traversal errored, or the call
//     was malformed. Something is wrong that the agent cannot fix by asking a
//     better question, and pretending otherwise would hand it a plausible
//     empty answer built on a broken read.
//
// Everything else is a RECOVERABLE STATE OF THE WORLD: no index yet, an empty
// index, a symbol that simply is not present. The tool worked perfectly; the
// answer is "nothing, and here is what to do next". Those return through
// guide(), which produces an ordinary SUCCESSFUL result whose text carries the
// next step. The next step is the load-bearing part — an agent that reads "no
// index — run 'cortex index'" acts on it, whereas one that reads a failure
// gives up on the server.

// errNotIndexed marks the recoverable "nothing has been indexed yet" state.
//
// It exists so callers can tell that condition apart from a storage failure.
// Both arrive from CurrentRun as an error; only one of them is RepoCortex
// malfunctioning, and they must not be answered the same way.
var errNotIndexed = errors.New("no committed index for this project")

// errIndexing marks the first index being built: nothing is committed yet, but
// something is on its way. Distinct from errNotIndexed so the guidance differs.
var errIndexing = errors.New("the first index for this project is still being built")

// currentRun resolves the committed index revision.
//
// The two failure modes are kept DISTINCT on purpose. "Nothing is indexed" is
// recoverable and gets guidance; a read that failed for any other reason is a
// malfunction and must not be dressed up as "you have not indexed yet" —
// that advice would send the user to re-run an index against a broken store.
func (s *Server) currentRun(ctx context.Context) (int64, string, error) {
	run, err := s.deps.Registry.CurrentRun(ctx)
	if err != nil {
		if errors.Is(err, registry.ErrNoCommittedRun) {
			staging, serr := s.deps.Registry.StagingRun(ctx)
			if serr != nil {
				return 0, "absent", fmt.Errorf("read the staging index: %w", serr)
			}
			if staging != nil {
				return 0, string(model.StatusIndexing), errIndexing
			}
			return 0, "absent", errNotIndexed
		}
		return 0, "absent", fmt.Errorf("read the committed index: %w", err)
	}
	s.rememberRunMeta(run.ID, run.GitRevision, run.StartedUnix)
	return run.ID, string(run.Status), nil
}

// rememberRunMeta caches a committed run's git revision and start time,
// keyed by run ID, for reply() to build Provenance from without a second
// registry read.
//
// Bounded to the current run plus the previous one: a long-lived server
// across many reindexes must not grow this map without bound (CLAUDE.md
// §4.4), and nothing past the current run is ever looked up again.
func (s *Server) rememberRunMeta(runID int64, gitRevision string, startedUnix int64) {
	s.runMetaMu.Lock()
	defer s.runMetaMu.Unlock()
	if _, exists := s.runMeta[runID]; !exists {
		s.runMetaOrder = append(s.runMetaOrder, runID)
	}
	s.runMeta[runID] = runMeta{gitRevision: gitRevision, startedUnix: startedUnix}
	for len(s.runMetaOrder) > 2 {
		oldest := s.runMetaOrder[0]
		s.runMetaOrder = s.runMetaOrder[1:]
		delete(s.runMeta, oldest)
	}
}

// noRun turns a currentRun failure into the result it deserves.
//
// Every tool starts by resolving the run, so this is the single most frequently
// hit branch in the server — and, before the policy above, the single most
// likely place for a first-call isError to teach an agent that RepoCortex is
// broken.
func (s *Server) noRun(ctx context.Context, req *sdk.CallToolRequest, err error) (*sdk.CallToolResult, *Response, error) {
	if errors.Is(err, errIndexing) {
		return s.guide(ctx, req, 0, string(model.StatusIndexing),
			"The first index for this project is still being built, so there is nothing to answer from yet.",
			"Wait for 'cortex index' to finish, then ask again. Do not start a second index. "+
				"If no index is running, a previous one was interrupted: run 'cortex index' again.")
	}
	if errors.Is(err, errNotIndexed) {
		return s.guide(ctx, req, 0, "absent",
			"No index has been built for this project yet, so there is nothing to answer from.",
			"Run 'cortex index' in the project root, then ask again. This is not a tool "+
				"failure: RepoCortex is working and the index is simply absent.")
	}
	return failFrom(err, "%v", err)
}

// guide returns a SUCCESS-shaped result describing a recoverable condition.
//
// Shaped exactly like a real answer — same framing, same revision line — with
// zero items and notes that say what happened and what to do about it. The
// agent reads a working tool that found nothing, which is the truth.
func (s *Server) guide(ctx context.Context, req *sdk.CallToolRequest, runID int64, status, summary string, notes ...string) (*sdk.CallToolResult, *Response, error) {
	return s.reply(ctx, req, &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       summary,
		Notes:         notes,
	})
}

// missingSymbol is the recoverable "that name is not in the index" answer.
//
// Shared by every tool that takes a symbol name, because the guidance is the
// same one and because a tool that grew its own wording would eventually grow
// its own isError with it.
func (s *Server) missingSymbol(ctx context.Context, req *sdk.CallToolRequest, runID int64, status, name string) (*sdk.CallToolResult, *Response, error) {
	return s.guide(ctx, req, runID, status,
		fmt.Sprintf("No symbol named %q in the index.", name),
		"The index has no definition under that exact name. Try search_code for a partial "+
			"match, check the spelling, or confirm the file is indexed — generated, oversized "+
			"and excluded files are not. Absence here is not proof the symbol does not exist.")
}

// reply packages a Response as an MCP result, bounded, framed and deduped.
//
// Every tool returns through here. Centralising it means bounding and untrusted
// framing cannot be forgotten by a tool author, which is the only way a rule
// like this survives contact with a growing tool set. Session dedup joins them
// for the same reason, and it runs AFTER Bound on purpose: the ledger must
// record what the agent actually received, never what a tool proposed before
// the bounds trimmed it. See dedup.go.
//
// It is also the ONE place Provenance is attached. Every tool already sets
// IndexRevision/IndexStatus on its own Response literal, and every one of
// those literals passes through here before it reaches the agent — so this is
// the single site where forgetting is impossible, rather than one more thing
// each tool author must remember to do.
func (s *Server) reply(ctx context.Context, req *sdk.CallToolRequest, r *Response) (*sdk.CallToolResult, *Response, error) {
	if r.IndexRevision > 0 {
		if note := s.stagingNote(ctx, r.IndexRevision); note != "" {
			r.Notes = append(r.Notes, note)
		}
	}
	if r.Provenance == nil {
		r.Provenance = s.provenanceFor(ctx, r.IndexRevision)
	}
	r.attachWeakConfidenceNote()
	r.Bound(s.limits)

	// Grade AFTER bounding, then re-bound, because each depends on the other:
	// a verdict computed before Bound describes results the agent never
	// receives, and a verdict rendered into the text adds bytes Bound has to
	// charge for. The loop settles that in practice after one pass — it runs
	// again only if charging the verdict cost an item, in which case the
	// histogram is stale and has to be recomputed against what survived. It
	// terminates because Items only ever shrinks.
	for {
		before := len(r.Items)
		r.gradeSelf()
		r.attachWeakConfidenceNote()
		r.Bound(s.limits)
		if len(r.Items) == before {
			break
		}
	}

	s.ledgers.forSession(sessionOf(req)).apply(s.deps.Config.ProjectName, r)
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: r.RenderText()}},
	}, r, nil
}

// provenanceFor builds the Provenance envelope for a response reporting
// runID.
//
// builtAtSHA comes from runMeta, cached by currentRun() moments earlier in
// this same request — no second registry read. headSHA comes from
// headSHA, read fresh on every call — see the field comment on headRepo for
// why that is cheap enough to not need caching.
func (s *Server) provenanceFor(ctx context.Context, runID int64) *Provenance {
	var builtAtSHA string
	var ageSeconds int64
	if runID != 0 {
		s.runMetaMu.RLock()
		meta, ok := s.runMeta[runID]
		s.runMetaMu.RUnlock()
		if ok {
			builtAtSHA = meta.gitRevision
			if meta.startedUnix > 0 {
				ageSeconds = time.Now().Unix() - meta.startedUnix
				if ageSeconds < 0 {
					ageSeconds = 0
				}
			}
		}
	}
	return ProvenanceFor(builtAtSHA, s.headSHA(ctx), ageSeconds)
}

// headSHA returns the repository's current HEAD, read fresh via go-git
// (never a `git` subprocess — see internal/acquire/git's package doc) on
// every call. Returns "" when the project is not a git repository, when
// HEAD could not be resolved (including a cancelled ctx), or when git
// support is unavailable; ProvenanceFor treats an empty HEAD as unknown,
// never as a match.
func (s *Server) headSHA(ctx context.Context) string {
	if s.headRepo == nil {
		return ""
	}
	sha, err := s.headRepo.HeadSHA(ctx)
	if err != nil {
		return ""
	}
	return sha
}

// stagingNote says when an answer from the committed run may be stale because
// a newer run is being built. Empty when there is nothing to say.
//
// A failed check is reported rather than swallowed: "could not check" and
// "nothing to check" must not render the same.
func (s *Server) stagingNote(ctx context.Context, committedID int64) string {
	staging, err := s.deps.Registry.StagingRun(ctx)
	if err != nil {
		return fmt.Sprintf("Could not check whether a newer index is being built: %v.", err)
	}
	if staging == nil || staging.ID <= committedID {
		return ""
	}
	return fmt.Sprintf("A newer index (run %d) is being built. This answer is from run %d and may "+
		"not reflect recent edits; ask again when indexing finishes. If no index is running, "+
		"a previous one was interrupted.", staging.ID, committedID)
}
