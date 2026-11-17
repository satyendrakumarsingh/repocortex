// Package tests answers "which tests cover this?" over the shared code
// knowledge graph.
//
// This is TestMind from the product spec, implemented — like impact — as a
// query layer rather than a separate product. It reads the same graph the same
// index produced.
//
// # Every finding records HOW it was found
//
// Test discovery combines three signals of very different strength, and an
// agent acting on the answer needs to know which one fired. A test found
// because a proven CALLS edge chain reaches the target is a fact. A test found
// because it happens to be named TestUpdateStatus is a convention that is
// usually right. A test found because it sits in the sibling test file is a
// guess about how the repository is organised. Collapsing all three into "3
// tests cover this" would let the third masquerade as the first, and the whole
// point of RepoCortex is that it does not do that.
//
// So every TestHit carries the full list of Signals that matched it, each with
// its own confidence and its own detail, plus Strongest for the common case of
// wanting one label.
//
// # Absence is a real answer
//
// A symbol with no discovered tests reports Untested, not the nearest
// vaguely-related test file. Returning something loosely related when nothing
// matched is how a coverage tool becomes worse than useless: it converts "we
// don't know" into "you're covered".
//
// This is also why co-location is defined narrowly — the sibling test file, not
// every test in the directory. See capability.IsSiblingTestFile for the
// reasoning.
//
// # Measured performance
//
// From bench_test.go on the same 100,000-symbol / 503,491-edge fixture
// internal/capability/impact uses (Apple M-series, -benchtime 50x), p95 for one
// complete FindTests:
//
//	all three signals, ordinary target        1.46 ms
//	  graph signal alone                      1.00 ms
//	  naming signal alone                     0.14 ms
//	  co-location signal alone                0.43 ms
//	all three signals, 1,000-caller hub      41.25 ms
//
// The naming signal is the cheapest despite issuing ten prefix probes per query,
// because SymbolIndex.WithPrefix binary-searches a sorted key set rather than
// scanning 100k map keys. The hub case is dominated by the graph traversal
// filtering a large frontier, not by the signals.
package tests

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Default bounds.
const (
	// DefaultMaxDepth is deeper than impact's default because test code
	// routinely reaches production code through one or two helper layers
	// (setup functions, table drivers, assertion helpers), and stopping at 3
	// would systematically miss the tests that matter most.
	DefaultMaxDepth = 4

	DefaultMaxNodes = 5000

	// maxNamingHits bounds how many symbols one naming probe may contribute, so
	// a target named `Get` cannot pull in every TestGet* in the repository.
	maxNamingHits = 50
)

// Signal is how a candidate test was discovered. The values are ordered by
// strength; Strongest picks the highest.
type Signal string

const (
	// SignalGraph: a test symbol transitively reaches the target through
	// recorded edges. The only signal that is evidence of execution rather than
	// evidence of intent.
	SignalGraph Signal = "graph"

	// SignalDispatch: a test reaches a method the target implements or
	// overrides (an interface method, a base-class method). The call is bound
	// to the declaration, so no edge leads to the implementation, yet the test
	// runs it whenever it holds this implementation. Which one it holds is
	// decided at run time, so this is weaker than a recorded call chain.
	SignalDispatch Signal = "interface_dispatch"

	// SignalNaming: the test's name follows a convention derived from the
	// target's name. Usually right, occasionally a coincidence, never proof.
	SignalNaming Signal = "naming_convention"

	// SignalColocation: the test lives in the sibling test file of the file
	// that defines the target. The weakest of the three: it says the repository
	// is organised conventionally, not that this test touches this symbol.
	SignalColocation Signal = "co_location"
)

func (s Signal) rank() int {
	switch s {
	case SignalGraph:
		return 4
	case SignalDispatch:
		return 3
	case SignalNaming:
		return 2
	case SignalColocation:
		return 1
	default:
		return 0
	}
}

