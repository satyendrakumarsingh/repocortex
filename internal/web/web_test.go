package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The tests here drive the REAL registry and the REAL graph over a seeded
// SQLite index, through captest — the same fixture builder capability/impact
// uses. A fake Registry would prove only that the fake agreed with the
// assertion; the claims worth testing are that confidence and evidence survive
// the whole way from a database row to a JSON field, and that a truncation is
// impossible to publish without a count beside it. Neither is testable against
// a stub.

// fixture seeds an index with a deliberate spread of confidences and returns a
// server over it.
//
// The shape matters. `hub` is called by four symbols at four different tiers,
// so a default (medium) floor MUST hide exactly the `low` caller and MUST say
// so. `orders_table` is written by one of the callers, and `GET /orders` exposes
// another, which is what the blast-radius facets read.
func fixture(t *testing.T, opts ...Option) (*Server, capability.Deps, int64) {
	t.Helper()

	syms := []captest.Sym{
		captest.S(1, "hub", "internal/core/hub.go").Export(),
		captest.S(2, "callerExact", "internal/core/exact.go").Export(),
		captest.S(3, "callerHigh", "internal/api/high.go").Export(),
		captest.S(4, "callerMedium", "internal/api/medium.go"),
		captest.S(5, "callerLow", "internal/api/low.go"),
		captest.S(6, "secondHop", "internal/api/second.go"),
		captest.S(7, "TestHub", "internal/core/hub_test.go"),
		captest.S(8, "orders", "db/schema.sql").WithKind("table"),
		captest.S(9, "GET /orders", "internal/api/routes.go").WithKind("endpoint"),
		captest.S(10, "lonely", "scripts/lonely.py"),
	}
	edges := []captest.Edge{
		captest.E(2, 1).WithConf(model.ConfidenceExact),
		captest.E(3, 1).WithConf(model.ConfidenceHigh),
		captest.E(4, 1).WithConf(model.ConfidenceMedium),
		captest.E(5, 1).WithConf(model.ConfidenceLow),
		captest.E(6, 3).WithConf(model.ConfidenceHigh),
		captest.E(7, 1).WithConf(model.ConfidenceHigh),
		// A caller writes a table, and an endpoint exposes another caller.
		{Src: 3, Dst: 8, Kind: model.EdgeWrites, Conf: model.ConfidenceHigh},
		{Src: 4, Dst: 8, Kind: model.EdgeReads, Conf: model.ConfidenceMedium},
		{Src: 9, Dst: 3, Kind: model.EdgeExposes, Conf: model.ConfidenceHigh},
	}
	deps, runID := captest.Build(t, syms, edges, captest.Refs{
		Resolved: 60, Unresolved: 30, Ambiguous: 6, Pending: 4,
	})

	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	srv := newServer(t, reg, deps, config.Config{ProjectName: "fixture"}, opts...)
	return srv, deps, runID
}

func newServer(t *testing.T, reg Registry, deps capability.Deps, cfg config.Config, opts ...Option) *Server {
	t.Helper()
	all := append([]Option{WithMaxNodes(DefaultMaxNodes)}, opts...)
	srv, err := New(Deps{
		Registry: reg, Caps: deps, Config: cfg, IndexPath: "/tmp/fixture/index.db",
	}, all...)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

// get performs a GET against the server and decodes the body into v.
func get(t *testing.T, srv *Server, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if v != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("GET %s: decode %d body %q: %v", path, rec.Code, rec.Body.String(), err)
		}
	}
	return rec
}

// raw decodes into a generic map so a test can assert a field is PRESENT, which
// a typed struct cannot distinguish from a zero value. An empty `totals: []`
// and "no such field" are different claims and the whole package rests on
// the difference.
func raw(t *testing.T, srv *Server, path string) map[string]any {
	t.Helper()
	var m map[string]any
	rec := get(t, srv, path, &m)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", path, rec.Code, rec.Body.String())
	}
	return m
}

// ---------------------------------------------------------------- overview

