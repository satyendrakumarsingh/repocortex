package compile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	devctx "github.com/satyendrakumarsingh/repocortex/internal/context"
)

type stubArch struct {
	components map[string]string
	rules      []arch.Rule
	asked      []string
}

func (s *stubArch) ComponentOf(path string) string {
	s.asked = append(s.asked, path)
	return s.components[path]
}
func (s *stubArch) Rules() []arch.Rule { return s.rules }

type stubImpact struct {
	byTarget map[string]Reach
	err      error
	asked    []string
}

func (s *stubImpact) Reach(_ context.Context, target string) (Reach, error) {
	s.asked = append(s.asked, target)
	if s.err != nil {
		return Reach{}, s.err
	}
	return s.byTarget[target], nil
}

func portedPack() devctx.Pack {
	return devctx.Pack{
		Interpretation: "Make the payment callback handler safe to replay.",
		Items: []devctx.Item{
			{Kind: "method", Name: "processCallback", Path: "src/api/CallbackController.java",
				Relevance: 0.9, Why: "named in the task", Confidence: "exact"},
			{Kind: "class", Name: "ChargeService", Path: "src/service/ChargeService.java",
				Relevance: 0.5, Why: "called by the handler", Confidence: "high"},
		},
	}
}

func stubPorts() (Ports, *stubArch, *stubImpact) {
	a := &stubArch{
		components: map[string]string{
			"src/api/CallbackController.java": "controller",
			"src/service/ChargeService.java":  "service",
		},
		rules: []arch.Rule{
			{ID: "controllers-do-not-touch-the-database", Description: "Controllers must go through a service.",
				From: []string{"controller"}, Forbidden: []string{"repository"}, Severity: "high"},
			{ID: "services-are-self-contained", From: []string{"service"}, AllowedTo: []string{"repository"}},
			{ID: "irrelevant-rule", From: []string{"reporting"}, Forbidden: []string{"payments"}},
		},
	}
	i := &stubImpact{byTarget: map[string]Reach{
		"processCallback": {
			Resolved: true, Target: "processCallback",
			Floor: "medium", Hidden: 9, Truncated: true,
			Callers: []Reached{
				{Symbol: "refund", Path: "src/service/ChargeService.java", Depth: 1,
					Confidence: "exact", Evidence: "src/service/ChargeService.java:88"},
				{Symbol: "append", Path: "src/ledger/LedgerWriter.java", Depth: 2, Confidence: "medium"},
			},
			Tests: []TestRef{{Name: "testCallback", Path: "src/test/CallbackTest.java",
				Signal: "graph", Confidence: "high"}},
		},
		"ChargeService": {Resolved: false},
	}}
	return Ports{Architecture: a, Impact: i}, a, i
}

func compileWithPorts(t *testing.T, ports Ports) Contract {
	t.Helper()
	c, err := CompileWith(context.Background(), "Add idempotency to payment callbacks",
		portedPack(), Options{}, ports)
	if err != nil {
		t.Fatalf("CompileWith: %v", err)
	}
	return c
}

// TestDeclaredRulesBecomeConstraintsCitingTheRule: the architecture the
// repository declared is a constraint the change must respect, and it is cited
// by id rather than restated as this package's own opinion.
func TestDeclaredRulesBecomeConstraintsCitingTheRule(t *testing.T) {
	ports, _, _ := stubPorts()
	c := compileWithPorts(t, ports)

	con, ok := constraintContaining(c, "controllers-do-not-touch-the-database")
	if !ok {
		t.Fatalf("the declared rule produced no constraint: %+v", c.Constraints)
	}
	if !strings.Contains(con.Text, "must not depend on repository") {
		t.Errorf("constraint text does not state the rule: %q", con.Text)
	}
	if !strings.Contains(con.Text, "Controllers must go through a service.") {
		t.Errorf("constraint drops the rule's own description: %q", con.Text)
	}
	for _, want := range []string{"architecture rule", "severity high", "component controller",
		"processCallback (src/api/CallbackController.java)"} {
		if !strings.Contains(con.Evidence, want) {
			t.Errorf("constraint evidence %q does not contain %q", con.Evidence, want)
		}
	}
	if _, ok := constraintContaining(c, "services-are-self-contained"); !ok {
		t.Error("an allow-list rule over a component in scope was not emitted")
	}
	if _, ok := constraintContaining(c, "irrelevant-rule"); ok {
		t.Error("a rule over a component nothing in scope belongs to was emitted anyway")
	}
}