// SignalMatch is one signal firing on one candidate, with its justification.
type SignalMatch struct {
	Signal Signal `json:"signal"`

	// Detail says precisely what matched — which edge chain, which naming
	// convention, which sibling file. Without it "naming_convention" is not
	// checkable by the reader.
	Detail string `json:"detail"`

	// Confidence is how strongly THIS SIGNAL supports the claim. For the graph
	// signal it is the path minimum from internal/graph, so it compounds along
	// the chain rather than reporting the last hop.
	Confidence model.Confidence `json:"confidence"`

	// Depth is hops from the target, for the graph signal only.
	Depth int `json:"depth,omitempty"`
}

// TestHit is one discovered test.
type TestHit struct {
	capability.Symbol

	// Kind is unit / integration / e2e where inferable from path and name, and
	// "unknown" where it is not. Guessing "unit" would be a claim.
	Kind capability.TestKind `json:"test_kind"`

	// Signals is every signal that matched, strongest first.
	Signals []SignalMatch `json:"signals"`

	// Strongest is Signals[0].Signal, hoisted for consumers that want one word.
	Strongest Signal `json:"strongest_signal"`

	// Confidence is the confidence of the strongest signal — never an average,
	// and never upgraded because several weak signals agreed. Three guesses
	// pointing the same way are still three guesses.
	Confidence model.Confidence `json:"confidence"`

	// Depth is how many calls lie between this test and the target on the
	// shortest recorded path: 1 calls it directly. Zero when no call path was
	// found (matched by name or by file only).
	Depth int `json:"depth,omitempty"`

	// Runnable is false for a helper in a test file that a test runner does
	// not run itself (see capability.IsRunnableTest). A helper is listed
	// apart: the tests that reach the target through it are what to run.
	Runnable bool `json:"runnable"`
}

// ErrorPath is a proven outgoing call from the target to a symbol that
// constructs or raises an error.
//
// Recorded during discovery so SuggestMissingScenarios can stay a pure function
// of facts already in the report. The CALLS edge is the evidence; the
// classification of the callee as error-constructing is a convention, and both
// halves are reported so the reader can judge the suggestion built on them.
type ErrorPath struct {
	Callee     capability.Symbol `json:"callee"`
	Confidence model.Confidence  `json:"confidence"`
	Evidence   model.Evidence    `json:"evidence"`
}

// TestReport is one test-discovery answer.
type TestReport struct {
	Target capability.Target `json:"target"`

	// Tests are the discovered tests, strongest signal first.
	Tests []TestHit `json:"tests"`

	// Untested is true when NO signal matched anything. Reported as a positive
	// finding rather than an empty list, because "we found nothing" and "we did
	// not look" must not render identically.
	Untested bool `json:"untested"`

	// SignalCounts is how many tests each signal contributed to, so a reader can
	// see at a glance whether the coverage claim rests on proven edges or on
	// filenames.
	SignalCounts map[Signal]int `json:"signal_counts"`

	// ProvenByGraph is the count of tests reached through recorded edges — the
	// only number here that is evidence of execution.
	ProvenByGraph int `json:"proven_by_graph"`

	// ErrorPaths are the target's proven calls into error construction. Input
	// to SuggestMissingScenarios.
	ErrorPaths []ErrorPath `json:"error_paths,omitempty"`

	Completeness capability.Completeness `json:"completeness"`

	// Binding is what the index did with the references spelled like the
	// target. A test reaches the target through one of those references, so
	// when they could not be bound, "no test found" says nothing about tests.
	Binding capability.TargetBinding `json:"binding"`

	// Undetermined is non-empty when the graph signal could not have found the
	// tests even if they exist, and says why. It qualifies Untested: Untested
	// with Undetermined set means "nothing was found, and the search could not
	// see", not "nothing covers this".
	Undetermined string `json:"undetermined,omitempty"`

	// ValueUse is the text check for a function passed as a value. A test that
	// reaches the target that way leaves no edge, so when it has a finding
	// "no test reaches this" is not something the report can say. Zero when the
	// check was not asked for.
	ValueUse capability.ValueUse `json:"value_use"`

	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncated_reason,omitempty"`
}

