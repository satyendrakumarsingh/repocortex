package multirepo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Bounds. A system graph across a large estate is unbounded in principle, and
// an unbounded report is unreadable and unshippable. Every cut is counted into
// Unjoined.Truncated, so a capped list is never mistaken for a complete one.
const (
	DefaultMaxEdges              = 500
	DefaultMaxUnjoinedCalls      = 200
	DefaultMaxUnconsumedEndpoint = 200
	DefaultMaxSharedTables       = 200

	// DefaultStaleAfter is where a member index stops describing the system as
	// it is now. Not a threshold anything fails — it only decides whether the
	// reader is warned that one corner of the picture is old.
	DefaultStaleAfter = 14 * 24 * time.Hour

	// exposesLookupLimit bounds the per-endpoint edge query. An endpoint node
	// exposes a handful of handlers at most; a route with more than this has
	// something else wrong with it.
	exposesLookupLimit = 16
)

// Federator assembles a system graph across member indexes.
//
// Holds no mutable state: members arrive per call and every intermediate lives
// in a Build local, so one Federator is safe to share across goroutines and
// cannot leak facts from one system into the next.
type Federator struct {
	maxEdges         int
	maxUnjoinedCalls int
	maxUnconsumed    int
	maxSharedTables  int
	runningVersion   string
	staleAfter       time.Duration
	now              func() time.Time
}

// Option configures a Federator. Functional options per CLAUDE.md §2.
type Option func(*Federator) error

// WithMaxEdges caps the cross-repository edges reported.
func WithMaxEdges(n int) Option {
	return func(f *Federator) error {
		if n <= 0 {
			return fmt.Errorf("multirepo: max edges must be positive, got %d", n)
		}
		f.maxEdges = n
		return nil
	}
}

// WithMaxUnjoined caps the unjoined-call and unconsumed-endpoint samples.
func WithMaxUnjoined(n int) Option {
	return func(f *Federator) error {
		if n <= 0 {
			return fmt.Errorf("multirepo: max unjoined must be positive, got %d", n)
		}
		f.maxUnjoinedCalls, f.maxUnconsumed = n, n
		return nil
	}
}

// WithMaxSharedTables caps the shared-table section.
func WithMaxSharedTables(n int) Option {
	return func(f *Federator) error {
		if n <= 0 {
			return fmt.Errorf("multirepo: max shared tables must be positive, got %d", n)
		}
		f.maxSharedTables = n
		return nil
	}
}

// WithRunningVersion tells the report which build is producing it, so a member
// stamped by a different one can be named as such.
func WithRunningVersion(v string) Option {
	return func(f *Federator) error {
		f.runningVersion = v
		return nil
	}
}

// WithStaleAfter sets the age at which a member index is called out as old.
func WithStaleAfter(d time.Duration) Option {
	return func(f *Federator) error {
		if d <= 0 {
			return fmt.Errorf("multirepo: stale-after must be positive, got %s", d)
		}
		f.staleAfter = d
		return nil
	}
}

// WithClock injects the clock used for index ages.
//
// Injected rather than read from time.Now inside the logic, so the staleness
// sentences are testable without sleeping and without a package-level variable
// (CLAUDE.md §4.2).
func WithClock(now func() time.Time) Option {
	return func(f *Federator) error {
		if now == nil {
			return errors.New("multirepo: clock must not be nil")
		}
		f.now = now
		return nil
	}
}

