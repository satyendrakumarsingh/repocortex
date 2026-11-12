package mcp

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/deadcode"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/hotspots"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file holds the `full`-profile tools: three that navigate the graph
// (callees, implementations, a path between two symbols) and two that report
// on the repository as a whole (unused definitions, change hotspots).
//
// None of them adds a way of knowing anything. Each is a different question
// asked of the same edges, so each inherits the same two obligations as
// find_callers: say how every relationship was established, and never let an
// empty or short answer read as proof.

// ---------------------------------------------------------------- arguments

type pathArgs struct {
	From     string `json:"from" jsonschema:"Symbol the path starts at, e.g. 'handleCallback'."`
	To       string `json:"to" jsonschema:"Symbol the path should reach, e.g. 'saveTransaction'."`
	FromPath string `json:"from_path,omitempty" jsonschema:"Optional file path to disambiguate 'from' when several symbols share the name."`
	ToPath   string `json:"to_path,omitempty" jsonschema:"Optional file path to disambiguate 'to' when several symbols share the name."`
	MaxDepth int    `json:"max_depth,omitempty" jsonschema:"Longest route to look for, in hops. Default 6. Raise it only if the answer was 'no route found' and you expect a long one."`
}

type deadCodeArgs struct {
	Path            string `json:"path,omitempty" jsonschema:"Only report definitions in files under this path prefix, e.g. 'internal/billing/'."`
	MinCertainty    string `json:"min_certainty,omitempty" jsonschema:"'likely' (the default) returns only candidates where every ordinary reason for a missing caller was ruled out. 'possible' adds the rest, which are mostly uses the index could not bind: search before trusting any of them."`
	IncludeExported bool   `json:"include_exported,omitempty" jsonschema:"Also report exported definitions, which are never better than 'possible'. Default false: in a library the whole public API has no caller in the index."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Most candidates to return. Default 25."`
}

type hotspotArgs struct {
	Path         string `json:"path,omitempty" jsonschema:"Only rank files under this path prefix."`
	Commits      int    `json:"commits,omitempty" jsonschema:"How many recent commits to read. Default 500."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Most files to return. Default 10."`
	IncludeTests bool   `json:"include_tests,omitempty" jsonschema:"Also rank test files. Default false: tests change with the code they cover."`
}

// registerStructure adds the full-profile tools.
func (s *Server) registerStructure(allowed map[string]bool) {
	descs := toolDescriptions()

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_callees",
		Description: descs["find_callees"],
	}, s.findCallees)

	addTool(s, allowed, &sdk.Tool{
		Name:        "find_implementations",
		Description: descs["find_implementations"],
	}, s.findImplementations)

	// The whole-repository reports read only the registry, so like
	// list_api_endpoints they register whether or not a graph is present.
	addTool(s, allowed, &sdk.Tool{
		Name:        "list_dead_code",
		Description: descs["list_dead_code"],
	}, s.findDeadCode)

	addTool(s, allowed, &sdk.Tool{
		Name:        "list_hotspots",
		Description: descs["list_hotspots"],
	}, s.findHotspots)

	if s.deps.Graph == nil {
		return
	}
	addTool(s, allowed, &sdk.Tool{
		Name:        "trace_call_path",
		Description: descs["trace_call_path"],
	}, s.traceCallPath)
}

// ---------------------------------------------------------------- shared lookup

// resolved is a symbol name turned into one definition.
type resolved struct {
	RunID  int64
	Status string
	Target registry.SymbolHit
	// Others counts further definitions sharing the name, so the answer can
	// say it chose one.
	Others int
}