// TestNoArchitectureSourceIsStatedRatherThanImplied.
func TestNoArchitectureSourceIsStatedRatherThanImplied(t *testing.T) {
	c := compileOrFail(t, "do a thing", portedPack(), Options{})
	if !unknownContaining(c, "No declared architecture was consulted") {
		t.Errorf("a contract compiled without the architecture port does not say so: %+v", c.Unknowns)
	}
	if !unknownContaining(c, "No impact analysis was consulted") {
		t.Errorf("a contract compiled without the impact port does not say so: %+v", c.Unknowns)
	}
	if len(c.BlastRadius) != 0 {
		t.Errorf("blast radius without an impact source: %+v", c.BlastRadius)
	}
}

// TestBlastRadiusCarriesConfidenceDepthAndEvidence.
func TestBlastRadiusCarriesConfidenceDepthAndEvidence(t *testing.T) {
	ports, _, imp := stubPorts()
	c := compileWithPorts(t, ports)

	if len(c.BlastRadius) != 2 {
		t.Fatalf("blast radius = %+v, want the two reached symbols", c.BlastRadius)
	}
	first := c.BlastRadius[0]
	if first.Symbol != "refund" || first.Depth != 1 || first.Confidence != "exact" {
		t.Errorf("shallowest, strongest reach is not first: %+v", c.BlastRadius)
	}
	if first.Target != "processCallback" {
		t.Errorf("reached symbol does not record the target it was reached from: %+v", first)
	}
	if first.Evidence == "" {
		t.Error("a direct reach lost its evidence")
	}
	if c.ImpactFloor != "medium" || c.ImpactHidden != 9 {
		t.Errorf("floor = %q hidden = %d, want the analysis's own filter carried through", c.ImpactFloor, c.ImpactHidden)
	}
	if len(c.Tests) != 1 || c.Tests[0].Covers != "processCallback" || c.Tests[0].Signal != "graph" {
		t.Errorf("named tests lost their signal or their subject: %+v", c.Tests)
	}

	// Only symbols are asked about, in scope order, and a file entity is never
	// used as a traversal target.
	if len(imp.asked) != 2 || imp.asked[0] != "processCallback" {
		t.Errorf("impact was asked %v, want the ranked scope symbols in order", imp.asked)
	}

	for _, want := range []string{
		"confidence floor", "9 symbol(s)", "lower bound",
		"could not resolve \"ChargeService\"",
	} {
		if !unknownContaining(c, want) {
			t.Errorf("unknowns do not state %q: %+v", want, c.Unknowns)
		}
	}
}

// TestReachOutsideScopeIsARiskWithEvidence: the difference between what the
// change edits and what it touches is the whole reason to consult impact.
func TestReachOutsideScopeIsARiskWithEvidence(t *testing.T) {
	ports, _, _ := stubPorts()
	c := compileWithPorts(t, ports)

	r, ok := riskContaining(c, "outside Scope")
	if !ok {
		t.Fatalf("reach outside the scope raised no risk: %+v", c.Risks)
	}
	if !strings.Contains(r.Evidence, "src/ledger/LedgerWriter.java") {
		t.Errorf("risk evidence does not name the symbol outside scope: %q", r.Evidence)
	}
	if strings.Contains(r.Evidence, "src/service/ChargeService.java") {
		t.Errorf("a symbol already in scope was reported as outside it: %q", r.Evidence)
	}
	// Only the depth-2 medium reach is outside scope here, so the risk is
	// medium: severity follows the STRENGTH of the relationship, never the
	// count of them.
	if r.Severity != severityMedium {
		t.Errorf("severity = %q, want %q for an inferred depth-2 reach", r.Severity, severityMedium)
	}
}