// New builds a Federator.
func New(opts ...Option) (*Federator, error) {
	f := &Federator{
		maxEdges:         DefaultMaxEdges,
		maxUnjoinedCalls: DefaultMaxUnjoinedCalls,
		maxUnconsumed:    DefaultMaxUnconsumedEndpoint,
		maxSharedTables:  DefaultMaxSharedTables,
		staleAfter:       DefaultStaleAfter,
		now:              time.Now,
	}
	for _, opt := range opts {
		if err := opt(f); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// memberFacts is everything one member contributed, loaded once.
type memberFacts struct {
	member    Member
	run       *model.IndexRun
	calls     []callRow
	endpoints []endpointRow
	tables    []tableRow

	// localConsumed is the set of this member's own endpoint symbol ids that
	// one of its own calls already bound to. It is what stops an endpoint whose
	// only caller lives in the same repository from being reported as
	// unconsumed, and it costs nothing: ClientCallReport already carries the
	// bound endpoint id.
	localConsumed map[int64]bool

	// exposes caches the EXPOSES edge confidence per endpoint symbol id, so a
	// route consumed by four members is looked up once.
	exposes map[int64]model.Confidence
}

// Build assembles the system graph.
//
// Members that could not be opened are carried through rather than dropped: an
// unavailable member is a FINDING, and the report is explicitly a floor rather
// than a total whenever there is one. The only hard errors are structural — too
// few members to have a "cross" at all, two members sharing a name, or a member
// whose index answered a query with an error, which means the file is there and
// broken rather than absent.
func (f *Federator) Build(ctx context.Context, members []Member) (*Report, error) {
	if len(members) < 2 {
		return nil, fmt.Errorf(
			"multirepo: a system graph needs at least two members, got %d — "+
				"with one repository, 'repocortex api' and 'repocortex db' already answer this",
			len(members))
	}
	seen := map[string]bool{}
	for _, m := range members {
		if m.Name == "" {
			return nil, errors.New("multirepo: every member must be named")
		}
		if seen[m.Name] {
			return nil, fmt.Errorf(
				"multirepo: two members are named %q; names identify repositories in "+
					"every line of the report and must be distinct", m.Name)
		}
		seen[m.Name] = true
	}

	// Sorted by name so every list below, and therefore the whole report, is
	// deterministic regardless of the order the flags were typed in.
	ordered := append([]Member(nil), members...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	facts := make([]*memberFacts, 0, len(ordered))
	for _, m := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mf, err := loadMember(ctx, m)
		if err != nil {
			return nil, err
		}
		facts = append(facts, mf)
	}

	rep := &Report{
		Edges:        []CrossRepoEdge{},
		SharedTables: []SharedTable{},
		Unjoined: Unjoined{
			Calls:              []UnjoinedCall{},
			AmbiguousCalls:     []AmbiguousCall{},
			Endpoints:          []UnconsumedEndpoint{},
			UnavailableMembers: []string{},
			Truncated:          map[string]int{},
		},
	}

	routes := buildRoutes(facts)
	if err := f.joinCalls(ctx, rep, facts, routes); err != nil {
		return nil, err
	}
	f.collectUnconsumed(rep, facts)
	f.joinTables(rep, facts)
	rep.Members = f.memberStatuses(facts)
	rep.Provenance = f.provenance(facts)
	rep.Caveats = standingCaveats()

	for _, mf := range facts {
		if !mf.member.Available() {
			rep.Unjoined.UnavailableMembers = append(rep.Unjoined.UnavailableMembers, mf.member.Name)
		}
	}
	if len(rep.Unjoined.Truncated) == 0 {
		rep.Unjoined.Truncated = nil
	}
	return rep, nil
}

// loadMember reads one member's four projections, or records why it could not.
func loadMember(ctx context.Context, m Member) (*memberFacts, error) {
	mf := &memberFacts{
		member:        m,
		localConsumed: map[int64]bool{},
		exposes:       map[int64]model.Confidence{},
	}
	if !m.Available() {
		if m.Unavailable == "" {
			mf.member.Unavailable = "no index source was supplied for this member"
		}
		return mf, nil
	}

	run, err := m.Source.Run(ctx, m.RunID)
	if err != nil {
		return nil, fmt.Errorf("multirepo: member %q: read run %d: %w", m.Name, m.RunID, err)
	}
	mf.run = run

	if mf.calls, err = m.Source.ClientCallReport(ctx, m.RunID); err != nil {
		return nil, fmt.Errorf("multirepo: member %q: read outbound calls: %w", m.Name, err)
	}
	if mf.endpoints, err = m.Source.EndpointReport(ctx, m.RunID); err != nil {
		return nil, fmt.Errorf("multirepo: member %q: read endpoints: %w", m.Name, err)
	}
	if mf.tables, err = m.Source.TableUsageReport(ctx, m.RunID); err != nil {
		return nil, fmt.Errorf("multirepo: member %q: read table usage: %w", m.Name, err)
	}

	for _, c := range mf.calls {
		if c.Binding == bindingBound && c.EndpointID > 0 {
			mf.localConsumed[c.EndpointID] = true
		}
	}
	return mf, nil
}

// bindingBound is the value internal/registry writes for a call it resolved
// inside its own repository. Spelled once, here.
const bindingBound = "bound"

// routeCandidate is one endpoint anywhere in the system, pre-canonicalised.
type routeCandidate struct {
	member   string
	endpoint endpointRow
	method   string // lowercased; "" means the framework stated no verb
	segs     []string
	// facts is the member this route belongs to, so the EXPOSES leg can be
	// looked up without a second search.
	facts *memberFacts
}

// label renders a candidate the way an ambiguity message names it.
func (rc routeCandidate) label() string { return rc.member + ": " + rc.endpoint.Signature }

// buildRoutes canonicalises every available member's endpoints once.
//
// Order is member name then the order EndpointReport returned (file path, then
// line), so ambiguity messages and edge lists are stable across runs.
func buildRoutes(facts []*memberFacts) []routeCandidate {
	var out []routeCandidate
	for _, mf := range facts {
		if !mf.member.Available() {
			continue
		}
		for _, ep := range mf.endpoints {
			method, path := splitSignature(ep.Signature)
			out = append(out, routeCandidate{
				member:   mf.member.Name,
				endpoint: ep,
				method:   canonMethod(method),
				segs:     canonSegments(path),
				facts:    mf,
			})
		}
	}
	return out
}

// matchOutcome is one call's cross-repository binding decision.
type matchOutcome struct {
	bound      bool
	ambiguous  bool
	candidates []string
	route      routeCandidate
	kind       MatchKind
	// verbUnstated is true when one side did not state an HTTP verb, so the
	// match rests on the path alone. It costs a confidence tier.
	verbUnstated bool
	// reason explains a non-match, for the unjoined list.
	reason string
}

// matchCall decides which endpoint, in ANOTHER member, a call consumes.
//
// The tiering is deliberately the same as internal/registry's intra-repository
// matcher, because a call must not mean two different things depending on which
// side of a repository line the route falls:
//
//   - A fully literal path against a route with no parameters is the strongest
//     claim available from source text.
//   - A match that needed a wildcard to absorb a segment drops a tier: the
//     route is right, which resource the call names is not proven.
//   - A dynamic call matched only by its literal prefix drops to low. All that
//     is known is that the target starts where the route starts.
//
// The ONE rule that is deliberately NOT carried across: registry prefers a
// literal route over a parameterised one when both match, because every
// framework's own router does exactly that. That preference is following a
// router's rule, and there is no router spanning two repositories — so a
// literal candidate in member A and a parameterised one in member B are two
// answers, not one, and this refuses. Within a single member the framework's
// rule still applies and is still honoured.
func matchCall(routes []routeCandidate, fromMember, method, path string, dynamic bool) matchOutcome {
	m := canonMethod(method)
	segs := canonSegments(path)

	if dynamic {
		return matchByPrefix(routes, fromMember, m, path, segs)
	}
	if len(segs) == 0 {
		// A literal call to "/" or to nothing. The root is a real route, but a
		// bare "/" is also what a mis-read target degrades to, and binding every
		// unreadable call to somebody's root endpoint would be the invented
		// dependency this package exists to avoid.
		return matchOutcome{reason: "the target canonicalises to the root path, which is " +
			"indistinguishable from a target this scan could not read"}
	}

	var exact, wild []routeCandidate
	for _, rt := range routes {
		if rt.member == fromMember {
			// Same-repository binding is internal/registry's job and has
			// already happened. Matching here as well would let a call that was
			// ambiguous inside its own repository reappear as a confident
			// cross-repo edge to that same repository.
			continue
		}
		if !methodsMatch(m, rt.method) {
			continue
		}
		used, ok := segmentsMatch(rt.segs, segs)
		if !ok {
			continue
		}
		if used == 0 {
			exact = append(exact, rt)
		} else {
			wild = append(wild, rt)
		}
	}

	switch {
	case len(exact) == 1:
		return matchOutcome{
			bound: true, route: exact[0], kind: MatchLiteralPath,
			verbUnstated: m == "" || exact[0].method == "",
		}
	case len(exact) > 1:
		return matchOutcome{ambiguous: true, candidates: labels(exact)}
	case len(wild) == 1:
		return matchOutcome{
			bound: true, route: wild[0], kind: MatchPathParameter,
			verbUnstated: m == "" || wild[0].method == "",
		}
	case len(wild) > 1:
		return matchOutcome{ambiguous: true, candidates: labels(wild)}
	default:
		return matchOutcome{reason: "no endpoint in any other member of this system declares that route"}
	}
}

// matchByPrefix handles a call whose target was assembled at run time.
//
// Only whole literal segments are trusted. `/api/user${id}` yields the literal
// prefix "/api/user", whose last segment is half a word — matching it against
// /api/user would bind a call that actually requests /api/users/7. So a prefix
// that does not end at a segment boundary loses its final segment.
func matchByPrefix(routes []routeCandidate, fromMember, method, rawPath string, segs []string) matchOutcome {
	if len(segs) > 0 && !strings.HasSuffix(strings.TrimSpace(rawPath), "/") {
		segs = segs[:len(segs)-1]
	}
	if len(segs) == 0 {
		return matchOutcome{reason: "the target was assembled at run time and revealed no whole literal segment"}
	}

	var hits []routeCandidate
	for _, rt := range routes {
		if rt.member == fromMember || !methodsMatch(method, rt.method) {
			continue
		}
		if prefixMatches(rt.segs, segs) {
			hits = append(hits, rt)
		}
	}
	switch {
	case len(hits) == 1:
		return matchOutcome{
			bound: true, route: hits[0], kind: MatchLiteralPrefix,
			verbUnstated: method == "" || hits[0].method == "",
		}
	case len(hits) > 1:
		return matchOutcome{ambiguous: true, candidates: labels(hits)}
	default:
		return matchOutcome{reason: "the literal prefix of a run-time target matches no endpoint in any other member"}
	}
}

func labels(rs []routeCandidate) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.label())
	}
	sort.Strings(out)
	return out
}

