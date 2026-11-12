package impact

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/git"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fakeChanges stands in for a git repository.
//
// The diff itself is not what these tests are about — ChangedSince is covered
// by internal/acquire/git's own suite — and standing up a real repository per
// case would make every assertion here depend on go-git's rename heuristics.
type fakeChanges struct {
	changes []git.Change
	err     error
	calls   int
	from    string
	to      string
}

func (f *fakeChanges) ChangedSince(ctx context.Context, from, to string) ([]git.Change, error) {
	f.calls++
	f.from, f.to = from, to
	if f.err != nil {
		return nil, f.err
	}
	return f.changes, ctx.Err()
}

// spyGraph records every traversal so a test can assert on the DIRECTION of the
// walk rather than only on the names that fell out of it. A result that happens
// to look right because the fixture is symmetric would pass the name assertion
// and still be walking the wrong way.
type spyGraph struct {
	inner   capability.Graph
	queries []graph.Query
}

func (s *spyGraph) Traverse(ctx context.Context, q graph.Query) (graph.Result, error) {
	s.queries = append(s.queries, q)
	return s.inner.Traverse(ctx, q)
}

func (s *spyGraph) Neighbours(ctx context.Context, symbolID int64, dir graph.Direction,
	kinds []model.EdgeKind, minConf model.Confidence) ([]graph.Neighbour, error) {
	return s.inner.Neighbours(ctx, symbolID, dir, kinds, minConf)
}

func spyDeps(deps capability.Deps) (capability.Deps, *spyGraph) {
	spy := &spyGraph{inner: deps.Graph}
	deps.Graph = spy
	return deps, spy
}

func modified(paths ...string) []git.Change {
	out := make([]git.Change, 0, len(paths))
	for _, p := range paths {
		out = append(out, git.Change{Path: p, Kind: git.ChangeModified})
	}
	return out
}

func sortedNames(in []Impacted) []string {
	out := make([]string, 0, len(in))
	for _, imp := range in {
		out = append(out, imp.Name)
	}
	sort.Strings(out)
	return out
}

func hasName(in []Impacted, name string) bool {
	for _, imp := range in {
		if imp.Name == name {
			return true
		}
	}
	return false
}

// TestAnalyzeDiffTraversesInboundNotOutbound is the whole design in one
// assertion. Changing a symbol threatens what DEPENDS ON it; what it calls is a
// different question, and answering that one under this name is worse than
// answering nothing.
func TestAnalyzeDiffTraversesInboundNotOutbound(t *testing.T) {
	t.Parallel()
	// caller -> changed -> callee. An edge A->B means A calls B.
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "changed", "svc/changed.go"),
			captest.S(2, "caller", "svc/caller.go"),
			captest.S(3, "callee", "svc/callee.go"),
		},
		[]captest.Edge{captest.E(2, 1), captest.E(1, 3)},
		realisticRefs)
	deps, spy := spyDeps(deps)

	src := &fakeChanges{changes: modified("svc/changed.go")}
	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: src, BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}

	if !hasName(rep.Impacted, "caller") {
		t.Errorf("caller not reported: a dependent of the change is at risk; got %v",
			sortedNames(rep.Impacted))
	}
	if hasName(rep.Impacted, "callee") {
		t.Errorf("callee reported: what the change calls is not what the change breaks; got %v",
			sortedNames(rep.Impacted))
	}
	if len(spy.queries) == 0 {
		t.Fatal("no traversal ran")
	}
	for i, q := range spy.queries {
		if q.Direction != graph.Incoming {
			t.Errorf("query %d Direction = %v, want graph.Incoming", i, q.Direction)
		}
	}
	if src.from != "HEAD~1" || src.to != "HEAD" {
		t.Errorf("ChangedSince(%q, %q), want (HEAD~1, HEAD)", src.from, src.to)
	}
}

// countingRegistry bounds how much the read surface is consulted.
//
// It used to assert that a diff with no changed files asked the registry
// NOTHING. That is no longer true and no longer wanted: the completeness block
// is now built on every return path, which costs one symbol-index load and one
// resolution-stats query, because a report whose caveats are empty and whose
// IndexedSymbols is zero is a report that says "nothing at risk, index empty,
// no reason to doubt me" — the exact false reassurance a stale index produces.
// Two cheap reads are the price of that sentence being false. What is still
// pinned here is the bound: ONE of each, never per-file and never per-seed.
type countingRegistry struct {
	inner      capability.Registry
	indexLoads int
	statLoads  int
}

func (c *countingRegistry) FindSymbolsByName(ctx context.Context, runID int64,
	name, kind string, limit int) ([]registry.SymbolHit, error) {
	return c.inner.FindSymbolsByName(ctx, runID, name, kind, limit)
}

func (c *countingRegistry) LoadSymbolIndex(ctx context.Context, runID int64) (map[string][]registry.SymbolIndexEntry, error) {
	c.indexLoads++
	return c.inner.LoadSymbolIndex(ctx, runID)
}

func (c *countingRegistry) ResolutionStats(ctx context.Context, runID int64) (registry.ResolutionStats, error) {
	c.statLoads++
	return c.inner.ResolutionStats(ctx, runID)
}