// resolveSymbol turns a name (and optional path hint) into one definition.
//
// On any outcome other than success it returns the finished result the caller
// must hand back unchanged, for the reason referencesFor documents: a handler
// that also returned an error would have its guidance replaced by the SDK's
// bare protocol error.
func (s *Server) resolveSymbol(ctx context.Context, req *sdk.CallToolRequest, name, pathHint string) (resolved, *sdk.CallToolResult, *Response) {
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		sc, r, _ := s.noRun(ctx, req, err)
		return resolved{}, sc, r
	}
	syms, err := s.deps.Registry.FindSymbolsByName(ctx, runID, name, "", 5)
	if err != nil {
		sc, r, _ := failFrom(err, "symbol lookup failed: %v", err)
		return resolved{}, sc, r
	}
	if len(syms) == 0 {
		sc, r, _ := s.missingSymbol(ctx, req, runID, status, name)
		return resolved{}, sc, r
	}
	target := syms[0]
	for _, sym := range syms {
		if pathHint != "" && strings.Contains(sym.FilePath, pathHint) {
			target = sym
			break
		}
	}
	return resolved{RunID: runID, Status: status, Target: target, Others: len(syms) - 1}, nil, nil
}

// ambiguityNote says the answer is for one of several same-named definitions.
func ambiguityNote(r resolved, param string) string {
	if r.Others <= 0 {
		return ""
	}
	return fmt.Sprintf("%d other definition(s) share the name %q; this answer is for the one at %s:%d. "+
		"Pass %s to choose another.", r.Others, r.Target.Name, r.Target.FilePath, r.Target.StartLine, param)
}

// outgoingItems renders edges that START at a symbol.
func outgoingItems(edges []registry.EdgeHit) []Item {
	items := make([]Item, 0, len(edges))
	for _, e := range edges {
		items = append(items, Item{
			Kind:       strings.ToLower(e.Kind),
			Name:       e.DstName,
			Path:       e.EvidenceFile,
			Line:       e.EvidenceLine,
			Confidence: e.Confidence,
			Source:     e.Source,
			Why:        e.Detail,
		})
	}
	return items
}

// ---------------------------------------------------------------- find_callees

func (s *Server) findCallees(ctx context.Context, req *sdk.CallToolRequest, args symbolRefArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Name) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty name, then call find_callees again.",
			"name must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	r, sc, scResp := s.resolveSymbol(ctx, req, args.Name, args.Path)
	if sc != nil {
		return sc, scResp, nil
	}

	filter := registry.EdgeFilter{MinConfidence: minConf, Kinds: []string{string(model.EdgeCalls)}}
	edges, err := s.deps.Registry.OutgoingEdges(ctx, r.RunID, r.Target.ID, s.limits.MaxItems, filter)
	if err != nil {
		return failFrom(err, "relationship lookup failed: %v", err)
	}

	resp := &Response{
		IndexRevision: r.RunID,
		IndexStatus:   r.Status,
		Summary: fmt.Sprintf("%s (%s:%d) calls %d symbol(s) the index could bind.",
			r.Target.Name, r.Target.FilePath, r.Target.StartLine, len(edges)),
		Items: outgoingItems(edges),
	}
	s.gateCoverage(ctx, resp, r, "callee lookup")
	if len(edges) == s.limits.MaxItems {
		resp.markTruncated(fmt.Sprintf(
			"the item cap (%d) was reached; more callees may exist — raise min_confidence to see the strongest", s.limits.MaxItems))
	}
	// The gap here has a different shape from find_callers', so it gets its
	// own sentence: a call into a library has no definition in this index and
	// can never appear, however well the repository itself resolves.
	resp.Notes = append(resp.Notes,
		"Only calls to definitions INSIDE this index are listed. Calls into libraries, the standard "+
			"library and generated code have no definition here to bind to, so this is what the "+
			"symbol calls within the repository, not everything it calls.")
	if n := ambiguityNote(r, "path"); n != "" {
		resp.Notes = append(resp.Notes, n)
	}
	return s.reply(ctx, req, resp)
}

// gateCoverage prepends the low-coverage caveat when the index's binding rate
// does not support a relationship answer. Shared wording with every other
// relationship tool, through coverageOf.
func (s *Server) gateCoverage(ctx context.Context, resp *Response, r resolved, subject string) {
	rs, err := s.deps.Registry.ResolutionStats(ctx, r.RunID)
	if err != nil {
		return
	}
	if gate := coverageOf(rs, r.Target.FilePath).caveat(subject); gate != nil {
		resp.Notes = append(gate, resp.Notes...)
	}
}

