package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// metricsScreenFixture is an index big enough that EVERY omission this screen
// can emit is emitted by some (level, filters) state, because a walk over the
// screen's disclosures can only check the disclosures a fixture produces.
//
// The shape, and what each part of it exists to produce:
//
//   - 24 directories x (one.go with an exported method and an unexported field,
//     two.go with an exported function, two_test.go with a test symbol) — more
//     than metricsTopN candidates at all three levels, so the row budget bites
//     everywhere; a non-callable kind and a test path, so the two reader
//     filters both remove something at symbol level.
//   - an edge inside one.go, and one from one.go to two.go: collapsed at
//     directory level and at file level respectively, so intra_group is real.
//   - a ring over the directories plus a reverse edge, so every level holds
//     short cycles and a small cycle budget is hit.
//   - an orphan symbol per directory, with no edge in either direction, so
//     isolated_node is non-zero at symbol level and folds away at the coarser
//     ones — which is what its remedy claims.
//   - one recursive edge, so self_loop is non-zero.
//   - two low-confidence edges, so the default medium floor hides something.
func metricsScreenFixture(t *testing.T) *Server {
	t.Helper()

	const dirs = 24
	var (
		syms  []captest.Sym
		edges []captest.Edge
	)
	id := int64(0)
	next := func() int64 { id++; return id }
	handle := make([]int64, dirs)
	run := make([]int64, dirs)

	for d := 0; d < dirs; d++ {
		dir := fmt.Sprintf("pkg/d%02d", d)
		h := next()
		syms = append(syms, captest.S(h, fmt.Sprintf("Handle%02d", d), dir+"/one.go").Export().WithKind("method"))
		f := next()
		syms = append(syms, captest.S(f, fmt.Sprintf("state%02d", d), dir+"/one.go").WithKind("field"))
		r := next()
		syms = append(syms, captest.S(r, fmt.Sprintf("Run%02d", d), dir+"/two.go").Export().WithKind("function"))
		x := next()
		syms = append(syms, captest.S(x, fmt.Sprintf("TestRun%02d", d), dir+"/two_test.go").Export().WithKind("function"))
		o := next()
		syms = append(syms, captest.S(o, fmt.Sprintf("orphan%02d", d), dir+"/one.go").WithKind("field"))
		handle[d], run[d] = h, r

		edges = append(edges,
			// Same file: collapsed at file AND directory level.
			captest.E(h, f).WithConf(model.ConfidenceHigh),
			// Same directory, two files: collapsed at directory level only.
			captest.E(h, r).WithConf(model.ConfidenceHigh),
			captest.E(x, r).WithConf(model.ConfidenceHigh),
			// Back the other way, so the two files of a directory form a cycle
			// the enumeration meets at file level too: a bound that is only ever
			// hit at one level leaves the other levels' disclosure unwalked.
			captest.E(r, h).WithConf(model.ConfidenceHigh),
		)
	}
	for d := 0; d < dirs; d++ {
		n := (d + 1) % dirs
		// A ring, and a return edge that makes every adjacent pair a 2-cycle at
		// every level: cycles exist for the enumeration to be bounded on.
		edges = append(edges,
			captest.E(handle[d], handle[n]).WithConf(model.ConfidenceHigh),
			captest.E(run[n], run[d]).WithConf(model.ConfidenceHigh),
		)
	}
	edges = append(edges,
		// Direct recursion: a self-loop at every level.
		captest.E(handle[0], handle[0]).WithConf(model.ConfidenceHigh),
		// Below the default medium floor.
		captest.E(run[2], handle[5]).WithConf(model.ConfidenceLow),
		captest.E(run[3], handle[6]).WithConf(model.ConfidenceLow),
	)

	deps, _ := captest.Build(t, syms, edges, captest.Refs{Resolved: 40, Unresolved: 8})
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	// A budget small enough to be hit: the disclosure is the half that ships
	// broken, and a budget no test reaches discloses nothing.
	return newServer(t, reg, deps, config.Config{ProjectName: "metrics-screen"},
		WithCycleBudget(metrics.CycleBudget{MaxLen: 4, MaxCycles: 2, MaxSteps: 200_000}))
}