// Options bounds one discovery run.
type Options struct {
	MaxDepth      int
	MaxNodes      int
	MinConfidence model.Confidence

	// HubDegreeLimit refuses to expand THROUGH a symbol wider than this many
	// edges. Zero, the default, disables it.
	//
	// It matters more here than almost anywhere else: this walk is four hops
	// deep, and a test-helper or assertion utility with a thousand callers is
	// the normal shape of a test suite, not a pathology. Crossing one turns
	// "which tests cover this symbol" into "which tests exist".
	HubDegreeLimit int

	// SkipNaming and SkipColocation disable individual signals. They exist so
	// tests in this package can prove each signal finds its tests
	// INDEPENDENTLY, rather than proving only that the union finds them.
	SkipGraph      bool
	SkipNaming     bool
	SkipColocation bool

	// CheckValueUse asks for the text check described in capability.ValueUse.
	// Off by default for the same reason as impact.Options.CheckValueUse: it
	// reads stored text, and a caller probing many symbols does not pay for it
	// once per symbol.
	CheckValueUse bool
}

// ProbeOptions bounds a discovery run that only needs to know WHETHER tests
// exist near a symbol, not to enumerate them: two hops, a few hundred nodes,
// and no expansion through wide helpers.
//
// A whole-repository audit asks that question of dozens of symbols in one
// call. With the default bounds each answer walks four hops and up to 5,000
// nodes, which the scale suite measured at 5.5 seconds for list_blindspots
// on a 250,000-symbol index. The probe finds any test that calls the symbol
// or calls its caller, which is where almost every real test is.
//
// A probe that comes back empty AND truncated has not looked far enough to
// say "untested". Callers must treat that as unknown; see Inconclusive.
func ProbeOptions() Options {
	return Options{MaxDepth: 2, MaxNodes: 400, HubDegreeLimit: 64}
}

// Inconclusive reports whether r found no tests but stopped at a bound, so
// "no tests" is not something it can claim.
func (r TestReport) Inconclusive() bool {
	return len(r.Tests) == 0 && r.Truncated
}

func (o Options) withDefaults() Options {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = DefaultMaxNodes
	}
	return o
}

// graphKinds are the edges a test may reach its subject through. TESTED_BY is
// included because an analyzer that can assert the relationship directly
// produces a stronger fact than a call chain, and dropping it would discard the
// best evidence available.
var graphKinds = []model.EdgeKind{
	model.EdgeCalls,
	// A test that constructs a type reaches it; before constructor calls had a
	// kind of their own they were CALLS edges and were traversed as such.
	model.EdgeInstantiates,
	model.EdgeReferences,
	model.EdgeTestedBy,
}

// FindTests discovers the tests that cover target.
func FindTests(ctx context.Context, deps capability.Deps, runID int64, target string) (TestReport, error) {
	return FindTestsWith(ctx, deps, runID, target, Options{})
}

// FindTestsWith is FindTests under explicit bounds and signal selection.
func FindTestsWith(ctx context.Context, deps capability.Deps, runID int64, target string, opts Options) (TestReport, error) {
	if err := deps.Validate(); err != nil {
		return TestReport{}, err
	}
	opts = opts.withDefaults()

	tgt, err := capability.ResolveTarget(ctx, deps, runID, target)
	if err != nil {
		return TestReport{}, err
	}
	return findTestsFor(ctx, deps, runID, tgt, opts)
}

// FindTestsForTarget is FindTestsWith for a caller that has already chosen the
// definition, by file and line or any other means. See impact.AnalyzeTarget for
// why a name is not always enough to choose one.
func FindTestsForTarget(ctx context.Context, deps capability.Deps, runID int64, tgt capability.Target, opts Options) (TestReport, error) {
	if err := deps.Validate(); err != nil {
		return TestReport{}, err
	}
	if tgt.SymbolID == 0 {
		return TestReport{}, fmt.Errorf("tests: %w: the target carries no symbol id", capability.ErrTargetNotFound)
	}
	return findTestsFor(ctx, deps, runID, tgt, opts.withDefaults())
}

