package architecture

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The fixture is the shape the capability exists for: an endpoint that never
// mentions the cardholder table, reaching it through a handler and a service.
//
//	GET /accounts/{id}  --EXPOSES-->  handle
//	handle              --CALLS---->  AccountService.save
//	AccountService.save --WRITES--->  card_tokens   (classified pci)
//
// Plus an unclassified table, an endpoint in no component, and a second, longer
// but stronger route used by the confidence tests.
func dataFixture() (*fakeRegistry, arch.Model) {
	reg := &fakeRegistry{
		files: []model.File{
			{ID: 1, Path: "src/api/Controller.java"},
			{ID: 2, Path: "src/service/AccountService.java"},
			{ID: 3, Path: "src/repository/CardRepo.java"},
			{ID: 4, Path: "db/schema.sql"},
			{ID: 5, Path: "scripts/admin.go"}, // in no declared component
		},
		symbols: []model.Symbol{
			{ID: 100, FileID: 1, Kind: model.KindEndpoint, Name: "GET /accounts/{id}"},
			{ID: 101, FileID: 1, Kind: model.KindMethod, Name: "handle"},
			{ID: 200, FileID: 2, Kind: model.KindMethod, Name: "AccountService.save"},
			{ID: 300, FileID: 3, Kind: model.KindMethod, Name: "CardRepo.persist"},
			{ID: 900, FileID: 4, Kind: model.KindTable, Name: "card_tokens"},
			{ID: 901, FileID: 4, Kind: model.KindTable, Name: "audit_log"},
			{ID: 500, FileID: 5, Kind: model.KindEndpoint, Name: "GET /admin/dump"},
		},
	}
	m := arch.Model{
		Components: []arch.Component{
			{Name: "api", Include: []string{"src/api/**"}},
			{Name: "payments", Include: []string{"src/service/**", "src/repository/**"}},
		},
		Data: []arch.DataClass{
			{Name: "pci", Include: []string{"card_*"}},
		},
	}
	return reg, m
}

func dedge(src, dst int64, kind model.EdgeKind, conf model.Confidence, path string, line int) model.Edge {
	return model.Edge{
		SrcID: src, DstID: dst, Kind: kind, Confidence: conf,
		Evidence: model.Evidence{
			FilePath: path,
			Range:    model.Range{Start: model.Position{Line: line}},
			Source:   model.SourceSourceCode,
		},
	}
}

// twoHopRoute is the canonical path: endpoint → handler → service → table.
func twoHopRoute(conf ...model.Confidence) []model.Edge {
	for len(conf) < 3 {
		conf = append(conf, model.ConfidenceHigh)
	}
	return []model.Edge{
		dedge(100, 101, model.EdgeExposes, conf[0], "src/api/Controller.java", 12),
		dedge(101, 200, model.EdgeCalls, conf[1], "src/api/Controller.java", 31),
		dedge(200, 900, model.EdgeWrites, conf[2], "src/service/AccountService.java", 88),
	}
}

func onlyPath(t *testing.T, rep Report) DataPath {
	t.Helper()
	if len(rep.DataPaths) != 1 {
		t.Fatalf("got %d data paths, want 1: %+v", len(rep.DataPaths), rep.DataPaths)
	}
	return rep.DataPaths[0]
}

