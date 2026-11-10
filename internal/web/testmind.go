package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestMind: which tests this repository has, and which symbols the GRAPH proves
// a test reaches.
//
// # The distinction this whole screen exists to preserve
//
// "No covering test was proven" and "this symbol has no tests" are different
// claims about different things. The first is a statement about THIS INDEX —
// what it could see, at what confidence, within the bounds it applied. The
// second is a statement about the CODE. Conflating them is how a coverage tool
// becomes worse than useless: it converts "we don't know" into "you're
// covered", or into "you're not", and both are lies of the same kind.
//
// So nothing in this payload is ever called "untested". Every symbol carries a
// `coverage` verdict from a closed vocabulary in which no value asserts absence
// of tests, and every verdict carries the sentence that says what it does and
// does not establish. The vocabulary is served as data (see CoverageVerdicts)
// for the same reason the confidence legend is: a client must not be able to
// invent a fourth meaning.
//
// # Nothing here is a second test-discovery implementation
//
// The per-symbol answer is capability/tests — FindTestsWith, the same call
// behind blast radius and `cortex tests` — with its three signals reported
// separately so a filename guess can never be counted as proof. The
// repository-level answer is one linear scan over the run's own CALLS,
// REFERENCES and TESTED_BY edges from test symbols, which is the depth-1
// projection of the same graph the traversal walks. It is labelled as depth-1
// in the response, because a direct edge and a four-hop chain are different
// evidence and the screen that hid the difference would be back to guessing.

// Coverage verdicts. A closed vocabulary, in which NO value says "this symbol
// has no tests" — see the file comment.
const (
	// CoverageProven: a test symbol reaches this symbol through a recorded
	// edge. Evidence of execution, not of intent.
	CoverageProven = "covering_test_proven"

	// CoverageNotProven: no test reaches it through a recorded edge at or above
	// the floor, within the bounds applied. A statement about this index.
	CoverageNotProven = "no_covering_test_proven"

	// CoverageSuggestedOnly: something was found, but only by naming convention
	// or by sibling-file co-location. A convention that is usually right is not
	// a proof, and merging it into CoverageProven is the failure mode.
	CoverageSuggestedOnly = "suggested_by_convention_only"
)

// CoverageVerdict is one verdict with the sentence that bounds it.
type CoverageVerdict struct {
	Verdict string `json:"verdict"`

	// Means is what the verdict establishes.
	Means string `json:"means"`

	// DoesNotMean is what it must never be read as. Present on every verdict,
	// including the positive one.
	DoesNotMean string `json:"does_not_mean"`
}

// CoverageVerdicts is the vocabulary, served as data so the client cannot
// invent a fourth meaning.
func CoverageVerdicts() []CoverageVerdict {
	return []CoverageVerdict{
		{
			Verdict: CoverageProven,
			Means: "A test symbol reaches this symbol through edges this index recorded. That is " +
				"evidence of execution rather than evidence of intent.",
			DoesNotMean: "It does not mean the test ASSERTS anything about this symbol, and it does " +
				"not mean the coverage is adequate. It means the call path exists.",
		},
		{
			Verdict: CoverageNotProven,
			Means: "No test reached this symbol through a recorded edge at or above the standard " +
				"of proof, within the depth and node bounds stated in `filters`. This is a statement " +
				"about what THIS INDEX could see.",
			DoesNotMean: "It does NOT mean the symbol is untested. A test invoked through a table " +
				"driver, a generated suite, a runtime harness, another repository, or a language " +
				"this index parses without binding references would not appear — and neither would " +
				"one reached only through an edge below the standard of proof, which is counted under what this view can't see.",
		},
		{
			Verdict: CoverageSuggestedOnly,
			Means: "A test was found by naming convention or by sibling-file co-location, and by no " +
				"recorded edge. Usually right; never proof.",
			DoesNotMean: "It does not mean the test exercises this symbol. `TestUpdateStatus` next " +
				"to `UpdateStatus` is a convention about how the repository is organised, not an " +
				"observation about what runs.",
		},
	}
}

// ProvenTest is one test symbol proved to reach a subject by a recorded edge.
type ProvenTest struct {
	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	Path     string `json:"file_path,omitempty"`

	// TestKind is unit / integration / e2e where inferable from path and name,
	// and "unknown" where it is not. Guessing "unit" would be a claim.
	TestKind string `json:"test_kind"`

	// Via is the edge kind — CALLS, REFERENCES or TESTED_BY. TESTED_BY is a
	// stronger fact than a call chain because an analyzer asserted it directly.
	Via string `json:"via"`

	Confidence string   `json:"confidence"`
	Evidence   Evidence `json:"evidence"`
}