// TestAnalyzeDiffOnEmptyDiffReturnsEmptyNotEverything pins the failure mode that
// makes this function dangerous to get wrong: internal/graph treats an empty
// root set as "nothing to walk" today, but a future seeding bug that turned it
// into "walk everything" would produce a confident, complete-looking blast
// radius for a diff that changed nothing. The guard is that no traversal is
// attempted at all — and, for a diff with no changed files, that the index is
// not even opened.
func TestAnalyzeDiffOnEmptyDiffReturnsEmptyNotEverything(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes []git.Change
		// wantUnindexed is how many changed paths carried no symbols in the run.
		wantUnindexed int
		// wantUnindexedUnknown separates the two guards. "All changed paths are
		// unindexed" is a statement about a non-empty diff; emitting it for a
		// diff with no changed files at all would read as "all 0 path(s) are
		// unindexed", and asserting its absence is what pins the empty-diff
		// branch as a distinct branch rather than a fall-through.
		wantUnindexedUnknown bool
		// wantIndexLoads and wantStatLoads bound the reads the completeness
		// block costs: exactly one each, on every path.
		wantIndexLoads int
		wantStatLoads  int
	}{
		{name: "no changed files", changes: nil, wantIndexLoads: 1, wantStatLoads: 1},
		{
			name:                 "changed files carry no indexed symbols",
			changes:              modified("docs/README.md", "svc/untracked.go"),
			wantUnindexed:        2,
			wantUnindexedUnknown: true,
			wantIndexLoads:       1,
			wantStatLoads:        1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, runID := captest.Build(t,
				[]captest.Sym{
					captest.S(1, "a", "svc/a.go"),
					captest.S(2, "b", "svc/b.go"),
				},
				[]captest.Edge{captest.E(1, 2)},
				realisticRefs)
			deps, spy := spyDeps(deps)
			counter := &countingRegistry{inner: deps.Registry}
			deps.Registry = counter

			rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
				Changes: &fakeChanges{changes: tc.changes}, BaseRev: "HEAD", HeadRev: "HEAD",
			})
			if err != nil {
				t.Fatalf("AnalyzeDiff: %v", err)
			}
			if len(rep.Impacted) != 0 {
				t.Fatalf("empty seed set produced %d impacted symbols; %v",
					len(rep.Impacted), sortedNames(rep.Impacted))
			}
			if len(rep.Seeds) != 0 {
				t.Fatalf("Seeds = %d, want 0", len(rep.Seeds))
			}
			if len(spy.queries) != 0 {
				t.Fatalf("%d traversal(s) ran on an empty seed set; want 0", len(spy.queries))
			}
			if got := len(rep.UnindexedFiles); got != tc.wantUnindexed {
				t.Errorf("UnindexedFiles = %d, want %d", got, tc.wantUnindexed)
			}
			if counter.indexLoads != tc.wantIndexLoads {
				t.Errorf("symbol index loaded %d time(s), want %d: an empty diff must not "+
					"consult the index at all", counter.indexLoads, tc.wantIndexLoads)
			}
			if counter.statLoads != tc.wantStatLoads {
				t.Errorf("resolution stats loaded %d time(s), want %d", counter.statLoads, tc.wantStatLoads)
			}
			if _, got := findDiffUnknown(rep, UnknownUnindexedChanges); got != tc.wantUnindexedUnknown {
				t.Errorf("unindexed-changes unknown present = %v, want %v: an empty diff has no "+
					"unindexed paths to explain", got, tc.wantUnindexedUnknown)
			}
		})
	}
}

// TestAnalyzeDiffPreservesTruncation: a bound hit by any seed's walk makes every
// count in the aggregate a floor. Re-deriving truncation from the merged rows —
// "I have 10 symbols and asked for 10, so maybe" — is exactly the guess this
// forbids; the traversal already knows, and its answer is copied.
func TestAnalyzeDiffPreservesTruncation(t *testing.T) {
	t.Parallel()
	syms := []captest.Sym{captest.S(1, "hub", "svc/hub.go")}
	var edges []captest.Edge
	for i := int64(2); i <= 60; i++ {
		syms = append(syms, captest.S(i, fmt.Sprintf("caller%d", i), fmt.Sprintf("svc/c%d.go", i)))
		edges = append(edges, captest.E(i, 1))
	}
	deps, runID := captest.Build(t, syms, edges, realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified("svc/hub.go")},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
		Options: Options{MaxDepth: 3, MaxNodes: 10},
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	if !rep.Truncated {
		t.Fatal("Truncated = false on a 59-dependent hub capped at 10 nodes")
	}
	if rep.TruncatedReason == "" {
		t.Error("TruncatedReason is empty; a truncated blast radius must say why")
	}
	if len(rep.Seeds) != 1 {
		t.Fatalf("Seeds = %d, want 1", len(rep.Seeds))
	}
	// The per-seed Report is carried through unaltered, so nothing the seed's
	// own analysis said about its bounds is lost in the aggregation.
	if !rep.Seeds[0].Report.Truncated {
		t.Error("seed Report.Truncated = false but the aggregate is truncated")
	}
	if rep.Seeds[0].Report.TruncatedReason != rep.TruncatedReason {
		t.Errorf("aggregate reason %q != seed reason %q",
			rep.TruncatedReason, rep.Seeds[0].Report.TruncatedReason)
	}
	if _, ok := findDiffUnknown(rep, UnknownUnresolvedReferences); !ok {
		t.Errorf("unresolved-references unknown missing from the aggregate: %+v", rep.Unknown)
	}
	if rep.Completeness.LowerBound != true {
		t.Error("Completeness.LowerBound = false")
	}
}

func findDiffUnknown(rep DiffReport, kind UnknownKind) (Unknown, bool) {
	for _, u := range rep.Unknown {
		if u.Kind == kind {
			return u, true
		}
	}
	return Unknown{}, false
}

