package multirepo

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fixedClock keeps the staleness sentences deterministic. The fixture times
// below are offsets from it.
var fixedClock = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) int64 { return fixedClock.Add(-d).Unix() }

func build(t *testing.T, members []Member, opts ...Option) *Report {
	t.Helper()
	opts = append([]Option{
		WithClock(func() time.Time { return fixedClock }),
		WithRunningVersion(testVersion),
	}, opts...)
	f, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rep, err := f.Build(context.Background(), members)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return rep
}

// webAndAPI is the shape the whole feature exists for: a frontend that calls a
// route it does not declare, and a backend in a DIFFERENT index that declares
// it.
func webAndAPI(t *testing.T) []Member {
	t.Helper()

	web := &fakeSource{
		run: readyRun(1, "8f3c1ab9de", at(2*time.Hour)),
		calls: []registry.ClientCallUsage{
			{
				CallID: 1, FilePath: "src/orders.ts", Line: 12,
				Method: "GET", Path: "/api/orders", Client: "fetch",
				Caller: "useOrders", Binding: "unbound",
			},
			{
				CallID: 2, FilePath: "src/payments.ts", Line: 40,
				Method: "POST", Path: "/api/payments/42", Client: "axios",
				Caller: "pay", Binding: "unbound",
			},
			{
				CallID: 3, FilePath: "src/telemetry.ts", Line: 7,
				Method: "POST", Path: "https://collector.example.com/v1/traces",
				Client: "fetch", Caller: "sendSpan", Binding: "unbound",
			},
		},
	}

	api := &fakeSource{
		run: readyRun(1, "a19d7e04bc", at(3*time.Hour)),
		endpoints: []registry.Endpoint{
			{
				SymbolID: 100, Signature: "GET /api/orders",
				FilePath: "internal/http/orders.go", Line: 31,
				Handler: "ListOrders", HandlerID: 200, HandlerPath: "internal/http/orders.go",
			},
			{
				SymbolID: 101, Signature: "POST /api/payments/{id}",
				FilePath: "internal/http/payments.go", Line: 18,
				Handler: "Pay", HandlerID: 201, HandlerPath: "internal/http/payments.go",
			},
			{
				SymbolID: 102, Signature: "DELETE /api/admin/purge",
				FilePath: "internal/http/admin.go", Line: 9,
				Handler: "Purge", HandlerID: 202, HandlerPath: "internal/http/admin.go",
			},
		},
		edges: map[int64][]registry.EdgeHit{
			// Deliberately MEDIUM: it makes the minimum come from the third leg
			// rather than from the route match, so a test asserting the minimum
			// is asserting the rule and not an accident of the fixture.
			100: {exposes(100, 200, model.ConfidenceMedium)},
			101: {exposes(101, 201, model.ConfidenceHigh)},
			102: {exposes(102, 202, model.ConfidenceHigh)},
		},
	}

	return []Member{
		{Name: "web", IndexPath: "/tmp/web/index.db", Source: web, RunID: 1},
		{Name: "api", IndexPath: "/tmp/api/index.db", Source: api, RunID: 1},
	}
}

