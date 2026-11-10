package verify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/compile"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The seeded world these tests verify against.
//
// One controller method in scope, one service it provably calls, one test that
// covers it by a proven call chain, and one report the contract explicitly
// excluded. Every finding below is a consequence of a change against THAT, so
// the fixtures are shared and the tests differ only in what was edited.
const (
	controllerPath = "src/api/CallbackController.java"
	servicePath    = "src/service/ChargeService.java"
	testPath       = "src/test/java/CallbackControllerTest.java"
	reportPath     = "src/reporting/MarketingReport.java"

	targetSymbol   = "processCallback"
	callerSymbol   = "ChargeService.refund"
	exclusionWhy   = "Nothing connects MarketingReport to the callback flow."
	violationRule  = "controllers-do-not-touch-the-database"
	violationDetal = "CallbackController.processCallback writes payments directly"
)

// fakeArch is the declared architecture, as compile sees it.
type fakeArch struct {
	components map[string]string
	rules      []arch.Rule
}

func (f fakeArch) ComponentOf(path string) string { return f.components[path] }
func (f fakeArch) Rules() []arch.Rule             { return f.rules }

// fakeImpact is the blast radius, as compile sees it.
type fakeImpact struct {
	byTarget map[string]compile.Reach
	asked    []string
}

func (f *fakeImpact) Reach(_ context.Context, target string) (compile.Reach, error) {
	f.asked = append(f.asked, target)
	r, ok := f.byTarget[target]
	if !ok {
		return compile.Reach{}, nil
	}
	return r, nil
}

// fakeArchSource is the post-change architecture check, as verify sees it.
type fakeArchSource struct {
	rep  architecture.Report
	err  error
	runs int
}

func (f *fakeArchSource) Check(_ context.Context, _ int64, _ model.Confidence) (architecture.Report, error) {
	f.runs++
	return f.rep, f.err
}

func seededPack() devctx.Pack {
	return devctx.Pack{
		Task:           "Add idempotency to payment callback processing",
		Interpretation: "Make the payment callback handler safe to replay.",
		Items: []devctx.Item{
			{
				Kind: "method", Name: targetSymbol, Path: controllerPath,
				Relevance: 0.94, Why: "named in the task and reached from the endpoint",
				Confidence: "exact",
			},
			{
				Kind: "test", Name: "testProcessCallbackIsIdempotent", Path: testPath,
				Relevance: 0.61, Why: "covers the handler under change",
				Confidence: "high",
			},
		},
		Excluded: []devctx.Exclusion{
			{Name: "MarketingReport", Path: reportPath, Reason: exclusionWhy},
		},
	}
}

func seededPorts() compile.Ports {
	return compile.Ports{
		Architecture: fakeArch{
			components: map[string]string{
				controllerPath: "controller",
				servicePath:    "service",
				testPath:       "controller",
			},
			rules: []arch.Rule{{
				ID:          violationRule,
				Description: "Controllers must go through a service.",
				From:        []string{"controller"},
				Forbidden:   []string{"repository"},
				Severity:    "high",
			}},
		},
		Impact: &fakeImpact{byTarget: map[string]compile.Reach{
			targetSymbol: {
				Resolved: true,
				Target:   targetSymbol,
				Floor:    string(model.ConfidenceMedium),
				Hidden:   12,
				Callers: []compile.Reached{
					{
						Symbol: callerSymbol, Path: servicePath, Kind: "method",
						Depth: 1, Confidence: string(model.ConfidenceExact),
						Evidence: servicePath + ":88",
					},
					{
						Symbol: "LedgerWriter.append", Path: "src/service/LedgerWriter.java",
						Depth: 2, Confidence: string(model.ConfidenceMedium),
					},
					{
						Symbol: "testProcessCallbackIsIdempotent", Path: testPath,
						Depth: 1, Confidence: string(model.ConfidenceHigh), IsTest: true,
					},
				},
				Tests: []compile.TestRef{{
					Name: "testProcessCallbackIsIdempotent", Path: testPath,
					Signal: "graph", Confidence: string(model.ConfidenceHigh),
				}},
			},
		}},
	}
}