// TestAnalyzeDiffDoesNotDoubleCountAcrossSeeds: a symbol that depends on two
// changed files is one thing to review, not two. It is reported once, at the
// shallowest depth any seed reached it by.
func TestAnalyzeDiffDoesNotDoubleCountAcrossSeeds(t *testing.T) {
	t.Parallel()
	// shared -> changedA, shared -> changedB (depth 1 from both)
	// far -> shared                          (depth 2 from both)
	// near -> changedB                       (depth 1, one seed only)
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "changedA", "svc/a.go"),
			captest.S(2, "changedB", "svc/b.go"),
			captest.S(3, "shared", "svc/shared.go"),
			captest.S(4, "far", "svc/far.go"),
			captest.S(5, "near", "svc/near.go"),
		},
		[]captest.Edge{
			captest.E(3, 1), captest.E(3, 2), captest.E(4, 3), captest.E(5, 2),
		},
		realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified("svc/a.go", "svc/b.go")},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	if len(rep.Seeds) != 2 {
		t.Fatalf("Seeds = %d, want 2", len(rep.Seeds))
	}

	seen := map[int64]int{}
	for _, imp := range rep.Impacted {
		seen[imp.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("symbol %d appears %d times in Impacted; want 1", id, n)
		}
	}
	if got, want := sortedNames(rep.Impacted), []string{"far", "near", "shared"}; len(got) != len(want) {
		t.Fatalf("Impacted = %v, want %v", got, want)
	}
	for _, imp := range rep.Impacted {
		if imp.Name == "shared" && imp.Depth != 1 {
			t.Errorf("shared Depth = %d, want 1 (shallowest across seeds)", imp.Depth)
		}
		if imp.Name == "far" && imp.Depth != 2 {
			t.Errorf("far Depth = %d, want 2", imp.Depth)
		}
	}
	// The changed symbols themselves are inputs, not collateral.
	if hasName(rep.Impacted, "changedA") || hasName(rep.Impacted, "changedB") {
		t.Errorf("a changed symbol is listed in its own blast radius: %v",
			sortedNames(rep.Impacted))
	}
	if rep.TotalImpacted() != len(rep.Impacted) {
		t.Errorf("TotalImpacted = %d, len(Impacted) = %d", rep.TotalImpacted(), len(rep.Impacted))
	}
}

// TestAnalyzeDiffConfidenceIsTheWeakestLinkNotTheStrongest: aggregation must not
// launder a guess into a proof. A symbol reached at the same depth from two
// seeds keeps a confidence no stronger than a single-seed analysis would have
// given it.
func TestAnalyzeDiffConfidenceIsPreservedFromTheSeedWalk(t *testing.T) {
	t.Parallel()
	// far --high--> mid --low--> changed
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "changed", "svc/changed.go"),
			captest.S(2, "mid", "svc/mid.go"),
			captest.S(3, "far", "svc/far.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithConf(model.ConfidenceLow),
			captest.E(3, 2).WithConf(model.ConfidenceHigh),
		},
		realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified("svc/changed.go")},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
		Options: Options{MinConfidence: model.ConfidenceLow},
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	for _, imp := range rep.Impacted {
		if imp.Confidence != model.ConfidenceLow {
			t.Errorf("%s confidence = %q, want low (the path minimum)", imp.Name, imp.Confidence)
		}
	}
}

// TestAnalyzeDiffSeedsFromRenamesAndDeletions: the symbols whose dependents are
// at risk live at the path the file had in the INDEXED run, which for a rename
// is the old name and for a deletion is the only name there is. Seeding only
// from Change.Path would silently return an empty radius for both.
func TestAnalyzeDiffSeedsFromRenamesAndDeletions(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "moved", "svc/old.go"),
			captest.S(2, "gone", "svc/gone.go"),
			captest.S(3, "callerOfMoved", "svc/c1.go"),
			captest.S(4, "callerOfGone", "svc/c2.go"),
		},
		[]captest.Edge{captest.E(3, 1), captest.E(4, 2)},
		realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: []git.Change{
			{Path: "svc/new.go", OldPath: "svc/old.go", Kind: git.ChangeRenamed},
			{Path: "svc/gone.go", Kind: git.ChangeDeleted},
		}},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	for _, want := range []string{"callerOfMoved", "callerOfGone"} {
		if !hasName(rep.Impacted, want) {
			t.Errorf("%s not reported; got %v", want, sortedNames(rep.Impacted))
		}
	}
}

// TestAnalyzeDiffBoundsTheSeedSet: a 900-file refactor must not silently launch
// 900 traversals. The cap is applied deterministically and stated.
func TestAnalyzeDiffBoundsTheSeedSet(t *testing.T) {
	t.Parallel()
	var syms []captest.Sym
	var paths []string
	for i := int64(1); i <= 12; i++ {
		p := fmt.Sprintf("svc/f%02d.go", i)
		syms = append(syms, captest.S(i, fmt.Sprintf("s%02d", i), p))
		paths = append(paths, p)
	}
	deps, runID := captest.Build(t, syms, nil, realisticRefs)
	deps, spy := spyDeps(deps)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified(paths...)},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
		MaxSeeds: 5,
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	if len(rep.Seeds) != 5 {
		t.Fatalf("Seeds = %d, want 5", len(rep.Seeds))
	}
	if rep.SeedsFound != 12 {
		t.Errorf("SeedsFound = %d, want 12", rep.SeedsFound)
	}
	if !rep.Truncated {
		t.Error("Truncated = false after dropping 7 seeds")
	}
	// Seeds are taken in symbol-id order, so ids 1..5 are analysed and 6..12 are
	// not. No traversal may be rooted at a dropped seed: a cap that still walked
	// everything would report a bounded answer while paying the unbounded cost.
	analysed := map[int64]bool{}
	for _, s := range rep.Seeds {
		analysed[s.ID] = true
	}
	for i, q := range spy.queries {
		for _, root := range q.Roots {
			if !analysed[root] {
				t.Errorf("query %d walked from dropped seed %d", i, root)
			}
		}
	}
}