func TestWebOverviewReportsCoverageConfidenceAndBlindSpots(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got OverviewResponse
	rec := get(t, srv, "/api/overview", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !got.Meta.Indexed {
		t.Fatal("meta.indexed is false over a committed run")
	}
	if !got.Meta.ReadOnly {
		t.Error("meta.read_only must be true: the portal never writes")
	}
	if got.Fingerprint.Symbols != 10 {
		t.Errorf("fingerprint.symbols = %d, want 10", got.Fingerprint.Symbols)
	}

	// The confidence distribution is the stacked bar, and it must carry every
	// tier separately rather than a single "confident" number.
	// exact 1 (callerExact), high 5 (callerHigh, secondHop, TestHub, the WRITES
	// and the EXPOSES), medium 2 (callerMedium, the READS), low 1 (callerLow).
	h := got.Confidence.Edges
	if h.Exact != 1 || h.High != 5 || h.Medium != 2 || h.Low != 1 {
		t.Errorf("confidence histogram = %+v, want exact 1 / high 5 / medium 2 / low 1", h)
	}
	if h.Total != 9 {
		t.Errorf("histogram total = %d, want 9", h.Total)
	}
	if got.Confidence.Actionable != 6 {
		t.Errorf("actionable (high+) = %d, want 6", got.Confidence.Actionable)
	}

	// Resolution coverage is published as recorded, never estimated.
	if got.Resolution.Total != 100 || got.Resolution.Unresolved != 30 {
		t.Errorf("resolution = %+v, want total 100 / unresolved 30", got.Resolution)
	}
	if !strings.Contains(got.Resolution.Caveat, "LOWER BOUND") {
		t.Errorf("resolution caveat does not state the lower bound: %q", got.Resolution.Caveat)
	}

	// The legend is served as data so the client cannot invent its own mapping,
	// and it must carry the MEASURED precision rather than adjectives.
	if len(got.Legend) != 4 {
		t.Fatalf("legend has %d tiers, want 4", len(got.Legend))
	}
	byTier := map[string]Tier{}
	for _, tier := range got.Legend {
		byTier[tier.Confidence] = tier
	}
	if byTier["low"].DefaultVisible {
		t.Error("`low` must be off by default")
	}
	if !strings.Contains(byTier["medium"].Precision, "5.7") {
		t.Errorf("medium precision = %q, want the measured 5.7-37.5%% range", byTier["medium"].Precision)
	}
	if byTier["exact"].Glyph == "" {
		t.Error("`exact` must carry a glyph: it is the only tier a compiler proved")
	}

	// Every section ends in a next action.
	for name, a := range map[string]NextAction{
		"fingerprint": got.Fingerprint.Next,
		"languages":   got.Languages.Next,
		"resolution":  got.Resolution.Next,
		"confidence":  got.Confidence.Next,
		"blind_spots": got.BlindSpots.Next,
		"freshness":   got.Freshness.Next,
	} {
		if a.Label == "" {
			t.Errorf("section %q has no next action", name)
		}
	}
}

func TestWebOverviewBlindSpotsNamePerLanguageGaps(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got OverviewResponse
	get(t, srv, "/api/overview", &got)

	// scripts/lonely.py contributes a Python symbol and not a single edge, so
	// EVERY impact-carrying kind must be reported blind for python. This is the
	// measured absence an LLM-asserted graph cannot express.
	var pythonCalls *BlindSpot
	for i := range got.BlindSpots.Spots {
		s := got.BlindSpots.Spots[i]
		if s.Language == "python" && s.Kind == string(model.EdgeCalls) {
			pythonCalls = &got.BlindSpots.Spots[i]
		}
	}
	if pythonCalls == nil {
		t.Fatalf("no blind spot reported for (python, CALLS); got %+v", got.BlindSpots.Spots)
	}
	if pythonCalls.Severity != "blind" {
		t.Errorf("(python, CALLS) severity = %q, want blind", pythonCalls.Severity)
	}
	if !strings.Contains(pythonCalls.Detail, "not evidence that no such relationship exists") {
		t.Errorf("blind spot detail does not warn against reading empty as absent: %q", pythonCalls.Detail)
	}
	if got.BlindSpots.Blind == 0 {
		t.Error("blind_count is 0 despite a language with no edges at all")
	}
}

// ---------------------------------------------------------------- omissions

func TestWebOverviewStatesWhatItHidesRatherThanDropping(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	m := raw(t, srv, "/api/overview")
	om, ok := m["omissions"].(map[string]any)
	if !ok {
		t.Fatalf("overview has no omissions block: %v", m)
	}
	// PRESENCE, not truthiness. A missing field and a zero are different claims.
	totals, ok := om["totals"].([]any)
	if !ok {
		t.Fatal("omissions.totals is absent; the rollup must always be stated")
	}
	hidden := totalFor(t, totals, ReasonBelowConfidenceFloor)
	if hidden != 1 {
		t.Errorf("below_confidence_floor total = %v, want 1 (the single `low` edge below the default floor)", hidden)
	}
	if om["complete"].(bool) {
		t.Error("omissions.complete is true although a low edge and 40 unbound references were left out")
	}
	list, _ := om["omitted"].([]any)
	if len(list) == 0 {
		t.Fatal("omissions.omitted is empty although the floor total is non-zero")
	}
	// The rollup must be arithmetic over `omitted`, never a second population.
	// This is the defect that shipped: a hoisted count and the entries it came
	// from rendered as two separate omissions of the same 8,015 edges.
	sum := 0
	for _, item := range list {
		o := item.(map[string]any)
		if o["reason"] == ReasonBelowConfidenceFloor {
			sum += int(o["count"].(float64))
		}
		if o["label"] == nil || o["label"] == "" {
			t.Errorf("omission %v carries no human label; the raw key is not a UI string", o["reason"])
		}
	}
	if sum != hidden {
		t.Errorf("totals[below_confidence_floor] = %d but the entries sum to %d — the rollup and its "+
			"rows disagree, which is exactly how one omission reads as two", hidden, sum)
	}
	var sawFloor, sawUnresolved bool
	for _, item := range list {
		o := item.(map[string]any)
		switch o["reason"] {
		case ReasonBelowConfidenceFloor:
			sawFloor = true
			if o["remedy"] == nil || o["remedy"].(string) == "" {
				t.Error("the confidence-floor omission states no remedy")
			}
		case ReasonUnresolvedReferences:
			sawUnresolved = true
			if o["count"].(float64) != 40 {
				t.Errorf("unresolved omission count = %v, want 40", o["count"])
			}
		}
	}
	if !sawFloor {
		t.Error("no below_confidence_floor omission recorded")
	}
	if !sawUnresolved {
		t.Error("the resolution gap is not stated as an omission")
	}
}

func TestWebOmissionsCannotSerialiseAFalseCompleteness(t *testing.T) {
	t.Parallel()

	// The point of the custom marshaller: `complete` is derived, never assigned,
	// so a handler that sets it optimistically cannot publish the claim.
	var om Omissions
	om.Complete = true // a handler lying, deliberately
	om.Hide(ReasonRowBudget, 3, "three rows dropped", "ask for more")

	var got map[string]any
	buf, err := json.Marshal(om)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["complete"].(bool) {
		t.Errorf("complete survived as true despite a recorded omission: %s", buf)
	}

	// And the empty case must still emit an array plus an explicit zero, so a
	// client never has to distinguish null from "nothing hidden".
	buf, _ = json.Marshal(Omissions{})
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if got["omitted"] == nil {
		t.Errorf("omitted serialised as null rather than []: %s", buf)
	}
	if got["totals"] == nil {
		t.Errorf("totals serialised as null rather than [] on an empty block: %s", buf)
	}
	if !got["complete"].(bool) {
		t.Errorf("an empty block must report complete: %s", buf)
	}
}

func TestWebCappedRecordsWhatItRemoved(t *testing.T) {
	t.Parallel()
	var om Omissions
	got := capped([]int{1, 2, 3, 4, 5}, 2, &om, ReasonRowBudget, "detail", "remedy")
	if len(got) != 2 {
		t.Fatalf("capped returned %d rows, want 2", len(got))
	}
	if len(om.Omitted) != 1 || om.Omitted[0].Count != 3 {
		t.Fatalf("capped did not record the remainder: %+v", om.Omitted)
	}
	// Below the limit nothing is recorded, so the block does not fill with
	// "0 items hidden" noise that trains readers to skip it.
	var clean Omissions
	if got := capped([]int{1}, 5, &clean, ReasonRowBudget, "d", "r"); len(got) != 1 || len(clean.Omitted) != 0 {
		t.Fatalf("capped recorded an omission it did not make: %+v", clean.Omitted)
	}
}

// ---------------------------------------------------------------- graph

func TestWebGraphAggregatesEdgesAndKeepsEvidence(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got GraphResponse
	rec := get(t, srv, "/api/graph", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Level != LevelContainers {
		t.Errorf("default level = %q, want containers", got.Level)
	}
	if got.Options.ContainerKind != "directory" {
		t.Errorf("container kind = %q, want directory (this fixture declares no architecture)", got.Options.ContainerKind)
	}
	if len(got.Containers) == 0 {
		t.Fatal("no containers returned")
	}
	if len(got.ContainerEdges) == 0 {
		t.Fatal("no aggregated container edges returned")
	}

	// internal/api -> internal/core collapses four call edges (exact, high,
	// medium, low). At the default medium floor the `low` one is excluded, and
	// the surviving bundle must be drawn at its WEAKEST survivor, not its best.
	var bundle *ContainerEdge
	for i := range got.ContainerEdges {
		e := got.ContainerEdges[i]
		if e.From == "internal/api" && e.To == "internal/core" {
			bundle = &got.ContainerEdges[i]
		}
	}
	if bundle == nil {
		t.Fatalf("no internal/api -> internal/core bundle; got %+v", got.ContainerEdges)
	}
	if bundle.Confidence != string(model.ConfidenceMedium) {
		t.Errorf("bundle confidence = %q, want medium: a bundle is no stronger than its weakest survivor", bundle.Confidence)
	}
	if bundle.Histogram.Total == 0 {
		t.Error("bundle carries no histogram, so a thick line cannot be read as guesses versus proofs")
	}
	if bundle.Evidence.FilePath == "" {
		t.Error("aggregated edge has no example evidence, so the bundle is not checkable")
	}
	if len(bundle.Kinds) == 0 {
		t.Error("aggregated edge does not say which kinds it collapsed")
	}

	if got.Omissions.HiddenByFloor() == 0 {
		t.Error("the graph hid a `low` edge at the default floor without stating a count")
	}
	if got.Filters.MinConfidence != string(DefaultFloor) {
		t.Errorf("filters.min_confidence = %q, want the applied default %q", got.Filters.MinConfidence, DefaultFloor)
	}
	if got.Filters.Selection == "" {
		t.Error("the view does not state its selection rule")
	}
}

func TestWebGraphLowFloorRestoresTheHiddenEdges(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var floored, full GraphResponse
	get(t, srv, "/api/graph", &floored)
	get(t, srv, "/api/graph?min_confidence=low", &full)

	if floored.Omissions.HiddenByFloor() == 0 {
		t.Fatal("the default floor reported nothing hidden")
	}
	if full.Omissions.HiddenByFloor() != 0 {
		t.Errorf("at floor=low nothing can be below the floor, got %d hidden", full.Omissions.HiddenByFloor())
	}
	// The stated count must be honest: lowering the floor has to actually
	// produce the relationships it promised.
	flooredEdges, fullEdges := 0, 0
	for _, e := range floored.ContainerEdges {
		flooredEdges += e.Count
	}
	for _, e := range full.ContainerEdges {
		fullEdges += e.Count
	}
	if fullEdges <= flooredEdges {
		t.Errorf("floor=low returned %d edges, floor=medium returned %d — the hidden count was not honest",
			fullEdges, flooredEdges)
	}
}

func TestWebGraphDrillsIntoAContainer(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got GraphResponse
	get(t, srv, "/api/graph?container=internal/core", &got)
	if got.Level != LevelSymbols {
		t.Fatalf("level = %q, want symbols", got.Level)
	}
	if len(got.Nodes) == 0 {
		t.Fatal("no symbol nodes in the drilled-in view")
	}
	var hub *SymbolNode
	var external bool
	for i := range got.Nodes {
		if got.Nodes[i].Name == "hub" {
			hub = &got.Nodes[i]
		}
		if got.Nodes[i].External {
			external = true
		}
	}
	if hub == nil {
		t.Fatal("hub is missing from its own container")
	}
	if hub.Container != "internal/core" || !hub.Exported || hub.Degree == 0 {
		t.Errorf("hub node = %+v, want container internal/core, exported, non-zero degree", *hub)
	}
	if hub.Confidence == "" {
		t.Error("symbol node carries no extraction confidence")
	}
	if !external {
		t.Error("no boundary node was pulled in, so the container's edges to elsewhere are invisible")
	}
	for _, e := range got.Edges {
		if e.Evidence.FilePath == "" {
			t.Errorf("symbol edge %d->%d has no evidence: an edge without a location is an assertion", e.Src, e.Dst)
		}
		if e.Confidence == "" {
			t.Errorf("symbol edge %d->%d has no confidence", e.Src, e.Dst)
		}
	}
}

func TestWebGraphUnknownContainerGivesGuidanceNotAnError(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got GraphResponse
	rec := get(t, srv, "/api/graph?container=does/not/exist", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: an unknown container is a typo, not a fault", rec.Code)
	}
	if len(got.Omissions.Omitted) == 0 {
		t.Error("an unknown container produced no stated omission")
	}
}

// ---------------------------------------------------------------- blast radius

func TestWebBlastCarriesEvidenceConfidenceAndHiddenCount(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got BlastResponse
	rec := get(t, srv, "/api/blast?symbol=hub", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Status != BlastOK {
		t.Fatalf("status = %q, want ok (guidance %+v)", got.Status, got.Help)
	}
	if got.Target == nil || got.Target.Name != "hub" {
		t.Fatalf("target = %+v", got.Target)
	}

	// Direct callers at the default medium floor: exact, high, medium and the
	// test. The `low` caller must be excluded AND counted.
	if len(got.Direct) != 4 {
		t.Errorf("direct callers = %d, want 4 at the medium floor: %+v", len(got.Direct), got.Direct)
	}
	if got.Omissions.HiddenByFloor() != 1 {
		t.Errorf("blast hid %d by the floor, want 1 (callerLow)", got.Omissions.HiddenByFloor())
	}

	// Evidence and per-row confidence must survive to the client. This is the
	// whole differentiation: an edge without a location is an assertion.
	for _, d := range got.Direct {
		if d.Confidence == "" {
			t.Errorf("direct caller %s carries no confidence", d.Name)
		}
		if len(d.Evidence) == 0 {
			t.Errorf("direct caller %s carries no evidence", d.Name)
			continue
		}
		if d.Evidence[0].FilePath == "" {
			t.Errorf("direct caller %s has an evidence entry with no file path", d.Name)
		}
	}

	// The path-minimum, not the last hop: secondHop reaches hub through
	// callerHigh, so it is indirect at depth 2.
	var second *string
	for i := range got.Indirect {
		if got.Indirect[i].Name == "secondHop" {
			n := got.Indirect[i].Name
			second = &n
			if got.Indirect[i].Depth != 2 {
				t.Errorf("secondHop depth = %d, want 2", got.Indirect[i].Depth)
			}
		}
	}
	if second == nil {
		t.Errorf("secondHop missing from the indirect set: %+v", got.Indirect)
	}

	// The mandatory honesty block travels verbatim.
	if !got.Completeness.LowerBound {
		t.Error("completeness.lower_bound is false")
	}
	if !got.Completeness.HasCaveat(capability.LowerBoundCaveat) {
		t.Error("the lower-bound caveat did not survive to the client")
	}
	if !got.Completeness.HasCaveat(capability.DynamicDispatchCaveat) {
		t.Error("the dynamic-dispatch caveat did not survive to the client")
	}
	if len(got.Unknown) == 0 {
		t.Error("no unknowns reported; the resolution gap alone guarantees one")
	}
	if got.Risk.Level == "" || len(got.Risk.Factors) == 0 {
		t.Error("risk arrived without its inputs; a bare level is unactionable")
	}

	// The counts keep the three claims apart.
	if got.Counts.Impacted != len(got.Direct)+len(got.Indirect) {
		t.Errorf("counts.impacted = %d, want direct+indirect", got.Counts.Impacted)
	}
}

func TestWebBlastReportsTablesAndEndpoints(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got BlastResponse
	get(t, srv, "/api/blast?symbol=hub", &got)

	if len(got.TablesWritten) != 1 || got.TablesWritten[0].Name != "orders" {
		t.Fatalf("tables_written = %+v, want the orders table", got.TablesWritten)
	}
	w := got.TablesWritten[0]
	if w.Relation != string(model.EdgeWrites) {
		t.Errorf("facet relation = %q, want WRITES", w.Relation)
	}
	if w.Confidence != string(model.ConfidenceHigh) {
		t.Errorf("facet confidence = %q, want high", w.Confidence)
	}
	if w.Evidence.FilePath == "" {
		t.Error("facet has no evidence, so the claim cannot be checked")
	}
	if len(w.Through) == 0 {
		t.Error("facet does not name which impacted symbol reaches it")
	}

	if len(got.TablesRead) != 1 || got.TablesRead[0].Confidence != string(model.ConfidenceMedium) {
		t.Errorf("tables_read = %+v, want orders at medium", got.TablesRead)
	}
	if len(got.Endpoints) != 1 || got.Endpoints[0].Name != "GET /orders" {
		t.Errorf("endpoints_exposed = %+v, want GET /orders", got.Endpoints)
	}
}

func TestWebBlastFindsCoveringTests(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got BlastResponse
	get(t, srv, "/api/blast?symbol=hub", &got)

	if got.Tests.Untested {
		t.Fatal("hub is reported untested although TestHub calls it")
	}
	if got.Tests.ProvenByGraph == 0 {
		t.Error("no test was proven by a recorded edge, although TestHub -> hub is one")
	}
	var found bool
	for _, h := range got.Tests.Tests {
		if h.Name == "TestHub" {
			found = true
			if h.Confidence == "" || len(h.Signals) == 0 {
				t.Errorf("TestHub arrived without its signals or confidence: %+v", h)
			}
		}
	}
	if !found {
		t.Errorf("TestHub missing from the covering tests: %+v", got.Tests.Tests)
	}
	if !got.Tests.Completeness.LowerBound {
		t.Error("the test report's completeness block did not survive")
	}
}

func TestWebBlastAmbiguousReturnsCandidatesRatherThanGuessing(t *testing.T) {
	t.Parallel()

	// Two definitions of Close, which is exactly the case where picking one
	// would produce a confidently wrong answer.
	syms := []captest.Sym{
		captest.S(1, "Close", "internal/a/a.go"),
		captest.S(2, "Close", "internal/b/b.go"),
		captest.S(3, "user", "internal/c/c.go"),
	}
	deps, _ := captest.Build(t, syms, []captest.Edge{captest.E(3, 1)}, captest.Refs{Resolved: 1})
	srv := newServer(t, deps.Registry.(*registry.Registry), deps, config.Config{ProjectName: "amb"})

	var got BlastResponse
	rec := get(t, srv, "/api/blast?symbol=Close", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: ambiguity is a normal outcome", rec.Code)
	}
	if got.Status != BlastAmbiguous {
		t.Fatalf("status = %q, want ambiguous", got.Status)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want 2", got.Candidates)
	}
	if got.Help == nil || got.Help.Title == "" {
		t.Error("an ambiguous query returned no guidance")
	}
	for _, c := range got.Candidates {
		if c.FQN == "" || c.Path == "" {
			t.Errorf("candidate %+v cannot be re-asked: no FQN or path", c)
		}
	}
}

func TestWebBlastNotFoundGivesGuidanceNotAFault(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got BlastResponse
	rec := get(t, srv, "/api/blast?symbol=NoSuchSymbolAnywhere", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: a typo is not a server fault", rec.Code)
	}
	if got.Status != BlastNotFound {
		t.Fatalf("status = %q, want not_found", got.Status)
	}
	if got.Help == nil || len(got.Help.Steps) == 0 {
		t.Error("a missing symbol produced no guidance")
	}
}

func TestWebBlastWithoutASymbolAsksForOne(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got BlastResponse
	rec := get(t, srv, "/api/blast", &got)
	if rec.Code != http.StatusOK || got.Status != BlastNoQuery {
		t.Fatalf("status %d / %q, want 200 / no_query", rec.Code, got.Status)
	}
	if got.Help == nil {
		t.Error("no guidance for an empty query")
	}
}

// ---------------------------------------------------------------- governance

func TestWebGovernanceWithoutAModelRefusesToReportAPass(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t) // config carries no architecture

	var got GovernanceResponse
	rec := get(t, srv, "/api/governance", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Declared {
		t.Fatal("architecture_declared is true although the fixture declares none")
	}
	if len(got.Violations) != 0 {
		t.Error("violations reported against a model that does not exist")
	}
	if got.Help == nil || !strings.Contains(got.Help.Detail, "DECLARED") {
		t.Errorf("guidance does not explain that components are declared, not inferred: %+v", got.Help)
	}
	if got.Next.Command == "" {
		t.Error("no next action offered for an undeclared architecture")
	}
}

func TestWebGovernanceReportsViolationsWithEvidenceAndGate(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		ProjectName: "governed",
		Architecture: arch.Model{
			Components: []arch.Component{
				{Name: "api", Include: []string{"internal/api/**"}},
				{Name: "core", Include: []string{"internal/core/**"}},
				{Name: "ghost", Include: []string{"nowhere/**"}},
			},
			Rules: []arch.Rule{{
				ID: "api-must-not-touch-core", From: []string{"api"},
				Forbidden: []string{"core"}, Severity: "high",
			}},
		},
	}
	srv, _, _ := fixture(t)
	srv.deps.Config = cfg
	srv.cached, srv.cachedID = nil, 0 // the container grouping changes with the model

	var got GovernanceResponse
	get(t, srv, "/api/governance", &got)

	if !got.Declared {
		t.Fatal("architecture_declared is false although components are declared")
	}
	if len(got.Violations) == 0 {
		t.Fatalf("no violations although internal/api calls internal/core: %+v", got)
	}
	v := got.Violations[0]
	if v.Confidence == "" {
		t.Error("a violation arrived without the confidence of the relationship behind it")
	}
	if v.FilePath == "" {
		t.Error("a violation arrived without evidence")
	}
	if v.RuleID != "api-must-not-touch-core" {
		t.Errorf("violation rule = %q", v.RuleID)
	}
	if got.ViolationsByConfidence.Total != len(got.Violations) {
		t.Errorf("violation histogram total = %d, want %d", got.ViolationsByConfidence.Total, len(got.Violations))
	}

	// The CI gate projection: `--fail-on high` must be failing here.
	if len(got.Gates) != 3 {
		t.Fatalf("gates = %+v, want one per severity", got.Gates)
	}
	var high *GateOutcome
	for i := range got.Gates {
		if got.Gates[i].FailOn == "high" {
			high = &got.Gates[i]
		}
	}
	if high == nil || high.Passes {
		t.Errorf("the high gate should be failing: %+v", high)
	}
	if !strings.Contains(high.Command, "--fail-on high") {
		t.Errorf("gate does not name the command: %q", high.Command)
	}

	// A component matching nothing is the most useful finding on the screen.
	if got.EmptyComponents == 0 {
		t.Error("the `ghost` component matched nothing and was not reported as empty")
	}
	// And unassigned symbols must be stated as an omission, since no rule saw them.
	var sawUnassigned bool
	for _, o := range got.Omissions.Omitted {
		if o.Reason == ReasonOutsideDeclaredComponents {
			sawUnassigned = true
		}
	}
	if !sawUnassigned && got.Unassigned > 0 {
		t.Error("symbols outside every component were not stated as an omission")
	}
}

// ---------------------------------------------------------------- no index

// TestWebUnindexedRepoYieldsGuidanceNotA500 is the case a reader meets first.
func TestWebUnindexedRepoYieldsGuidanceNotA500(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	srv := newServer(t, reg, capability.NewDeps(reg, graph.New(db)), config.Config{ProjectName: "empty"})

	for _, path := range []string{"/api/overview", "/api/graph", "/api/blast?symbol=x", "/api/governance", "/api/symbols?q=x"} {
		var m map[string]any
		rec := get(t, srv, path, &m)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200 with guidance; body %s", path, rec.Code, rec.Body.String())
			continue
		}
		meta, ok := m["meta"].(map[string]any)
		if !ok {
			t.Errorf("GET %s: no meta block", path)
			continue
		}
		if meta["indexed"].(bool) {
			t.Errorf("GET %s: meta.indexed is true on a store with no committed run", path)
		}
		g, ok := meta["guidance"].(map[string]any)
		if !ok {
			t.Errorf("GET %s: no guidance for an unindexed repository", path)
			continue
		}
		steps, _ := g["steps"].([]any)
		if len(steps) == 0 {
			t.Errorf("GET %s: guidance names no commands to run", path)
			continue
		}
		var mentionsIndex bool
		for _, s := range steps {
			if strings.Contains(s.(string), "repocortex index") {
				mentionsIndex = true
			}
		}
		if !mentionsIndex {
			t.Errorf("GET %s: guidance does not tell the reader to run `repocortex index`: %v", path, steps)
		}
	}

	// Health must still answer, because "the server is broken" and "there is no
	// index" have to be distinguishable.
	var h map[string]any
	if rec := get(t, srv, "/api/health", &h); rec.Code != http.StatusOK || h["status"] != "ok" {
		t.Errorf("health on an unindexed repo: %d %v", rec.Code, h)
	}
}

// ---------------------------------------------------------------- guards

func TestWebIsReadOnlyAtTheProtocolLevel(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/overview", nil)
		req.Host = "127.0.0.1:7777"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/overview: status %d, want 405", method, rec.Code)
		}
	}
}