// CoveredSymbol is one symbol and the tests the graph proves reach it.
type CoveredSymbol struct {
	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	FQN      string `json:"fqn"`
	Kind     string `json:"kind"`
	Path     string `json:"file_path,omitempty"`

	// Coverage is always CoverageProven on this list — the list exists to make
	// the positive claim. The field is carried anyway so a client renders one
	// vocabulary everywhere rather than inferring the verdict from which array
	// a row arrived in.
	Coverage string `json:"coverage"`

	// Confidence is the WEAKEST tier among the proving edges. A symbol reached
	// by one proven test and one guess is only as covered as the guess if the
	// guess is what links them.
	Confidence string `json:"confidence"`

	ProvenBy   []ProvenTest `json:"proven_by"`
	ProvenTest int          `json:"proven_test_count"`
}

// TestFileRow is one file this index recognises as a test file.
type TestFileRow struct {
	Path     string `json:"file_path"`
	Language string `json:"language,omitempty"`

	// TestKind is inferred from the path, and is "unknown" when the path gave
	// no reliable signal.
	TestKind string `json:"test_kind"`

	// Symbols is how many symbols this index extracted from the file.
	// TestSymbols is how many of those NAME themselves as tests (Test*, test_*,
	// *Spec, and the equivalents). Both, because the pair is what makes a gap
	// visible: a recognised test file that yielded no symbol at all, or one
	// whose symbols the naming convention does not recognise, is a parser or
	// convention gap rather than an empty file — and a single number could not
	// tell those apart.
	//
	// Note that this file is counted as a test file by its PATH, not by these
	// names. See testCoverage for why the proven claim never rests on a name.
	Symbols     int `json:"symbols"`
	TestSymbols int `json:"test_symbols"`

	// ReachesSymbols is how many non-test symbols this file's tests reach
	// through a recorded edge at or above the floor.
	ReachesSymbols int `json:"reaches_symbols"`
}

// SymbolCoverage is the per-symbol answer, from capability/tests.
//
// Deliberately NOT blast.go's TestSection: that type carries `untested`, which
// is the exact word this screen refuses to publish. The underlying report's
// boolean is honest in its own vocabulary — capability/tests documents it as
// "no signal matched" and appends a caveat saying so — but a field literally
// named `untested` on a coverage screen will be rendered as a verdict about the
// code sooner or later, and the failure would be silent.
type SymbolCoverage struct {
	Target *capability.Target `json:"target,omitempty"`

	// Verdict is one of the CoverageVerdicts values.
	Verdict string `json:"coverage"`

	// VerdictDetail is the sentence for this specific target, naming the bounds
	// that were applied. Not a lookup into the vocabulary: it carries the
	// symbol's name and the floor, so a reader who copies one line out of this
	// payload copies a complete claim.
	VerdictDetail string `json:"coverage_detail"`

	// Proven is the tests reached through recorded edges. Suggested is the
	// tests found only by naming convention or co-location. Two fields, never
	// one list with a signal column: the whole point is that they are different
	// evidence, and a client that renders only Proven is still correct.
	Proven    []tests.TestHit `json:"proven_by_graph"`
	Suggested []tests.TestHit `json:"suggested_by_convention"`

	ProvenCount    int            `json:"proven_count"`
	SuggestedCount int            `json:"suggested_count"`
	SignalCounts   map[string]int `json:"signal_counts"`

	// EdgesFromUnrecognisedTests is how many of Suggested are there because a
	// recorded edge reaches the target from a symbol that is NOT in a test file
	// and that the parser did not type as a test — it merely carries a
	// test-looking name. The edge is real; what is unproven is that its source
	// is a test. Counted rather than silently reclassified, because the reader
	// is entitled to know why a graph hit is not in the proven column.
	EdgesFromUnrecognisedTests int `json:"graph_edges_from_unrecognised_tests"`

	Completeness capability.Completeness `json:"completeness"`
	Truncated    bool                    `json:"truncated"`
}