func TestAnalyzeDiffWrapsChangeSourceFailure(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "a", "svc/a.go")}, nil, realisticRefs)

	sentinel := errors.New("boom")
	_, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{err: sentinel}, BaseRev: "a", HeadRev: "b",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the source error", err)
	}
}

func TestAnalyzeDiffRequiresAChangeSource(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "a", "svc/a.go")}, nil, realisticRefs)
	if _, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		BaseRev: "a", HeadRev: "b",
	}); err == nil {
		t.Fatal("nil ChangeSource accepted")
	}
}

func TestAnalyzeDiffContextCancellationPropagates(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "changed", "svc/changed.go"),
			captest.S(2, "caller", "svc/caller.go"),
		},
		[]captest.Edge{captest.E(2, 1)}, realisticRefs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := AnalyzeDiff(ctx, deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified("svc/changed.go")},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The production repository must satisfy the port without an adapter; if
// ChangedSince ever changes shape this fails to compile rather than at run time.
var _ ChangeSource = (*git.Repo)(nil)

// fakeRuns supplies the provenance row. A struct rather than a bare string so a
// test can also exercise "the registry failed" and "there is no such run".
type fakeRuns struct {
	rev  string
	nil_ bool
	err  error
}

func (f *fakeRuns) Run(ctx context.Context, runID int64) (*model.IndexRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.nil_ {
		return nil, nil
	}
	return &model.IndexRun{ID: runID, GitRevision: f.rev}, nil
}

// fakeResolver is a rev-parse table. Anything absent is unresolvable, which is
// what an index built in a different repository looks like from here.
type fakeResolver map[string]string

func (f fakeResolver) ResolveRevision(ctx context.Context, rev string) (string, error) {
	if rev == "" {
		return "", nil
	}
	if sha, ok := f[rev]; ok {
		return sha, nil
	}
	return "", fmt.Errorf("resolve %q: unknown revision", rev)
}

const (
	shaHead = "1111111111111111111111111111111111111111"
	shaBase = "2222222222222222222222222222222222222222"
	shaElse = "3333333333333333333333333333333333333333"
)

func revTable() fakeResolver {
	return fakeResolver{
		"HEAD":     shaHead,
		"main":     shaHead,
		"HEAD~1":   shaBase,
		"v1.0":     shaBase,
		shaHead:    shaHead,
		shaBase:    shaBase,
		shaElse:    shaElse,
		"unstable": shaElse,
	}
}

// TestAnalyzeDiffChecksTheIndexWasBuiltAtTheChange is the guard against this
// package's worst failure: a fully-formed, confident blast radius computed from
// an index of code that is at neither end of the diff. Nothing about such a
// report looks wrong — the counts are real, the symbols exist, they are simply
// about other code.
//
// The false-positive half matters as much. An index at HEAD answering
// HEAD~1..HEAD is the NORMAL case, and a warning that fires there is a warning
// readers learn to skip, taking the real one with it.
func TestAnalyzeDiffChecksTheIndexWasBuiltAtTheChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		runs       RunSource
		resolver   RevisionResolver
		base, head string
		wantWarn   bool
		// wantIn is a fragment the warning must contain, so the test pins WHAT
		// was reported and not merely that something was.
		wantIn string
	}{
		{
			name: "index at head of the analysed range does not warn",
			runs: &fakeRuns{rev: shaHead}, resolver: revTable(),
			base: "HEAD~1", head: "HEAD",
		},
		{
			name: "index at base of the analysed range does not warn",
			runs: &fakeRuns{rev: shaBase}, resolver: revTable(),
			base: "HEAD~1", head: "HEAD",
		},
		{
			name: "symbolic names naming the same commit do not warn",
			runs: &fakeRuns{rev: shaHead}, resolver: revTable(),
			base: "v1.0", head: "main",
		},
		{
			name: "literal equality needs no resolver",
			runs: &fakeRuns{rev: "HEAD"},
			base: "HEAD~1", head: "HEAD",
		},
		{
			name: "index at an unrelated commit warns",
			runs: &fakeRuns{rev: shaElse}, resolver: revTable(),
			base: "HEAD~1", head: "HEAD",
			wantWarn: true, wantIn: "STALE INDEX",
		},
		{
			name: "empty recorded revision is uncertain, not fine",
			runs: &fakeRuns{rev: ""}, resolver: revTable(),
			base: "HEAD~1", head: "HEAD",
			wantWarn: true, wantIn: "recorded no git revision",
		},
		{
			name:     "no run source at all is uncertain, not fine",
			resolver: revTable(),
			base:     "HEAD~1", head: "HEAD",
			wantWarn: true, wantIn: "No run source was supplied",
		},
		{
			name: "a revision this repository cannot resolve is uncertain",
			runs: &fakeRuns{rev: "9999999999999999999999999999999999999999"}, resolver: revTable(),
			base: "HEAD~1", head: "HEAD",
			wantWarn: true, wantIn: "could not resolve",
		},
		{
			name: "an unresolvable endpoint is uncertain, not a mismatch",
			runs: &fakeRuns{rev: shaHead}, resolver: revTable(),
			base: "no-such-branch", head: "no-such-tag",
			wantWarn: true, wantIn: "could not be resolved",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, runID := captest.Build(t,
				[]captest.Sym{
					captest.S(1, "changed", "svc/changed.go"),
					captest.S(2, "caller", "svc/caller.go"),
				},
				[]captest.Edge{captest.E(2, 1)}, realisticRefs)

			rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
				Changes:   &fakeChanges{changes: modified("svc/changed.go")},
				Runs:      tc.runs,
				Revisions: tc.resolver,
				BaseRev:   tc.base, HeadRev: tc.head,
			})
			if err != nil {
				t.Fatalf("AnalyzeDiff: %v", err)
			}
			// The analysis itself must still happen: a provenance doubt is a
			// caveat on the answer, not a refusal to compute one.
			if !hasName(rep.Impacted, "caller") {
				t.Fatalf("caller not reported; got %v", sortedNames(rep.Impacted))
			}

			u, got := findDiffUnknown(rep, UnknownStaleIndex)
			if got != tc.wantWarn {
				t.Fatalf("stale-index unknown present = %v, want %v (unknowns: %+v)",
					got, tc.wantWarn, rep.Unknown)
			}
			if rep.RevisionMatchesRange == tc.wantWarn {
				t.Errorf("RevisionMatchesRange = %v alongside warn = %v",
					rep.RevisionMatchesRange, tc.wantWarn)
			}
			if !tc.wantWarn {
				return
			}
			if !strings.Contains(u.Detail, tc.wantIn) {
				t.Errorf("warning does not say %q:\n%s", tc.wantIn, u.Detail)
			}
			// A warning only in a list a consumer may filter by kind is a
			// warning that gets filtered. It has to be in the block every
			// report renders.
			if !rep.Completeness.HasCaveat(u.Detail) {
				t.Errorf("Completeness does not carry the stale-index warning; caveats: %v",
					rep.Completeness.Caveats)
			}
		})
	}
}