func findTestsFor(ctx context.Context, deps capability.Deps, runID int64, tgt capability.Target, opts Options) (TestReport, error) {
	idx, err := deps.SymbolIndex(ctx, runID)
	if err != nil {
		return TestReport{}, err
	}

	// acc merges signals per symbol. A test found by all three signals is ONE
	// test with three pieces of supporting evidence, not three tests — counting
	// it thrice would inflate exactly the number a reader uses to decide
	// whether a change is safe.
	acc := newAccumulator(tgt)

	var truncated bool
	var truncReason string
	var reached []int64

	if !opts.SkipGraph {
		res, err := deps.Graph.Traverse(ctx, graph.Query{
			Roots:          []int64{tgt.SymbolID},
			Kinds:          graphKinds,
			MinConfidence:  opts.MinConfidence,
			MaxDepth:       opts.MaxDepth,
			MaxNodes:       opts.MaxNodes,
			HubDegreeLimit: opts.HubDegreeLimit,
			// Incoming: a test CALLS the code under test, so the test is
			// upstream of the target.
			Direction: graph.Incoming,
		})
		if err != nil {
			return TestReport{}, fmt.Errorf("tests: traverse from %s: %w", tgt.FQN, err)
		}
		truncated, truncReason = res.Truncated, res.TruncatedReason
		for _, n := range res.Nodes {
			if n.Depth == 0 {
				continue
			}
			reached = append(reached, n.SymbolID)
			sym, ok := idx.Lookup(n.SymbolID)
			if !ok || !capability.IsTestSymbol(sym.Kind, sym.Name, sym.FilePath) {
				continue
			}
			acc.add(sym, SignalMatch{
				Signal: SignalGraph,
				Detail: fmt.Sprintf(
					"reaches %s through %d recorded edge(s), last hop %s; confidence is the weakest edge on that path",
					tgt.Name, n.Depth, orUnknown(string(n.Via))),
				Confidence: n.Confidence,
				Depth:      n.Depth,
			})
		}
	}

	if !opts.SkipGraph {
		dt, dreason, err := addDispatchHits(ctx, deps, idx, acc, tgt, opts)
		if err != nil {
			return TestReport{}, err
		}
		if dt && !truncated {
			truncated, truncReason = true, dreason
		}
	}

	if !opts.SkipNaming {
		addNamingHits(acc, idx, tgt)
	}
	if !opts.SkipColocation {
		addColocationHits(acc, idx, tgt)
	}

	errPaths, err := findErrorPaths(ctx, deps, idx, tgt)
	if err != nil {
		return TestReport{}, err
	}

	stats, err := deps.ResolutionStats(ctx, runID)
	if err != nil {
		return TestReport{}, err
	}

	rep := TestReport{
		Target:          tgt,
		Tests:           acc.finish(),
		ErrorPaths:      errPaths,
		Truncated:       truncated,
		TruncatedReason: truncReason,
		Completeness:    capability.NewCompleteness(stats, idx.Len(), truncated, truncReason),
	}
	rep.Untested = len(rep.Tests) == 0
	rep.SignalCounts = map[Signal]int{}
	for _, t := range rep.Tests {
		for _, s := range t.Signals {
			rep.SignalCounts[s.Signal]++
		}
		if t.Strongest == SignalGraph {
			rep.ProvenByGraph++
		}
	}
	rep.Binding, err = deps.TargetBinding(ctx, runID, tgt)
	if err != nil {
		return TestReport{}, err
	}
	if rep.Target.Language == "" {
		rep.Target.Language = rep.Binding.Language
	}
	// Only the graph signal is blinded by unbound references; a test found by
	// name or by sibling file is found either way. So the doubt is stated
	// whenever no test was PROVEN, and it is the whole finding when none was
	// found at all.
	if reason, ok := rep.Binding.Undetermined(); ok && rep.ProvenByGraph == 0 {
		rep.Undetermined = reason
		rep.Completeness.Caveats = append(rep.Completeness.Caveats,
			"Whether a test reaches "+tgt.Name+" through a call could not be determined: "+reason+".")
	}
	if opts.CheckValueUse {
		rep.ValueUse, err = deps.ValueUse(ctx, runID, tgt, idx, reached)
		if err != nil {
			return TestReport{}, err
		}
		if rep.ValueUse.Any() {
			rep.Completeness.Caveats = append(rep.Completeness.Caveats, rep.ValueUse.Lead()+".")
		}
	}
	if rep.Untested {
		rep.Completeness.Caveats = append(rep.Completeness.Caveats,
			fmt.Sprintf("No test was found for %s by any signal (call graph, naming convention, or sibling test file). "+
				"That means RepoCortex found no evidence of coverage — it does NOT mean the symbol is untested. "+
				"Tests invoked through table drivers, generated suites, another repository, or a runtime harness "+
				"leave no static trace.", tgt.Name))
	}
	return rep, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unrecorded"
	}
	return s
}