func TestWebRefusesNonLoopbackHostHeaders(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	// DNS rebinding: a public page resolves its own name to 127.0.0.1 and reads
	// this API with the victim's browser. The Host header is what that attack
	// cannot forge into a loopback name.
	for _, host := range []string{"evil.example.com", "attacker.test:7777", "10.0.0.5:7777"} {
		req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, rec.Code)
		}
	}
	for _, host := range []string{"localhost:7777", "127.0.0.1:7777", "[::1]:7777", "sub.localhost:7777"} {
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: status %d, want 200", host, rec.Code)
		}
	}

	// With an explicit non-loopback bind the operator has already accepted the
	// exposure, and the guard cannot work there anyway.
	open, _, _ := fixture(t, WithAllowRemoteHosts(true))
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "10.0.0.5:7777"
	rec := httptest.NewRecorder()
	open.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with WithAllowRemoteHosts(true): status %d, want 200", rec.Code)
	}
}

func TestWebServesTheEmbeddedPortalWithoutAnyRemoteOrigin(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	rec := get(t, srv, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status %d", rec.Code)
	}
	body := rec.Body.String()
	// The shell must load its own script and stylesheet and reach for nothing
	// else. What it DRAWS with them is the client's business; asserting a
	// specific element here made this test fail on a frontend refactor that had
	// nothing to do with the property it exists to protect. A module served
	// with the wrong content type fails silently in the browser, leaving an
	// empty rail, and this is the only place that failure would be caught.
	for _, want := range []string{"/assets/app.js", "/assets/app.css", "/assets/js/main.mjs"} {
		if !strings.Contains(body, want) {
			t.Errorf("the shell does not load %s", want)
		}
	}
	// No remote FETCH. The SVG namespace URI in a data: favicon is a name, not
	// an origin — nothing is retrieved from it — so the check is on the
	// attributes that actually load something.
	for _, attr := range []string{`src="http`, `src='http`, `href="http`, `href='http`, "//cdn", "unpkg", "jsdelivr"} {
		if strings.Contains(body, attr) {
			t.Errorf("the shell loads from a remote origin (%q); the portal is offline by construction", attr)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP does not confine the page to this origin: %q", csp)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}

	for _, asset := range []string{"/assets/app.js", "/assets/app.css", "/assets/js/main.mjs", "/assets/js/nav.mjs"} {
		r := get(t, srv, asset, nil)
		if r.Code != http.StatusOK || r.Body.Len() == 0 {
			t.Errorf("GET %s: status %d, %d bytes", asset, r.Code, r.Body.Len())
		}
		if strings.HasSuffix(asset, ".mjs") {
			if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
				t.Errorf("GET %s: Content-Type = %q, want prefix text/javascript", asset, ct)
			}
		}
	}

	// No npm, no CDN: nothing in the served assets may reference another host.
	for _, asset := range []string{"/", "/assets/app.js", "/assets/app.css", "/assets/js/main.mjs", "/assets/js/nav.mjs"} {
		text := get(t, srv, asset, nil).Body.String()
		for _, bad := range []string{"https://", "http://cdn", "unpkg.com", "jsdelivr", "googleapis"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s references a remote origin (%q); the portal must work offline", asset, bad)
			}
		}
	}
}