func TestAnalyzeDiffReportsTheIndexedRevision(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "changed", "svc/changed.go")}, nil, realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified("svc/changed.go")},
		Runs:    &fakeRuns{rev: shaHead}, Revisions: revTable(),
		BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	if rep.IndexedRevision != shaHead {
		t.Errorf("IndexedRevision = %q, want %q", rep.IndexedRevision, shaHead)
	}
}

func TestAnalyzeDiffWrapsRunSourceFailure(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "a", "svc/a.go")}, nil, realisticRefs)
	sentinel := errors.New("run row unreadable")
	_, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified("svc/a.go")},
		Runs:    &fakeRuns{err: sentinel},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the run-source error", err)
	}
}

// fakeRepo is fakeChanges that also knows where it is rooted, which is what
// makes the git-root/index-root mismatch reproducible without a real checkout.
type fakeRepo struct {
	fakeChanges
	root string
}

func (f *fakeRepo) Root() string { return f.root }

// TestAnalyzeDiffStatesThatEveryChangedFileWasUnindexed pins the second silent
// failure: git.Open walks up to the enclosing .git, so ChangedSince returns
// REPO-relative paths while the symbol index holds paths relative to the
// directory the indexer walked. Index a subdirectory and every changed path
// misses, producing a clean, error-free "nothing at risk" over a diff that
// changed indexed code.
func TestAnalyzeDiffStatesThatEveryChangedFileWasUnindexed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		indexRoot string
		wantSeeds int
		wantWarn  bool
	}{
		{
			name:     "paths in a different space produce a stated empty answer",
			wantWarn: true,
		},
		{
			name:      "an index root rebases the diff into the index's path space",
			indexRoot: "/repo/sub",
			wantSeeds: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, runID := captest.Build(t,
				[]captest.Sym{
					captest.S(1, "changed", "svc/changed.go"),
					captest.S(2, "caller", "svc/caller.go"),
				},
				[]captest.Edge{captest.E(2, 1)}, realisticRefs)

			src := &fakeRepo{root: "/repo"}
			src.changes = modified("sub/svc/changed.go")

			rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
				Changes: src, IndexRoot: tc.indexRoot,
				Runs: &fakeRuns{rev: "HEAD"}, BaseRev: "HEAD~1", HeadRev: "HEAD",
			})
			if err != nil {
				t.Fatalf("AnalyzeDiff: %v", err)
			}
			if len(rep.Seeds) != tc.wantSeeds {
				t.Fatalf("Seeds = %d, want %d", len(rep.Seeds), tc.wantSeeds)
			}
			u, got := findDiffUnknown(rep, UnknownUnindexedChanges)
			if got != tc.wantWarn {
				t.Fatalf("unindexed-changes unknown present = %v, want %v (unknowns: %+v)",
					got, tc.wantWarn, rep.Unknown)
			}
			if !tc.wantWarn {
				if !hasName(rep.Impacted, "caller") {
					t.Errorf("caller not reported after rebasing; got %v", sortedNames(rep.Impacted))
				}
				return
			}
			if !strings.Contains(u.Detail, "different spaces") {
				t.Errorf("the empty answer does not name the path-space cause:\n%s", u.Detail)
			}
			if len(rep.UnindexedFiles) != 1 {
				t.Errorf("UnindexedFiles = %v, want the one changed path", rep.UnindexedFiles)
			}
		})
	}
}

