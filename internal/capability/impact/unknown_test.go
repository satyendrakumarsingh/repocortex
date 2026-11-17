package impact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// goRefs is enough bound Go references that the language is seen to yield
// them, with nothing spelled like the symbols a test asks about.
var goRefs = captest.Refs{Resolved: 8, Unresolved: 2}

// phpSymbols is a PHP codebase large enough that "no reference from any of
// it" says something about the language: chooseHandler and two dozen others.
func phpSymbols() []captest.Sym {
	syms := []captest.Sym{captest.S(1, "chooseHandler", "src/Utils.php")}
	for i := int64(0); i < 24; i++ {
		syms = append(syms, captest.S(10+i, "method", "src/Client.php").WithFQN(fmt.Sprintf("src/Client.php#m%d", i)))
	}
	return syms
}

func namedRefs(base captest.Refs, named ...captest.NamedRef) captest.Refs {
	base.Named = named
	return base
}

func repeatRef(n int, r captest.NamedRef) []captest.NamedRef {
	out := make([]captest.NamedRef, n)
	for i := range out {
		out[i] = r
		out[i].Line = r.Line + i
	}
	return out
}

// TestRiskIsUnknownWhenTheCallersCouldNotBeBound is the audit's finding as a
// property of the score: "0 direct, 0 indirect. Risk low (score 20)" was
// printed for Gson.fromJson, which has 792 references spelled like it, every
// one recorded as ambiguous.
func TestRiskIsUnknownWhenTheCallersCouldNotBeBound(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		syms []captest.Sym
		refs captest.Refs
		want string // must appear in Risk.Undetermined
	}{
		{
			name: "every reference to the name is ambiguous",
			syms: []captest.Sym{captest.S(1, "fromJson", "lib/gson.go")},
			refs: namedRefs(goRefs, repeatRef(7, captest.NamedRef{
				Name: "fromJson", Line: 10, Resolution: registry.ResolutionAmbiguous})...),
			want: "7 of the 7 reference(s) spelled like this name are not listed as its callers: 7 ambiguous",
		},
		{
			name: "references are unresolved",
			syms: []captest.Sym{captest.S(1, "fromJson", "lib/gson.go")},
			refs: namedRefs(goRefs, repeatRef(3, captest.NamedRef{
				Name: "fromJson", Line: 10, Resolution: registry.ResolutionUnresolved})...),
			want: "3 unresolved",
		},
		{
			name: "references bound from outside any indexed function",
			syms: []captest.Sym{captest.S(1, "defineGetter", "lib/request.go")},
			refs: namedRefs(goRefs, repeatRef(12, captest.NamedRef{
				Name: "defineGetter", Path: "lib/request.go", Line: 100,
				Resolution: registry.ResolutionResolved, Target: 1})...),
			want: "12 bound to it from outside any indexed function",
		},
		{
			name: "the language yields no references at all",
			syms: phpSymbols(),
			refs: goRefs, // Go references exist; PHP has none
			want: "RepoCortex does not extract calls from PHP",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, runID := captest.Build(t, tc.syms, nil, tc.refs)
			rep, err := Analyze(ctx, deps, runID, tc.syms[0].Name)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Risk.Level != LevelUnknown {
				t.Errorf("Level = %q, want %q: a score computed without the callers is not a low score",
					rep.Risk.Level, LevelUnknown)
			}
			if !strings.Contains(rep.Risk.Undetermined, tc.want) {
				t.Errorf("Undetermined = %q, want it to say %q", rep.Risk.Undetermined, tc.want)
			}
			// It is a counted unknown and a listed factor, not only a field.
			if u, ok := findUnknown(rep, UnknownUnboundName); !ok || !strings.Contains(u.Detail, tc.want) {
				t.Errorf("no %s unknown carrying the reason; unknowns = %+v", UnknownUnboundName, rep.Unknown)
			}
			var factor *Factor
			for i := range rep.Risk.Factors {
				switch rep.Risk.Factors[i].Name {
				case "unbound_references":
					factor = &rep.Risk.Factors[i]
				case "observed_tests":
					// "No test reaches this target" was stated, and scored, for
					// symbols whose callers were simply not bound.
					if rep.Risk.Factors[i].Points != 0 ||
						!strings.Contains(rep.Risk.Factors[i].Detail, "could NOT be determined") {
						t.Errorf("observed_tests claims an absence it could not see: %+v", rep.Risk.Factors[i])
					}
				}
			}
			if factor == nil || factor.Points != 0 {
				t.Errorf("the undetermined callers are not a listed, zero-point factor: %+v", rep.Risk.Factors)
			}
			if !strings.Contains(rep.Risk.Formula, "unknown") {
				t.Errorf("the formula does not say how a level becomes unknown: %q", rep.Risk.Formula)
			}
		})
	}
}