// TestCounts is the headline arithmetic.
type TestCounts struct {
	// TestFiles is files recognised by the PATH convention (or holding a symbol
	// the parser itself typed as a test). TestSymbols is every symbol inside
	// one — the population excluded from NonTestSymbols below, so the two are
	// complements over the whole index and cannot both be wrong in the same
	// direction.
	TestFiles   int `json:"test_files"`
	TestSymbols int `json:"test_symbols"`

	// NonTestSymbols is the denominator. Stated, because "412 symbols have a
	// proven covering test" is meaningless without it.
	NonTestSymbols int `json:"non_test_symbols"`

	SymbolsWithProvenTest int `json:"symbols_with_a_proven_covering_test"`

	// SymbolsWithoutProvenTest is NonTestSymbols minus the above. Named for
	// what it is — the absence of a PROOF — and never for what it is not.
	SymbolsWithoutProvenTest int `json:"symbols_with_no_proven_covering_test"`

	ProvenEdges int `json:"proving_edges"`
}

// TestFilters echoes the bounds actually applied.
type TestFilters struct {
	MinConfidence string `json:"min_confidence"`

	// Depth is the traversal depth for the per-symbol answer. The
	// repository-level lists are depth 1 by construction — see RepoScope.
	MaxDepth int `json:"max_depth"`

	MaxTestsPerSymbol int `json:"max_tests_per_symbol"`
}

// TestsResponse is the whole TestMind screen.
type TestsResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	// Verdicts is the coverage vocabulary, served as data.
	Verdicts []CoverageVerdict `json:"coverage_legend"`

	Filters TestFilters `json:"filters"`
	Query   string      `json:"query"`

	Files   []TestFileRow   `json:"test_files"`
	Covered []CoveredSymbol `json:"covered_symbols"`

	// RepoScope states exactly what the two lists above are and are not.
	RepoScope string `json:"scope"`

	// Symbol is the deep answer for ?symbol=, from capability/tests. Absent
	// when no symbol was asked for.
	Symbol *SymbolCoverage `json:"symbol,omitempty"`

	// Candidates is populated when a bare name matched several definitions.
	// The portal will not pick one: a coverage answer about the wrong `Close`
	// is not a partial answer, it is a wrong one.
	Candidates []Candidate `json:"candidates,omitempty"`

	Counts TestCounts `json:"counts"`

	Help *Guidance  `json:"guidance,omitempty"`
	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

const (
	// maxTestsPerSymbol bounds one symbol's proving-test list.
	maxTestsPerSymbol = 12

	testRepoScope = "`test_files` is every file this index recognises as a test file BY ITS PATH — a " +
		"`_test.go` suffix, a `.spec.ts` name, a `tests/` or `__tests__/` directory — or that holds a " +
		"symbol the parser itself typed as a test. A test-looking NAME alone does not qualify a symbol " +
		"here, because everything below is labelled proven and a naming convention is not a proof. " +
		"`covered_symbols` " +
		"lists only symbols a test reaches through a DIRECT recorded edge (depth 1) at or above the " +
		"standard of proof. A symbol absent from that list is a symbol no direct edge proved covered — " +
		"it is NOT a symbol without tests, and it may still be reached through a longer chain: ask " +
		"?symbol=<name> for the full traversal from capability/tests."
)

// recognisedTest is THE definition of "this symbol is a test", for every claim
// this screen labels proven.
//
// Kind or path. Never the name, and that is the whole point of the function
// existing rather than the check being inlined twice. capability.IsTestSymbol
// also accepts a test-looking NAME, which is correct where it is used —
// capability/tests reports a name match as SignalNaming and never as proof —
// and wrong the moment the result is called proven. On a real index the wide
// predicate classified `extractImportSpec` (a production function in
// go-extractor.ts, caught by the "Spec" suffix rule) as a test, and a call it
// makes was published as PROVEN COVERAGE of the callee.
//
// Both the repository-level scan and the per-symbol answer run every candidate
// through this, so the two cannot report different numbers of proven tests for
// the same index — which they did, before it existed.
func recognisedTest(kind, filePath string) bool {
	return kind == string(model.KindTest) || capability.IsTestPath(filePath)
}

// testGraphKinds are the edges a test may reach its subject through — the same
// three capability/tests traverses, restated here because this scan is the
// depth-1 projection of that traversal and the two must not diverge.
var testGraphKinds = map[model.EdgeKind]struct{}{
	model.EdgeCalls:      {},
	model.EdgeReferences: {},
	model.EdgeTestedBy:   {},
}