// TestAnalyzeDiffFoldsBelowFloorUnknownsIntoOneRow: every seed reports its own
// below-floor exclusion, so a 200-seed diff emitted ~200 rows differing only in
// a number. Nothing was lost, but the unknowns a reader must act on were buried
// in them.
func TestAnalyzeDiffFoldsBelowFloorUnknownsIntoOneRow(t *testing.T) {
	t.Parallel()
	var syms []captest.Sym
	var edges []captest.Edge
	var paths []string
	for i := int64(1); i <= 6; i++ {
		p := fmt.Sprintf("svc/changed%d.go", i)
		syms = append(syms, captest.S(i, fmt.Sprintf("changed%d", i), p))
		paths = append(paths, p)
		// One weak caller each: excluded by the default floor, counted once per
		// seed, and therefore ~one Unknown row per seed before the fold.
		id := 100 + i
		syms = append(syms, captest.S(id, fmt.Sprintf("weak%d", i), fmt.Sprintf("svc/weak%d.go", i)))
		edges = append(edges, captest.E(id, i).WithConf(model.ConfidenceLow))
	}
	deps, runID := captest.Build(t, syms, edges, realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified(paths...)},
		Runs:    &fakeRuns{rev: "HEAD"}, BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	rows := 0
	var folded Unknown
	for _, u := range rep.Unknown {
		if u.Kind == UnknownBelowConfidenceFloor {
			rows++
			folded = u
		}
	}
	if rows != 1 {
		t.Fatalf("below-floor unknown rows = %d, want 1 across %d seeds", rows, len(rep.Seeds))
	}
	if folded.Count != int64(len(rep.Seeds)) {
		t.Errorf("folded Count = %d, want %d (one hidden caller per seed)", folded.Count, len(rep.Seeds))
	}
	// The sum double-counts a symbol hidden from two seeds, so the row must say
	// so rather than read as a distinct-symbol total.
	if !strings.Contains(folded.Detail, "SUM") {
		t.Errorf("folded row does not state that it is a sum:\n%s", folded.Detail)
	}
	// Nothing is lost: each seed still carries its own figure.
	for _, s := range rep.Seeds {
		if _, ok := findUnknown(s.Report, UnknownBelowConfidenceFloor); !ok {
			t.Errorf("seed %s lost its own below-floor unknown", s.FQN)
		}
	}
}

// TestAnalyzeDiffStatesProvenanceOnTheEmptyPathsToo is the fix for a warning
// that fired everywhere except where it mattered.
//
// A stale index and a mis-rooted one BOTH show up as "no changed file carries a
// symbol" or as a diff that seeds nothing. Those were the two paths that
// returned before the completeness block was built, so the report a reader most
// needed the caveat on was the one report that carried none: no caveats, no
// LowerBound flag, an IndexedSymbols of zero, and a clean-looking "nothing at
// risk".
func TestAnalyzeDiffStatesProvenanceOnTheEmptyPathsToo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes []git.Change
		// wantUnindexedCaveat additionally requires the all-unindexed
		// explanation to reach the caveat block, not only the Unknown list.
		wantUnindexedCaveat bool
	}{
		{name: "stale index and an empty diff", changes: nil},
		{
			name:                "stale index and a diff of only unindexed files",
			changes:             modified("docs/README.md", "svc/untracked.go"),
			wantUnindexedCaveat: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, runID := captest.Build(t,
				[]captest.Sym{
					captest.S(1, "a", "svc/a.go"),
					captest.S(2, "b", "svc/b.go"),
				},
				[]captest.Edge{captest.E(1, 2)}, realisticRefs)

			rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
				Changes: &fakeChanges{changes: tc.changes},
				Runs:    &fakeRuns{rev: shaElse}, Revisions: revTable(),
				BaseRev: "HEAD~1", HeadRev: "HEAD",
			})
			if err != nil {
				t.Fatalf("AnalyzeDiff: %v", err)
			}
			u, ok := findDiffUnknown(rep, UnknownStaleIndex)
			if !ok {
				t.Fatalf("no stale-index unknown on an early path; unknowns: %+v", rep.Unknown)
			}
			if len(rep.Completeness.Caveats) == 0 {
				t.Fatal("Completeness.Caveats is empty: the report claims nothing is at risk and " +
					"states no reason to doubt it")
			}
			if !rep.Completeness.HasCaveat(u.Detail) {
				t.Errorf("the stale-index warning never reached the caveat block; caveats: %v",
					rep.Completeness.Caveats)
			}
			// LowerBound is documented as always true. An early return that
			// leaves it false publishes a machine-readable claim of
			// completeness on the least complete answer this package gives.
			if !rep.Completeness.LowerBound {
				t.Error("Completeness.LowerBound = false on an early path")
			}
			if !rep.Completeness.HasCaveat(capability.LowerBoundCaveat) {
				t.Error("the mandatory lower-bound caveat is missing from an early path")
			}
			// The denominator is real, not a zero that reads as "the index is
			// empty" — which is a different and much worse diagnosis.
			if rep.Completeness.IndexedSymbols != 2 {
				t.Errorf("Completeness.IndexedSymbols = %d, want 2",
					rep.Completeness.IndexedSymbols)
			}
			if tc.wantUnindexedCaveat {
				un, ok := findDiffUnknown(rep, UnknownUnindexedChanges)
				if !ok {
					t.Fatalf("no unindexed-changes unknown; unknowns: %+v", rep.Unknown)
				}
				if !rep.Completeness.HasCaveat(un.Detail) {
					t.Errorf("the all-unindexed explanation never reached the caveat block; caveats: %v",
						rep.Completeness.Caveats)
				}
			}
		})
	}
}