// addNamingHits applies the naming-convention signal.
//
// Candidates are generated from the target's name and looked up as PREFIXES, so
// TestUpdateStatus and TestUpdateStatus_NotFound both match with one binary
// search. What may follow the prefix is constrained (see
// capability.ConventionalTestSuffix) because an unconstrained prefix match
// would let target `Update` claim `TestUpdateStatus`.
func addNamingHits(acc *accumulator, idx *capability.SymbolIndex, tgt capability.Target) {
	for _, cand := range capability.TestNameCandidates(tgt.Name) {
		hits := idx.WithPrefix(cand, capability.ConventionalTestSuffix)
		if len(hits) > maxNamingHits {
			hits = hits[:maxNamingHits]
		}
		for _, sym := range hits {
			if sym.ID == tgt.SymbolID {
				continue
			}
			// A conventional name in a non-test file is a coincidence more
			// often than a test — a production helper called ParserSpec, say.
			// Requiring the file to look like test code as well costs almost no
			// recall and removes most of the false positives.
			if !capability.IsTestPath(sym.FilePath) && !capability.IsTestName(sym.Name) {
				continue
			}
			conf := model.ConfidenceLow
			detail := fmt.Sprintf("name %q matches the test-naming convention %q derived from %q, in test file %s",
				sym.Name, cand, tgt.Name, sym.FilePath)
			if sym.Name == cand && capability.IsTestPath(sym.FilePath) {
				// Exact conventional name in a test file. Still a convention,
				// but the strongest form of one.
				conf = model.ConfidenceMedium
				detail = fmt.Sprintf("name %q is exactly the test-naming convention for %q, in test file %s",
					sym.Name, tgt.Name, sym.FilePath)
			}
			acc.add(sym, SignalMatch{Signal: SignalNaming, Detail: detail, Confidence: conf})
		}
	}
}

// addColocationHits applies the co-location signal.
//
// Restricted to the SIBLING test file — same directory, stem corresponding to
// the target's file — rather than every test in the directory. Plain
// co-location asserts that all twenty tests beside a file cover that file,
// which is wrong nineteen times out of twenty and is precisely the "something
// loosely related" this package must not return.
func addColocationHits(acc *accumulator, idx *capability.SymbolIndex, tgt capability.Target) {
	if tgt.FilePath == "" {
		return
	}
	dir := tgt.FilePath
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i]
	} else {
		dir = "."
	}
	for _, sym := range idx.InDir(dir) {
		if sym.ID == tgt.SymbolID || !capability.IsSiblingTestFile(tgt.FilePath, sym.FilePath) {
			continue
		}
		if !capability.IsTestSymbol(sym.Kind, sym.Name, sym.FilePath) {
			continue
		}
		acc.add(sym, SignalMatch{
			Signal: SignalColocation,
			Detail: fmt.Sprintf("%s is the sibling test file of %s; this says the repository is organised "+
				"conventionally, not that this test exercises %s",
				sym.FilePath, tgt.FilePath, tgt.Name),
			Confidence: model.ConfidenceLow,
		})
	}
}

// errorConstructors are the conventional names for creating or raising an
// error, across the languages RepoCortex indexes.
//
// This list classifies a callee that a PROVEN CALLS EDGE already points at. It
// is never used on its own to decide anything — spec §23 forbids generating
// suggestions from keywords alone, and a name in this list with no edge behind
// it produces nothing.
var errorConstructors = []string{
	"New", "Errorf", "WithMessage", "WithStack", "Wrap", "Wrapf",
	"panic", "raise", "throw", "reject", "fail", "abort",
}