func (s *Server) handleTests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("symbol"))
	floor := confidenceParam(r, "min_confidence", DefaultFloor)
	depth := intParam(r, "depth", tests.DefaultMaxDepth, 1, 6)

	resp := TestsResponse{
		Meta: meta, Legend: Legend(), Verdicts: CoverageVerdicts(), Query: query,
		Filters: TestFilters{
			MinConfidence: string(floor), MaxDepth: depth,
			MaxTestsPerSymbol: maxTestsPerSymbol,
		},
		Files: []TestFileRow{}, Covered: []CoveredSymbol{}, RepoScope: testRepoScope,
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label: "Index this repository", Command: "cortex index",
			Detail: "Test files are recognised during indexing; there is nothing committed to read.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var om Omissions
	files, covered, counts, hiddenEdges := idx.testCoverage(floor)
	resp.Counts = counts

	resp.Files = capped(files, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Test files beyond the first %d (ordered by test symbols, then path) are not "+
			"listed. counts.test_files is the full figure.", maxRows), "")
	resp.Covered = capped(covered, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Covered symbols beyond the first %d (ordered by proving tests, then name) are not "+
			"listed. counts.symbols_with_a_proven_covering_test is the full figure — and a symbol "+
			"missing from this page is NOT a symbol without a proven test.", maxRows),
		"ask ?symbol=<name> for one symbol's full answer")
	if resp.Files == nil {
		resp.Files = []TestFileRow{}
	}
	if resp.Covered == nil {
		resp.Covered = []CoveredSymbol{}
	}

	om.Hide(ReasonBelowConfidenceFloor, hiddenEdges,
		fmt.Sprintf("CALLS / REFERENCES / TESTED_BY relationships from a test symbol recorded weaker "+
			"than %s. Each is a coverage claim this screen is not making, so a symbol listed as having "+
			"no proven covering test may be reached through one of them.", tierWord(floor)),
		"lower the standard of proof to `low`")

	if query != "" {
		cov, cands, guidance, err := s.symbolCoverage(ctx, run.ID, query, floor, depth, &om)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		resp.Symbol = cov
		resp.Candidates = cands
		if guidance != nil {
			resp.Help = guidance
		}
	}

	if resp.Help == nil {
		resp.Help = testGuidance(counts, query)
	}
	resp.Next = testNextAction(counts, floor)

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// symbolCoverage answers one symbol through capability/tests.
//
// The report's own Untested boolean is deliberately not forwarded; the verdict
// is derived from which SIGNALS fired, so a naming-convention hit can never be
// presented as proof and an empty result can never be presented as absence of
// tests. See SymbolCoverage's comment.
func (s *Server) symbolCoverage(ctx context.Context, runID int64, query string,
	floor model.Confidence, depth int, om *Omissions) (*SymbolCoverage, []Candidate, *Guidance, error) {

	rep, err := tests.FindTestsWith(ctx, s.deps.Caps, runID, query, tests.Options{
		MaxDepth: depth, MinConfidence: floor,
	})
	if err != nil {
		var amb *capability.AmbiguousTargetError
		switch {
		case errors.As(err, &amb):
			return nil, candidatesOf(amb.Candidates), &Guidance{
				Title: fmt.Sprintf("%q matches %d definitions", query, len(amb.Candidates)),
				Detail: "The portal will not pick one. A coverage answer about the wrong symbol is not " +
					"a partial answer, it is a wrong one.",
				Steps: []string{"Choose a candidate below, or re-ask with its fully-qualified name."},
			}, nil
		case errors.Is(err, capability.ErrTargetNotFound):
			return nil, nil, &Guidance{
				Title:  fmt.Sprintf("No symbol named %q in this index", query),
				Detail: "That is a normal outcome — a typo, or a symbol this index could not extract from its language.",
				Steps: []string{
					"Check the spelling, and try the fully-qualified name.",
					"Check the overview's blind spots: a language with no extracted symbols of that kind will never match.",
				},
			}, nil
		default:
			return nil, nil, nil, fmt.Errorf("web: covering tests for %q: %w", query, err)
		}
	}

	target := rep.Target
	cov := &SymbolCoverage{
		Target: &target, Proven: []tests.TestHit{}, Suggested: []tests.TestHit{},
		SignalCounts: map[string]int{},
		Completeness: rep.Completeness, Truncated: rep.Truncated,
	}
	for k, v := range rep.SignalCounts {
		cov.SignalCounts[string(k)] = v
	}
	for _, hit := range rep.Tests {
		// A graph edge is proof of a call. It is proof of COVERAGE only if the
		// thing at the other end is a test, and a symbol that merely looks like
		// one by name is not. Such a hit is not dropped — capability/tests found
		// a real edge — it is filed with the other conventions and counted, so
		// the reason it is not in the proven list is visible.
		if hit.Strongest == tests.SignalGraph && recognisedTest(hit.Symbol.Kind, hit.FilePath) {
			cov.Proven = append(cov.Proven, hit)
			continue
		}
		if hit.Strongest == tests.SignalGraph {
			cov.EdgesFromUnrecognisedTests++
		}
		cov.Suggested = append(cov.Suggested, hit)
	}
	cov.ProvenCount = len(cov.Proven)
	cov.SuggestedCount = len(cov.Suggested)

	cov.Proven = capped(cov.Proven, maxRows, om, ReasonRowBudget,
		fmt.Sprintf("Graph-proven covering tests for %s beyond the first %d are not listed.",
			target.Name, maxRows), "")
	cov.Suggested = capped(cov.Suggested, maxRows, om, ReasonRowBudget,
		fmt.Sprintf("Convention-suggested tests for %s beyond the first %d are not listed.",
			target.Name, maxRows), "")

	switch {
	case cov.ProvenCount > 0:
		cov.Verdict = CoverageProven
		cov.VerdictDetail = fmt.Sprintf(
			"%d test(s) reach %s through edges this index recorded at %s or better. That is evidence "+
				"the call path exists; it is not a statement about what those tests assert.",
			cov.ProvenCount, target.Name, tierWord(floor))
	case cov.SuggestedCount > 0:
		cov.Verdict = CoverageSuggestedOnly
		cov.VerdictDetail = fmt.Sprintf(
			"No recorded edge links a recognised test to %s at %s or better. %d candidate(s) were "+
				"found by naming convention, by the sibling test file, or by an edge whose source only "+
				"LOOKS like a test — usually right, never proof, and reported separately for exactly "+
				"that reason%s.",
			target.Name, tierWord(floor), cov.SuggestedCount, unrecognisedClause(cov.EdgesFromUnrecognisedTests))
	default:
		cov.Verdict = CoverageNotProven
		cov.VerdictDetail = fmt.Sprintf(
			"No covering test was PROVEN for %s: no signal matched — not the call graph at %s or "+
				"better within depth %d, not the naming convention, not the sibling test file. This "+
				"says what this index could see. It does NOT say %s is untested: a test invoked "+
				"through a table driver, a generated suite, another repository or a runtime harness "+
				"would not appear here, and neither would one reached only through an edge below the "+
				"standard of proof.",
			target.Name, tierWord(floor), depth, target.Name)
	}
	if rep.Truncated {
		om.Note(ReasonTraversalBound,
			"The test-discovery traversal for "+target.Name+" was truncated: "+rep.TruncatedReason+
				". A test beyond that boundary would not appear, and its absence is not evidence.",
			"raise ?depth= or narrow the query")
	}
	return cov, nil, nil, nil
}