// ---------------------------------------------------------------- find_implementations

// hierarchyKinds are the edges that record a type relationship.
var hierarchyKinds = []string{string(model.EdgeImplements), string(model.EdgeExtends)}

func (s *Server) findImplementations(ctx context.Context, req *sdk.CallToolRequest, args symbolRefArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Name) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty name, then call find_implementations again.",
			"name must not be empty")
	}
	minConf, err := parseMinConfidence(args.MinConfidence)
	if err != nil {
		return failFrom(err, "%v", err)
	}
	r, sc, scResp := s.resolveSymbol(ctx, req, args.Name, args.Path)
	if sc != nil {
		return sc, scResp, nil
	}

	filter := registry.EdgeFilter{MinConfidence: minConf, Kinds: hierarchyKinds}
	// The cap is split so a type with hundreds of subtypes cannot crowd its
	// own supertypes out of the answer: what a type IS matters as much to a
	// reader as what is built on it, and there are always far fewer of them.
	half := max(s.limits.MaxItems/2, 1)
	down, err := s.deps.Registry.IncomingEdges(ctx, r.RunID, r.Target.ID, half, filter)
	if err != nil {
		return failFrom(err, "relationship lookup failed: %v", err)
	}
	up, err := s.deps.Registry.OutgoingEdges(ctx, r.RunID, r.Target.ID, half, filter)
	if err != nil {
		return failFrom(err, "relationship lookup failed: %v", err)
	}

	resp := &Response{
		IndexRevision: r.RunID,
		IndexStatus:   r.Status,
		Summary: fmt.Sprintf("%s (%s:%d): %d implementing or extending it, %d it implements or extends.",
			r.Target.Name, r.Target.FilePath, r.Target.StartLine, len(down), len(up)),
	}
	for _, e := range down {
		why := "implements or extends " + r.Target.Name
		if e.Detail != "" {
			why += " — " + e.Detail
		}
		resp.Items = append(resp.Items, Item{
			Kind: "subtype", Name: e.SrcName, Path: e.EvidenceFile, Line: e.EvidenceLine,
			Confidence: e.Confidence, Source: e.Source, Why: why,
		})
	}
	for _, e := range up {
		why := r.Target.Name + " " + strings.ToLower(e.Kind) + " this"
		if e.Detail != "" {
			why += " — " + e.Detail
		}
		resp.Items = append(resp.Items, Item{
			Kind: "supertype", Name: e.DstName, Path: e.EvidenceFile, Line: e.EvidenceLine,
			Confidence: e.Confidence, Source: e.Source, Why: why,
		})
	}

	s.gateCoverage(ctx, resp, r, "type hierarchy lookup")
	if len(down) == half || len(up) == half {
		resp.markTruncated(fmt.Sprintf(
			"one direction reached its cap (%d); more may exist — raise min_confidence or pass a path", half))
	}
	resp.Notes = append(resp.Notes,
		"A type relationship is recorded only where the source states it (extends, implements, an "+
			"embedded type) or a compiler index proved it. Structural typing — a Go type that satisfies "+
			"an interface without naming it, a TypeScript object that happens to fit — is visible only "+
			"after 'repocortex scip import'. Treat an empty list as 'none declared', not 'none exist'.")
	if n := ambiguityNote(r, "path"); n != "" {
		resp.Notes = append(resp.Notes, n)
	}
	return s.reply(ctx, req, resp)
}

// ---------------------------------------------------------------- trace_call_path

const (
	// defaultPathDepth is how long a route trace_call_path looks for unless
	// asked. Six hops spans a request handler to a storage call in most
	// layered services; beyond it a "route" is rarely an explanation.
	defaultPathDepth = 6
	// maxPathDepth bounds what a caller may ask for.
	maxPathDepth = 12
	// maxTracedPaths is how many shortest routes are rendered. Each costs one
	// item per hop, so this and the depth together bound the response.
	maxTracedPaths = 3
)