func isErrorConstructor(sym capability.Symbol) bool {
	name := sym.Name
	for _, c := range errorConstructors {
		if name == c {
			// Bare New/Wrap only counts when the FQN says it is about errors —
			// otherwise every constructor in the repository qualifies.
			if c == "New" || c == "Wrap" || c == "Wrapf" || c == "WithMessage" || c == "WithStack" {
				return strings.Contains(strings.ToLower(sym.FQN), "err")
			}
			return true
		}
	}
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, "newerr") || strings.HasPrefix(lower, "raise") ||
		strings.HasSuffix(name, "Error") && strings.HasPrefix(name, "New")
}

// findErrorPaths records the target's proven calls into error construction.
func findErrorPaths(
	ctx context.Context,
	deps capability.Deps,
	idx *capability.SymbolIndex,
	tgt capability.Target,
) ([]ErrorPath, error) {
	callees, err := deps.Graph.Neighbours(ctx, tgt.SymbolID, graph.Outgoing,
		[]model.EdgeKind{model.EdgeCalls}, "")
	if err != nil {
		return nil, fmt.Errorf("tests: callees of %s: %w", tgt.FQN, err)
	}
	var out []ErrorPath
	seen := make(map[int64]struct{}, len(callees))
	for _, n := range callees {
		if _, dup := seen[n.SymbolID]; dup {
			continue
		}
		sym, ok := idx.Lookup(n.SymbolID)
		if !ok || !isErrorConstructor(sym) {
			continue
		}
		seen[n.SymbolID] = struct{}{}
		out = append(out, ErrorPath{Callee: sym, Confidence: n.Confidence, Evidence: n.Evidence})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Callee.ID < out[j].Callee.ID })
	return out, nil
}

// accumulator merges signal matches per symbol.
type accumulator struct {
	target capability.Target
	order  []int64
	hits   map[int64]*TestHit
}

func newAccumulator(t capability.Target) *accumulator {
	return &accumulator{target: t, hits: map[int64]*TestHit{}}
}

func (a *accumulator) add(sym capability.Symbol, m SignalMatch) {
	if sym.ID == a.target.SymbolID {
		return
	}
	h, ok := a.hits[sym.ID]
	if !ok {
		h = &TestHit{
			Symbol:   sym,
			Kind:     capability.ClassifyTestKind(sym.FilePath, sym.Name),
			Runnable: capability.IsRunnableTest(sym.Kind, sym.Name, sym.FilePath),
		}
		a.hits[sym.ID] = h
		a.order = append(a.order, sym.ID)
	}
	if m.Signal == SignalGraph && m.Depth > 0 && (h.Depth == 0 || m.Depth < h.Depth) {
		h.Depth = m.Depth
	}
	for _, existing := range h.Signals {
		if existing.Signal == m.Signal {
			// Keep the strongest instance of a repeated signal (the shallowest,
			// highest-confidence path) rather than accumulating near-duplicates.
			if m.Confidence.Rank() > existing.Confidence.Rank() {
				a.replace(h, m)
			}
			return
		}
	}
	h.Signals = append(h.Signals, m)
}

func (a *accumulator) replace(h *TestHit, m SignalMatch) {
	for i := range h.Signals {
		if h.Signals[i].Signal == m.Signal {
			h.Signals[i] = m
			return
		}
	}
}