func seededContract(t *testing.T) compile.Contract {
	t.Helper()
	c, err := compile.CompileWith(context.Background(),
		"Add idempotency to payment callback processing", seededPack(), compile.Options{}, seededPorts())
	if err != nil {
		t.Fatalf("CompileWith: %v", err)
	}
	if len(c.BlastRadius) == 0 {
		t.Fatal("seeded contract has no blast radius; the impact port was not consulted")
	}
	return c
}

// violatingArchSource returns one violation, on a compiler-proven edge, in the
// file the change touched.
func violatingArchSource() *fakeArchSource {
	return &fakeArchSource{rep: architecture.Report{
		RulesEvaluated: 1,
		Violations: []architecture.Violation{{
			RuleID: violationRule, Severity: "high", Verdict: "forbidden",
			From: "controller", To: "repository",
			FromName: targetSymbol, ToName: "PaymentRepository.insert",
			Kind: "writes", Confidence: string(model.ConfidenceExact),
			FilePath: controllerPath, Line: 142, Detail: violationDetal,
		}},
	}}
}

func verifyOrFail(t *testing.T, v *Verifier, c compile.Contract, changed []string) Report {
	t.Helper()
	rep, err := v.Verify(context.Background(), c, changed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep
}

func findingOfKind(rep Report, k Kind) (Finding, bool) {
	for _, f := range rep.Findings {
		if f.Kind == k {
			return f, true
		}
	}
	return Finding{}, false
}

// TestBrokenRuleAndMissedCallerBothFailWithEvidence is the case the whole
// package exists for: an agent edited the handler, broke a rule someone wrote
// down, and left a proven caller alone.
func TestBrokenRuleAndMissedCallerBothFailWithEvidence(t *testing.T) {
	c := seededContract(t)
	src := violatingArchSource()
	v := New(WithArchitecture(src, 7))

	// The controller changed. ChargeService, which provably calls it, did not.
	rep := verifyOrFail(t, v, c, []string{controllerPath})

	if src.runs != 1 {
		t.Fatalf("architecture source consulted %d times, want 1", src.runs)
	}
	if rep.Disposition != DispositionFail {
		t.Fatalf("disposition = %q, want %q (findings: %+v)", rep.Disposition, DispositionFail, rep.Findings)
	}
	if got := rep.Disposition.ExitCode(); got != 1 {
		t.Errorf("exit code = %d, want 1", got)
	}

	viol, ok := findingOfKind(rep, KindArchitectureViolation)
	if !ok {
		t.Fatalf("no architecture violation reported: %+v", rep.Findings)
	}
	if viol.Standing != StandingFact {
		t.Errorf("violation standing = %q, want %q on a compiler-proven edge", viol.Standing, StandingFact)
	}
	if !strings.Contains(viol.Evidence, violationRule) {
		t.Errorf("violation evidence does not name the rule: %q", viol.Evidence)
	}
	if !strings.Contains(viol.Evidence, controllerPath+":142") {
		t.Errorf("violation evidence does not carry the file and line: %q", viol.Evidence)
	}
	if viol.Confidence != model.ConfidenceExact {
		t.Errorf("violation confidence = %q, want it carried through unchanged", viol.Confidence)
	}
	if viol.Severity != "high" {
		t.Errorf("violation severity = %q, want the rule's own severity", viol.Severity)
	}

	missed, ok := findingOfKind(rep, KindMissedCaller)
	if !ok {
		t.Fatalf("no missed caller reported: %+v", rep.Findings)
	}
	if missed.Symbol != callerSymbol || missed.Path != servicePath {
		t.Errorf("missed caller = %q in %q, want %q in %q", missed.Symbol, missed.Path, callerSymbol, servicePath)
	}
	if missed.Standing != StandingFact {
		t.Errorf("missed caller standing = %q, want %q on an exact edge", missed.Standing, StandingFact)
	}
	for _, want := range []string{targetSymbol, "depth 1", "exact", servicePath + ":88"} {
		if !strings.Contains(missed.Evidence, want) {
			t.Errorf("missed caller evidence %q does not contain %q", missed.Evidence, want)
		}
	}

	// The depth-2 medium reach must NOT be reported as a fact, and the
	// low-confidence region the floor hid must be stated rather than implied.
	for _, f := range rep.Findings {
		if f.Kind == KindMissedCaller && f.Symbol == "LedgerWriter.append" && f.Standing == StandingFact {
			t.Error("a medium-confidence depth-2 reach was reported as a fact")
		}
	}
	if !excludedMentions(rep, "12") {
		t.Errorf("the report does not state what the confidence floor hid: %+v", rep.Excluded)
	}
}

// TestCleanChangePasses: the same contract, honoured.
func TestCleanChangePasses(t *testing.T) {
	c := seededContract(t)
	src := &fakeArchSource{rep: architecture.Report{RulesEvaluated: 1}}
	v := New(WithArchitecture(src, 7))

	rep := verifyOrFail(t, v, c, []string{
		controllerPath, servicePath, "src/service/LedgerWriter.java", testPath,
	})

	if rep.Disposition != DispositionPass {
		t.Fatalf("disposition = %q, want %q; findings: %+v", rep.Disposition, DispositionPass, rep.Findings)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("clean change produced %d finding(s): %+v", len(rep.Findings), rep.Findings)
	}
	if rep.Disposition.ExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", rep.Disposition.ExitCode())
	}
	if !rep.Checked.ArchitectureConsulted {
		t.Error("a pass must record that the architecture was actually consulted")
	}
	if rep.Checked.ChangedFiles != 4 {
		t.Errorf("checked %d changed files, want 4", rep.Checked.ChangedFiles)
	}
}