// unrecognisedClause names the graph hits that did not qualify as proof, or
// nothing at all when there were none.
func unrecognisedClause(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d of them a recorded edge from a symbol outside any test file, which proves "+
		"a call and not a test)", n)
}

func testGuidance(counts TestCounts, query string) *Guidance {
	switch {
	case counts.TestFiles == 0:
		return &Guidance{
			Title: "This index recognised no test files",
			Detail: "Test files are recognised by path and naming convention (a `_test.go` suffix, a " +
				"`tests/` or `spec/` directory, and the equivalents in other ecosystems). None matched " +
				"here. That is a statement about what this index saw, not a claim that this repository " +
				"has no tests.",
			Steps: []string{
				"cortex index — if the test tree was excluded by .cortex.yml, it was never indexed",
				"Check the overview's blind spots: a language whose symbols were never extracted yields no test symbols either",
				"cortex tests <symbol> — the same discovery for one symbol, in the terminal",
			},
		}
	case counts.SymbolsWithProvenTest == 0:
		return &Guidance{
			Title: fmt.Sprintf("%d test file(s), and no symbol has a proven covering test", counts.TestFiles),
			Detail: "The tests are indexed, but no CALLS, REFERENCES or TESTED_BY edge links one of them " +
				"to a symbol at or above the standard of proof. That normally means references were " +
				"never bound, or the language is parsed without binding them — NOT that the tests " +
				"exercise nothing.",
			Steps: []string{
				"cortex resolve — binds references into the edges this screen reads",
				"cortex indexers status — a compiler-backed index turns relationships inferred from names into proven ones",
				"Lower the standard of proof to " + tierWord(model.ConfidenceLow) + ", to see what it is holding back",
			},
		}
	case query == "":
		return &Guidance{
			Title: "Ask about one symbol for the full answer",
			Detail: "The list above is the DIRECT (depth 1) projection. A test reaching a symbol through " +
				"two helper layers is real coverage and is not on it.",
			Steps: []string{
				"Add ?symbol=<name> to run the full traversal from capability/tests.",
				"A bare name matching several definitions returns the candidates rather than guessing.",
			},
		}
	default:
		return nil
	}
}