// TestEndpointReachesClassifiedTableReportsThePath is the whole capability: not
// "this endpoint touches PCI", which nobody can act on, but the route, with a
// file and line on every hop.
func TestEndpointReachesClassifiedTableReportsThePath(t *testing.T) {
	reg, m := dataFixture()
	reg.edges = twoHopRoute()

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	p := onlyPath(t, rep)

	if p.Endpoint != "GET /accounts/{id}" || p.Table != "card_tokens" {
		t.Fatalf("path misattributed: %+v", p)
	}
	if p.EndpointComponent != "api" {
		t.Errorf("origin component = %q, want api", p.EndpointComponent)
	}
	if len(p.Classifications) != 1 || p.Classifications[0] != "pci" {
		t.Errorf("classification not carried: %v", p.Classifications)
	}
	if p.Access != "write" {
		t.Errorf("access = %q, want write", p.Access)
	}

	// The path itself, in order, is the finding.
	via := p.Via()
	if len(via) != 2 || via[0] != "handle" || via[1] != "AccountService.save" {
		t.Fatalf("via = %v, want [handle AccountService.save]", via)
	}
	if len(p.Steps) != 3 {
		t.Fatalf("got %d steps, want 3: %+v", len(p.Steps), p.Steps)
	}
	want := []struct {
		symbol, kind, file string
		line               int
	}{
		{"handle", "EXPOSES", "src/api/Controller.java", 12},
		{"AccountService.save", "CALLS", "src/api/Controller.java", 31},
		{"card_tokens", "WRITES", "src/service/AccountService.java", 88},
	}
	for i, w := range want {
		s := p.Steps[i]
		if s.Symbol != w.symbol || s.Kind != w.kind || s.FilePath != w.file || s.Line != w.line {
			t.Errorf("step %d = %+v, want %v", i, s, w)
		}
	}
	// Every hop must be independently checkable, or the path asks to be taken
	// on faith.
	for i, s := range p.Steps {
		if s.FilePath == "" || s.Line == 0 {
			t.Errorf("step %d cites no source location: %+v", i, s)
		}
	}

	rendered := p.String()
	for _, frag := range []string{
		`endpoint "GET /accounts/{id}"`,
		`pci table "card_tokens"`,
		"(write)",
		"via handle → AccountService.save",
		"confidence high",
	} {
		if !strings.Contains(rendered, frag) {
			t.Errorf("rendered path %q is missing %q", rendered, frag)
		}
	}
}

// TestUnclassifiedTableProducesNothing. A table nobody labelled cannot produce
// a finding — and must be counted, because it is the largest way this check can
// be wrong.
func TestUnclassifiedTableProducesNothing(t *testing.T) {
	reg, m := dataFixture()
	reg.edges = []model.Edge{
		dedge(100, 101, model.EdgeExposes, model.ConfidenceHigh, "src/api/Controller.java", 12),
		dedge(101, 200, model.EdgeCalls, model.ConfidenceHigh, "src/api/Controller.java", 31),
		dedge(200, 901, model.EdgeWrites, model.ConfidenceHigh, "src/service/AccountService.java", 90), // audit_log
	}
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataPaths) != 0 {
		t.Fatalf("an unclassified table produced a finding: %+v", rep.DataPaths)
	}
	if rep.DataCoverage.UnclassifiedTables != 1 || rep.DataCoverage.ClassifiedTables != 1 {
		t.Errorf("table coverage not reported: %+v", rep.DataCoverage)
	}
	var mentioned bool
	for _, c := range rep.DataCoverage.Caveats() {
		if strings.Contains(c, "no classification") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the unchecked table was not surfaced as a caveat: %v", rep.DataCoverage.Caveats())
	}
}

// TestPathConfidenceIsTheMinimumAlongIt is the house rule, and the reason this
// check can be believed. A path is only as believed as its weakest hop.
func TestPathConfidenceIsTheMinimumAlongIt(t *testing.T) {
	cases := []struct {
		name    string
		confs   []model.Confidence
		want    model.Confidence
		weakest int
	}{
		{"all strong", []model.Confidence{model.ConfidenceHigh, model.ConfidenceHigh, model.ConfidenceHigh}, model.ConfidenceHigh, 0},
		{"one medium hop in the middle", []model.Confidence{model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceHigh}, model.ConfidenceMedium, 1},
		{"a weak first hop", []model.Confidence{model.ConfidenceLow, model.ConfidenceHigh, model.ConfidenceHigh}, model.ConfidenceLow, 0},
		{"a weak last hop", []model.Confidence{model.ConfidenceHigh, model.ConfidenceHigh, model.ConfidenceLow}, model.ConfidenceLow, 2},
		{"the weakest of two weak hops", []model.Confidence{model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow}, model.ConfidenceLow, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, m := dataFixture()
			reg.edges = twoHopRoute(tc.confs...)

			rep, err := Check(context.Background(), reg, 1, m, "")
			if err != nil {
				t.Fatal(err)
			}
			p := onlyPath(t, rep)
			if p.Confidence != string(tc.want) {
				t.Fatalf("path confidence = %q, want %q (the minimum along %v)", p.Confidence, tc.want, tc.confs)
			}
			if p.Weakest != tc.weakest {
				t.Errorf("weakest step = %d, want %d", p.Weakest, tc.weakest)
			}
			step, from, ok := p.WeakestHop()
			if !ok || step.Confidence != string(tc.want) {
				t.Errorf("WeakestHop = %+v (from %q), want the %s hop", step, from, tc.want)
			}
			// The qualification has to travel with the claim.
			if !strings.Contains(p.String(), "confidence "+string(tc.want)) {
				t.Errorf("rendered path does not carry its confidence: %s", p.String())
			}
		})
	}
}