// TestOutOfScopeEditQuotesTheContractsOwnReason: the contract said why, and the
// report must say the same thing in the same words.
func TestOutOfScopeEditQuotesTheContractsOwnReason(t *testing.T) {
	c := seededContract(t)
	v := New()

	rep := verifyOrFail(t, v, c, []string{controllerPath, servicePath, testPath, reportPath})

	f, ok := findingOfKind(rep, KindOutOfScopeEdit)
	if !ok {
		t.Fatalf("editing an excluded file produced no finding: %+v", rep.Findings)
	}
	if f.Path != reportPath {
		t.Errorf("finding path = %q, want %q", f.Path, reportPath)
	}
	if f.Detail != exclusionWhy {
		t.Errorf("finding detail = %q, want the contract's reason verbatim: %q", f.Detail, exclusionWhy)
	}
	if !strings.Contains(f.Evidence, reportPath) {
		t.Errorf("finding evidence does not cite the contract entry: %q", f.Evidence)
	}
	if f.Standing != StandingFact {
		t.Errorf("standing = %q; an edit to an excluded file is observed, not inferred", f.Standing)
	}
	if rep.Disposition != DispositionFail {
		t.Errorf("disposition = %q, want %q", rep.Disposition, DispositionFail)
	}

	// No architecture source was supplied, and the report must say so rather
	// than let silence read as "no rule was broken".
	if !unknownsMention(rep, "no declared rule was evaluated") {
		t.Errorf("unknowns do not record the unasked architecture question: %+v", rep.Unknowns)
	}
}

// TestReportIsByteIdenticalAcrossRunsAndInputOrder pins the property a CI gate
// rests on.
func TestReportIsByteIdenticalAcrossRunsAndInputOrder(t *testing.T) {
	c := seededContract(t)
	changed := []string{controllerPath, reportPath, "src/service/LedgerWriter.java"}
	shuffled := []string{"src/service/LedgerWriter.java", reportPath, "./" + controllerPath, controllerPath}

	first := marshal(t, verifyOrFail(t, New(WithArchitecture(violatingArchSource(), 7)), c, changed))
	for i := 0; i < 5; i++ {
		again := marshal(t, verifyOrFail(t, New(WithArchitecture(violatingArchSource(), 7)), c, changed))
		if again != first {
			t.Fatalf("run %d differs from run 0:\n%s\n---\n%s", i+1, first, again)
		}
	}
	reordered := marshal(t, verifyOrFail(t, New(WithArchitecture(violatingArchSource(), 7)), c, shuffled))
	if reordered != first {
		t.Fatalf("input order changed the report:\n%s\n---\n%s", first, reordered)
	}
}