// TestCrossRepoEdgeIsFoundAndCarriesTheMinimum is the claim the command makes.
func TestCrossRepoEdgeIsFoundAndCarriesTheMinimum(t *testing.T) {
	rep := build(t, webAndAPI(t))

	if len(rep.Edges) != 2 {
		t.Fatalf("want 2 cross-repo edges, got %d: %+v", len(rep.Edges), rep.Edges)
	}

	byPath := map[string]CrossRepoEdge{}
	for _, e := range rep.Edges {
		byPath[e.Path] = e
	}

	orders, ok := byPath["/api/orders"]
	if !ok {
		t.Fatalf("the literal call was not joined: %+v", rep.Edges)
	}
	if orders.FromMember != "web" || orders.ToMember != "api" {
		t.Errorf("edge crosses %s -> %s, want web -> api", orders.FromMember, orders.ToMember)
	}
	if orders.ToEndpoint != "GET /api/orders" || orders.ToHandler != "ListOrders" {
		t.Errorf("edge lands on %q/%q, want GET /api/orders / ListOrders",
			orders.ToEndpoint, orders.ToHandler)
	}
	if orders.Match != MatchLiteralPath {
		t.Errorf("match kind = %q, want %q", orders.Match, MatchLiteralPath)
	}
	// call=high, route match=high, EXPOSES=medium. The minimum is medium, and
	// reporting high here would be laundering the weakest hop.
	if orders.Confidence != model.ConfidenceMedium {
		t.Errorf("confidence = %q, want medium (the weakest leg); legs: %+v",
			orders.Confidence, orders.Legs)
	}
	if len(orders.Legs) != 3 {
		t.Fatalf("want 3 legs so the minimum can be audited, got %d: %+v", len(orders.Legs), orders.Legs)
	}
	weakest := model.ConfidenceExact
	for _, l := range orders.Legs {
		if l.Confidence.Rank() < weakest.Rank() {
			weakest = l.Confidence
		}
	}
	if orders.Confidence != weakest {
		t.Errorf("edge confidence %q is not the minimum leg %q", orders.Confidence, weakest)
	}

	// The wildcard hop is a tier weaker than the literal one even though its
	// EXPOSES edge is stronger, which is the whole point of taking a minimum
	// over a path rather than a maximum over evidence.
	pay, ok := byPath["/api/payments/42"]
	if !ok {
		t.Fatalf("the parameterised call was not joined: %+v", rep.Edges)
	}
	if pay.Match != MatchPathParameter {
		t.Errorf("match kind = %q, want %q", pay.Match, MatchPathParameter)
	}
	if pay.Confidence != model.ConfidenceMedium {
		t.Errorf("wildcard match confidence = %q, want medium; legs: %+v", pay.Confidence, pay.Legs)
	}
}

// TestNothingIsEverExact is the rule this product's differentiation rests on.
//
// Asserted over the whole report rather than over one edge, and with an EXPOSES
// edge deliberately stamped `exact` on the far side: a compiler-proven last hop
// must not be able to launder a guessed route match into a compiler-proven
// chain.
func TestNothingIsEverExact(t *testing.T) {
	members := webAndAPI(t)
	api, ok := members[0].Source.(*fakeSource)
	if !ok || len(api.endpoints) == 0 {
		api, ok = members[1].Source.(*fakeSource)
		if !ok {
			t.Fatal("fixture shape changed")
		}
	}
	for id := range api.edges {
		api.edges[id] = []registry.EdgeHit{{
			SrcID: id, DstID: id + 100, Kind: string(model.EdgeExposes),
			Confidence: string(model.ConfidenceExact), Source: string(model.SourceCompiler),
		}}
	}

	rep := build(t, members)
	if len(rep.Edges) == 0 {
		t.Fatal("fixture produced no edges, so the assertion below would be vacuous")
	}
	for _, e := range rep.Edges {
		if e.Confidence == model.ConfidenceExact {
			t.Errorf("cross-repo edge %s -> %s claims `exact`: %+v", e.FromMember, e.ToMember, e)
		}
		if !e.Confidence.Valid() {
			t.Errorf("cross-repo edge carries an invalid confidence %q", e.Confidence)
		}
		heuristic := false
		for _, l := range e.Legs {
			if l.Name == "route match" {
				heuristic = l.Heuristic
			}
		}
		if !heuristic {
			t.Errorf("the route match leg is not marked heuristic: %+v", e.Legs)
		}
	}
	for _, tbl := range rep.SharedTables {
		if tbl.Confidence == model.ConfidenceExact {
			t.Errorf("shared table %q claims `exact`", tbl.Table)
		}
	}

	// The rendered form must not contain the word either, since that is what a
	// reader actually sees.
	var buf bytes.Buffer
	if err := Render(&buf, rep); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "[exact]") {
			t.Errorf("rendered report grades something `exact`: %s", line)
		}
	}
}