// TestReadingPathRanksSomethingAtEveryLevel: the section was filtered on a
// metricsNode field only the symbol projection ever sets, so at file and
// directory level it ranked nothing — 0 rows of 963 and of 197 on the apm index
// — while its caveat said it was showing exported symbols. Exported is a
// property of a symbol; a directory does not have one.
func TestReadingPathRanksSomethingAtEveryLevel(t *testing.T) {
	t.Parallel()
	srv := metricsScreenFixture(t)

	for _, level := range []MetricsLevel{LevelSymbol, LevelFile, LevelDirectory} {
		var got MetricsResponse
		rec := get(t, srv, "/api/metrics?level="+string(level), &got)
		if rec.Code != http.StatusOK {
			t.Fatalf("level=%s: status %d: %s", level, rec.Code, rec.Body.String())
		}
		read := got.Sections[2]
		if read.Measure != "pagerank" {
			t.Fatalf("level=%s: third section measure = %q, want pagerank", level, read.Measure)
		}
		if len(read.Rows) == 0 {
			t.Errorf("level=%s: the reading-path section ranked nothing of %d considered candidate(s)",
				level, read.Considered)
		}
	}
}

// TestReadingPathCaveatDescribesWhatThisLevelActuallyDid: the caveat is the
// section's own statement of what the numbers under it mean, and it must be
// true at the level it is printed at — the standard every other sentence on
// this screen is held to.
func TestReadingPathCaveatDescribesWhatThisLevelActuallyDid(t *testing.T) {
	t.Parallel()
	srv := metricsScreenFixture(t)

	var sym MetricsResponse
	get(t, srv, "/api/metrics?level=symbol", &sym)
	if c := sym.Sections[2].Caveat; !strings.Contains(c, "exported") {
		t.Errorf("symbol-level caveat does not state the exported-only restriction: %q", c)
	}
	for _, level := range []MetricsLevel{LevelFile, LevelDirectory} {
		var got MetricsResponse
		get(t, srv, "/api/metrics?level="+string(level), &got)
		c := got.Sections[2].Caveat
		if !strings.Contains(c, "level=symbol") {
			t.Errorf("level=%s: caveat does not say the exported-only restriction is symbol-level only: %q", level, c)
		}
		// The symbol-level sentence, printed at a level that did not do it, is
		// the defect: it claims a restriction this level never applied.
		if c == sym.Sections[2].Caveat {
			t.Errorf("level=%s: caveat is the symbol-level sentence verbatim: %q", level, c)
		}
	}
}

// TestReaderFilterCountIsNotTheSectionsOwnRestriction: with both filters off,
// a row_filter omission is a count filed under a false label — it tells the
// reader to clear filters that are already clear, and on the apm index it
// mislabelled 7,948 rows at symbol level alone.
func TestReaderFilterCountIsNotTheSectionsOwnRestriction(t *testing.T) {
	t.Parallel()
	srv := metricsScreenFixture(t)

	for _, level := range []MetricsLevel{LevelSymbol, LevelFile, LevelDirectory} {
		var got MetricsResponse
		rec := get(t, srv, "/api/metrics?level="+string(level), &got)
		if rec.Code != http.StatusOK {
			t.Fatalf("level=%s: status %d", level, rec.Code)
		}
		for i, sec := range got.Sections {
			for _, om := range sec.Omissions.Omitted {
				if om.Reason == ReasonRowFilter {
					t.Errorf("level=%s section %d (%s): %d row(s) reported as removed by filters "+
						"while both filters are off: %+v", level, i, sec.Measure, om.Count, om)
				}
			}
		}
	}

	// And with a filter ON the count is real, so the assertion above is not
	// passing because nothing is ever counted.
	var filtered MetricsResponse
	get(t, srv, "/api/metrics?level=symbol&kinds=callable", &filtered)
	found := false
	for _, sec := range filtered.Sections {
		for _, om := range sec.Omissions.Omitted {
			if om.Reason == ReasonRowFilter && om.Count > 0 {
				found = true
			}
		}
	}
	if !found {
		t.Error("kinds=callable removed nothing on a fixture that carries non-callable symbols")
	}
}

// ---------------------------------------------------------- remedies work