func (s *Server) traceCallPath(ctx context.Context, req *sdk.CallToolRequest, args pathArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.From) == "" || strings.TrimSpace(args.To) == "" {
		return failWith("invalid_argument",
			"Pass both 'from' and 'to' as symbol names, then call trace_call_path again.",
			"from and to must both be non-empty")
	}
	depth := args.MaxDepth
	switch {
	case depth < 0:
		return failWith("invalid_argument",
			"Pass max_depth as a positive number of hops, or omit it for the default.",
			"max_depth must not be negative")
	case depth == 0:
		depth = defaultPathDepth
	case depth > maxPathDepth:
		depth = maxPathDepth
	}

	from, sc, scResp := s.resolveSymbol(ctx, req, args.From, args.FromPath)
	if sc != nil {
		return sc, scResp, nil
	}
	to, sc, scResp := s.resolveSymbol(ctx, req, args.To, args.ToPath)
	if sc != nil {
		return sc, scResp, nil
	}

	paths, err := s.deps.Graph.Paths(ctx, from.Target.ID, to.Target.ID, maxTracedPaths, depth)
	if err != nil {
		return failFrom(err, "path search failed: %v", err)
	}

	resp := &Response{IndexRevision: from.RunID, IndexStatus: from.Status}
	s.gateCoverage(ctx, resp, from, "path search")

	if len(paths) == 0 {
		resp.Summary = fmt.Sprintf("No route found from %s to %s within %d hop(s).",
			from.Target.Name, to.Target.Name, depth)
		resp.Notes = append(resp.Notes,
			"This is not proof that none exists. A route passes only through relationships the "+
				"index bound, so one unresolved call, an interface dispatch or a callback anywhere "+
				"along the way breaks it. Try find_callers on the destination and find_callees on "+
				"the start to see where the two halves stop.")
	} else {
		hops := len(paths[0]) - 1
		resp.Summary = fmt.Sprintf("%d shortest route(s) from %s to %s, %d hop(s) each.",
			len(paths), from.Target.Name, to.Target.Name, hops)
		for pi, p := range paths {
			for hi := 0; hi+1 < len(p); hi++ {
				resp.Items = append(resp.Items, s.hopItem(ctx, from.RunID, p[hi], p[hi+1], pi+1, hi+1, len(p)-1))
			}
		}
		resp.Notes = append(resp.Notes,
			"Only SHORTEST routes are shown. Longer routes between the same two symbols may exist "+
				"and are not listed. A route is as trustworthy as its weakest hop: read the "+
				"confidence on each one.")
	}
	for _, n := range []string{ambiguityNote(from, "from_path"), ambiguityNote(to, "to_path")} {
		if n != "" {
			resp.Notes = append(resp.Notes, n)
		}
	}
	return s.reply(ctx, req, resp)
}

// hopItem renders one step of a route, with the evidence for that step.
func (s *Server) hopItem(ctx context.Context, runID, src, dst int64, pathNo, hopNo, hops int) Item {
	where := fmt.Sprintf("route %d, hop %d of %d", pathNo, hopNo, hops)
	e, ok, err := s.deps.Registry.EdgeBetween(ctx, runID, src, dst)
	if err != nil || !ok {
		// The traversal found the hop moments ago, so this is a reindex
		// committing mid-request. Say so rather than inventing a line.
		return Item{Kind: "hop", Name: fmt.Sprintf("#%d -> #%d", src, dst),
			Why: where + "; its evidence could not be re-read (the index may have just been rebuilt)"}
	}
	return Item{
		Kind:       strings.ToLower(e.Kind),
		Name:       e.SrcName + " -> " + e.DstName,
		Path:       e.EvidenceFile,
		Line:       e.EvidenceLine,
		Confidence: e.Confidence,
		Source:     e.Source,
		Why:        where,
	}
}

// ---------------------------------------------------------------- list_dead_code

const defaultDeadCodeItems = 25