// TestUnmatchedCallIsReportedNotDropped: a call into a service nobody indexed is
// the most common thing in a real estate, and dropping it would both hide a
// dependency and destroy the denominator of "how much did we account for".
func TestUnmatchedCallIsReportedNotDropped(t *testing.T) {
	rep := build(t, webAndAPI(t))

	if len(rep.Unjoined.Calls) != 1 {
		t.Fatalf("want 1 unjoined call, got %d: %+v", len(rep.Unjoined.Calls), rep.Unjoined.Calls)
	}
	c := rep.Unjoined.Calls[0]
	if !strings.Contains(c.Path, "collector.example.com") {
		t.Errorf("wrong call reported as unjoined: %+v", c)
	}
	if c.Member != "web" || c.FilePath != "src/telemetry.ts" || c.Line != 7 {
		t.Errorf("unjoined call lost its location: %+v", c)
	}
	if c.Reason == "" {
		t.Error("an unjoined call must say why it could not be joined")
	}

	// And the endpoint nobody calls is the mirror image of the same honesty.
	var purge bool
	for _, e := range rep.Unjoined.Endpoints {
		if e.Endpoint == "DELETE /api/admin/purge" {
			purge = true
		}
	}
	if !purge {
		t.Errorf("an endpoint no member calls was not reported: %+v", rep.Unjoined.Endpoints)
	}
	for _, e := range rep.Unjoined.Endpoints {
		if e.Endpoint == "GET /api/orders" {
			t.Error("a consumed endpoint was reported as unconsumed")
		}
	}

	var buf bytes.Buffer
	if err := Render(&buf, rep); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	t.Logf("rendered:\n%s", out)
	for _, want := range []string{
		"COULD NOT JOIN",
		"collector.example.com",
		"DELETE /api/admin/purge",
		"WHAT THIS IS NOT",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report omits %q", want)
		}
	}
}

// TestAmbiguousCallIsRefused: two members that could both answer a request are
// two answers, not a tie to be broken, because no router spans them.
func TestAmbiguousCallIsRefused(t *testing.T) {
	caller := &fakeSource{
		run: readyRun(1, "aaa", at(time.Hour)),
		calls: []registry.ClientCallUsage{{
			CallID: 1, FilePath: "src/app.ts", Line: 3,
			Method: "GET", Path: "/api/health", Caller: "probe", Binding: "unbound",
		}},
	}
	one := &fakeSource{
		run: readyRun(1, "bbb", at(time.Hour)),
		endpoints: []registry.Endpoint{{
			SymbolID: 10, Signature: "GET /api/health",
			FilePath: "a.go", Line: 1, Handler: "H", HandlerID: 11,
		}},
		edges: map[int64][]registry.EdgeHit{10: {exposes(10, 11, model.ConfidenceHigh)}},
	}
	two := &fakeSource{
		run: readyRun(1, "ccc", at(time.Hour)),
		endpoints: []registry.Endpoint{{
			SymbolID: 20, Signature: "GET /api/health",
			FilePath: "b.go", Line: 1, Handler: "H2", HandlerID: 21,
		}},
		edges: map[int64][]registry.EdgeHit{20: {exposes(20, 21, model.ConfidenceHigh)}},
	}

	rep := build(t, []Member{
		{Name: "web", Source: caller, RunID: 1},
		{Name: "svc-one", Source: one, RunID: 1},
		{Name: "svc-two", Source: two, RunID: 1},
	})

	if len(rep.Edges) != 0 {
		t.Fatalf("an ambiguous call produced %d edge(s); picking one would be an invented dependency: %+v",
			len(rep.Edges), rep.Edges)
	}
	if len(rep.Unjoined.AmbiguousCalls) != 1 {
		t.Fatalf("want the ambiguity reported, got %+v", rep.Unjoined.AmbiguousCalls)
	}
	got := rep.Unjoined.AmbiguousCalls[0].Candidates
	if len(got) != 2 || got[0] != "svc-one: GET /api/health" || got[1] != "svc-two: GET /api/health" {
		t.Errorf("candidates = %v, want both members named and sorted", got)
	}
}