/*
	TestEveryRemedyThisScreenOffersIsActionableInTheStateThatProducedIt

	This is a test about a CLASS of defect, not about three defects. Three times
	in this screen's history a disclosure named a remedy that could not work:

	  - intra_group told the reader to ask at level=symbol, which the handler had
	    just refused to answer and silently downgraded to level=file;
	  - a level downgrade told them to raise min_confidence on an index where
	    every symbol carries the same tier, so raising it removes everything or
	    nothing;
	  - row_filter told them to clear the kinds and exclude_tests filters while
	    both were already off.

	Each was found by reading one payload, and each was fixed on its own. What
	they have in common is that nothing checked the remedy against the STATE that
	produced it, and a remedy is exactly that kind of claim: doing this named
	thing changes this answer.

	So this walks every omission the screen can emit — top level and per section,
	all three levels, both filters on and off — and holds each one to that claim
	mechanically:

	  - A remedy naming a control is FOLLOWED. The state it advises must differ
	    from the state in hand (which is the third defect), the response must come
	    back at the control it was asked for rather than at some fallback (the
	    first), and the omission must be gone or strictly smaller (the second).
	  - An UNSIZED omission — one recorded with Note, whose count is 0 because
	    what it withholds is precisely what was not computed — has no count to
	    shrink, and the walk used to stop there. That left every Note-recorded
	    remedy checked only for being ANSWERED and never for having WORKED, which
	    is the same class as the three defects above: cycle_budget promised that
	    at a coarser level "the enumeration finishes" while MaxCycles and MaxSteps
	    are level-independent counts a dense repository exhausts at every level.
	    So an unsized remedy is now held to its own claim: one that promises
	    completion must actually complete, and every other one declares in
	    unsizedEffects what following it must measurably do.
	  - A remedy naming no control must SAY there is nothing to do, in words, and
	    must not name a control at all.
	  - A remedy naming an action outside this screen is declared per reason, in
	    the table below, and can only be checked as far as "it names no control
	    already set". That is the honest limit of a mechanical check and it is
	    written down rather than papered over with an assertion that passes.

	A reason absent from the table fails the test, so a new omission on this
	screen cannot ship without deciding how its remedy is checkable.
*/

// remedyClass is how a remedy can be held to its claim.
type remedyClass int

const (
	classFollow    remedyClass = iota // names a control this test can operate
	classNoOp                         // states, in words, that there is nothing to do
	classOffScreen                    // names an action outside this screen's controls
)

func (c remedyClass) String() string {
	switch c {
	case classFollow:
		return "follow"
	case classNoOp:
		return "no-op"
	default:
		return "off-screen"
	}
}

// remedyClassesByReason is the declared checkability of every omission this
// screen emits. A reason may allow more than one class when the honest remedy
// depends on the state — cycle_budget can be followed to a coarser level, and
// at the coarsest level there is nothing coarser to ask for.
var remedyClassesByReason = map[string][]remedyClass{
	ReasonRowBudget:            {classOffScreen},
	ReasonRowFilter:            {classFollow},
	ReasonSectionScope:         {classFollow},
	ReasonZeroScore:            {classNoOp},
	ReasonBelowConfidenceFloor: {classFollow},
	ReasonSampledBetweenness:   {classNoOp},
	ReasonSelfLoop:             {classOffScreen},
	ReasonIntraGroup:           {classFollow},
	ReasonCycleLengthBound:     {classNoOp},
	ReasonCycleBudget:          {classFollow, classNoOp},
	ReasonIsolatedNode:         {classFollow, classNoOp},
}

// noOpPhrases are the ways this screen is allowed to say "nothing to do". A
// fixed list, so a remedy cannot become a no-op by being vague.
var noOpPhrases = []string{"Nothing to do", "Nothing coarser", "None —"}

// offScreenPhrases are the actions outside this screen a remedy may name. Each
// is a real thing a reader can do and none of them is checkable from here,
// which is why the list is short and declared rather than pattern-matched.
var offScreenPhrases = []string{"repocortex export", "Read the symbol directly"}

// completionPhrases are the ways a remedy can promise that taking it finishes
// the operation the omission cut short. A promise is checkable and is checked:
// after following it, the reason must be gone.
var completionPhrases = []string{"finishes", "completes", "the complete set"}

func promisesCompletion(remedy string) bool {
	for _, p := range completionPhrases {
		if strings.Contains(remedy, p) {
			return true
		}
	}
	return false
}

// unsizedEffects is what following an unsized omission's remedy must measurably
// achieve, declared per reason for the same reason remedyClassesByReason is: a
// check nobody wrote down is a check that passes by accident.
var unsizedEffects = map[string]func(before, after MetricsResponse) string{
	// The cycle budget's remedy claims only that the coarser projection is
	// smaller, so that is what is measured. It deliberately does NOT claim the
	// enumeration finishes there — MaxCycles and MaxSteps are counts, not
	// proportions, and apm lists 154 cycles at file level and 199 at directory
	// level, so a densely cyclic repository exhausts them at every level.
	ReasonCycleBudget: func(before, after MetricsResponse) string {
		b, a := before.Sections[0].Considered, after.Sections[0].Considered
		if a >= b {
			return fmt.Sprintf("the remedy sends the reader to a projection it calls smaller, and it "+
				"holds %d candidate(s) against %d here", a, b)
		}
		return ""
	},
}