func testNextAction(counts TestCounts, floor model.Confidence) NextAction {
	switch {
	case counts.TestFiles == 0:
		return NextAction{
			Label:   "Confirm the test tree was indexed",
			Detail:  "An excluded or unparsed test tree produces this screen exactly as an untested repository would, and the two must not be confused.",
			Command: "cortex index",
			Screen:  "overview#blind-spots",
		}
	case counts.SymbolsWithProvenTest == 0:
		return NextAction{
			Label:   "Bind references so coverage can be proven",
			Detail:  fmt.Sprintf("No test reaches any symbol through an edge at %s or better.", tierWord(floor)),
			Command: "cortex resolve",
		}
	default:
		return NextAction{
			Label: "Check the blast radius before you change one of these",
			Detail: fmt.Sprintf("%d of %d non-test symbol(s) have a directly proven covering test. The "+
				"other %d are symbols this index did not PROVE covered — which is not the same as "+
				"symbols without tests.",
				counts.SymbolsWithProvenTest, counts.NonTestSymbols, counts.SymbolsWithoutProvenTest),
			Screen: "blast",
		}
	}
}

// testCoverage projects the run's test edges onto files and symbols.
//
// One linear scan over the cached compact edges. Depth 1 only, and labelled as
// such in the response: a direct edge from a test to a symbol is a fact the
// index recorded, while a chain is a traversal, and capability/tests owns
// traversals. Doing a second one here would be a second place for "reports
// depth-3 reachability as proven coverage" to be reintroduced.
//
// The returned int is how many qualifying edges fell below the floor.
func (idx *structuralIndex) testCoverage(floor model.Confidence) (
	[]TestFileRow, []CoveredSymbol, TestCounts, int) {

	var counts TestCounts

	// Which symbols are tests, and which files hold them.
	//
	// The predicate is deliberately NARROWER than capability.IsTestSymbol, which
	// also accepts a test-looking NAME. That is right for capability/tests,
	// where a name match is reported as SignalNaming and never as proof — and
	// wrong here, where the output of this scan is labelled proven. On a real
	// index the wide predicate classified `extractImportSpec` (a production
	// function in go-extractor.ts, matched only by the "Spec" suffix rule) as a
	// test, and this screen then published a call it makes as PROVEN COVERAGE
	// of the callee. A filename guess dressed as a graph proof is the single
	// thing this screen exists to prevent, so the proven claim rests on the
	// parser's own verdict or on the path, never on the name.
	isTest := make(map[int64]bool, len(idx.syms))
	symbolsPerFile := map[int64]int{}
	testFuncsPerFile := map[int64]int{}
	testFile := map[int64]bool{}
	for id, sym := range idx.syms {
		symbolsPerFile[sym.FileID]++
		path := idx.symbolPath(id)
		if sym.Kind == model.KindTest || capability.IsTestName(sym.Name) {
			testFuncsPerFile[sym.FileID]++
		}
		if recognisedTest(string(sym.Kind), path) {
			isTest[id] = true
			testFile[sym.FileID] = true
			counts.TestSymbols++
			continue
		}
		counts.NonTestSymbols++
	}

	type covAcc struct {
		row   CoveredSymbol
		tests []ProvenTest
		seen  map[int64]struct{}
	}
	covered := map[int64]*covAcc{}
	reachesPerFile := map[int64]map[int64]struct{}{}
	hidden := 0

	for i := range idx.edges {
		e := idx.edges[i]
		if _, ok := testGraphKinds[e.Kind]; !ok {
			continue
		}
		src, dst := e.Src, e.Dst
		if e.Kind == model.EdgeTestedBy {
			// TESTED_BY is recorded subject -> test, the opposite direction
			// from a call. Reading it as written would report the subject as
			// the test, which is the sort of inversion that produces a
			// confident, backwards answer.
			src, dst = dst, src
		}
		if !isTest[src] || isTest[dst] {
			continue
		}
		if _, ok := idx.syms[dst]; !ok {
			continue
		}
		if !e.Confidence.AtLeast(floor) {
			hidden++
			continue
		}

		testSym := idx.syms[src]
		testPath := idx.symbolPath(src)
		if set, ok := reachesPerFile[testSym.FileID]; ok {
			set[dst] = struct{}{}
		} else {
			reachesPerFile[testSym.FileID] = map[int64]struct{}{dst: {}}
		}

		a, ok := covered[dst]
		if !ok {
			sym := idx.syms[dst]
			name := sym.Name
			if name == "" {
				name = fmt.Sprintf("symbol#%d", dst)
			}
			a = &covAcc{
				row: CoveredSymbol{
					SymbolID: dst, Name: name, FQN: sym.FQN, Kind: string(sym.Kind),
					Path: idx.symbolPath(dst), Coverage: CoverageProven,
					Confidence: tierOf(e.Confidence),
				},
				seen: map[int64]struct{}{},
			}
			covered[dst] = a
		}
		// Weakest wins on the symbol's own tier: a symbol proved covered by one
		// exact edge and one medium guess is only as certain as the claim the
		// reader is looking at, and the conservative summary is the honest one.
		if model.Confidence(a.row.Confidence).Rank() > e.Confidence.Rank() {
			a.row.Confidence = tierOf(e.Confidence)
		}
		if _, dup := a.seen[src]; dup {
			continue
		}
		a.seen[src] = struct{}{}
		testName := testSym.Name
		if testName == "" {
			testName = fmt.Sprintf("symbol#%d", src)
		}
		a.tests = append(a.tests, ProvenTest{
			SymbolID: src, Name: testName, Path: testPath,
			TestKind:   string(capability.ClassifyTestKind(testPath, testName)),
			Via:        string(e.Kind),
			Confidence: tierOf(e.Confidence),
			Evidence:   Evidence{FilePath: e.File, Line: int(e.Line)},
		})
		counts.ProvenEdges++
	}

	out := make([]CoveredSymbol, 0, len(covered))
	for _, a := range covered {
		sort.Slice(a.tests, func(i, j int) bool {
			if a.tests[i].Name != a.tests[j].Name {
				return a.tests[i].Name < a.tests[j].Name
			}
			return a.tests[i].SymbolID < a.tests[j].SymbolID
		})
		a.row.ProvenTest = len(a.tests)
		if len(a.tests) > maxTestsPerSymbol {
			a.tests = a.tests[:maxTestsPerSymbol]
		}
		a.row.ProvenBy = a.tests
		out = append(out, a.row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProvenTest != out[j].ProvenTest {
			return out[i].ProvenTest > out[j].ProvenTest
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].SymbolID < out[j].SymbolID
	})

	// Every file the PATH convention recognises, whether or not the index
	// extracted a symbol from it: a test file that yielded nothing is a parser
	// gap, and omitting it would make that gap look like an absence of tests.
	for fileID, info := range idx.files {
		if capability.IsTestPath(info.Path) {
			testFile[fileID] = true
		}
	}
	files := make([]TestFileRow, 0, len(testFile))
	for fileID := range testFile {
		info := idx.files[fileID]
		files = append(files, TestFileRow{
			Path: info.Path, Language: info.Language,
			TestKind:       string(capability.ClassifyTestKind(info.Path, "")),
			Symbols:        symbolsPerFile[fileID],
			TestSymbols:    testFuncsPerFile[fileID],
			ReachesSymbols: len(reachesPerFile[fileID]),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].TestSymbols != files[j].TestSymbols {
			return files[i].TestSymbols > files[j].TestSymbols
		}
		return files[i].Path < files[j].Path
	})

	counts.TestFiles = len(files)
	counts.SymbolsWithProvenTest = len(out)
	counts.SymbolsWithoutProvenTest = counts.NonTestSymbols - counts.SymbolsWithProvenTest
	if counts.SymbolsWithoutProvenTest < 0 {
		counts.SymbolsWithoutProvenTest = 0
	}
	return files, out, counts, hidden
}