// TestSelfMatchesAreNotCrossRepoEdges: binding a call to a route in its OWN
// repository is internal/registry's job and has already happened. Redoing it
// here would let a call that was ambiguous inside one repository reappear as a
// confident edge to that same repository.
func TestSelfMatchesAreNotCrossRepoEdges(t *testing.T) {
	self := &fakeSource{
		run: readyRun(1, "aaa", at(time.Hour)),
		calls: []registry.ClientCallUsage{
			{CallID: 1, FilePath: "a.ts", Line: 1, Method: "GET", Path: "/api/x",
				Caller: "c", Binding: "unbound"},
			// A call this repository already answered. Excluded from matching
			// and counted, so the call arithmetic closes.
			{CallID: 2, FilePath: "a.ts", Line: 2, Method: "GET", Path: "/api/y",
				Caller: "c", Binding: "bound", EndpointID: 11, Endpoint: "GET /api/y"},
		},
		endpoints: []registry.Endpoint{
			{SymbolID: 10, Signature: "GET /api/x", FilePath: "a.go", Line: 1},
			{SymbolID: 11, Signature: "GET /api/y", FilePath: "a.go", Line: 2},
		},
	}
	other := &fakeSource{run: readyRun(1, "bbb", at(time.Hour))}

	rep := build(t, []Member{
		{Name: "mono", Source: self, RunID: 1},
		{Name: "other", Source: other, RunID: 1},
	})

	if len(rep.Edges) != 0 {
		t.Errorf("a call matched a route in its own repository: %+v", rep.Edges)
	}
	if len(rep.Unjoined.Calls) != 1 || rep.Unjoined.Calls[0].Path != "/api/x" {
		t.Errorf("the self-matching call should be reported as unjoined, got %+v", rep.Unjoined.Calls)
	}
	for _, m := range rep.Members {
		if m.Name == "mono" && m.LocallyAnswered != 1 {
			t.Errorf("locally answered = %d, want 1", m.LocallyAnswered)
		}
	}
	// A route consumed only from inside its own repository is not "called by
	// nobody"; reporting it as such would send a reader to delete live code.
	for _, e := range rep.Unjoined.Endpoints {
		if e.Endpoint == "GET /api/y" {
			t.Error("a locally consumed endpoint was reported as unconsumed")
		}
	}
}

// TestSharedTablesAreJoinedAndCaveated: two repositories writing one table name
// is the coupling nobody involved can see, and the name-only match is the
// caveat that keeps it honest.
func TestSharedTablesAreJoinedAndCaveated(t *testing.T) {
	orders := &fakeSource{
		run: readyRun(1, "aaa", at(time.Hour)),
		tables: []registry.TableUsage{
			{Table: "payments", Declared: true, Readers: 3, Writers: 2, Accesses: 5},
			{Table: "orders_only_here", Readers: 1, Accesses: 1},
		},
	}
	ledger := &fakeSource{
		run: readyRun(1, "bbb", at(time.Hour)),
		tables: []registry.TableUsage{
			{Table: "payments", Readers: 1, Writers: 4, Accesses: 5, Uncertain: 1},
		},
	}

	rep := build(t, []Member{
		{Name: "ledger", Source: ledger, RunID: 1},
		{Name: "orders", Source: orders, RunID: 1},
	})

	if len(rep.SharedTables) != 1 {
		t.Fatalf("want exactly the shared table, got %+v", rep.SharedTables)
	}
	st := rep.SharedTables[0]
	if st.Table != "payments" {
		t.Errorf("shared table = %q, want payments", st.Table)
	}
	if !st.WriteWrite {
		t.Error("two members writing one table must be flagged write/write")
	}
	if len(st.Writers) != 2 {
		t.Errorf("writers = %v, want both members", st.Writers)
	}
	if len(st.Owners) != 1 || st.Owners[0] != "orders" {
		t.Errorf("owners = %v, want only the member that declares it", st.Owners)
	}
	// One access was assembled at run time, so the table NAME is not certain.
	if st.Confidence != model.ConfidenceMedium {
		t.Errorf("confidence = %q, want medium — an uncertain access weakens the coupling claim",
			st.Confidence)
	}

	var buf bytes.Buffer
	if err := Render(&buf, rep); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(buf.String(), "NAME ALONE") {
		t.Errorf("the name-only caveat is missing from the rendered report:\n%s", buf.String())
	}
}