// TestNothingToCheckIsNotAPass keeps the gate honest when the inputs vanish.
func TestNothingToCheckIsNotAPass(t *testing.T) {
	c := seededContract(t)
	if rep := verifyOrFail(t, New(), c, nil); rep.Disposition != DispositionUnverified {
		t.Errorf("empty change set: disposition = %q, want %q", rep.Disposition, DispositionUnverified)
	}
	empty := compile.Contract{Intent: "do a thing"}
	rep := verifyOrFail(t, New(), empty, []string{controllerPath})
	if rep.Disposition != DispositionUnverified {
		t.Errorf("empty contract: disposition = %q, want %q", rep.Disposition, DispositionUnverified)
	}
	if rep.Disposition.ExitCode() != 2 {
		t.Errorf("unverified exit code = %d, want 2 (a setup failure, not a change failure)", rep.Disposition.ExitCode())
	}
}

// TestMissedCallerRespectsTheConfidenceFloor: a caller reached only across an
// edge below the floor is excluded and SAID to be excluded.
func TestMissedCallerRespectsTheConfidenceFloor(t *testing.T) {
	c := seededContract(t)
	// Raise the floor above the depth-2 medium reach; only the exact one may
	// survive.
	v := New(WithMinConfidence(model.ConfidenceExact))
	rep := verifyOrFail(t, v, c, []string{controllerPath})

	for _, f := range rep.Findings {
		if f.Kind == KindMissedCaller && f.Confidence != model.ConfidenceExact {
			t.Errorf("finding below the raised floor was reported: %+v", f)
		}
	}
	if !excludedMentions(rep, "exact") {
		t.Errorf("the raised floor is not stated in the exclusions: %+v", rep.Excluded)
	}
}

// TestNoMissedCallerWhenTheTargetItselfDidNotChange: the blast radius of a
// symbol nobody touched is not a set of missed callers.
func TestNoMissedCallerWhenTheTargetItselfDidNotChange(t *testing.T) {
	c := seededContract(t)
	rep := verifyOrFail(t, New(), c, []string{"src/util/Strings.java"})
	if _, ok := findingOfKind(rep, KindMissedCaller); ok {
		t.Errorf("missed callers reported for an untouched target: %+v", rep.Findings)
	}
	if !unknownsMention(rep, "No file defining a scope symbol was changed") {
		t.Errorf("the reason nothing was checked is not stated: %+v", rep.Unknowns)
	}
}

// TestUncoveredChangedSymbolIsALeadNotAFact: a coverage gap rests on test
// discovery, which is signal-based, so it must never fail a build on its own.
func TestUncoveredChangedSymbolIsALeadNotAFact(t *testing.T) {
	c := seededContract(t)
	c.Tests = nil // the contract named no covering test
	rep := verifyOrFail(t, New(), c, []string{controllerPath, servicePath})

	if _, ok := findingOfKind(rep, KindTestCoverageGap); ok {
		t.Error("a gap was reported as a finding when the contract named no tests at all; that is a gap in the contract and belongs in Unknowns")
	}
	if !unknownsMention(rep, "names no covering test") {
		t.Errorf("unknowns do not record the missing coverage claim: %+v", rep.Unknowns)
	}

	// Now a contract that DOES name a test, but only by convention.
	c2 := seededContract(t)
	c2.Tests = []compile.TestRef{{
		Name: "testProcessCallbackIsIdempotent", Path: testPath,
		Signal: "naming_convention", Confidence: string(model.ConfidenceLow),
		Covers: targetSymbol,
	}}
	rep2 := verifyOrFail(t, New(), c2, []string{controllerPath, servicePath})
	gap, ok := findingOfKind(rep2, KindTestCoverageGap)
	if !ok {
		t.Fatalf("a convention-only test produced no lead: %+v", rep2.Findings)
	}
	if gap.Standing != StandingLead {
		t.Errorf("coverage standing = %q, want %q", gap.Standing, StandingLead)
	}
	if !strings.Contains(gap.Evidence, "naming_convention") {
		t.Errorf("evidence does not name the signal: %q", gap.Evidence)
	}
	if rep2.Disposition != DispositionAdvisory {
		t.Errorf("disposition = %q, want %q: leads alone must not fail a build", rep2.Disposition, DispositionAdvisory)
	}
	if rep2.Disposition.ExitCode() != 0 {
		t.Errorf("advisory exit code = %d, want 0", rep2.Disposition.ExitCode())
	}
}