func TestWebUnknownAPIRouteIsAJSON404(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var m map[string]any
	rec := get(t, srv, "/api/nope", &m)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if m["endpoints"] == nil {
		t.Error("the 404 does not name the routes that do exist")
	}
}

// ---------------------------------------------------------------- symbols

func TestWebSymbolSearchOrdersMechanicallyAndStatesTheRule(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var got SymbolsResponse
	get(t, srv, "/api/symbols?q=caller", &got)
	if len(got.Matches) == 0 {
		t.Fatal("no matches for `caller`")
	}
	if got.Ordering == "" {
		t.Error("the ordering rule is not stated")
	}
	for _, m := range got.Matches {
		if !strings.Contains(strings.ToLower(m.Name), "caller") {
			t.Errorf("unexpected match %q", m.Name)
		}
		if m.FQN == "" || m.Path == "" {
			t.Errorf("match %q cannot be used to re-ask: %+v", m.Name, m)
		}
	}

	// An exact name beats a prefix beats a substring.
	var exact SymbolsResponse
	get(t, srv, "/api/symbols?q=hub", &exact)
	if len(exact.Matches) == 0 || exact.Matches[0].Name != "hub" {
		t.Errorf("exact name did not sort first: %+v", exact.Matches)
	}
}

// ---------------------------------------------------------------- internals