// TestUnavailableMemberIsReportedNotDropped is the rule that a federated view
// omitting a member is worse than no federated view.
func TestUnavailableMemberIsReportedNotDropped(t *testing.T) {
	members := webAndAPI(t)
	members = append(members, Member{
		Name:        "billing",
		IndexPath:   "/tmp/billing/.repocortex/index.db",
		Unavailable: "no index at /tmp/billing/.repocortex/index.db. Build one in that checkout.",
	})

	rep := build(t, members)

	if rep.Complete() {
		t.Error("a report missing a member must not call itself complete")
	}
	if len(rep.Unjoined.UnavailableMembers) != 1 || rep.Unjoined.UnavailableMembers[0] != "billing" {
		t.Errorf("unavailable members = %v, want [billing]", rep.Unjoined.UnavailableMembers)
	}
	var found bool
	for _, m := range rep.Members {
		if m.Name != "billing" {
			continue
		}
		found = true
		if m.Available || m.Unavailable == "" {
			t.Errorf("the missing member is not marked unavailable: %+v", m)
		}
	}
	if !found {
		t.Error("the missing member is absent from the members section entirely")
	}

	var buf bytes.Buffer
	if err := Render(&buf, rep); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	t.Logf("rendered:\n%s", out)
	for _, want := range []string{"BILLING IS MISSING FROM THIS GRAPH", "floor", "INCOMPLETE"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report omits %q:\n%s", want, out)
		}
	}
}

// TestMixedProducersAndSkewAreReported: an index six weeks old, or written by
// another build, describes a system that may no longer exist, and only the tool
// can know that.
func TestMixedProducersAndSkewAreReported(t *testing.T) {
	fresh := &fakeSource{run: readyRun(1, "aaa", at(time.Hour))}

	old := &fakeSource{run: &model.IndexRun{
		ID: 1, Status: model.StatusDegraded, GitRevision: "",
		ProducerVersion: "0.9.0", IndexState: model.IndexStatePartial,
		FilesUnaccounted: 7, FinishedUnix: at(42 * 24 * time.Hour),
	}}

	rep := build(t, []Member{
		{Name: "fresh", Source: fresh, RunID: 1},
		{Name: "stale", Source: old, RunID: 1},
	})

	if !rep.Provenance.MixedProducers {
		t.Error("two producer versions must be reported as mixed")
	}
	if rep.Provenance.SkewHours < 24 {
		t.Errorf("skew = %.1fh, want the 42-day spread", rep.Provenance.SkewHours)
	}
	if len(rep.Provenance.Notes) == 0 {
		t.Fatal("provenance must say something when the estate is mixed and skewed")
	}

	var warnings []string
	for _, m := range rep.Members {
		if m.Name == "stale" {
			warnings = m.Warnings
		}
	}
	joined := strings.Join(warnings, " | ")
	for _, want := range []string{"degraded", "partial", "no commit", "0.9.0", "day(s) ago"} {
		if !strings.Contains(joined, want) {
			t.Errorf("stale member warnings omit %q: %s", want, joined)
		}
	}
}