// TestAWeakHopIsNotLaunderedIntoACompliance Claim. A route that crosses one
// heuristic edge is a lead, not a fact: it must be reported at the weak
// confidence, and it must disappear behind a confidence floor rather than being
// promoted by the strong hops around it.
func TestAWeakHopIsNotLaunderedIntoAComplianceClaim(t *testing.T) {
	reg, m := dataFixture()
	// A rule that would make this a high-severity finding, so the test proves
	// the weak edge is not laundered even when a rule is desperate to fire.
	m.DataRules = []arch.DataRule{{
		ID: "pci-origin", Classification: []string{"pci"},
		AllowedFrom: []string{"payments"}, Severity: "high",
	}}
	// The middle hop is a name-match guess.
	reg.edges = twoHopRoute(model.ConfidenceHigh, model.ConfidenceLow, model.ConfidenceExact)

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	p := onlyPath(t, rep)
	if p.Confidence != string(model.ConfidenceLow) {
		t.Fatalf("a path over a low hop was reported at %q; a guess became a compliance claim", p.Confidence)
	}
	if len(rep.DataViolations) != 1 {
		t.Fatalf("got %d data violations, want 1: %+v", len(rep.DataViolations), rep.DataViolations)
	}
	if rep.DataViolations[0].Path.Confidence != string(model.ConfidenceLow) {
		t.Errorf("the violation dropped the weak confidence: %+v", rep.DataViolations[0])
	}

	// And behind a floor it is suppressed, counted, and not silently dropped.
	rep, err = Check(context.Background(), reg, 1, m, model.ConfidenceHigh)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataPaths) != 0 || len(rep.DataViolations) != 0 {
		t.Fatalf("a low-confidence path survived --min-confidence high: %+v", rep.DataPaths)
	}
	if rep.DataCoverage.PathsBelowFloor != 1 {
		t.Fatalf("the hidden path was not counted: %+v", rep.DataCoverage)
	}
	var mentioned bool
	for _, c := range rep.DataCoverage.Caveats() {
		if strings.Contains(c, "min-confidence") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("the confidence floor hid a path without saying so: %v", rep.DataCoverage.Caveats())
	}
}

// TestTheStrongestRouteIsReportedNotTheShortest. Two routes to the same table:
// a short one over a guess and a longer one that is proven throughout. Taking
// the shortest would understate the exposure as a lead when a fact exists.
func TestTheStrongestRouteIsReportedNotTheShortest(t *testing.T) {
	reg, m := dataFixture()
	reg.edges = []model.Edge{
		dedge(100, 101, model.EdgeExposes, model.ConfidenceHigh, "src/api/Controller.java", 12),
		// Short route, one weak hop.
		dedge(101, 300, model.EdgeCalls, model.ConfidenceLow, "src/api/Controller.java", 40),
		// Long route, strong throughout.
		dedge(101, 200, model.EdgeCalls, model.ConfidenceHigh, "src/api/Controller.java", 31),
		dedge(200, 300, model.EdgeCalls, model.ConfidenceHigh, "src/service/AccountService.java", 60),
		dedge(300, 900, model.EdgeWrites, model.ConfidenceHigh, "src/repository/CardRepo.java", 15),
	}
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	p := onlyPath(t, rep)
	if p.Confidence != string(model.ConfidenceHigh) {
		t.Fatalf("confidence = %q, want high: the proven route was passed over for the guess", p.Confidence)
	}
	via := p.Via()
	if len(via) != 3 || via[1] != "AccountService.save" {
		t.Fatalf("via = %v, want the three-hop proven route", via)
	}
}

// TestReadAndWriteAreSeparateFindings. A write to a cardholder table is a
// different regulatory fact from a read, and merging them loses one of them.
func TestReadAndWriteAreSeparateFindings(t *testing.T) {
	reg, m := dataFixture()
	reg.edges = []model.Edge{
		dedge(100, 101, model.EdgeExposes, model.ConfidenceHigh, "src/api/Controller.java", 12),
		dedge(101, 200, model.EdgeCalls, model.ConfidenceHigh, "src/api/Controller.java", 31),
		dedge(200, 900, model.EdgeWrites, model.ConfidenceHigh, "src/service/AccountService.java", 88),
		dedge(200, 900, model.EdgeReads, model.ConfidenceHigh, "src/service/AccountService.java", 92),
	}
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataPaths) != 2 {
		t.Fatalf("got %d paths, want one read and one write: %+v", len(rep.DataPaths), rep.DataPaths)
	}
	if rep.DataPaths[0].Access != "read" || rep.DataPaths[1].Access != "write" {
		t.Errorf("accesses = %q, %q; want read then write", rep.DataPaths[0].Access, rep.DataPaths[1].Access)
	}
}