// TestNoFindingIsEmittedWithoutEvidence pins the exit filter.
func TestNoFindingIsEmittedWithoutEvidence(t *testing.T) {
	c := seededContract(t)
	rep := verifyOrFail(t, New(WithArchitecture(violatingArchSource(), 7)),
		c, []string{controllerPath, reportPath})
	if len(rep.Findings) == 0 {
		t.Fatal("expected findings")
	}
	for _, f := range rep.Findings {
		if strings.TrimSpace(f.Evidence) == "" {
			t.Errorf("finding with no evidence: %+v", f)
		}
		if f.Standing != StandingFact && f.Standing != StandingLead {
			t.Errorf("finding with no standing: %+v", f)
		}
	}
}

// TestNoDeclaredArchitectureIsNotAFailure: a repository that declared nothing
// has broken no rule.
func TestNoDeclaredArchitectureIsNotAFailure(t *testing.T) {
	c := seededContract(t)
	src := &fakeArchSource{err: architecture.ErrNoModel}
	rep := verifyOrFail(t, New(WithArchitecture(src, 7)), c, []string{controllerPath, servicePath, testPath})
	if _, ok := findingOfKind(rep, KindArchitectureViolation); ok {
		t.Error("a repository with no declared architecture produced a violation")
	}
	if !unknownsMention(rep, "declares no architecture") {
		t.Errorf("the absent model is not recorded: %+v", rep.Unknowns)
	}
}

// TestPreexistingViolationsAreNotAttributedToThisChange: a gate that is always
// red is a gate that gets bypassed.
func TestPreexistingViolationsAreNotAttributedToThisChange(t *testing.T) {
	c := seededContract(t)
	src := violatingArchSource()
	rep := verifyOrFail(t, New(WithArchitecture(src, 7)), c, []string{servicePath})
	if _, ok := findingOfKind(rep, KindArchitectureViolation); ok {
		t.Errorf("a violation in an untouched file was attributed to this change: %+v", rep.Findings)
	}
	if !excludedMentions(rep, "pre-existing") {
		t.Errorf("the untouched violation is not accounted for: %+v", rep.Excluded)
	}
}

func excludedMentions(rep Report, substr string) bool {
	for _, e := range rep.Excluded {
		if strings.Contains(e.Reason, substr) || strings.Contains(itoa(e.Count), substr) {
			return true
		}
	}
	return false
}