// joinCalls is the cross-repository hop.
func (f *Federator) joinCalls(
	ctx context.Context,
	rep *Report,
	facts []*memberFacts,
	routes []routeCandidate,
) error {
	// consumed records which endpoints anywhere took part in a cross-repo edge,
	// keyed by member name and endpoint symbol id.
	consumed := map[string]map[int64]bool{}

	for _, mf := range facts {
		if !mf.member.Available() {
			continue
		}
		for _, c := range mf.calls {
			if err := ctx.Err(); err != nil {
				return err
			}
			if c.Binding == bindingBound {
				// This repository answers its own call. Not a cross-repo
				// dependency; counted in MemberStatus.LocallyAnswered so the
				// call arithmetic closes.
				continue
			}

			out := matchCall(routes, mf.member.Name, c.Method, c.Path, c.Dynamic)
			switch {
			case out.ambiguous:
				f.addAmbiguous(rep, mf.member.Name, c, out.candidates)
			case !out.bound:
				f.addUnjoined(rep, mf.member.Name, c, out.reason)
			default:
				edge, err := f.buildEdge(ctx, mf, c, out)
				if err != nil {
					return err
				}
				if consumed[out.route.member] == nil {
					consumed[out.route.member] = map[int64]bool{}
				}
				consumed[out.route.member][out.route.endpoint.SymbolID] = true
				if len(rep.Edges) < f.maxEdges {
					rep.Edges = append(rep.Edges, edge)
				} else {
					rep.Unjoined.Truncated["cross_repo_edges"]++
				}
			}
		}
	}

	sort.Slice(rep.Edges, func(i, j int) bool { return edgeLess(rep.Edges[i], rep.Edges[j]) })
	sort.Slice(rep.Unjoined.Calls, func(i, j int) bool {
		a, b := rep.Unjoined.Calls[i], rep.Unjoined.Calls[j]
		if a.Member != b.Member {
			return a.Member < b.Member
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		return a.Line < b.Line
	})
	sort.Slice(rep.Unjoined.AmbiguousCalls, func(i, j int) bool {
		a, b := rep.Unjoined.AmbiguousCalls[i], rep.Unjoined.AmbiguousCalls[j]
		if a.Member != b.Member {
			return a.Member < b.Member
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})

	// Stash for collectUnconsumed, which needs it and runs next.
	f.markConsumed(facts, consumed)
	return nil
}

// markConsumed folds the cross-repo matches into each member's consumed set, so
// collectUnconsumed asks one question — "did anything in this system call it" —
// rather than two.
func (f *Federator) markConsumed(facts []*memberFacts, consumed map[string]map[int64]bool) {
	for _, mf := range facts {
		for id := range consumed[mf.member.Name] {
			mf.localConsumed[id] = true
		}
	}
}

func edgeLess(a, b CrossRepoEdge) bool {
	switch {
	case a.FromMember != b.FromMember:
		return a.FromMember < b.FromMember
	case a.FromFile != b.FromFile:
		return a.FromFile < b.FromFile
	case a.FromLine != b.FromLine:
		return a.FromLine < b.FromLine
	case a.Path != b.Path:
		return a.Path < b.Path
	case a.ToMember != b.ToMember:
		return a.ToMember < b.ToMember
	default:
		return a.ToEndpoint < b.ToEndpoint
	}
}

func (f *Federator) addUnjoined(rep *Report, member string, c callRow, reason string) {
	if len(rep.Unjoined.Calls) >= f.maxUnjoinedCalls {
		rep.Unjoined.Truncated["unjoined_calls"]++
		return
	}
	rep.Unjoined.Calls = append(rep.Unjoined.Calls, UnjoinedCall{
		Member: member, Caller: c.Caller, FilePath: c.FilePath, Line: c.Line,
		Method: c.Method, Path: c.Path, Dynamic: c.Dynamic, Client: c.Client,
		Reason: reason,
	})
}

func (f *Federator) addAmbiguous(rep *Report, member string, c callRow, candidates []string) {
	if len(rep.Unjoined.AmbiguousCalls) >= f.maxUnjoinedCalls {
		rep.Unjoined.Truncated["ambiguous_calls"]++
		return
	}
	rep.Unjoined.AmbiguousCalls = append(rep.Unjoined.AmbiguousCalls, AmbiguousCall{
		Member: member, Caller: c.Caller, FilePath: c.FilePath, Line: c.Line,
		Method: c.Method, Path: c.Path, Candidates: candidates,
	})
}

// buildEdge assembles one cross-repository edge and grades every leg of it.
func (f *Federator) buildEdge(
	ctx context.Context,
	from *memberFacts,
	c callRow,
	out matchOutcome,
) (CrossRepoEdge, error) {
	legs := []Leg{callLeg(c), matchLeg(out)}

	ep := out.route.endpoint
	var note string
	if ep.HandlerID > 0 {
		conf, err := out.route.facts.exposesConfidence(ctx, ep)
		if err != nil {
			return CrossRepoEdge{}, err
		}
		if conf.Valid() {
			legs = append(legs, Leg{
				Name: "endpoint exposes handler", Confidence: conf,
				Detail:    "recorded EXPOSES edge in " + out.route.member,
				Heuristic: conf != model.ConfidenceExact,
			})
		} else {
			note = "the EXPOSES edge from this route to its handler carries no readable " +
				"confidence, so the last hop is ungraded and the figure above covers only the hops before it"
		}
	} else {
		note = "this route's framework syntax revealed no handler, so the path stops at the " +
			"route rather than reaching a function in " + out.route.member
	}

	confs := make([]model.Confidence, 0, len(legs))
	for _, l := range legs {
		confs = append(confs, l.Confidence)
	}

	return CrossRepoEdge{
		FromMember: from.member.Name,
		FromSymbol: c.Caller,
		FromFile:   c.FilePath,
		FromLine:   c.Line,
		Client:     c.Client,
		Method:     c.Method,
		Path:       c.Path,
		Dynamic:    c.Dynamic,

		ToMember:       out.route.member,
		ToEndpoint:     ep.Signature,
		ToEndpointFile: ep.FilePath,
		ToEndpointLine: ep.Line,
		ToHandler:      ep.Handler,
		ToHandlerFile:  ep.HandlerPath,

		Match:      out.kind,
		Confidence: minConfidence(confs...),
		Legs:       legs,
		Note:       note,
	}, nil
}

// callLeg grades the outbound call itself.
//
// The staged call's OWN extractor confidence is not carried by
// registry.ClientCallUsage, so this grades what the report can actually see:
// whether the target was written out or assembled at run time. A literal call
// is treated as `high`, which is the ceiling a text scan can claim in any case
// — `fetch(` proves nothing a compiler would sign. That substitution is named
// in Report.Caveats rather than hidden here.
func callLeg(c callRow) Leg {
	if c.Dynamic {
		return Leg{
			Name: "outbound call", Confidence: model.ConfidenceLow, Heuristic: true,
			Detail: "target assembled at run time; only its literal prefix is known",
		}
	}
	detail := "literal target read from source text"
	if c.Client != "" {
		detail += " (" + c.Client + ")"
	}
	return Leg{Name: "outbound call", Confidence: model.ConfidenceHigh, Heuristic: true, Detail: detail}
}

// matchLeg grades the hop this package adds, which is the one nothing proved.
func matchLeg(out matchOutcome) Leg {
	var conf model.Confidence
	var detail string
	switch out.kind {
	case MatchLiteralPath:
		conf, detail = model.ConfidenceHigh, "literal path match"
	case MatchPathParameter:
		conf, detail = model.ConfidenceMedium, "path parameter match: a wildcard absorbed a segment"
	case MatchLiteralPrefix:
		conf, detail = model.ConfidenceLow, "literal prefix of a run-time target, single candidate route"
	default:
		conf, detail = model.ConfidenceLow, "unclassified match"
	}
	if out.verbUnstated {
		conf = demote(conf)
		detail += "; one side stated no HTTP verb, so the match rests on the path alone"
	}
	return Leg{
		Name: "route match", Confidence: conf, Heuristic: true,
		Detail: detail + " — a heuristic across two repositories, never compiler-proven",
	}
}

// exposesConfidence reads the recorded confidence of a route's EXPOSES edge,
// caching per endpoint.
func (mf *memberFacts) exposesConfidence(ctx context.Context, ep endpointRow) (model.Confidence, error) {
	if c, ok := mf.exposes[ep.SymbolID]; ok {
		return c, nil
	}
	edges, err := mf.member.Source.OutgoingEdges(ctx, mf.member.RunID, ep.SymbolID, exposesLookupLimit,
		registry.EdgeFilter{Kinds: []string{string(model.EdgeExposes)}})
	if err != nil {
		return "", fmt.Errorf("multirepo: member %q: read EXPOSES edges for %q: %w",
			mf.member.Name, ep.Signature, err)
	}
	var found model.Confidence
	for _, e := range edges {
		if ep.HandlerID > 0 && e.DstID != ep.HandlerID {
			continue
		}
		c := model.Confidence(e.Confidence)
		if c.Valid() && (!found.Valid() || c.Rank() < found.Rank()) {
			// Weakest, not strongest: a route resolving to two handlers is only
			// as well understood as its worst-understood one.
			found = c
		}
	}
	mf.exposes[ep.SymbolID] = found
	return found, nil
}

// collectUnconsumed names the routes nobody in this system calls.
func (f *Federator) collectUnconsumed(rep *Report, facts []*memberFacts) {
	for _, mf := range facts {
		if !mf.member.Available() {
			continue
		}
		for _, ep := range mf.endpoints {
			if mf.localConsumed[ep.SymbolID] {
				continue
			}
			if len(rep.Unjoined.Endpoints) >= f.maxUnconsumed {
				rep.Unjoined.Truncated["unconsumed_endpoints"]++
				continue
			}
			rep.Unjoined.Endpoints = append(rep.Unjoined.Endpoints, UnconsumedEndpoint{
				Member: mf.member.Name, Endpoint: ep.Signature,
				FilePath: ep.FilePath, Line: ep.Line, Handler: ep.Handler,
			})
		}
	}
	sort.Slice(rep.Unjoined.Endpoints, func(i, j int) bool {
		a, b := rep.Unjoined.Endpoints[i], rep.Unjoined.Endpoints[j]
		if a.Member != b.Member {
			return a.Member < b.Member
		}
		if a.Endpoint != b.Endpoint {
			return a.Endpoint < b.Endpoint
		}
		return a.Line < b.Line
	})
}

// joinTables is the second join: one table name, two or more members.
//
// Deliberately the crudest join in the report, and the caveat on it is
// correspondingly loud. Table names are matched by NAME ALONE because a name is
// all the index holds — internal/registry strips the schema prefix at
// extraction, and no repository records which DATABASE it connects to. Two
// services that each own a private `users` table are indistinguishable here
// from two services sharing one. What makes it worth reporting anyway is the
// asymmetry of the mistake: a shared table nobody knew about causes an outage,
// and a false pairing costs one reader one minute.
func (f *Federator) joinTables(rep *Report, facts []*memberFacts) {
	byTable := map[string]map[string]*TableParticipant{}

	for _, mf := range facts {
		if !mf.member.Available() {
			continue
		}
		for _, t := range mf.tables {
			name := strings.TrimSpace(strings.ToLower(t.Table))
			if name == "" {
				continue
			}
			if byTable[name] == nil {
				byTable[name] = map[string]*TableParticipant{}
			}
			p := byTable[name][mf.member.Name]
			if p == nil {
				p = &TableParticipant{Member: mf.member.Name}
				byTable[name][mf.member.Name] = p
			}
			// TableUsageReport groups by (table, bound symbol), so one member can
			// contribute several rows for one table — the declared one and the
			// unbound one. Summed rather than replaced.
			p.Accesses += t.Accesses
			p.Reads += t.Readers
			p.Writes += t.Writers
			p.Uncertain += t.Uncertain
			p.Declared = p.Declared || t.Declared
		}
	}

	names := make([]string, 0, len(byTable))
	for name, members := range byTable {
		if len(members) < 2 {
			// One member naming a table is not a coupling. It is already
			// answered by `repocortex db` inside that repository.
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		members := byTable[name]
		st := SharedTable{Table: name, Confidence: model.ConfidenceHigh}
		memberNames := make([]string, 0, len(members))
		for n := range members {
			memberNames = append(memberNames, n)
		}
		sort.Strings(memberNames)

		var uncertain bool
		for _, n := range memberNames {
			p := *members[n]
			st.Participants = append(st.Participants, p)
			if p.Writes > 0 {
				st.Writers = append(st.Writers, n)
			}
			if p.Reads > 0 {
				st.Readers = append(st.Readers, n)
			}
			if p.Declared {
				st.Owners = append(st.Owners, n)
			}
			if p.Uncertain > 0 {
				uncertain = true
			}
		}
		st.WriteWrite = len(st.Writers) > 1
		if uncertain {
			// A run-time-assembled statement named this table. The coupling may
			// be real and the table name may equally be a fragment.
			st.Confidence = demote(st.Confidence)
			st.Note = "at least one access was assembled at run time, so the table name itself is not certain"
		}
		st.Confidence = capHeuristic(st.Confidence)
		if len(st.Owners) == 0 {
			st.Owners = []string{}
			if st.Note == "" {
				st.Note = "no member of this system declares this table; every one of them merely queries it"
			}
		}
		if st.Writers == nil {
			st.Writers = []string{}
		}
		if st.Readers == nil {
			st.Readers = []string{}
		}

		if len(rep.SharedTables) >= f.maxSharedTables {
			rep.Unjoined.Truncated["shared_tables"]++
			continue
		}
		rep.SharedTables = append(rep.SharedTables, st)
	}

	// Write/write couplings first: they are the finding, and a reader who stops
	// after the first screen should have seen them.
	sort.SliceStable(rep.SharedTables, func(i, j int) bool {
		a, b := rep.SharedTables[i], rep.SharedTables[j]
		if a.WriteWrite != b.WriteWrite {
			return a.WriteWrite
		}
		return a.Table < b.Table
	})
}

// memberStatuses builds the section that is printed first and never omitted.
func (f *Federator) memberStatuses(facts []*memberFacts) []MemberStatus {
	now := f.now()
	out := make([]MemberStatus, 0, len(facts))
	for _, mf := range facts {
		st := MemberStatus{
			Name:      mf.member.Name,
			IndexPath: mf.member.IndexPath,
			Available: mf.member.Available(),
			Warnings:  []string{},
		}
		if !st.Available {
			st.Unavailable = mf.member.Unavailable
			out = append(out, st)
			continue
		}

		st.RunID = mf.member.RunID
		st.Endpoints = len(mf.endpoints)
		st.OutboundCalls = len(mf.calls)
		st.Tables = len(mf.tables)
		for _, c := range mf.calls {
			if c.Binding == bindingBound {
				st.LocallyAnswered++
			}
		}
		if mf.run != nil {
			st.GitRevision = mf.run.GitRevision
			st.ProducerVersion = mf.run.ProducerVersion
			st.Status = string(mf.run.Status)
			st.IndexState = string(mf.run.IndexState)
			st.AccuracyMode = mf.run.AccuracyMode
			st.IndexedUnix = mf.run.FinishedUnix
			if st.IndexedUnix > 0 {
				st.AgeHours = now.Sub(time.Unix(st.IndexedUnix, 0)).Hours()
			}
			st.Warnings = memberWarnings(mf.run, st, f.runningVersion, f.staleAfter)
		}
		out = append(out, st)
	}
	return out
}

// memberWarnings names the conditions under which this member's contribution
// should not be read as a current description of that repository.
func memberWarnings(run *model.IndexRun, st MemberStatus, running string, staleAfter time.Duration) []string {
	out := []string{}
	if run.Status != model.StatusReady {
		out = append(out, fmt.Sprintf(
			"the run is %s, so facts from this member may be missing rather than absent", run.Status))
	}
	if !run.Complete() {
		out = append(out, fmt.Sprintf(
			"the index is partial — %d discovered file(s) reached no terminal outcome, "+
				"so an endpoint or a call this member makes may simply not have been seen",
			run.FilesUnaccounted))
	}
	if run.GitRevision == "" {
		out = append(out, "no commit was recorded for this run, so which revision of this "+
			"repository the report describes cannot be stated")
	}
	switch {
	case run.ProducerVersion == "":
		out = append(out, "this index does not record which build wrote it, so whether its "+
			"resolution rules match the rest of the system is unknown")
	case running != "" && run.ProducerVersion != running:
		out = append(out, fmt.Sprintf(
			"indexed by repocortex %s while this report is produced by %s; resolution rules "+
				"change between builds", run.ProducerVersion, running))
	}
	if st.IndexedUnix > 0 && time.Duration(st.AgeHours*float64(time.Hour)) > staleAfter {
		out = append(out, fmt.Sprintf(
			"indexed %.0f day(s) ago — a system graph is only as current as its least "+
				"current member", st.AgeHours/24))
	}
	if st.Endpoints == 0 && st.OutboundCalls == 0 {
		out = append(out, "this member declares no endpoints and makes no outbound HTTP call, "+
			"so it cannot appear on either end of a cross-repository edge; if that is "+
			"surprising, its framework may not be one route extraction covers")
	}
	return out
}

// provenance summarises WHEN and BY WHAT the system was indexed.
func (f *Federator) provenance(facts []*memberFacts) Provenance {
	p := Provenance{
		RunningVersion:   f.runningVersion,
		ProducerVersions: []string{},
		Notes:            []string{},
	}
	seen := map[string]bool{}
	var unstamped, available int
	for _, mf := range facts {
		if !mf.member.Available() || mf.run == nil {
			continue
		}
		available++
		v := mf.run.ProducerVersion
		if v == "" {
			unstamped++
		}
		if !seen[v] {
			seen[v] = true
			p.ProducerVersions = append(p.ProducerVersions, v)
		}
		if t := mf.run.FinishedUnix; t > 0 {
			if p.OldestIndexedUnix == 0 || t < p.OldestIndexedUnix {
				p.OldestIndexedUnix = t
			}
			if t > p.NewestIndexedUnix {
				p.NewestIndexedUnix = t
			}
		}
	}
	sort.Strings(p.ProducerVersions)
	p.MixedProducers = len(p.ProducerVersions) > 1
	if p.OldestIndexedUnix > 0 && p.NewestIndexedUnix > 0 {
		p.SkewHours = time.Unix(p.NewestIndexedUnix, 0).Sub(time.Unix(p.OldestIndexedUnix, 0)).Hours()
	}

	if p.MixedProducers {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"Members were indexed by %d different builds (%s). Resolution rules change "+
				"between builds, so this graph holds edges no single binary would have produced.",
			len(p.ProducerVersions), strings.Join(quoteAll(p.ProducerVersions), ", ")))
	}
	if unstamped > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"%d member(s) record no producer build, which is unknown provenance rather than "+
				"known staleness. Re-index them to be sure.", unstamped))
	}
	if p.SkewHours >= 24 {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"The members were indexed %.0f day(s) apart. This graph describes no single "+
				"moment of the system; it is the union of %d snapshots taken across that window.",
			p.SkewHours/24, available))
	}
	return p
}