// TestForbiddenOriginIsAViolationAndFailsTheGate. The commercial point: a CI
// job that fails on a forbidden import but not on an endpoint reaching
// cardholder data has its priorities inverted.
func TestForbiddenOriginIsAViolationAndFailsTheGate(t *testing.T) {
	reg, m := dataFixture()
	m.DataRules = []arch.DataRule{{
		ID: "pci-origin", Classification: []string{"pci"},
		Forbidden: []string{"api"}, Severity: "high",
	}}
	reg.edges = twoHopRoute()

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataViolations) != 1 {
		t.Fatalf("got %d data violations, want 1: %+v", len(rep.DataViolations), rep.DataViolations)
	}
	v := rep.DataViolations[0]
	if v.RuleID != "pci-origin" || v.Verdict != string(arch.VerdictForbidden) || v.Classification != "pci" {
		t.Errorf("violation misattributed: %+v", v)
	}
	if len(v.Path.Steps) != 3 {
		t.Errorf("the violation dropped the path, which is the finding: %+v", v.Path)
	}
	if !strings.Contains(v.String(), "via handle → AccountService.save") {
		t.Errorf("rendered violation does not name the route: %s", v.String())
	}
	if got := rep.HighestSeverity(); got != "high" {
		t.Fatalf("HighestSeverity = %q; a PCI exposure would not fail --fail-on high", got)
	}
}

// TestAnOriginOffTheAllowListIsGatedNotForbidden. The two verdicts triage
// differently: one is a rule someone wrote to stop exactly this, the other is a
// component nobody thought to list.
func TestAnOriginOffTheAllowListIsGatedNotForbidden(t *testing.T) {
	reg, m := dataFixture()
	m.DataRules = []arch.DataRule{{
		ID: "pci-origin", Classification: []string{"pci"},
		AllowedFrom: []string{"payments"}, Severity: "medium",
	}}
	reg.edges = twoHopRoute()

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataViolations) != 1 {
		t.Fatalf("got %d data violations, want 1: %+v", len(rep.DataViolations), rep.DataViolations)
	}
	if got := rep.DataViolations[0].Verdict; got != string(arch.VerdictNotAllowed) {
		t.Errorf("verdict = %q, want not_allowed", got)
	}
}

// TestAllowedOriginProducesAPathButNoViolation. The inventory is reported
// whether or not a rule covers it: the map of what can reach regulated data is
// the useful half of a compliance review.
func TestAllowedOriginProducesAPathButNoViolation(t *testing.T) {
	reg, m := dataFixture()
	m.DataRules = []arch.DataRule{{
		ID: "pci-origin", Classification: []string{"pci"},
		AllowedFrom: []string{"api"}, Severity: "high",
	}}
	reg.edges = twoHopRoute()

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataViolations) != 0 {
		t.Fatalf("an allow-listed origin was flagged: %+v", rep.DataViolations)
	}
	if len(rep.DataPaths) != 1 {
		t.Fatalf("the permitted path was omitted from the inventory: %+v", rep.DataPaths)
	}
}

// TestAnUnassignedEndpointIsListedNotJudged. No component means no rule can
// name it, and judging it would invent a boundary the model never declared —
// but hiding it would let the most dangerous case go unmentioned.
func TestAnUnassignedEndpointIsListedNotJudged(t *testing.T) {
	reg, m := dataFixture()
	m.DataRules = []arch.DataRule{{
		ID: "pci-origin", Classification: []string{"pci"},
		AllowedFrom: []string{"payments"}, Severity: "high",
	}}
	reg.edges = []model.Edge{
		dedge(500, 200, model.EdgeCalls, model.ConfidenceHigh, "scripts/admin.go", 9),
		dedge(200, 900, model.EdgeWrites, model.ConfidenceHigh, "src/service/AccountService.java", 88),
	}
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	p := onlyPath(t, rep)
	if p.Endpoint != "GET /admin/dump" || p.EndpointComponent != "" {
		t.Fatalf("unassigned endpoint path misattributed: %+v", p)
	}
	if len(rep.DataViolations) != 0 {
		t.Errorf("an endpoint in no component was judged: %+v", rep.DataViolations)
	}
	if rep.DataCoverage.UnassignedEndpoints != 1 {
		t.Errorf("the unjudged endpoint was not counted: %+v", rep.DataCoverage)
	}
}

