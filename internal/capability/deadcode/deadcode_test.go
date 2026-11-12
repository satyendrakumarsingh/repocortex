package deadcode

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// fakeSource serves a fixed slice through the same paging contract the
// registry honours, so the scan's paging is exercised without SQLite.
type fakeSource struct {
	rows  []registry.UnreferencedSymbol
	calls int
	// usedAt maps a name to code lines, in pkg/elsewhere.go, where it is used.
	// A name absent from the map is used nowhere.
	usedAt map[string][]int
	// selfAt maps a name to lines in its own file, pkg/x.go, where it occurs.
	selfAt map[string][]int
	// unknown lists names the text index cannot answer for.
	unknown map[string]bool
}

func (f *fakeSource) IdentifierMentions(_ context.Context, _ int64, name string) ([]registry.Mention, bool, error) {
	if f.unknown[name] {
		return nil, false, nil
	}
	var out []registry.Mention
	if lines := f.selfAt[name]; len(lines) > 0 {
		out = append(out, registry.Mention{Path: "pkg/x.go", Lines: lines})
	}
	if lines := f.usedAt[name]; len(lines) > 0 {
		out = append(out, registry.Mention{Path: "pkg/elsewhere.go", Lines: lines})
	}
	return out, true, nil
}

func (f *fakeSource) UnreferencedSymbols(_ context.Context, _, afterID int64, limit int) ([]registry.UnreferencedSymbol, error) {
	f.calls++
	var out []registry.UnreferencedSymbol
	for _, r := range f.rows {
		if r.ID > afterID {
			out = append(out, r)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func row(id int64, name string) registry.UnreferencedSymbol {
	return registry.UnreferencedSymbol{
		ID: id, Kind: "function", Name: name, FilePath: "pkg/x.go", Language: "go",
		StartLine: int(id) * 10, EndLine: int(id)*10 + 4,
	}
}

// deps returns real capability deps over an empty committed run: Find reads
// resolution stats through them for its completeness block.
func deps(t *testing.T) (capability.Deps, int64) {
	t.Helper()
	return captest.Build(t, []captest.Sym{captest.S(1, "seed", "seed.go")}, nil, captest.Refs{})
}

func find(t *testing.T, src Source, opts Options) *Report {
	t.Helper()
	d, runID := deps(t)
	rep, err := Find(context.Background(), d, src, runID, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestGradingNamesEveryReasonACandidateIsOnlyPossible(t *testing.T) {
	clean := row(1, "helper")

	exported := row(2, "Public")
	exported.Exported = true

	unbound := row(3, "maybeUsed")
	unbound.NameUnbound = true

	override := row(4, "persist")
	override.Overrides = true

	partial := row(5, "inBrokenFile")
	partial.PartialParse = true

	rep := find(t, &fakeSource{rows: []registry.UnreferencedSymbol{clean, exported, unbound, override, partial}},
		Options{IncludeExported: true})

	by := map[string]Candidate{}
	for _, c := range rep.Candidates {
		by[c.Name] = c
	}
	if c := by["helper"]; c.Certainty != CertaintyLikely || len(c.Reasons) != 0 {
		t.Errorf("helper = %+v, want likely with no reasons", c)
	}
	for name, want := range map[string]Reason{
		"Public": ReasonExported, "maybeUsed": ReasonNameUnbound,
		"persist": ReasonOverrides, "inBrokenFile": ReasonPartialParse,
	} {
		c := by[name]
		if c.Certainty != CertaintyPossible {
			t.Errorf("%s: certainty %q, want possible", name, c.Certainty)
		}
		if len(c.Reasons) != 1 || c.Reasons[0] != want {
			t.Errorf("%s: reasons %v, want [%s]", name, c.Reasons, want)
		}
		if want.Text() == "" {
			t.Errorf("reason %q has no sentence", want)
		}
	}
	if rep.Likely != 1 || rep.Possible != 4 {
		t.Errorf("counts: likely %d possible %d, want 1 and 4", rep.Likely, rep.Possible)
	}
}

// The default hides exported definitions: a library's public API is
// "unreferenced" by construction, and leading with it buries the findings.
func TestExportedDefinitionsAreSkippedUnlessAsked(t *testing.T) {
	exported := row(1, "Public")
	exported.Exported = true
	src := &fakeSource{rows: []registry.UnreferencedSymbol{exported, row(2, "private")}}

	rep := find(t, src, Options{})
	if len(rep.Candidates) != 1 || rep.Candidates[0].Name != "private" {
		t.Errorf("default candidates = %v, want just private", candidateNames(rep))
	}
	if rep.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 so the reader sees what was left out", rep.Skipped)
	}
}

func TestExpectedUncalledSymbolsAreSkippedAndCounted(t *testing.T) {
	test := row(1, "TestSomething")
	test.FilePath = "pkg/x_test.go"
	inTestFile := row(2, "helper")
	inTestFile.FilePath = "pkg/x_test.go"

	src := &fakeSource{rows: []registry.UnreferencedSymbol{test, inTestFile, row(3, "main"), row(4, "init"), row(5, "real")}}
	rep := find(t, src, Options{})

	if got := candidateNames(rep); len(got) != 1 || got[0] != "real" {
		t.Errorf("candidates = %v, want [real]", got)
	}
	if rep.Skipped != 4 {
		t.Errorf("skipped = %d, want 4", rep.Skipped)
	}
}

// A language the index only parses has no cross-file references at all, so
// every definition in it would look dead. Those are a coverage limit, reported
// as one, never as findings.
func TestUnjudgeableLanguagesAreReportedNotListed(t *testing.T) {
	lua := row(1, "luaFn")
	lua.Language = "lua"
	src := &fakeSource{rows: []registry.UnreferencedSymbol{lua, row(2, "goFn")}}

	rep := find(t, src, Options{Judgeable: func(l string) bool { return l == "go" }})

	if got := candidateNames(rep); len(got) != 1 || got[0] != "goFn" {
		t.Errorf("candidates = %v, want [goFn]", got)
	}
	if rep.SkippedUnjudged != 1 || len(rep.UnjudgedLangs) != 1 || rep.UnjudgedLangs[0] != "lua" {
		t.Errorf("unjudged = %d %v, want 1 [lua]", rep.SkippedUnjudged, rep.UnjudgedLangs)
	}
}

func TestMinCertaintyAndPathPrefixFilter(t *testing.T) {
	unbound := row(1, "maybe")
	unbound.NameUnbound = true
	elsewhere := row(2, "other")
	elsewhere.FilePath = "cmd/main.go"
	src := &fakeSource{rows: []registry.UnreferencedSymbol{unbound, elsewhere, row(3, "sure")}}

	rep := find(t, src, Options{MinCertainty: CertaintyLikely, PathPrefix: "pkg/"})
	if got := candidateNames(rep); len(got) != 1 || got[0] != "sure" {
		t.Errorf("candidates = %v, want [sure]", got)
	}
	// The count still says a possible candidate exists under the prefix.
	if rep.Possible != 1 {
		t.Errorf("possible = %d, want 1: a filtered row is still counted", rep.Possible)
	}
}

// Likely before possible, then the longest first: the first row is the one
// most worth a reader's minute.
func TestOrderIsCertaintyThenSize(t *testing.T) {
	short := row(1, "short")
	short.StartLine, short.EndLine = 1, 2
	long := row(2, "long")
	long.StartLine, long.EndLine = 10, 200
	possibleHuge := row(3, "possibleHuge")
	possibleHuge.StartLine, possibleHuge.EndLine = 1, 5000
	possibleHuge.NameUnbound = true

	rep := find(t, &fakeSource{rows: []registry.UnreferencedSymbol{short, possibleHuge, long}}, Options{})
	got := strings.Join(candidateNames(rep), ",")
	if got != "long,short,possibleHuge" {
		t.Errorf("order = %s, want long,short,possibleHuge", got)
	}
}

// Memory must follow Limit, not the amount of dead code: the scan keeps the
// best Limit rows and still counts every one it examined.
func TestLimitKeepsTheBestAcrossPages(t *testing.T) {
	var rows []registry.UnreferencedSymbol
	for i := int64(1); i <= pageSize*2+50; i++ {
		r := row(i, fmt.Sprintf("fn%d", i))
		r.StartLine, r.EndLine = 1, int(i) // later rows are longer
		rows = append(rows, r)
	}
	src := &fakeSource{rows: rows}

	rep := find(t, src, Options{Limit: 5})
	if len(rep.Candidates) != 5 {
		t.Fatalf("returned %d candidates, want 5", len(rep.Candidates))
	}
	if rep.Likely != len(rows) {
		t.Errorf("likely = %d, want %d: the count covers what was examined, not what was returned", rep.Likely, len(rows))
	}
	// The five longest are the last five inserted, which only a scan that
	// read every page could know.
	if want := fmt.Sprintf("fn%d", len(rows)); rep.Candidates[0].Name != want {
		t.Errorf("first candidate = %s, want %s", rep.Candidates[0].Name, want)
	}
	if src.calls < 3 {
		t.Errorf("source was paged %d time(s); expected at least 3", src.calls)
	}
}

func TestScanBudgetTruncatesAndSaysSo(t *testing.T) {
	var rows []registry.UnreferencedSymbol
	for i := int64(1); i <= 100; i++ {
		rows = append(rows, row(i, fmt.Sprintf("fn%d", i)))
	}
	rep := find(t, &fakeSource{rows: rows}, Options{ScanBudget: 10})

	if !rep.Completeness.Truncated {
		t.Fatal("a scan stopped by its budget must report itself truncated")
	}
	if rep.Likely != 10 {
		t.Errorf("likely = %d, want 10 (what was examined)", rep.Likely)
	}
}

// The warning against deleting on this evidence cannot be omitted by a caller.
func TestEveryReportCarriesTheLimits(t *testing.T) {
	rep := find(t, &fakeSource{}, Options{})
	if !rep.Completeness.HasCaveat(Limits) {
		t.Error("report is missing the Limits caveat")
	}
	if !rep.Completeness.HasCaveat(capability.LowerBoundCaveat) {
		t.Error("report is missing the lower-bound caveat")
	}
}

// A function passed as a value leaves no call edge and does leave its name in
// the text. The graph alone would call it likely unused; the text check must
// demote it and say why.
func TestNameMentionedElsewhereIsDemoted(t *testing.T) {
	src := &fakeSource{
		rows:    []registry.UnreferencedSymbol{row(1, "handler"), row(2, "trulyAlone"), row(3, "opaque")},
		usedAt:  map[string][]int{"handler": {7}},
		unknown: map[string]bool{"opaque": true},
	}
	rep := find(t, src, Options{})

	by := map[string]Candidate{}
	for _, c := range rep.Candidates {
		by[c.Name] = c
	}
	if c := by["handler"]; c.Certainty != CertaintyPossible || len(c.Reasons) != 1 || c.Reasons[0] != ReasonMentioned {
		t.Errorf("handler = %+v, want possible because its name appears elsewhere", c)
	}
	if c := by["trulyAlone"]; c.Certainty != CertaintyLikely {
		t.Errorf("trulyAlone = %+v, want likely", c)
	}
	// "The text index could not look" is not evidence of absence.
	if c := by["opaque"]; c.Certainty != CertaintyPossible {
		t.Errorf("opaque = %+v, want possible when the text check cannot answer", c)
	}
	if rep.Likely != 1 || rep.Possible != 2 {
		t.Errorf("counts: likely %d possible %d, want 1 and 2 after demotion", rep.Likely, rep.Possible)
	}
	if rep.TextChecked != 3 {
		t.Errorf("text checked = %d, want 3", rep.TextChecked)
	}
	for _, r := range Reasons() {
		if r.Label() == "" || r.Text() == "" {
			t.Errorf("reason %q is missing its label or sentence", r)
		}
	}
	if rep.Candidates[0].Name != "trulyAlone" {
		t.Errorf("the surviving likely candidate should lead, got %s", rep.Candidates[0].Name)
	}
}

// The definition line and anything inside the definition's own body (recursion,
// a log line naming the function) are not uses by anything else.
func TestMentionsInsideTheDefinitionDoNotDemote(t *testing.T) {
	r := row(1, "recursive") // lines 10-14 of pkg/x.go
	src := &fakeSource{
		rows:   []registry.UnreferencedSymbol{r},
		selfAt: map[string][]int{"recursive": {10, 12}},
	}
	rep := find(t, src, Options{})
	if c := rep.Candidates[0]; c.Certainty != CertaintyLikely {
		t.Errorf("recursive = %+v, want likely: both mentions are inside its own span", c)
	}

	// The same name on a line OUTSIDE the span, in the same file, is a use.
	src.selfAt["recursive"] = []int{10, 40}
	rep = find(t, src, Options{})
	if c := rep.Candidates[0]; c.Certainty != CertaintyPossible {
		t.Errorf("recursive = %+v, want possible: line 40 is outside its definition", c)
	}
}

// Two definitions sharing a name are each other's definitions, not each
// other's uses.
func TestSameNamedDefinitionsDoNotDemoteEachOther(t *testing.T) {
	a, b := row(1, "helper"), row(2, "helper") // lines 10-14 and 20-24
	rep := find(t, &fakeSource{
		rows:   []registry.UnreferencedSymbol{a, b},
		selfAt: map[string][]int{"helper": {10, 20}},
	}, Options{})
	for _, c := range rep.Candidates {
		if c.Certainty != CertaintyLikely {
			t.Errorf("%s at line %d was demoted by its own namesake: %+v", c.Name, c.Line, c)
		}
	}
}

// The check stops once the answer is full, so a large shortlist costs only as
// many text lookups as it takes to confirm Limit rows.
func TestTextCheckStopsWhenTheAnswerIsFull(t *testing.T) {
	var rows []registry.UnreferencedSymbol
	for i := int64(1); i <= 300; i++ {
		rows = append(rows, row(i, fmt.Sprintf("fn%d", i)))
	}
	rep := find(t, &fakeSource{rows: rows}, Options{Limit: 5})
	if rep.TextChecked != 5 {
		t.Errorf("text checked = %d, want 5: nothing was demoted, so five lookups fill the answer", rep.TextChecked)
	}
	if len(rep.Candidates) != 5 {
		t.Errorf("returned %d candidates, want 5", len(rep.Candidates))
	}
}

// A demoted row must be replaced from the shortlist, not leave the answer short.
func TestDemotedRowsAreReplacedFromTheShortlist(t *testing.T) {
	var rows []registry.UnreferencedSymbol
	usedAt := map[string][]int{}
	for i := int64(1); i <= 12; i++ {
		r := row(i, fmt.Sprintf("fn%d", i))
		r.StartLine, r.EndLine = 1, int(100-i) // fn1 is the longest
		rows = append(rows, r)
		if i <= 3 {
			usedAt[r.Name] = []int{3} // the three longest are used elsewhere
		}
	}
	rep := find(t, &fakeSource{rows: rows, usedAt: usedAt}, Options{Limit: 3, MinCertainty: CertaintyLikely})
	if got := strings.Join(candidateNames(rep), ","); got != "fn4,fn5,fn6" {
		t.Errorf("candidates = %s, want the next three likely ones (fn4,fn5,fn6)", got)
	}
}

func TestNilSourceIsAnError(t *testing.T) {
	d, runID := deps(t)
	if _, err := Find(context.Background(), d, nil, runID, Options{}); err == nil {
		t.Error("a nil source must be an error, not a nil dereference")
	}
}

// End to end over the real registry: a called function is not a candidate and
// an uncalled one is.
func TestFindOverTheRealRegistry(t *testing.T) {
	d, runID := captest.Build(t,
		[]captest.Sym{
			captest.S(1, "used", "pkg/a.go"),
			captest.S(2, "caller", "pkg/a.go"),
			captest.S(3, "orphan", "pkg/b.go"),
		},
		[]captest.Edge{captest.E(2, 1)},
		captest.Refs{},
	)
	src, ok := d.Registry.(Source)
	if !ok {
		t.Fatal("the registry does not satisfy deadcode.Source")
	}
	rep, err := Find(context.Background(), d, src, runID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range rep.Candidates {
		got[c.Name] = true
	}
	if got["used"] {
		t.Error("used has a caller and must not be a candidate")
	}
	if !got["orphan"] || !got["caller"] {
		t.Errorf("orphan and caller have no incoming edge; candidates = %v", candidateNames(rep))
	}
}

func candidateNames(rep *Report) []string {
	out := make([]string, len(rep.Candidates))
	for i, c := range rep.Candidates {
		out[i] = c.Name
	}
	return out
}