// TestRiskStaysDeterminedWhenNothingIsUnbound: unknown is for missing
// evidence. With every reference to the name accounted for, low is a finding.
func TestRiskStaysDeterminedWhenNothingIsUnbound(t *testing.T) {
	// Nothing calls helper, and no reference anywhere is spelled like it: the
	// absence is one the index can stand behind as far as names go.
	deps, runID := captest.Build(t,
		[]captest.Sym{captest.S(1, "helper", "lib/a.go"), captest.S(2, "caller", "lib/a.go"), captest.S(3, "used", "lib/a.go")},
		[]captest.Edge{captest.E(2, 3)}, goRefs)
	rep, err := Analyze(context.Background(), deps, runID, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Risk.Level != LevelLow || rep.Risk.Undetermined != "" {
		t.Errorf("Level = %q, Undetermined = %q; want a determined low", rep.Risk.Level, rep.Risk.Undetermined)
	}
	if !rep.Binding.Known || rep.Binding.References != 0 {
		t.Errorf("Binding = %+v; want it measured, with no reference to the name", rep.Binding)
	}
	if _, ok := findUnknown(rep, UnknownUnboundName); ok {
		t.Error("an unbound-name unknown on a symbol with no unbound reference")
	}
}

// TestAHigherLevelSurvivesMissingCallers: moderate and high are floors, and a
// floor is still true when evidence is missing. Only low is withdrawn.
func TestAHigherLevelSurvivesMissingCallers(t *testing.T) {
	syms := []captest.Sym{captest.S(1, "hub", "lib/hub.go")}
	var edges []captest.Edge
	for i := int64(2); i <= 9; i++ {
		syms = append(syms, captest.S(i, "caller", "lib/c.go").WithFQN("lib/c.go#caller"+string(rune('a'+i))))
		edges = append(edges, captest.E(i, 1))
	}
	deps, runID := captest.Build(t, syms, edges, namedRefs(goRefs, repeatRef(5, captest.NamedRef{
		Name: "hub", Line: 10, Resolution: registry.ResolutionAmbiguous})...))
	rep, err := Analyze(context.Background(), deps, runID, "hub")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Risk.Level == LevelUnknown || rep.Risk.Level == LevelLow {
		t.Fatalf("Level = %q; eight direct callers are a floor that missing callers cannot lower", rep.Risk.Level)
	}
	if rep.Risk.Undetermined == "" {
		t.Error("the callers that could not be bound are not reported beside the level")
	}
}

// TestExportStatusIsUnknownOutsideTheTrustedLanguages: the flag follows the
// first letter of the name, which is Go's rule and nobody else's.
func TestExportStatusIsUnknownOutsideTheTrustedLanguages(t *testing.T) {
	ctx := context.Background()
	publicAPI := func(t *testing.T, sym captest.Sym) Factor {
		t.Helper()
		deps, runID := captest.Build(t, []captest.Sym{sym}, nil, captest.Refs{Resolved: 4})
		rep, err := Analyze(ctx, deps, runID, sym.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range rep.Risk.Factors {
			if f.Name == "public_api" {
				return f
			}
		}
		t.Fatal("no public_api factor")
		return Factor{}
	}

	// `public T fromJson(...)`: lower-case, recorded unexported, and public.
	java := publicAPI(t, captest.S(1, "fromJson", "src/Gson.java"))
	if java.Points != 0 || !strings.Contains(java.Detail, "UNKNOWN") || strings.Contains(java.Detail, "is unexported") {
		t.Errorf("Java: %+v; want the export status reported as unknown", java)
	}
	// An upper-case Java name is no better known.
	javaUpper := publicAPI(t, captest.S(1, "Gson", "src/Gson.java").Export())
	if javaUpper.Points != 0 || !strings.Contains(javaUpper.Detail, "UNKNOWN") {
		t.Errorf("Java, capitalised: %+v; want unknown, not exported", javaUpper)
	}

	goLower := publicAPI(t, captest.S(1, "helper", "lib/a.go"))
	if !strings.Contains(goLower.Detail, "is unexported") {
		t.Errorf("Go, lower-case: %+v; want unexported stated", goLower)
	}
	goUpper := publicAPI(t, captest.S(1, "Helper", "lib/a.go").Export())
	if goUpper.Points != pointsExported || !strings.Contains(goUpper.Detail, "is exported") {
		t.Errorf("Go, exported: %+v; want exported stated and scored", goUpper)
	}
}

// TestAnAmbiguousQualifiedNameIsNotResolvedToItsFirstOverload: overloads share
// one qualified name, and the re-ask the old error asked for picked the first.
func TestAnAmbiguousQualifiedNameIsNotResolvedToItsFirstOverload(t *testing.T) {
	syms := []captest.Sym{
		captest.S(1, "fromJson", "src/Gson.java").WithFQN("src/Gson.java:fromJson"),
		captest.S(2, "fromJson", "src/Gson.java").WithFQN("src/Gson.java:fromJson"),
		captest.S(3, "other", "src/Gson.java"),
	}
	deps, runID := captest.Build(t, syms, nil, captest.Refs{Resolved: 4})

	_, err := Analyze(context.Background(), deps, runID, "src/Gson.java:fromJson")
	var amb *capability.AmbiguousTargetError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want an ambiguity: two definitions carry this qualified name", err)
	}
	if len(amb.Candidates) != 2 {
		t.Errorf("%d candidates, want 2", len(amb.Candidates))
	}
	// The message tells them apart, which a list of the shared name cannot.
	if msg := err.Error(); strings.Count(msg, "src/Gson.java:fromJson") > 1 || !strings.Contains(msg, "select one by file and line") || !strings.Contains(msg, "--path") {
		t.Errorf("message = %q", msg)
	}

	// A caller that already chose one is not asked again.
	tgt := capability.Target{Query: "fromJson", SymbolID: 2, Name: "fromJson", FQN: "src/Gson.java:fromJson",
		Kind: "function", FilePath: "src/Gson.java"}
	rep, err := AnalyzeTarget(context.Background(), deps, runID, tgt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Target.SymbolID != 2 {
		t.Errorf("analysed symbol %d, want the one selected", rep.Target.SymbolID)
	}
	if _, err := AnalyzeTarget(context.Background(), deps, runID, capability.Target{Name: "x"}, Options{}); err == nil {
		t.Error("a target with no symbol id was analysed")
	}
}

// TestLevelRankPutsUnknownAboveLow: when a change touches several symbols the
// riskiest is reported, and "could not tell" must outrank "can tell, and it is
// small".
func TestLevelRankPutsUnknownAboveLow(t *testing.T) {
	order := []string{LevelLow, LevelUnknown, LevelModerate, LevelHigh}
	for i := 1; i < len(order); i++ {
		if LevelRank(order[i]) <= LevelRank(order[i-1]) {
			t.Errorf("%q does not outrank %q", order[i], order[i-1])
		}
	}
	if !riskier(Risk{Level: LevelUnknown, Score: 0}, Risk{Level: LevelLow, Score: 20}) {
		t.Error("an undetermined risk ranks below a low one with a higher score")
	}
	if riskier(Risk{Level: LevelUnknown, Score: 20}, Risk{Level: LevelModerate, Score: 5}) {
		t.Error("an undetermined risk outranks a moderate one")
	}
}