// TestNoDataDeclarationMeansNoTraversal. Repositories that have not opted in
// pay nothing for this rule kind existing.
func TestNoDataDeclarationMeansNoTraversal(t *testing.T) {
	reg, m := dataFixture()
	m.Data = nil
	reg.edges = twoHopRoute()

	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataPaths) != 0 || rep.DataCoverage.ClassifiedTables != 0 {
		t.Fatalf("an unclassified repository was traversed anyway: %+v", rep.DataCoverage)
	}
	if rep.DataCoverage.UnclassifiedTables != 0 {
		t.Errorf("tables were counted for a model that classifies nothing: %+v", rep.DataCoverage)
	}
}

// TestContainsEdgesAreNotAPath. A file contains every symbol in it; traversing
// containment would make every endpoint reach everything its neighbours touch,
// and the report would be uniformly true and uniformly useless.
func TestContainsEdgesAreNotAPath(t *testing.T) {
	reg, m := dataFixture()
	reg.edges = []model.Edge{
		dedge(100, 101, model.EdgeExposes, model.ConfidenceHigh, "src/api/Controller.java", 12),
		dedge(101, 200, model.EdgeContains, model.ConfidenceHigh, "src/api/Controller.java", 31),
		dedge(200, 900, model.EdgeWrites, model.ConfidenceHigh, "src/service/AccountService.java", 88),
	}
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataPaths) != 0 {
		t.Fatalf("a path was routed through a CONTAINS edge: %+v", rep.DataPaths)
	}
}

// TestATableIsADestinationNotAWaypoint. Routing "through" a table would claim a
// data flow the graph never recorded.
func TestATableIsADestinationNotAWaypoint(t *testing.T) {
	reg, m := dataFixture()
	m.Data = append(m.Data, arch.DataClass{Name: "pii", Include: []string{"audit_log"}})
	reg.edges = []model.Edge{
		dedge(100, 101, model.EdgeExposes, model.ConfidenceHigh, "src/api/Controller.java", 12),
		dedge(101, 901, model.EdgeReads, model.ConfidenceHigh, "src/api/Controller.java", 31),
		dedge(901, 900, model.EdgeReferences, model.ConfidenceHigh, "db/schema.sql", 4),
	}
	rep, err := Check(context.Background(), reg, 1, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DataPaths) != 1 || rep.DataPaths[0].Table != "audit_log" {
		t.Fatalf("a path was routed through a table: %+v", rep.DataPaths)
	}
}

// TestCancellationStopsTheTraversal. Every graph walk honours ctx.
func TestCancellationStopsTheTraversal(t *testing.T) {
	reg, m := dataFixture()
	reg.edges = twoHopRoute()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Check(ctx, reg, 1, m, "")
	if err == nil {
		t.Fatal("a cancelled traversal returned a report as if it were complete")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// TestDataOutputIsDeterministic. This report lands in CI, and a diff that
// reorders on every run makes "did this get worse?" unanswerable.
func TestDataOutputIsDeterministic(t *testing.T) {
	reg, m := dataFixture()
	m.DataRules = []arch.DataRule{{
		ID: "pci-origin", Classification: []string{"pci"},
		AllowedFrom: []string{"payments"}, Severity: "high",
	}}
	reg.edges = append(twoHopRoute(), []model.Edge{
		dedge(101, 300, model.EdgeCalls, model.ConfidenceHigh, "src/api/Controller.java", 40),
		dedge(300, 900, model.EdgeReads, model.ConfidenceHigh, "src/repository/CardRepo.java", 15),
		dedge(500, 200, model.EdgeCalls, model.ConfidenceHigh, "scripts/admin.go", 9),
	}...)

	var first string
	for i := 0; i < 20; i++ {
		rep, err := Check(context.Background(), reg, 1, m, "")
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, p := range rep.DataPaths {
			b.WriteString(p.String())
			b.WriteString(";")
		}
		for _, v := range rep.DataViolations {
			b.WriteString(v.String())
			b.WriteString(";")
		}
		got := b.String()
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("data report order varies between runs:\n%s\n%s", first, got)
		}
	}
}