func (s *Server) findDeadCode(ctx context.Context, req *sdk.CallToolRequest, args deadCodeArgs) (*sdk.CallToolResult, *Response, error) {
	// The default is the likely tier alone. The possible tier is dominated by
	// uses the index could not bind, and an agent handed twenty-five of those
	// under the heading "dead code" will act on some of them. Asking for
	// exported definitions is asking for the possible tier, since an exported
	// definition is never graded higher.
	minCert := deadcode.CertaintyLikely
	switch strings.ToLower(strings.TrimSpace(args.MinCertainty)) {
	case "":
		if args.IncludeExported {
			minCert = ""
		}
	case string(deadcode.CertaintyPossible):
		minCert = ""
	case string(deadcode.CertaintyLikely):
	default:
		return failWith("invalid_argument",
			"Pass min_certainty as 'likely' or 'possible', or omit it.",
			"unknown min_certainty %q", args.MinCertainty)
	}
	if args.Limit < 0 {
		return failWith("invalid_argument", "Pass limit as a positive number, or omit it.",
			"limit must not be negative")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	limit := args.Limit
	if limit == 0 {
		limit = defaultDeadCodeItems
	}
	limit = min(limit, s.limits.MaxItems)

	rep, err := deadcode.Find(ctx, s.capDeps, s.deps.Registry, runID, deadcode.Options{
		Limit:           limit,
		MinCertainty:    minCert,
		PathPrefix:      strings.TrimSpace(args.Path),
		IncludeExported: args.IncludeExported,
		Judgeable:       s.deps.Judgeable,
	})
	if err != nil {
		return failFrom(err, "dead-code scan: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%d definition(s) have no caller in the index: %d likely unused, %d possibly. "+
			"Showing %d.", rep.Likely+rep.Possible, rep.Likely, rep.Possible, len(rep.Candidates)),
	}
	for _, c := range rep.Candidates {
		resp.Items = append(resp.Items, Item{
			Kind: c.Kind, Name: c.Name, FQN: c.FQN, Path: c.Path, Line: c.Line,
			Why: deadCodeWhy(c),
		})
	}

	// The warning leads. A list of deletable-looking functions is the most
	// actionable thing this server returns, and the most dangerous to act on
	// unread.
	resp.Notes = append(resp.Notes, deadcode.Limits)
	if rs := rep.Completeness.Resolution; rs.Total > 0 && rep.Completeness.BindingRate < coverageFloor {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"COVERAGE TOO LOW — only %.0f%% of the references that name something this index defines "+
				"are bound. Most of what is listed here is more likely unbound than unused. Run "+
				"'repocortex resolve', and 'repocortex scip import' where an indexer exists, before trusting it.",
			100*rep.Completeness.BindingRate))
	}
	if rep.SkippedUnjudged > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d definition(s) in %s were not judged: the index parses those languages without "+
				"binding their references, so every definition in them would look unused.",
			rep.SkippedUnjudged, strings.Join(rep.UnjudgedLangs, ", ")))
	}
	if n := rep.Skipped - rep.SkippedUnjudged; n > 0 {
		what := "tests, entry points and exported definitions"
		if args.IncludeExported {
			what = "tests and entry points"
		}
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d further unreferenced definition(s) were left out as expected: %s.", n, what))
	}
	if rep.LikelyUnchecked > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"Every row shown as likely was also checked against the indexed source text. %d more were "+
				"graded likely from the graph alone and not checked, so the likely total in the summary "+
				"is an upper bound; pass a path to check them.", rep.LikelyUnchecked))
	}
	if minCert == deadcode.CertaintyLikely && rep.Possible > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"%d more candidate(s) are graded possible and not shown. Most are uses the index could not "+
				"bind. Pass min_certainty='possible' to list them.", rep.Possible))
	}
	if rep.Completeness.Truncated {
		resp.markTruncated(rep.Completeness.TruncatedReason)
	}
	return s.reply(ctx, req, resp)
}