func (a *accumulator) finish() []TestHit {
	out := make([]TestHit, 0, len(a.order))
	for _, id := range a.order {
		h := a.hits[id]
		sort.SliceStable(h.Signals, func(i, j int) bool {
			if h.Signals[i].Signal.rank() != h.Signals[j].Signal.rank() {
				return h.Signals[i].Signal.rank() > h.Signals[j].Signal.rank()
			}
			return h.Signals[i].Confidence.Rank() > h.Signals[j].Confidence.Rank()
		})
		h.Strongest = h.Signals[0].Signal
		// The overall confidence is the strongest SIGNAL's confidence, not the
		// maximum across signals and not an average. Two weak signals agreeing
		// is not one strong signal.
		h.Confidence = h.Signals[0].Confidence
		out = append(out, *h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Strongest.rank() != out[j].Strongest.rank() {
			return out[i].Strongest.rank() > out[j].Strongest.rank()
		}
		// Nearest first: the test that calls the target directly is the one
		// to run first, and a list cut by a page keeps the closest.
		if di, dj := depthKey(out[i].Depth), depthKey(out[j].Depth); di != dj {
			return di < dj
		}
		if out[i].Confidence.Rank() != out[j].Confidence.Rank() {
			return out[i].Confidence.Rank() > out[j].Confidence.Rank()
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// depthKey orders a hit with no call path after every hit that has one.
func depthKey(d int) int {
	if d <= 0 {
		return 1 << 30
	}
	return d
}

// supertypeKinds are the edges from an implementation to what it implements.
var supertypeKinds = []model.EdgeKind{model.EdgeImplements, model.EdgeExtends}

// maxSupertypeHops bounds the climb from a class to its supertypes
// (SqlStore -> AbstractStore -> Store); a deeper hierarchy is rare and the
// walk is per target.
const maxSupertypeHops = 3

// declaration is a method the target implements or overrides.
type declaration struct {
	sym  capability.Symbol
	conf model.Confidence // the weakest edge on the way up
	why  string           // how it was found, for the detail line
}

// addDispatchHits finds tests that call a method the target implements.
//
// A test written against an interface calls Store.save; the call is bound to
// Store.save, and no edge leads from it to SqlStore.save, so a walk of the
// callers of SqlStore.save never meets the test. Measured on a four-file
// Java fixture: the only test of the implementation was invisible and the
// method was reported untested.
//
// The declarations come from two kinds of edge: a method-level IMPLEMENTS
// edge (Go records them: Walker.Root IMPLEMENTS Rooter.Root), or the class's
// IMPLEMENTS or EXTENDS edges and a method of the same name in the supertype
// (Java, Kotlin, C#, TypeScript record them on the type). The callers of each
// declaration are walked as the target's are, and a test found that way is
// at most medium when the declaration has another implementation: the test
// may hold that one instead.
func addDispatchHits(ctx context.Context, deps capability.Deps, idx *capability.SymbolIndex, acc *accumulator, tgt capability.Target, opts Options) (bool, string, error) {
	if tgt.Kind != string(model.KindMethod) && tgt.Kind != string(model.KindFunction) {
		return false, "", nil
	}
	decls, err := declarationsOf(ctx, deps, idx, tgt, opts.MinConfidence)
	if err != nil || len(decls) == 0 {
		return false, "", err
	}
	truncated, reason := false, ""
	for _, d := range decls {
		conf := d.conf
		impls, err := deps.Graph.Neighbours(ctx, d.sym.ID, graph.Incoming, supertypeKinds, opts.MinConfidence)
		if err != nil {
			return false, "", fmt.Errorf("tests: implementations of %s: %w", d.sym.FQN, err)
		}
		others := 0
		for _, n := range impls {
			if n.SymbolID != tgt.SymbolID {
				others++
			}
		}
		if owner, ok := ownerOf(idx, d.sym); ok && others == 0 {
			// Type-level edges: count the supertype's other subtypes.
			subs, err := deps.Graph.Neighbours(ctx, owner.ID, graph.Incoming, supertypeKinds, opts.MinConfidence)
			if err != nil {
				return false, "", fmt.Errorf("tests: subtypes of %s: %w", owner.FQN, err)
			}
			if len(subs) > 1 {
				others = len(subs) - 1
			}
		}
		if others > 0 && conf.Rank() > model.ConfidenceMedium.Rank() {
			conf = model.ConfidenceMedium
		}

		res, err := deps.Graph.Traverse(ctx, graph.Query{
			Roots:          []int64{d.sym.ID},
			Kinds:          graphKinds,
			MinConfidence:  opts.MinConfidence,
			MaxDepth:       max(opts.MaxDepth-1, 1),
			MaxNodes:       opts.MaxNodes,
			HubDegreeLimit: opts.HubDegreeLimit,
			Direction:      graph.Incoming,
		})
		if err != nil {
			return false, "", fmt.Errorf("tests: traverse from %s: %w", d.sym.FQN, err)
		}
		if res.Truncated && !truncated {
			truncated, reason = true, "dispatch through "+d.sym.Name+": "+res.TruncatedReason
		}
		for _, n := range res.Nodes {
			if n.Depth == 0 {
				continue
			}
			sym, ok := idx.Lookup(n.SymbolID)
			if !ok || !capability.IsTestSymbol(sym.Kind, sym.Name, sym.FilePath) {
				continue
			}
			c := n.Confidence
			if conf.Rank() < c.Rank() {
				c = conf
			}
			detail := fmt.Sprintf("reaches %s through %d recorded edge(s); %s %s, so the call runs %s when the test holds this implementation",
				qualifiedName(d.sym), n.Depth, tgt.Name, d.why, tgt.Name)
			if others > 0 {
				detail += fmt.Sprintf(" (%d other implementation(s) exist)", others)
			}
			acc.add(sym, SignalMatch{Signal: SignalDispatch, Detail: detail, Confidence: c, Depth: n.Depth + 1})
		}
	}
	return truncated, reason, nil
}

// declarationsOf returns the methods the target implements or overrides.
func declarationsOf(ctx context.Context, deps capability.Deps, idx *capability.SymbolIndex, tgt capability.Target, minConf model.Confidence) ([]declaration, error) {
	var out []declaration
	seen := map[int64]bool{tgt.SymbolID: true}
	addDecl := func(sym capability.Symbol, conf model.Confidence, why string) {
		if seen[sym.ID] {
			return
		}
		seen[sym.ID] = true
		out = append(out, declaration{sym: sym, conf: conf, why: why})
	}

	direct, err := deps.Graph.Neighbours(ctx, tgt.SymbolID, graph.Outgoing, supertypeKinds, minConf)
	if err != nil {
		return nil, fmt.Errorf("tests: what %s implements: %w", tgt.FQN, err)
	}
	for _, n := range direct {
		if sym, ok := idx.Lookup(n.SymbolID); ok && sym.Kind == string(model.KindMethod) {
			addDecl(sym, n.Confidence, verbPhrase(n.Kind)+" "+qualifiedName(sym))
		}
	}

	owner, ok := ownerOf(idx, capability.Symbol{ID: tgt.SymbolID, Name: tgt.Name, FQN: tgt.FQN})
	if !ok {
		return out, nil
	}
	type step struct {
		id   int64
		conf model.Confidence
	}
	frontier := []step{{owner.ID, model.ConfidenceExact}}
	visited := map[int64]bool{owner.ID: true}
	for hop := 0; hop < maxSupertypeHops && len(frontier) > 0; hop++ {
		var next []step
		for _, st := range frontier {
			ups, err := deps.Graph.Neighbours(ctx, st.id, graph.Outgoing, supertypeKinds, minConf)
			if err != nil {
				return nil, fmt.Errorf("tests: supertypes of %s: %w", owner.FQN, err)
			}
			for _, n := range ups {
				if visited[n.SymbolID] {
					continue
				}
				visited[n.SymbolID] = true
				conf := n.Confidence
				if st.conf.Rank() < conf.Rank() {
					conf = st.conf
				}
				next = append(next, step{n.SymbolID, conf})
				super, ok := idx.Lookup(n.SymbolID)
				if !ok {
					continue
				}
				if m, ok := idx.ByFQN(super.FQN + "." + tgt.Name); ok {
					addDecl(m, conf, fmt.Sprintf("is in %s, which %s %s", owner.Name, verbPhrase(n.Kind), super.Name))
				}
			}
		}
		frontier = next
	}
	return out, nil
}

// ownerOf is the type a method is declared in, read from its qualified name
// ("path:Class.method" -> "path:Class").
func ownerOf(idx *capability.SymbolIndex, s capability.Symbol) (capability.Symbol, bool) {
	file, qual, ok := strings.Cut(s.FQN, ":")
	if !ok {
		return capability.Symbol{}, false
	}
	i := strings.LastIndexByte(qual, '.')
	if i <= 0 {
		return capability.Symbol{}, false
	}
	return idx.ByFQN(file + ":" + qual[:i])
}

// qualifiedName is a symbol's name with its type ("Store.save"), as a reader
// would write it.
func qualifiedName(s capability.Symbol) string {
	if _, qual, ok := strings.Cut(s.FQN, ":"); ok && qual != "" {
		return qual
	}
	return s.Name
}

func verbPhrase(k model.EdgeKind) string {
	if k == model.EdgeExtends {
		return "extends"
	}
	return "implements"
}