func TestWebHistogramFloorArithmeticCountsUnknownAsHidden(t *testing.T) {
	t.Parallel()

	var h Histogram
	h.add(model.ConfidenceExact)
	h.add(model.ConfidenceHigh)
	h.add(model.ConfidenceMedium)
	h.add(model.ConfidenceLow)
	h.add(model.Confidence("")) // a row the index never labelled

	if h.Total != 5 || h.Unknown != 1 {
		t.Fatalf("histogram = %+v", h)
	}
	if got := h.AtOrAbove(model.ConfidenceMedium); got != 3 {
		t.Errorf("AtOrAbove(medium) = %d, want 3", got)
	}
	// An unlabelled row cannot be shown to clear a floor, so it must count as
	// hidden rather than quietly surviving one.
	if got := h.Below(model.ConfidenceMedium); got != 2 {
		t.Errorf("Below(medium) = %d, want 2 (low + unknown)", got)
	}
}

func TestWebStructuralIndexIsCachedPerRun(t *testing.T) {
	t.Parallel()
	srv, _, runID := fixture(t)

	ctx := context.Background()
	a, err := srv.structural(ctx, runID)
	if err != nil {
		t.Fatalf("structural: %v", err)
	}
	b, err := srv.structural(ctx, runID)
	if err != nil {
		t.Fatalf("structural: %v", err)
	}
	if a != b {
		t.Error("the structural index was rebuilt for the same run; three whole-run scans per request is what the cache exists to avoid")
	}
	// A different run id must not be served from the cache.
	if c, err := srv.structural(ctx, runID+1); err == nil && c == a {
		t.Error("a different run was served the cached index")
	}
}

// totalFor reads one reason out of the serialised rollup.
func totalFor(t *testing.T, totals []any, reason string) int {
	t.Helper()
	for _, item := range totals {
		o, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if o["reason"] == reason {
			return int(o["count"].(float64))
		}
	}
	return 0
}