// TestAnalyzeDiffKeepsChangedFilesInOneUnambiguousPathSpace pins the collision.
//
// With the index rooted at /repo/sub, "sub/svc/changed.go" rebases to
// "svc/changed.go" — which is also the literal name of a DIFFERENT file at the
// repository root. Reporting the rebased path beside the un-rebased one put two
// identical strings in ChangedFiles naming two different files, and left
// UnindexedFiles pointing at one of them with no way to tell which.
func TestAnalyzeDiffKeepsChangedFilesInOneUnambiguousPathSpace(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "changed", "svc/changed.go"),
			captest.S(2, "caller", "svc/caller.go"),
		},
		[]captest.Edge{captest.E(2, 1)}, realisticRefs)

	src := &fakeRepo{root: "/repo"}
	src.changes = modified("sub/svc/changed.go", "svc/changed.go")

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: src, IndexRoot: "/repo/sub",
		Runs: &fakeRuns{rev: "HEAD"}, BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	want := []string{"sub/svc/changed.go", "svc/changed.go"}
	if got := rep.ChangedFiles; !equalStrings(got, want) {
		t.Errorf("ChangedFiles = %v, want %v (repo space, one string per file)", got, want)
	}
	// The root-level svc/changed.go is NOT inside the indexed subtree, so it is
	// the unindexed one — and the reader must be able to see that it is that
	// file and not the seeded one.
	if got, want := rep.UnindexedFiles, []string{"svc/changed.go"}; !equalStrings(got, want) {
		t.Errorf("UnindexedFiles = %v, want %v", got, want)
	}
	if rep.IndexRootPrefix != "sub/" {
		t.Errorf("IndexRootPrefix = %q, want %q: without it the reader cannot map "+
			"ChangedFiles onto Seeds[i].IndexedPath", rep.IndexRootPrefix, "sub/")
	}
	if len(rep.Seeds) != 1 {
		t.Fatalf("Seeds = %d, want 1", len(rep.Seeds))
	}
	if rep.Seeds[0].IndexedPath != "svc/changed.go" {
		t.Errorf("Seed.IndexedPath = %q, want the index-space name", rep.Seeds[0].IndexedPath)
	}
	// The un-rebased duplicate must not have been seeded from: it is a
	// different file that merely shares the index-space name.
	if rep.SeedsFound != 1 {
		t.Errorf("SeedsFound = %d, want 1: the root-level namesake was seeded from", rep.SeedsFound)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAnalyzeDiffNamesAMissingRunAsMissing: "recorded no git revision" sends the
// reader to re-index. A run id the registry has never heard of is a different
// problem with a different fix, and the two shared one message.
func TestAnalyzeDiffNamesAMissingRunAsMissing(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "changed", "svc/changed.go")}, nil, realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified("svc/changed.go")},
		Runs:    &fakeRuns{nil_: true}, Revisions: revTable(),
		BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	u, ok := findDiffUnknown(rep, UnknownStaleIndex)
	if !ok {
		t.Fatalf("a missing run row produced no doubt at all; unknowns: %+v", rep.Unknown)
	}
	if !strings.Contains(u.Detail, "does not exist") {
		t.Errorf("the missing run is not named as missing:\n%s", u.Detail)
	}
	if strings.Contains(u.Detail, "recorded no git revision") {
		t.Errorf("a missing run row is reported as a run that recorded nothing:\n%s", u.Detail)
	}
}

// TestAnalyzeDiffReportsAnIndexRootItCannotApply: IndexRoot set against a change
// source that cannot say where it is rooted means no rebasing happens at all.
// Nothing was reported unless EVERY file missed, so the half-matching case — the
// one that produces a partly-wrong report — looked clean.
func TestAnalyzeDiffReportsAnIndexRootItCannotApply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		changes   ChangeSource
		indexRoot string
		wantWarn  bool
	}{
		{
			name:      "change source is not a Rooter",
			changes:   &fakeChanges{changes: modified("svc/changed.go")},
			indexRoot: "/repo/sub",
			wantWarn:  true,
		},
		{
			name:      "change source reports an empty root",
			changes:   &fakeRepo{fakeChanges: fakeChanges{changes: modified("svc/changed.go")}},
			indexRoot: "/repo/sub",
			wantWarn:  true,
		},
		{
			name: "index root outside the repository",
			changes: &fakeRepo{
				fakeChanges: fakeChanges{changes: modified("svc/changed.go")}, root: "/repo",
			},
			indexRoot: "/elsewhere",
			wantWarn:  true,
		},
		{
			name: "index root equal to the git root is not a doubt",
			changes: &fakeRepo{
				fakeChanges: fakeChanges{changes: modified("svc/changed.go")}, root: "/repo",
			},
			indexRoot: "/repo",
		},
		{
			name:    "no index root at all is not a doubt",
			changes: &fakeChanges{changes: modified("svc/changed.go")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, runID := captest.Build(t,
				[]captest.Sym{
					captest.S(1, "changed", "svc/changed.go"),
					captest.S(2, "caller", "svc/caller.go"),
				},
				[]captest.Edge{captest.E(2, 1)}, realisticRefs)

			rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
				Changes: tc.changes, IndexRoot: tc.indexRoot,
				Runs: &fakeRuns{rev: "HEAD"}, BaseRev: "HEAD~1", HeadRev: "HEAD",
			})
			if err != nil {
				t.Fatalf("AnalyzeDiff: %v", err)
			}
			u, got := findDiffUnknown(rep, UnknownPathSpace)
			if got != tc.wantWarn {
				t.Fatalf("path-space unknown present = %v, want %v (unknowns: %+v)",
					got, tc.wantWarn, rep.Unknown)
			}
			if !tc.wantWarn {
				return
			}
			if !strings.Contains(u.Detail, tc.indexRoot) {
				t.Errorf("the unknown does not name the index root %q:\n%s", tc.indexRoot, u.Detail)
			}
			if !rep.Completeness.HasCaveat(u.Detail) {
				t.Errorf("the path-space warning never reached the caveat block; caveats: %v",
					rep.Completeness.Caveats)
			}
		})
	}
}