// metricsRequest is one state of the screen: the controls a reader is holding.
type metricsRequest struct {
	level MetricsLevel
	f     MetricsFilters
	floor model.Confidence
}

func (r metricsRequest) url() string {
	kinds := ""
	if r.f.Callable {
		kinds = "callable"
	}
	tests := "0"
	if r.f.ExcludeTests {
		tests = "1"
	}
	return fmt.Sprintf("/api/metrics?level=%s&kinds=%s&exclude_tests=%s&min_confidence=%s",
		r.level, kinds, tests, r.floor)
}

func (r metricsRequest) String() string {
	return fmt.Sprintf("level=%s callable=%t exclude_tests=%t floor=%s",
		r.level, r.f.Callable, r.f.ExcludeTests, r.floor)
}

// classifyRemedy reads a remedy for the controls it names and returns the state
// it advises. The parsing is deliberately literal: a remedy a reader is meant to
// act on has to name the control in the terms the API uses for it, and one this
// parser cannot read is one a reader has to guess at.
func classifyRemedy(remedy string, in metricsRequest) (remedyClass, metricsRequest, string) {
	advised := in
	named := false

	// Levels: the first one named is the one a reader would take.
	first, at := MetricsLevel(""), len(remedy)
	for _, l := range []MetricsLevel{LevelSymbol, LevelFile, LevelDirectory} {
		if i := strings.Index(remedy, "level="+string(l)); i >= 0 && i < at {
			first, at = l, i
		}
	}
	if first != "" {
		advised.level, named = first, true
	}
	switch {
	case strings.Contains(remedy, "Clear the kinds and exclude_tests filters"):
		advised.f, named = MetricsFilters{}, true
	case strings.Contains(remedy, "Clear the kinds filter"):
		advised.f.Callable, named = false, true
	case strings.Contains(remedy, "Clear the exclude_tests filter"):
		advised.f.ExcludeTests, named = false, true
	default:
		if strings.Contains(remedy, "kinds=callable") {
			advised.f.Callable, named = true, true
		}
		if strings.Contains(remedy, "exclude_tests=1") {
			advised.f.ExcludeTests, named = true, true
		}
	}
	if strings.Contains(remedy, "min_confidence") {
		advised.floor, named = model.ConfidenceLow, true
	}
	if named {
		return classFollow, advised, ""
	}
	for _, p := range noOpPhrases {
		if strings.Contains(remedy, p) {
			return classNoOp, advised, ""
		}
	}
	for _, p := range offScreenPhrases {
		if strings.Contains(remedy, p) {
			return classOffScreen, advised, ""
		}
	}
	return classFollow, advised, "names no control this screen has, no action off it, and does not say there is nothing to do"
}

func TestEveryRemedyThisScreenOffersIsActionableInTheStateThatProducedIt(t *testing.T) {
	t.Parallel()
	srv := metricsScreenFixture(t)

	seen := map[string]bool{}
	for _, level := range []MetricsLevel{LevelSymbol, LevelFile, LevelDirectory} {
		for _, f := range []MetricsFilters{
			{}, {Callable: true}, {ExcludeTests: true}, {Callable: true, ExcludeTests: true},
		} {
			state := metricsRequest{level: level, f: f, floor: DefaultFloor}
			var got MetricsResponse
			rec := get(t, srv, state.url(), &got)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d: %s", state, rec.Code, rec.Body.String())
			}
			for _, sc := range metricsScopes(got) {
				for _, om := range sc.om.Omitted {
					seen[om.Reason] = true
					checkRemedy(t, srv, state, got, sc, om)
				}
			}
		}
	}

	// A walk proves nothing about a disclosure it never met. These are the
	// omissions this fixture is built to provoke, and a silent one is a hole in
	// the walk rather than a clean run.
	for reason := range remedyClassesByReason {
		if !seen[reason] {
			t.Errorf("the walk never met %q, so nothing here checked its remedy", reason)
		}
	}
}

// metricsScope is one omission block and where it lives, so a follow-up
// response is read at the same place rather than anywhere it might match.
type metricsScope struct {
	name    string
	section int // -1 for the response-level block
	om      Omissions
}

func metricsScopes(r MetricsResponse) []metricsScope {
	out := []metricsScope{{name: "response", section: -1, om: r.Omissions}}
	for i, sec := range r.Sections {
		out = append(out, metricsScope{
			name: fmt.Sprintf("section %d (%s)", i, sec.Measure), section: i, om: sec.Omissions,
		})
	}
	return out
}