// deadCodeWhy is the one-line justification shown beside a candidate.
func deadCodeWhy(c deadcode.Candidate) string {
	size := fmt.Sprintf("%d line(s)", c.Lines())
	if c.Certainty == deadcode.CertaintyLikely {
		return "likely unused, " + size + ": unexported, no caller, no unbound reference with this name, " +
			"and the name appears nowhere else in the indexed text"
	}
	reasons := make([]string, 0, len(c.Reasons))
	for _, r := range c.Reasons {
		reasons = append(reasons, r.Text())
	}
	return "possibly unused, " + size + ": no caller in the index, but " + strings.Join(reasons, "; ")
}

// ---------------------------------------------------------------- list_hotspots

// defaultHotspotItems is how many files are ranked unless asked. Each can carry
// up to three coupled files as rows of their own, so ten hotspots is at most
// forty items, inside the response's item bound.
const defaultHotspotItems = 10

func (s *Server) findHotspots(ctx context.Context, req *sdk.CallToolRequest, args hotspotArgs) (*sdk.CallToolResult, *Response, error) {
	if args.Commits < 0 || args.Limit < 0 {
		return failWith("invalid_argument", "Pass commits and limit as positive numbers, or omit them.",
			"commits and limit must not be negative")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}
	if s.deps.History == nil {
		return s.guide(ctx, req, runID, status,
			"No commit history is available to this server, so nothing can be ranked.",
			"Hotspots are read from git history. The project is not a git repository, or its "+
				"history could not be opened. Everything else this server answers is unaffected.")
	}

	limit := args.Limit
	if limit == 0 {
		limit = defaultHotspotItems
	}
	limit = min(limit, s.limits.MaxItems)

	rep, err := hotspots.Rank(ctx, s.deps.History, s.deps.Registry, runID, hotspots.Options{
		Limit:        limit,
		CommitWindow: args.Commits,
		PathPrefix:   strings.TrimSpace(args.Path),
		IncludeTests: args.IncludeTests,
	})
	if err != nil {
		return failFrom(err, "hotspot ranking: %v", err)
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary: fmt.Sprintf("%d file(s) ranked by change frequency x size, from the last %d commit(s) "+
			"touching %d indexed file(s).", len(rep.Hotspots), rep.CommitsRead, rep.FilesChanged),
		Notes: rep.Notes,
	}
	for _, h := range rep.Hotspots {
		// Name is the base name and Path the whole path: the two are bounded
		// separately, and a long path repeated in both would be cut in both.
		resp.Items = append(resp.Items, Item{
			Kind: "file", Name: path.Base(h.Path), Path: h.Path, Source: string(model.SourceGit),
			Why: hotspotWhy(h),
		})
		// Each coupled file is its own row, directly under its hotspot. Folded
		// into the hotspot's one-line reason they were cut off by the per-item
		// byte bound as soon as paths were long, and the coupling is the part
		// of this answer no other tool can give.
		for _, p := range h.ChangesWith {
			resp.Items = append(resp.Items, Item{
				Kind: "changes_with", Name: path.Base(p.Path), Path: p.Path, Source: string(model.SourceGit),
				Why: fmt.Sprintf("committed with %s in %d of its %d commit(s)", path.Base(h.Path), p.Together, h.Commits),
			})
		}
	}
	if len(rep.Hotspots) > 0 {
		resp.Notes = append(resp.Notes,
			"The score orders this list and means nothing across repositories. A changes_with row "+
				"names a file committed together with the hotspot above it: when no call or import "+
				"connects them, that is a dependency analyze_impact cannot see. Check those files "+
				"when you change the hotspot.")
	}
	return s.reply(ctx, req, resp)
}

// hotspotWhy is the one-line account of a hotspot's rank.
func hotspotWhy(h hotspots.Hotspot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "score %d: %d commit(s), %s, %d symbol(s), %d inbound relationship(s)",
		h.Score, h.Commits, humanSize(h.Size), h.Symbols, h.Dependents)
	if h.LastChanged > 0 {
		fmt.Fprintf(&b, ", last changed %s", time.Unix(h.LastChanged, 0).UTC().Format("2006-01-02"))
	}
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// compile-time check that the graph the server holds can trace a path.
var _ interface {
	Paths(ctx context.Context, from, to int64, maxPaths, maxDepth int) ([][]int64, error)
} = (*graph.Graph)(nil)