// TestAnalyzeDiffRanksTheSameWayAnalyzeDoes pins the two surfaces to one
// ranking rule.
//
// `repocortex impact --base/--head` and the analyze_impact tool answer the same
// question over the same index. If they order the blast radius by different
// keys, they disagree about which symbols are riskiest, and whichever one
// truncates first drops a different tail. analyzeTarget ranks by the
// traversal's own cost order; the diff path used to re-sort by (Depth, rowid),
// which threw that away.
//
// The fixture makes the two orders visibly different. Costs from the target:
// proofCaller 1 (exact, depth 1), deepProof 2 (exact+exact, depth 2),
// guessCaller 5 (medium, depth 1), deepGuess 10 (medium+medium, depth 2).
// (Depth, ID) would say guessCaller, proofCaller, deepProof, deepGuess.
func TestAnalyzeDiffRanksTheSameWayAnalyzeDoes(t *testing.T) {
	t.Parallel()
	deps, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "changed", "svc/changed.go"),
			captest.S(2, "guessCaller", "svc/guess.go"),
			captest.S(3, "proofCaller", "svc/proof.go"),
			captest.S(4, "deepProof", "svc/deep_proof.go"),
			captest.S(5, "deepGuess", "svc/deep_guess.go"),
		},
		[]captest.Edge{
			captest.E(2, 1).WithConf(model.ConfidenceMedium),
			captest.E(3, 1).WithConf(model.ConfidenceExact),
			captest.E(4, 3).WithConf(model.ConfidenceExact),
			captest.E(5, 2).WithConf(model.ConfidenceMedium),
		},
		realisticRefs)

	single, err := Analyze(context.Background(), deps, runID, "changed")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	src := &fakeChanges{changes: modified("svc/changed.go")}
	diff, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: src, BaseRev: "HEAD~1", HeadRev: "HEAD",
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}

	wantFlat := []string{"proofCaller", "deepProof", "guessCaller", "deepGuess"}
	if got := names(diff.Impacted); !equalStrings(got, wantFlat) {
		t.Fatalf("diff Impacted = %v, want %v (cheapest accumulated cost first, as the traversal ranked it)",
			got, wantFlat)
	}

	// The single-target report splits the same ranking into two sections, so
	// each section must be the matching subsequence of the flat list.
	if got, want := names(single.Direct), namesAtDepth(diff.Impacted, func(d int) bool { return d == 1 }); !equalStrings(got, want) {
		t.Errorf("Analyze Direct = %v, but the diff path ranks its depth-1 symbols %v", got, want)
	}
	if got, want := names(single.Indirect), namesAtDepth(diff.Impacted, func(d int) bool { return d > 1 }); !equalStrings(got, want) {
		t.Errorf("Analyze Indirect = %v, but the diff path ranks its deeper symbols %v", got, want)
	}
}

func namesAtDepth(in []Impacted, keep func(int) bool) []string {
	out := make([]string, 0, len(in))
	for _, imp := range in {
		if keep(imp.Depth) {
			out = append(out, imp.Name)
		}
	}
	return out
}

// TestAnalyzeDiffStatesEachTruncationReasonOnce is the source-side half of the
// duplication fix.
//
// A diff report merges one truncation reason per seed. Concatenating them gave
// a 151-seed diff a ~700-byte reason that said the same thing 151 times, which
// every renderer then had to condense on its own -- and two condensations of
// the same field disagreed on screen. Aggregating here means one fact is
// counted once, and every surface, --json included, sees the same counts.
func TestAnalyzeDiffStatesEachTruncationReasonOnce(t *testing.T) {
	t.Parallel()
	// Ten changed files, each a hub whose dependents are left unexpanded by the
	// depth limit. Eight hubs are the same width, so eight walks produce the
	// IDENTICAL reason; two differ, so the aggregate must keep them apart
	// rather than fold everything into one count.
	widths := []int{5, 5, 5, 5, 5, 5, 5, 5, 7, 9}
	var syms []captest.Sym
	var edges []captest.Edge
	var changed []string
	id := int64(0)
	for h, w := range widths {
		id++
		hub := id
		path := fmt.Sprintf("svc/hub%d.go", h)
		syms = append(syms, captest.S(hub, fmt.Sprintf("hub%d", h), path))
		changed = append(changed, path)
		for c := 0; c < w; c++ {
			id++
			syms = append(syms, captest.S(id, fmt.Sprintf("caller%d_%d", h, c), fmt.Sprintf("svc/c%d_%d.go", h, c)))
			edges = append(edges, captest.E(id, hub))
		}
	}
	deps, runID := captest.Build(t, syms, edges, realisticRefs)

	rep, err := AnalyzeDiff(context.Background(), deps, runID, DiffRequest{
		Changes: &fakeChanges{changes: modified(changed...)},
		BaseRev: "HEAD~1", HeadRev: "HEAD",
		Options: Options{MaxDepth: 1, MaxNodes: 500},
	})
	if err != nil {
		t.Fatalf("AnalyzeDiff: %v", err)
	}
	if !rep.Truncated {
		t.Fatal("Truncated = false across ten hubs capped at 10 nodes")
	}
	if len(rep.Seeds) != 10 {
		t.Fatalf("Seeds = %d, want 10", len(rep.Seeds))
	}

	clauses := strings.Split(rep.TruncatedReason, "; ")
	seen := make(map[string]int, len(clauses))
	for _, c := range clauses {
		seen[stripMultiplier(c)]++
	}
	for c, n := range seen {
		if n > 1 {
			t.Errorf("reason clause %q appears %d times; it must appear once, with its multiplicity",
				c, n)
		}
	}
	if !strings.Contains(rep.TruncatedReason, "(x") && !strings.Contains(rep.TruncatedReason, "(×") {
		t.Errorf("ten walks hit the same bound but the reason states no multiplicity: %q",
			rep.TruncatedReason)
	}
	if got := len(rep.TruncatedReason); got > 400 {
		t.Errorf("aggregated reason is %d bytes; the point of aggregating is that it does not grow with the seed count:\n%s",
			got, rep.TruncatedReason)
	}
}

// stripMultiplicity removes a trailing " (xN)" so two clauses can be compared
// on what they SAY rather than on how many walks said it.
func stripMultiplier(s string) string {
	if i := strings.LastIndex(s, " (×"); i >= 0 && strings.HasSuffix(s, ")") {
		return s[:i]
	}
	return s
}