// TestBuildRefusesDegenerateInput covers the two structural errors, which are
// errors rather than findings because there is no report to produce.
func TestBuildRefusesDegenerateInput(t *testing.T) {
	f, err := New()
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{run: readyRun(1, "aaa", at(time.Hour))}

	if _, err := f.Build(context.Background(), []Member{{Name: "solo", Source: src, RunID: 1}}); err == nil {
		t.Error("a one-member system graph must be refused")
	}
	_, err = f.Build(context.Background(), []Member{
		{Name: "dup", Source: src, RunID: 1},
		{Name: "dup", Source: src, RunID: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "named") {
		t.Errorf("duplicate member names must be refused, got %v", err)
	}
}

// TestBrokenMemberIndexSurfaces: a file that is present and answers a query with
// an error is a different situation from a file that is absent, and swallowing
// it would report a member as having contributed nothing.
func TestBrokenMemberIndexSurfaces(t *testing.T) {
	f, err := New()
	if err != nil {
		t.Fatal(err)
	}
	good := &fakeSource{run: readyRun(1, "aaa", at(time.Hour))}
	broken := &fakeSource{run: readyRun(1, "bbb", at(time.Hour)), failOn: "ClientCallReport"}

	_, err = f.Build(context.Background(), []Member{
		{Name: "good", Source: good, RunID: 1},
		{Name: "broken", Source: broken, RunID: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("a member whose index errors must surface, got %v", err)
	}
}

// TestRenderIsDeterministic: the report is meant to be diffable, and members
// arriving in a different flag order must not reorder a single line.
func TestRenderIsDeterministic(t *testing.T) {
	a := webAndAPI(t)
	b := webAndAPI(t)
	b[0], b[1] = b[1], b[0]

	var one, two bytes.Buffer
	if err := Render(&one, build(t, a)); err != nil {
		t.Fatal(err)
	}
	if err := Render(&two, build(t, b)); err != nil {
		t.Fatal(err)
	}
	if one.String() != two.String() {
		t.Errorf("rendering depends on member order:\n--- a ---\n%s\n--- b ---\n%s", one.String(), two.String())
	}
}

// TestCrossRepoRoutesAgreeWithTheIntraRepoVocabulary pins the canonicalisation
// rules this package restates from internal/registry.
//
// The two must agree segment for segment: a route text must not mean two
// different things depending on which side of a repository line it falls. If
// internal/registry ever exports its matcher, delete route.go and this test with
// it.
func TestCrossRepoRoutesAgreeWithTheIntraRepoVocabulary(t *testing.T) {
	cases := []struct {
		name      string
		route     string
		call      string
		wantMatch bool
		wantWilds int
	}{
		{"three spellings of one parameter", "/users/{id}", "/users/42", true, 1},
		{"express colon", "/users/:id", "/users/42", true, 1},
		{"flask angle", "/users/<int:id>", "/users/42", true, 1},
		{"next.js bracket", "/users/[id]", "/users/42", true, 1},
		{"absolute call url", "/v1/orders", "https://api.example.com/v1/orders", true, 0},
		{"query string is not identity", "/api/search", "/api/search?q=cat", true, 0},
		{"case folds", "/API/Orders", "/api/orders", true, 0},
		{"trailing slash folds", "/orders/", "/orders", true, 0},
		{"doubled slash is a typo", "/api/orders", "/api//orders", true, 0},
		{"a wildcard absorbs exactly one segment", "/api/*", "/api/a/b", false, 0},
		{"different route", "/api/orders", "/api/refunds", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wilds, ok := segmentsMatch(canonSegments(tc.route), canonSegments(tc.call))
			if ok != tc.wantMatch {
				t.Fatalf("%q vs %q matched=%v, want %v", tc.route, tc.call, ok, tc.wantMatch)
			}
			if ok && wilds != tc.wantWilds {
				t.Errorf("%q vs %q spent %d wildcard(s), want %d", tc.route, tc.call, wilds, tc.wantWilds)
			}
		})
	}
}

// TestVerbUnstatedCostsATier: matching on the path alone, because one side did
// not spell an HTTP verb, is weaker evidence and must be graded as such.
func TestVerbUnstatedCostsATier(t *testing.T) {
	caller := &fakeSource{
		run: readyRun(1, "aaa", at(time.Hour)),
		calls: []registry.ClientCallUsage{{
			CallID: 1, FilePath: "a.ts", Line: 1, Method: "", Path: "/api/health",
			Caller: "probe", Binding: "unbound",
		}},
	}
	svc := &fakeSource{
		run: readyRun(1, "bbb", at(time.Hour)),
		endpoints: []registry.Endpoint{{
			SymbolID: 10, Signature: "GET /api/health", FilePath: "a.go", Line: 1,
			Handler: "H", HandlerID: 11,
		}},
		edges: map[int64][]registry.EdgeHit{10: {exposes(10, 11, model.ConfidenceHigh)}},
	}

	rep := build(t, []Member{
		{Name: "web", Source: caller, RunID: 1},
		{Name: "svc", Source: svc, RunID: 1},
	})
	if len(rep.Edges) != 1 {
		t.Fatalf("want 1 edge, got %+v", rep.Edges)
	}
	if got := rep.Edges[0].Confidence; got != model.ConfidenceMedium {
		t.Errorf("confidence = %q, want medium: the verb was unstated, so the match rests on "+
			"the path alone; legs %+v", got, rep.Edges[0].Legs)
	}
}

// TestDynamicCallBindsOnlyByWholeSegments: `/api/user${id}` must not bind to
// /api/user, because the call actually requests /api/users/7.
func TestDynamicCallBindsOnlyByWholeSegments(t *testing.T) {
	caller := &fakeSource{
		run: readyRun(1, "aaa", at(time.Hour)),
		calls: []registry.ClientCallUsage{
			{CallID: 1, FilePath: "a.ts", Line: 1, Method: "GET", Path: "/api/user",
				Dynamic: true, Caller: "c", Binding: "unbound"},
			{CallID: 2, FilePath: "a.ts", Line: 2, Method: "GET", Path: "/api/orders/",
				Dynamic: true, Caller: "c", Binding: "unbound"},
		},
	}
	svc := &fakeSource{
		run: readyRun(1, "bbb", at(time.Hour)),
		endpoints: []registry.Endpoint{
			{SymbolID: 10, Signature: "GET /api/user", FilePath: "a.go", Line: 1},
			{SymbolID: 11, Signature: "GET /api/orders/{id}", FilePath: "a.go", Line: 2,
				Handler: "Get", HandlerID: 21},
		},
		edges: map[int64][]registry.EdgeHit{11: {exposes(11, 21, model.ConfidenceHigh)}},
	}

	rep := build(t, []Member{
		{Name: "web", Source: caller, RunID: 1},
		{Name: "svc", Source: svc, RunID: 1},
	})

	if len(rep.Edges) != 1 {
		t.Fatalf("want exactly the segment-boundary match, got %+v", rep.Edges)
	}
	e := rep.Edges[0]
	if e.ToEndpoint != "GET /api/orders/{id}" {
		t.Errorf("bound to %q; the half-word prefix must not bind", e.ToEndpoint)
	}
	if e.Confidence != model.ConfidenceLow {
		t.Errorf("a run-time target bound by prefix is %q, want low", e.Confidence)
	}
	// `/api/user${id}` reveals the literal prefix "/api/user", whose last
	// segment is half a word — so it is dropped, the call is left claiming only
	// "/api/...", and both routes qualify. Reported as ambiguous rather than
	// bound to the route whose text it happens to spell, which is the mistake
	// this rule exists to prevent.
	if len(rep.Unjoined.AmbiguousCalls) != 1 || rep.Unjoined.AmbiguousCalls[0].Path != "/api/user" {
		t.Fatalf("the half-word prefix call must be reported, got %+v", rep.Unjoined.AmbiguousCalls)
	}
	for _, cand := range rep.Unjoined.AmbiguousCalls[0].Candidates {
		if strings.HasSuffix(cand, "GET /api/user") && len(rep.Unjoined.AmbiguousCalls[0].Candidates) == 1 {
			t.Error("the half-word prefix bound to the route it merely spells")
		}
	}
}