// TestAProvenDirectCallerOutsideScopeRaisesTheSeverity is the other half of the
// rule above: one compiler-proven caller nobody listed is a harder fact than
// nine inferred ones.
func TestAProvenDirectCallerOutsideScopeRaisesTheSeverity(t *testing.T) {
	c := compileWithPorts(t, Ports{Impact: &stubImpact{byTarget: map[string]Reach{
		"processCallback": {
			Resolved: true, Target: "processCallback", Floor: "medium",
			Callers: []Reached{{
				Symbol: "insert", Path: "src/repo/PaymentRepository.java",
				Depth: 1, Confidence: "exact", Evidence: "src/repo/PaymentRepository.java:31",
			}},
		},
	}}})
	r, ok := riskContaining(c, "outside Scope")
	if !ok {
		t.Fatalf("no risk raised: %+v", c.Risks)
	}
	if r.Severity != severityHigh {
		t.Errorf("severity = %q, want %q for a proven depth-1 caller outside scope", r.Severity, severityHigh)
	}
}

// TestMarkdownRendersTheNewSectionsAndOnlyWhenPopulated.
func TestMarkdownRendersTheNewSectionsAndOnlyWhenPopulated(t *testing.T) {
	ports, _, _ := stubPorts()
	md := compileWithPorts(t, ports).Markdown()
	for _, want := range []string{"## Blast radius", "## Tests", "depth 1, exact confidence", "found by graph"} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered contract lacks %q:\n%s", want, md)
		}
	}
	if empty := emptySections(md); len(empty) > 0 {
		t.Errorf("empty sections rendered: %v", empty)
	}

	bare := compileOrFail(t, "do a thing", portedPack(), Options{}).Markdown()
	if strings.Contains(bare, "## Blast radius") || strings.Contains(bare, "## Tests") {
		t.Errorf("sections rendered with nothing in them:\n%s", bare)
	}
}

// TestAFailedImpactAnalysisFailsCompilation rather than degrading silently.
func TestAFailedImpactAnalysisFailsCompilation(t *testing.T) {
	want := errors.New("registry unavailable")
	_, err := CompileWith(context.Background(), "do a thing", portedPack(), Options{},
		Ports{Impact: &stubImpact{err: want}})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want it to wrap %v", err, want)
	}
}

// TestPortedContractIsByteIdenticalAcrossRuns.
func TestPortedContractIsByteIdenticalAcrossRuns(t *testing.T) {
	ports, _, _ := stubPorts()
	first := compileWithPorts(t, ports).Markdown()
	for i := 0; i < 5; i++ {
		fresh, _, _ := stubPorts()
		if again := compileWithPorts(t, fresh).Markdown(); again != first {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i+1, first, again)
		}
	}
}

// TestCompileIsUnchangedWithoutPorts pins that the ports are additive: the
// existing entry point must still produce the contract it always did.
func TestCompileIsUnchangedWithoutPorts(t *testing.T) {
	viaCompile, err := Compile("do a thing", richPack(), Options{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	viaPorts, err := CompileWith(context.Background(), "do a thing", richPack(), Options{}, Ports{})
	if err != nil {
		t.Fatalf("CompileWith: %v", err)
	}
	if viaCompile.Markdown() != viaPorts.Markdown() {
		t.Error("Compile and a zero-Ports CompileWith disagree")
	}
}

func unknownContaining(c Contract, substr string) bool {
	for _, u := range c.Unknowns {
		if strings.Contains(u, substr) {
			return true
		}
	}
	return false
}