func (s metricsScope) in(r MetricsResponse) (Omissions, bool) {
	if s.section < 0 {
		return r.Omissions, true
	}
	if s.section >= len(r.Sections) {
		return Omissions{}, false
	}
	return r.Sections[s.section].Omissions, true
}

func checkRemedy(t *testing.T, srv *Server, state metricsRequest, before MetricsResponse, sc metricsScope, om Omission) {
	t.Helper()
	where := fmt.Sprintf("%s / %s / %s", state, sc.name, om.Reason)

	allowed, known := remedyClassesByReason[om.Reason]
	if !known {
		t.Errorf("%s: this screen emits an omission with no declared way to check its remedy. "+
			"Add it to remedyClassesByReason and say how a reader acts on it.", where)
		return
	}
	if om.Remedy == "" {
		t.Errorf("%s: omission carries no remedy at all: %+v", where, om)
		return
	}

	class, advised, problem := classifyRemedy(om.Remedy, state)
	if problem != "" {
		t.Errorf("%s: remedy %q %s", where, om.Remedy, problem)
		return
	}
	if !hasClass(allowed, class) {
		t.Errorf("%s: remedy %q reads as %s, which is not a way this reason is allowed to be checked",
			where, om.Remedy, class)
		return
	}

	switch class {
	case classNoOp, classOffScreen:
		// Both are unfollowable by construction; what CAN be checked is that
		// neither smuggles in a control, which is how "clear the filters you
		// have not set" would read if it were phrased as advice.
		if _, _, p := classifyRemedy(om.Remedy, state); p != "" {
			t.Errorf("%s: %v", where, p)
		}
		return
	}

	if advised == state {
		t.Errorf("%s: remedy %q advises the state the reader is already in (%s), so taking it changes nothing",
			where, om.Remedy, state)
		return
	}

	var after MetricsResponse
	rec := get(t, srv, advised.url(), &after)
	if rec.Code != http.StatusOK {
		t.Errorf("%s: following the remedy returned %d", where, rec.Code)
		return
	}
	// The response must come back at the control it was asked for. A handler
	// that answers a refused level at some fallback makes every remedy naming
	// that level a dead end, which is exactly how the first of the three
	// defects reached a reader.
	if after.Level != advised.level || after.Filters != advised.f || after.Floor != advised.floor {
		t.Errorf("%s: remedy %q asked for %s, response came back at level=%s callable=%t exclude_tests=%t floor=%s",
			where, om.Remedy, advised, after.Level, after.Filters.Callable, after.Filters.ExcludeTests, after.Floor)
		return
	}

	block, ok := sc.in(after)
	if !ok {
		return
	}
	if om.Count == 0 {
		checkUnsizedRemedyWorked(t, where, om, block, before, after)
		return
	}
	for _, still := range block.Omitted {
		if still.Reason != om.Reason {
			continue
		}
		if still.Count >= om.Count {
			t.Errorf("%s: remedy %q was taken and the omission did not shrink: %d -> %d",
				where, om.Remedy, om.Count, still.Count)
		}
		return
	}
}

// checkUnsizedRemedyWorked holds a Note-recorded omission's remedy to its own
// claim. There is no count to shrink, which is not the same as nothing to
// check: a remedy that promises the operation completes is checked for
// completion, and one that promises something narrower declares in
// unsizedEffects what following it must do.
func checkUnsizedRemedyWorked(
	t *testing.T, where string, om Omission, block Omissions, before, after MetricsResponse,
) {
	t.Helper()

	still := false
	for _, o := range block.Omitted {
		if o.Reason == om.Reason {
			still = true
		}
	}
	if promisesCompletion(om.Remedy) {
		if still {
			t.Errorf("%s: remedy %q promises the operation finishes there, and it is still cut short "+
				"at the state the remedy named", where, om.Remedy)
		}
		return
	}
	effect, ok := unsizedEffects[om.Reason]
	if !ok {
		t.Errorf("%s: this omission cannot be sized, so nothing here checks that its remedy WORKED. "+
			"Add it to unsizedEffects and say what following it must measurably do.", where)
		return
	}
	if !still {
		// The omission is gone, which is more than the declared effect asks for.
		return
	}
	if p := effect(before, after); p != "" {
		t.Errorf("%s: remedy %q was taken and %s", where, om.Remedy, p)
	}
}

func hasClass(in []remedyClass, c remedyClass) bool {
	for _, x := range in {
		if x == c {
			return true
		}
	}
	return false
}