func quoteAll(vs []string) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if v == "" {
			out = append(out, "unstamped")
			continue
		}
		out = append(out, `"`+v+`"`)
	}
	return out
}

// standingCaveats are the limitations of the METHOD, printed whether or not the
// data happens to exercise them. They are the reading rule for every number
// above, which is why they do not vary with the data.
func standingCaveats() []string {
	return []string{
		"Route matching is a HEURISTIC. `/users/{id}`, `/users/:id` and `/users/%s` denote " +
			"one route in three syntaxes and nothing proved they meet, so no fact in this " +
			"report is ever `exact` — that tier is reserved for compiler-proven facts.",
		"A cross-repository edge's confidence is the MINIMUM over its legs, the route match " +
			"included. A chain through one heuristic hop is a lead, not a fact.",
		"The outbound call's own extractor confidence is not carried by the index's call " +
			"report, so a literal, non-dynamic call is graded `high` — the ceiling a text " +
			"scan can claim — and a run-time-assembled one `low`.",
		"Shared tables are matched by NAME ALONE. No index records which database a service " +
			"connects to, so two services that each own a private `users` table are " +
			"indistinguishable here from two sharing one.",
		"\"Nothing calls this endpoint\" means nothing in THIS system does. A mobile client, " +
			"a cron job, a partner or a repository absent from the member list would not appear.",
	}
}