func unknownsMention(rep Report, substr string) bool {
	for _, u := range rep.Unknowns {
		if strings.Contains(u, substr) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func marshal(t *testing.T, rep Report) string {
	t.Helper()
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return string(b)
}

// TestAFileHoldingBothAnIncludedAndAnExcludedSymbolIsNotAViolation pins the
// distinction the check exists to make.
//
// Scope and non-scope are symbol-level, and the context engine ranks each symbol
// on its own, so one file routinely supplies both. Reading the file-level edit as
// a violation because SOME symbol in it was excluded fired on the very first real
// change: `store.go` was the whole task and the report called it out of scope
// twice. Without the guard this test sees a FACT and a `fail` disposition.
func TestAFileHoldingBothAnIncludedAndAnExcludedSymbolIsNotAViolation(t *testing.T) {
	c := seededContract(t)
	if len(c.Scope) == 0 {
		t.Fatal("seeded contract has no scope; nothing to shadow")
	}
	shared := c.Scope[0].Path

	// Same path on both sides — different symbols, one file.
	c.OutOfScope = append(c.OutOfScope, compile.ScopeItem{
		Symbol: "unrelatedHelper",
		Path:   shared,
		Why:    "a helper in the same file that the task does not touch",
	})

	rep := verifyOrFail(t, New(), c, []string{shared})

	for _, f := range rep.Findings {
		if f.Kind == KindOutOfScopeEdit && f.Path == shared {
			t.Fatalf("a file that is BOTH in scope and out of scope was reported as an out-of-scope edit: %+v", f)
		}
	}
	// Deliberately no disposition assertion: this fixture's blast radius names a
	// caller that did not change, so it fails on a missed caller regardless. That
	// is a different question, and asserting on the disposition here would couple
	// this test to the answer to it.
	// The declined exclusion is stated, never silently dropped.
	var said bool
	for _, u := range rep.Unknowns {
		if strings.Contains(u, "ALSO holds an in-scope symbol") {
			said = true
		}
	}
	if !said {
		t.Errorf("the exclusion this check declined to enforce was not reported: %v", rep.Unknowns)
	}
}

// TestUndeclaredOnlyChangeIsUnverifiedNotAPass is the regression for the worst
// defect the portal had.
//
// Reproduced live at 1280x720 against a real repository: a task contract was
// compiled, two files were named as changed, and the verify screen rendered a
// large green PASS with `exit 0` over the line `0 in scope · 0 explicitly
// excluded · 2 undeclared` — directly above its own unknowns card saying the
// contract took no position on either file. Both sentences came from this
// package. The screen's own prose says the two states must never render the
// same, and the verdict badge rendered them the same.
//
// A run that verified nothing must not read as a pass.
func TestUndeclaredOnlyChangeIsUnverifiedNotAPass(t *testing.T) {
	c := seededContract(t)

	// Two files the contract takes no position on. Not excluded — that would be
	// a finding — simply absent from both lists, which is the ordinary shape of
	// a change a bounded contract did not anticipate.
	rep := verifyOrFail(t, New(), c, []string{
		"docs/runbooks/oncall.md",
		"deploy/helm/values.yaml",
	})

	if len(rep.Findings) != 0 {
		t.Fatalf("this fixture must produce no findings; got %+v", rep.Findings)
	}
	if rep.Disposition == DispositionPass {
		t.Fatal("a change the contract took no position on rendered as `pass`. " +
			"Nothing about it was checked: `pass` claims every question was asked and " +
			"none produced a finding, and neither half of that is true here.")
	}
	if rep.Disposition != DispositionUnverified {
		t.Fatalf("disposition = %q, want %q", rep.Disposition, DispositionUnverified)
	}
	if got := rep.Disposition.ExitCode(); got != 2 {
		t.Errorf("exit code = %d, want 2 — a failure of the setup, distinguishable by a "+
			"pipeline from both a pass (0) and a failed change (1)", got)
	}
	if rep.Checked.DeclaredChangedFiles != 0 {
		t.Errorf("DeclaredChangedFiles = %d, want 0", rep.Checked.DeclaredChangedFiles)
	}
	if rep.Answered() {
		t.Error("Answered() is true although no question engaged the change set")
	}

	// The questions table must say so per row, and `asked` must not be allowed
	// to stand in for `answered`.
	if len(rep.Questions) != 4 {
		t.Fatalf("questions = %d, want 4", len(rep.Questions))
	}
	var askedButUnanswered int
	for _, q := range rep.Questions {
		if q.Answered {
			t.Errorf("question %q reports answered against a change it did not engage", q.ID)
		}
		if q.Detail == "" {
			t.Errorf("question %q carries no detail", q.ID)
		}
		if q.Asked {
			askedButUnanswered++
		}
	}
	if askedButUnanswered == 0 {
		t.Error("no question was even asked, so this fixture does not exercise the " +
			"asked-but-unanswered case the defect lived in")
	}
	if !unknownsMention(rep, "nothing about this change was verified") &&
		!unknownsMention(rep, "None of the four questions engaged") {
		t.Errorf("the verdict is unverified and the unknowns do not say why: %+v", rep.Unknowns)
	}
}

// TestChangeTouchingScopeStillPasses is the other half: the fix must not turn
// every clean verification into `unverified`. A gate that never passes is a gate
// that gets switched off just as surely as one that never fails.
func TestChangeTouchingScopeStillPasses(t *testing.T) {
	c := seededContract(t)
	c.Tests = nil
	c.BlastRadius = nil

	rep := verifyOrFail(t, New(), c, []string{controllerPath})
	if len(rep.Findings) != 0 {
		t.Fatalf("expected a clean run, got %+v", rep.Findings)
	}
	if rep.Disposition != DispositionPass {
		t.Fatalf("disposition = %q, want %q: the contract names %s and %s was changed, "+
			"so the scope question genuinely engaged this change",
			rep.Disposition, DispositionPass, controllerPath, controllerPath)
	}
	if !rep.Answered() {
		t.Error("Answered() is false on a run that passed")
	}
}
