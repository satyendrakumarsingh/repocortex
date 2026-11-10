package web

import (
	stdcontext "context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/verify"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The three screens added last: cross-stack, task contract, verify report.
//
// Driven against the REAL registry, the REAL graph and the REAL compile and
// verify packages over a seeded SQLite index, for the same reason the four
// original screens are: what is worth asserting is that a confidence, a piece
// of evidence and a stated gap survive the whole way from a database row to a
// JSON field. A fake would only prove the fake agreed.

// stackFixture seeds a complete front-to-back chain plus two deliberate stumps.
//
//	Checkout  --CONSUMES--> GET /orders --EXPOSES--> ordersHandler
//	ordersHandler --CALLS--> orderStore --WRITES--> orders
//	                                    --READS-->  customers
//
// `POST /refunds` has a handler and nothing else: no caller, no outbound call,
// no table. `GET /health` has nothing at all. Both exist so the test can assert
// that a chain which stops short SAYS SO, which is the property the whole screen
// is built on — a stump and a genuinely data-free handler must not render alike.
func stackFixture(t *testing.T, opts ...Option) (*Server, capability.Deps, int64) {
	t.Helper()

	syms := []captest.Sym{
		captest.S(1, "CheckoutPanel", "web/src/checkout.tsx").Export(),
		captest.S(2, "GET /orders", "internal/api/routes.go").WithKind("endpoint"),
		captest.S(3, "ordersHandler", "internal/api/orders.go").Export(),
		captest.S(4, "orderStore", "internal/store/orders.go").Export(),
		captest.S(5, "orders", "db/schema.sql").WithKind("table"),
		captest.S(6, "customers", "db/schema.sql").WithKind("table"),
		captest.S(7, "POST /refunds", "internal/api/routes.go").WithKind("endpoint"),
		captest.S(8, "refundsHandler", "internal/api/refunds.go").Export(),
		captest.S(9, "GET /health", "internal/api/routes.go").WithKind("endpoint"),
		// A second, WEAKER caller of the same route, so the screen can be shown
		// to drop it at a raised floor and to say how many it dropped.
		captest.S(10, "LegacyCart", "web/src/legacy.tsx"),
	}
	edges := []captest.Edge{
		{Src: 1, Dst: 2, Kind: model.EdgeConsumes, Conf: model.ConfidenceHigh},
		{Src: 10, Dst: 2, Kind: model.EdgeConsumes, Conf: model.ConfidenceLow},
		{Src: 2, Dst: 3, Kind: model.EdgeExposes, Conf: model.ConfidenceExact},
		{Src: 7, Dst: 8, Kind: model.EdgeExposes, Conf: model.ConfidenceExact},
		captest.E(3, 4).WithConf(model.ConfidenceHigh),
		{Src: 4, Dst: 5, Kind: model.EdgeWrites, Conf: model.ConfidenceHigh},
		{Src: 4, Dst: 6, Kind: model.EdgeReads, Conf: model.ConfidenceMedium},
	}
	deps, runID := captest.Build(t, syms, edges, captest.Refs{Resolved: 40, Unresolved: 10})

	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	srv := newTaskServer(t, reg, deps, config.Config{ProjectName: "stack-fixture"}, opts...)
	return srv, deps, runID
}

// newTaskServer wires the context-pack port, which the contract and verify
// screens need and the four graph screens do not.
func newTaskServer(t *testing.T, reg *registry.Registry, deps capability.Deps, cfg config.Config, opts ...Option) *Server {
	t.Helper()
	srv, err := New(Deps{
		Registry: reg, Packs: reg, Caps: deps, Config: cfg, IndexPath: "/tmp/fixture/index.db",
	}, append([]Option{WithMaxNodes(DefaultMaxNodes)}, opts...)...)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

// ---------------------------------------------------------------- cross-stack

func TestWebCrossStackDrawsTheWholeChainWithPerHopEvidence(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	var got CrossStackResponse
	rec := get(t, srv, "/api/crossstack", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	byName := map[string]StackChain{}
	for _, c := range got.Chains {
		byName[c.Endpoint.Name] = c
	}
	orders, ok := byName["GET /orders"]
	if !ok {
		t.Fatalf("GET /orders is missing from %d chain(s)", len(got.Chains))
	}

	// Hop 1: the component that calls the route. At the default `medium` floor
	// the `low` caller must be gone AND counted, not gone in silence.
	if len(orders.Clients) != 1 || orders.Clients[0].Node.Name != "CheckoutPanel" {
		t.Fatalf("clients = %+v, want exactly CheckoutPanel at the default floor", orders.Clients)
	}
	if orders.Clients[0].Relation != string(model.EdgeConsumes) {
		t.Errorf("client hop relation = %q, want CONSUMES", orders.Clients[0].Relation)
	}
	if orders.Clients[0].Evidence.FilePath == "" {
		t.Error("a CONSUMES hop with no file:line is an assertion, not evidence")
	}

	// Hops 2-5.
	if orders.Handler == nil || orders.Handler.Node.Name != "ordersHandler" {
		t.Fatalf("handler = %+v, want ordersHandler through EXPOSES", orders.Handler)
	}
	if orders.Handler.Confidence != string(model.ConfidenceExact) {
		t.Errorf("handler hop confidence = %q, want exact", orders.Handler.Confidence)
	}
	if len(orders.Services) != 1 || orders.Services[0].Node.Name != "orderStore" {
		t.Fatalf("services = %+v, want orderStore reached from the handler", orders.Services)
	}
	tables := map[string]string{}
	for _, tb := range orders.Tables {
		tables[tb.Node.Name] = tb.Relation
	}
	if tables["orders"] != string(model.EdgeWrites) || tables["customers"] != string(model.EdgeReads) {
		t.Fatalf("tables = %+v, want orders WRITES and customers READS", tables)
	}
	if !orders.Writes {
		t.Error("a chain reaching a WRITES edge must report writes: it is the fact that turns the chain into a decision")
	}
	if orders.Reach != StageTable {
		t.Errorf("reach = %q, want %q — this chain runs the whole way", orders.Reach, StageTable)
	}

	// A chain is drawn at its WEAKEST hop. Here that is the `medium` READS.
	if orders.Weakest != string(model.ConfidenceMedium) {
		t.Errorf("weakest = %q, want medium — the READS hop is the floor of this chain, and a bundle "+
			"drawn at its best member is the overclaim this product exists to avoid", orders.Weakest)
	}
}

func TestWebCrossStackNamesEveryPlaceAChainStops(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	var got CrossStackResponse
	get(t, srv, "/api/crossstack", &got)

	byName := map[string]StackChain{}
	for _, c := range got.Chains {
		byName[c.Endpoint.Name] = c
	}

	// A route with a handler and nothing behind it must say so at every stage
	// it failed to reach. Silence here would render identically to a handler
	// that genuinely touches no data, and those are opposite facts.
	refunds, ok := byName["POST /refunds"]
	if !ok {
		t.Fatal("POST /refunds is missing: a route nothing calls must still appear")
	}
	stages := map[StackStage]string{}
	for _, g := range refunds.Gaps {
		stages[g.Stage] = g.Reason
	}
	for _, want := range []StackStage{StageClient, StageService, StageTable} {
		if stages[want] == "" {
			t.Errorf("POST /refunds states no gap at stage %q; gaps = %+v", want, refunds.Gaps)
		}
	}
	if refunds.Reach != StageHandler {
		t.Errorf("POST /refunds reach = %q, want handler", refunds.Reach)
	}

	health, ok := byName["GET /health"]
	if !ok {
		t.Fatal("GET /health is missing: a route with no handler is still a route")
	}
	var sawHandlerGap bool
	for _, g := range health.Gaps {
		if g.Stage == StageHandler {
			sawHandlerGap = true
			if g.Detail == "" {
				t.Error("a handler gap with no detail teaches the reader nothing")
			}
		}
	}
	if !sawHandlerGap {
		t.Errorf("GET /health has no handler and does not say so; gaps = %+v", health.Gaps)
	}

	// The aggregate is what the page leads with.
	if len(got.Gaps) == 0 {
		t.Fatal("the response states no aggregated gaps at all")
	}
	if got.Counts.Endpoints != 3 {
		t.Errorf("counts.endpoints = %d, want 3", got.Counts.Endpoints)
	}
	if got.Counts.WithClient != 1 || got.Counts.WithHandler != 2 || got.Counts.Page.Complete != 1 {
		t.Errorf("counts = %+v, want 1 with a caller, 2 with a handler, 1 complete", got.Counts)
	}
	// Whole-index and page-scoped figures must be reported separately. They
	// were once the same row: "411 routes" counted every endpoint while
	// "12 with a declared handler" counted only the current page.
	if got.Counts.Page.Scope == "" {
		t.Error("page-scoped counts do not say they are page-scoped")
	}
	if got.Page.Total != got.Counts.Endpoints {
		t.Errorf("page.total = %d but counts.endpoints = %d; the pager must index the whole route list",
			got.Page.Total, got.Counts.Endpoints)
	}
}

func TestWebCrossStackCountsWhatTheFloorHides(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	// The `low` CONSUMES edge is hidden at the default floor. It must be
	// COUNTED, and lowering the floor must bring it back — which is what makes
	// the count checkable rather than a disclaimer.
	var def CrossStackResponse
	get(t, srv, "/api/crossstack", &def)
	if def.Omissions.HiddenByFloor() == 0 {
		t.Fatalf("nothing reported hidden at the default floor, but a `low` CONSUMES edge exists: %+v", def.Omissions)
	}
	if def.Omissions.Complete {
		t.Error("omissions.complete is true while a caller is being hidden")
	}

	var low CrossStackResponse
	get(t, srv, "/api/crossstack?min_confidence=low", &low)
	var clients int
	for _, c := range low.Chains {
		if c.Endpoint.Name == "GET /orders" {
			clients = len(c.Clients)
		}
	}
	if clients != 2 {
		t.Fatalf("at floor `low` GET /orders has %d client(s), want 2 — the hidden count must be recoverable", clients)
	}
}

// ---------------------------------------------------------------- contract

func TestWebContractPutsUnknownsAtTheTopLevel(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	var got ContractResponse
	rec := get(t, srv, "/api/contract?intent="+url.QueryEscape("Add idempotency to the orders handler"), &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Status != contractOK {
		t.Fatalf("status = %q, guidance = %+v", got.Status, got.Help)
	}
	if got.Contract == nil {
		t.Fatal("no contract in an ok response")
	}

	// The hoist is the point of the screen. A client rendering the struct
	// field-by-field would put unknowns after risks, which is where a reader
	// stops looking.
	if len(got.Unknowns) == 0 {
		t.Fatal("a contract with no unknowns at the top level: the compiler names gaps on every real input, " +
			"and this field is what stops them being buried")
	}
	if len(got.Unknowns) != len(got.Contract.Unknowns) {
		t.Errorf("hoisted unknowns (%d) disagree with the contract's own (%d)",
			len(got.Unknowns), len(got.Contract.Unknowns))
	}
	if got.Counts.Unknowns != len(got.Unknowns) {
		t.Errorf("counts.unknowns = %d, want %d", got.Counts.Unknowns, len(got.Unknowns))
	}

	// Every constraint cites its source. A constraint with no evidence is an
	// opinion, and compile refuses to emit one — this asserts the refusal
	// survives serialisation.
	for _, c := range got.Contract.Constraints {
		if strings.TrimSpace(c.Evidence) == "" {
			t.Errorf("constraint %q reached the wire with no evidence", c.Text)
		}
	}

	// The omission block is never optimistic, and `complete` is derived.
	if got.Omissions.Complete && len(got.Omissions.Omitted) > 0 {
		t.Error("omissions.complete is true beside recorded omissions")
	}
	if got.Next.Label == "" {
		t.Error("the contract screen ends without a next action")
	}
}

func TestWebContractWithoutAnIntentAsksForOneRatherThanFailing(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	m := raw(t, srv, "/api/contract")
	if m["status"] != string(contractNoIntent) {
		t.Errorf("status = %v, want %q", m["status"], contractNoIntent)
	}
	if _, ok := m["guidance"]; !ok {
		t.Error("an empty form answered with no guidance teaches the reader the tool is broken")
	}
	// `unknowns` must be present and an array even here: absent and empty are
	// different claims, and only one of them a client can render.
	if _, ok := m["unknowns"].([]any); !ok {
		t.Errorf("unknowns = %#v, want an array on every response", m["unknowns"])
	}
}

// ---------------------------------------------------------------- verify

func TestWebVerifySeparatesFactsFromLeadsAndCarriesTheExitCode(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	q := "/api/verify?intent=" + url.QueryEscape("Add idempotency to the orders handler") +
		"&changed=" + url.QueryEscape("internal/api/orders.go,internal/store/orders.go")

	var got VerifyResponse
	rec := get(t, srv, q, &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got.Status != VerifyOK {
		t.Fatalf("status = %q, guidance = %+v", got.Status, got.Help)
	}
	if got.Report == nil {
		t.Fatal("no report in an ok response")
	}

	// The partition is the whole vocabulary. Facts and leads must be separate
	// arrays whose counts reconcile with the report's own tally, so a client
	// cannot render them identically by accident.
	if got.Counts.Facts != len(got.Facts) || got.Counts.Leads != len(got.Leads) {
		t.Errorf("counts (%d facts / %d leads) disagree with the arrays (%d / %d)",
			got.Counts.Facts, got.Counts.Leads, len(got.Facts), len(got.Leads))
	}
	if got.Counts.Facts+got.Counts.Leads != got.Counts.Findings {
		t.Errorf("facts + leads = %d, findings = %d — every finding must have a standing",
			got.Counts.Facts+got.Counts.Leads, got.Counts.Findings)
	}
	for _, f := range got.Facts {
		if f.Standing != verify.StandingFact {
			t.Errorf("a %q-standing finding is in the facts array: %+v", f.Standing, f)
		}
	}
	for _, f := range got.Leads {
		if f.Standing != verify.StandingLead {
			t.Errorf("a %q-standing finding is in the leads array: %+v", f.Standing, f)
		}
	}

	// The signal and the exit code come from one place, so a green page and a
	// failing pipeline cannot describe the same run.
	if got.ExitCode != verify.Disposition(got.Verdict).ExitCode() {
		t.Errorf("exit_code %d does not match disposition %q", got.ExitCode, got.Verdict)
	}
	if want := signalFor(verify.Disposition(got.Verdict)); got.Signal != want {
		t.Errorf("signal = %q, want %q for disposition %q", got.Signal, want, got.Verdict)
	}

	// The change set is placed against the contract, and the arithmetic adds up.
	if got.FileCounts.Changed != 2 {
		t.Errorf("file_counts.changed = %d, want 2", got.FileCounts.Changed)
	}
	if got.FileCounts.InScope+got.FileCounts.Excluded+got.FileCounts.Undeclared != got.FileCounts.Changed {
		t.Errorf("file placement does not partition the change set: %+v", got.FileCounts)
	}
	if len(got.Files) != got.FileCounts.Changed {
		t.Errorf("%d file verdicts for %d changed files", len(got.Files), got.FileCounts.Changed)
	}

	if got.Unknowns == nil {
		t.Error("unknowns is null: a verdict rendered without its gaps reads as a clean bill of health")
	}
}

func TestWebVerifyRefusesToCallAnEmptyChangeSetAPass(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	var got VerifyResponse
	get(t, srv, "/api/verify?intent="+url.QueryEscape("Add idempotency"), &got)

	if got.Status != VerifyNoChanges {
		t.Fatalf("status = %q, want %q", got.Status, VerifyNoChanges)
	}
	if got.Signal == "green" {
		t.Fatal(`an empty change set reported as "green": "we looked and found nothing" and "there was ` +
			`nothing to look at" must never render the same`)
	}
	if got.ExitCode != 2 {
		t.Errorf("exit_code = %d, want 2 — unverified is a failure of the setup, not of the change", got.ExitCode)
	}
	if got.Help == nil {
		t.Error("no guidance on an incomplete form")
	}
}

func TestWebVerifyFlagsAnEditToAFileTheContractExcluded(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	// Compile first, then change exactly what the contract named as out of
	// scope. The point is not that this fixture always produces an exclusion —
	// it is that when one exists, editing it is reported as a FACT: no
	// inference is involved, the file was named and the file was touched.
	var brief ContractResponse
	get(t, srv, "/api/contract?intent="+url.QueryEscape("Add idempotency to the orders handler"), &brief)
	if brief.Contract == nil || len(brief.Contract.OutOfScope) == 0 {
		t.Skip("this fixture's contract excluded nothing by name; the scope-breach path needs an exclusion to exercise")
	}
	excluded := brief.Contract.OutOfScope[0].Path

	var got VerifyResponse
	get(t, srv, "/api/verify?intent="+url.QueryEscape("Add idempotency to the orders handler")+
		"&changed="+url.QueryEscape(excluded), &got)

	var sawScopeFinding bool
	for _, f := range got.Facts {
		if f.Kind == verify.KindOutOfScopeEdit {
			sawScopeFinding = true
			if f.Evidence == "" {
				t.Error("a scope breach with no evidence is an opinion")
			}
		}
	}
	if !sawScopeFinding {
		t.Errorf("editing %q, which the contract excluded, produced no fact: facts = %+v, verdict = %q",
			excluded, got.Facts, got.Verdict)
	}
	for _, fv := range got.Files {
		if fv.Path == normalisePortalPath(excluded) && fv.Kind != FileExcluded {
			t.Errorf("file %q placed as %q, want %q", fv.Path, fv.Kind, FileExcluded)
		}
	}
}

// TestWebTaskScreensDegradeWithGuidanceWhenThePackPortIsAbsent pins the
// fallback: a server built without the context-pack port must say WHICH wiring
// is missing rather than returning an empty brief that reads like a thin
// repository.
func TestWebTaskScreensDegradeWithGuidanceWhenThePackPortIsAbsent(t *testing.T) {
	t.Parallel()
	_, deps, _ := stackFixture(t)

	srv, err := New(Deps{
		// A Registry that satisfies web.Registry and NOTHING wider, so the
		// PackReader assertion in packReader() genuinely fails.
		Registry: narrowRegistry{deps.Registry.(*registry.Registry)},
		Caps:     deps,
		Config:   config.Config{ProjectName: "narrow"},
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	var got ContractResponse
	get(t, srv, "/api/contract?intent="+url.QueryEscape("anything at all"), &got)
	if got.Status != contractUnavailable {
		t.Fatalf("status = %q, want %q", got.Status, contractUnavailable)
	}
	if got.Help == nil || !strings.Contains(got.Help.Detail, "context-pack port") {
		t.Errorf("guidance does not name the missing wiring: %+v", got.Help)
	}
}

// narrowRegistry exposes exactly web.Registry and no more.
type narrowRegistry struct{ inner *registry.Registry }

func (n narrowRegistry) CurrentRun(ctx stdcontext.Context) (*model.IndexRun, error) {
	return n.inner.CurrentRun(ctx)
}
func (n narrowRegistry) Counts(ctx stdcontext.Context, runID int64) (registry.Counts, error) {
	return n.inner.Counts(ctx, runID)
}
func (n narrowRegistry) LanguageCounts(ctx stdcontext.Context, runID int64) (map[string]int, error) {
	return n.inner.LanguageCounts(ctx, runID)
}
func (n narrowRegistry) ResolutionStats(ctx stdcontext.Context, runID int64) (registry.ResolutionStats, error) {
	return n.inner.ResolutionStats(ctx, runID)
}
func (n narrowRegistry) Files(ctx stdcontext.Context, runID int64) ([]model.File, error) {
	return n.inner.Files(ctx, runID)
}
func (n narrowRegistry) Symbols(ctx stdcontext.Context, runID int64) ([]model.Symbol, error) {
	return n.inner.Symbols(ctx, runID)
}
func (n narrowRegistry) Edges(ctx stdcontext.Context, runID int64) ([]model.Edge, error) {
	return n.inner.Edges(ctx, runID)
}
func (n narrowRegistry) EndpointReport(ctx stdcontext.Context, runID int64) ([]registry.Endpoint, error) {
	return n.inner.EndpointReport(ctx, runID)
}
func (n narrowRegistry) TableUsageReport(ctx stdcontext.Context, runID int64) ([]registry.TableUsage, error) {
	return n.inner.TableUsageReport(ctx, runID)
}

// TestWebArchitectureGovernanceStillReadsAfterTheTaskScreensLanded is a guard,
// not a new claim: the two task screens introduced an architecture adapter of
// their own, and the governance screen must keep using capability/architecture
// directly rather than routing through it.
func TestWebArchitectureAdapterAgreesWithTheGovernanceScreen(t *testing.T) {
	t.Parallel()
	_, deps, runID := stackFixture(t)

	reg := deps.Registry.(*registry.Registry)
	src := archSource{reg: reg, model: arch.Model{}}
	if _, err := src.Check(t.Context(), runID, model.ConfidenceMedium); err == nil {
		t.Error("checking an undeclared architecture returned no error; ErrNoModel is what the " +
			"governance screen keys its guidance off")
	}
}

// TestWebCrossStackPagingReachesEveryRouteExactlyOnce is the regression for a
// pager that was unusable by design.
//
// Driven live, `#cs-range` was empty, both buttons were disabled, and 399 of 411
// routes could not be reached from the UI at all. The client bug was real, but
// so was the API's share of it: the only statement of where you were in the list
// was a SENTENCE inside an omission's detail, present only when the page was
// truncated, which the client parsed with a regular expression.
//
// This walks the whole list one page at a time using nothing but the `page`
// object, and asserts the two properties a page boundary must have: every route
// appears, and none appears twice.
func TestWebCrossStackPagingReachesEveryRouteExactlyOnce(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	var first CrossStackResponse
	get(t, srv, "/api/crossstack?limit=1", &first)
	total := first.Page.Total
	if total < 2 {
		t.Fatalf("fixture has %d routes; the paging boundary cannot be exercised", total)
	}
	if first.Page.Limit != 1 || first.Page.Offset != 0 {
		t.Fatalf("page = %+v, want offset 0 limit 1", first.Page)
	}
	if first.Page.Ordering == "" {
		t.Error("the pager does not state the order its offsets index into; without a stated total " +
			"order a page boundary cannot be shown to be coherent")
	}

	seen := map[int64]int{}
	page := first
	steps := 0
	for {
		steps++
		if steps > total+2 {
			t.Fatalf("paging did not terminate after %d steps over %d routes", steps, total)
		}
		if page.Page.Returned != len(page.Chains) {
			t.Errorf("page.returned = %d but %d chains were sent", page.Page.Returned, len(page.Chains))
		}
		for _, c := range page.Chains {
			seen[c.Endpoint.SymbolID]++
		}
		if !page.Page.HasNext {
			if page.Page.NextOffset != -1 {
				t.Errorf("no next page but next_offset = %d", page.Page.NextOffset)
			}
			break
		}
		if page.Page.NextOffset <= page.Page.Offset {
			t.Fatalf("next_offset %d does not advance past offset %d", page.Page.NextOffset, page.Page.Offset)
		}
		var next CrossStackResponse
		get(t, srv, fmt.Sprintf("/api/crossstack?limit=1&offset=%d", page.Page.NextOffset), &next)
		page = next
	}

	if len(seen) != total {
		t.Errorf("paging surfaced %d distinct routes of %d — routes were DROPPED at a page boundary", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("route %d appeared on %d pages — the boundary duplicated it", id, n)
		}
	}
}

// TestWebCrossStackHeadlineCountsDoNotChangeWithThePage pins the split between
// whole-index and page-scoped arithmetic.
func TestWebCrossStackHeadlineCountsDoNotChangeWithThePage(t *testing.T) {
	t.Parallel()
	srv, _, _ := stackFixture(t)

	var a, b CrossStackResponse
	get(t, srv, "/api/crossstack?limit=1", &a)
	get(t, srv, "/api/crossstack?limit=1&offset=1", &b)

	if a.Counts.Endpoints != b.Counts.Endpoints ||
		a.Counts.WithClient != b.Counts.WithClient ||
		a.Counts.WithHandler != b.Counts.WithHandler {
		t.Errorf("headline counts changed with the page: %+v then %+v. A number a reader quotes "+
			"must not depend on which page they were on when they read it.", a.Counts, b.Counts)
	}
	if a.Counts.Page.Chains != 1 || b.Counts.Page.Chains != 1 {
		t.Errorf("page-scoped chain counts = %d, %d, want 1 each", a.Counts.Page.Chains, b.Counts.Page.Chains)
	}
}

// TestWebGraphFileCountsAreNamedAndAgreeWithGovernance is the regression for the
// two numbers a user could not choose between.
//
// Graph said `task-app 551 files`; governance said 3,425 for the same component
// in the same session. Both were computed correctly from different populations
// and both were labelled "files".
func TestWebGraphFileCountsAreNamedAndAgreeWithGovernance(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)
	srv.deps.Config = config.Config{
		ProjectName: "governed",
		Architecture: arch.Model{Components: []arch.Component{
			{Name: "api", Include: []string{"internal/api/**"}},
			{Name: "core", Include: []string{"internal/core/**"}},
		}},
	}
	srv.cached, srv.cachedID = nil, 0 // the container grouping changes with the model

	var g GraphResponse
	get(t, srv, "/api/graph", &g)
	if len(g.Containers) == 0 {
		t.Skip("fixture produced no containers")
	}
	byName := map[string]ContainerNode{}
	for _, c := range g.Containers {
		byName[c.ID] = c
		if c.FilesMatched < c.FilesWithSymbols {
			t.Errorf("%s: files_matched %d < files_with_symbols %d, which cannot be true — "+
				"a symbol-bearing file is a matched file", c.ID, c.FilesMatched, c.FilesWithSymbols)
		}
	}

	var gov GovernanceResponse
	get(t, srv, "/api/governance", &gov)
	if !gov.Declared {
		t.Skip("fixture declares no architecture; governance has no component rows to compare")
	}
	compared := 0
	for _, row := range gov.Components {
		c, ok := byName[row.Name]
		if !ok {
			continue
		}
		compared++
		if row.FilesMatched != c.FilesMatched {
			t.Errorf("%s: governance says files_matched=%d, graph says %d — two screens, one index, "+
				"one component, two numbers", row.Name, row.FilesMatched, c.FilesMatched)
		}
		if row.FilesWithSymbols != c.FilesWithSymbols {
			t.Errorf("%s: governance says files_with_symbols=%d, graph says %d",
				row.Name, row.FilesWithSymbols, c.FilesWithSymbols)
		}
		if row.FilesWithoutSymbols != row.FilesMatched-row.FilesWithSymbols {
			t.Errorf("%s: files_without_symbols does not reconcile the other two", row.Name)
		}
	}
	if compared == 0 {
		t.Skip("no component appears on both screens in this fixture")
	}
}

// TestWebGraphClusteringIsDeterministic. Three determinism bugs have shipped in
// this codebase from unordered reads feeding first-wins rules, and a community
// partition is exactly that shape: every node's label depends on the order its
// neighbours were visited in.
func TestWebGraphClusteringIsDeterministic(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var first []byte
	for i := 0; i < 8; i++ {
		var g GraphResponse
		get(t, srv, "/api/graph", &g)
		buf, err := json.Marshal(struct {
			Clusters []Cluster        `json:"clusters"`
			Edges    []AggregatedEdge `json:"cluster_edges"`
		}{g.Clusters, g.ClusterEdges})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i == 0 {
			first = buf
			continue
		}
		if string(buf) != string(first) {
			t.Fatalf("clustering is not byte-identical across runs.\nrun 0: %s\nrun %d: %s", first, i, buf)
		}
	}

	var g GraphResponse
	get(t, srv, "/api/graph", &g)
	for _, c := range g.Containers {
		if c.Cluster == "" {
			t.Errorf("container %s carries no cluster assignment", c.ID)
		}
	}
	for _, c := range g.Clusters {
		if c.Method == "" || c.Caveat == "" {
			t.Errorf("cluster %s does not say how it was produced or what it does not claim", c.ID)
		}
	}
}

// TestWebGraphNodeBudgetIsNeverSilent. A cap that drops nodes without saying so
// produces a picture that is wrong in the one way a reader cannot detect: it
// looks complete.
func TestWebGraphNodeBudgetIsNeverSilent(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var all GraphResponse
	get(t, srv, "/api/graph", &all)
	if len(all.Containers) < 2 {
		t.Skip("fixture has fewer than two containers; the budget cannot bite")
	}

	var one GraphResponse
	get(t, srv, "/api/graph?nodes=1", &one)
	if len(one.Containers) != 1 {
		t.Fatalf("?nodes=1 drew %d containers", len(one.Containers))
	}
	b := one.NodeBudget
	if b.Requested != 1 || b.Drawn != 1 {
		t.Errorf("budget = %+v, want requested 1 drawn 1", b)
	}
	if b.Available != len(all.Containers) {
		t.Errorf("budget.available = %d, want %d (every container before the cap)", b.Available, len(all.Containers))
	}
	if b.Excluded != b.Available-b.Drawn {
		t.Errorf("budget.excluded = %d does not reconcile available %d and drawn %d", b.Excluded, b.Available, b.Drawn)
	}
	if b.Rule == "" || b.Parameter == "" {
		t.Error("the budget does not state the rule it kept by, or the control that raises it")
	}
	// And the same figure must reach the omission block, because that is where
	// a reader is trained to look for what a view left out.
	if got := omissionTotal(one.Omissions, ReasonNodeBudget); got != b.Excluded {
		t.Errorf("omissions record %d excluded by the node budget, the budget says %d", got, b.Excluded)
	}
}

// TestWebGraphAggregatedEdgeCountAndKindsShareADenominator. The row
// `(no declared component) → gateway · edges 50 · CALLS×572` was rendered
// against a real repository: Count was conditioned on the confidence floor and
// Kinds was not, in adjacent columns of the same row.
func TestWebGraphAggregatedEdgeCountAndKindsShareADenominator(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	for _, floor := range []string{"low", "medium", "high"} {
		var g GraphResponse
		get(t, srv, "/api/graph?min_confidence="+floor, &g)
		for _, e := range g.ContainerEdges {
			sum := 0
			for _, k := range e.Kinds {
				sum += k.Count
			}
			if sum != e.Count {
				t.Errorf("floor=%s edge %s→%s: count=%d but kinds sum to %d — two populations in one row",
					floor, e.From, e.To, e.Count, sum)
			}
		}
	}
}

// TestWebGraphLevelsAreAddressable covers the rung that was missing between
// "19 containers" and "1,620 symbols".
func TestWebGraphLevelsAreAddressable(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	var base GraphResponse
	get(t, srv, "/api/graph", &base)
	if len(base.Containers) == 0 {
		t.Skip("fixture produced no containers")
	}
	if len(base.Options.Levels) != 3 {
		t.Errorf("options.levels = %v, want the three addressable levels", base.Options.Levels)
	}
	// The largest container, not the first: alphabetical order puts
	// "(repository root)" first and it may hold nothing.
	target, biggest := "", -1
	for _, c := range base.Containers {
		if c.Symbols > biggest {
			target, biggest = c.ID, c.Symbols
		}
	}
	if biggest <= 0 {
		t.Skip("no container in this fixture holds a symbol")
	}

	var files GraphResponse
	get(t, srv, "/api/graph?level=files&container="+url.QueryEscape(target), &files)
	if files.Level != LevelFiles {
		t.Fatalf("level = %q, want files", files.Level)
	}
	if len(files.Files) == 0 {
		t.Fatalf("no files drawn for container %q, which holds %d symbols", target, biggest)
	}
	for _, f := range files.Files {
		if f.Path == "" {
			t.Error("a file node carries no path")
		}
	}
	// The aggregate picture must still be present, so a client can draw the
	// surrounding context without a second request.
	if len(files.Containers) == 0 {
		t.Error("a drilled-in level dropped the container view it drilled from")
	}

	var syms GraphResponse
	get(t, srv, "/api/graph?level=symbols&container="+url.QueryEscape(target), &syms)
	if syms.Level != LevelSymbols {
		t.Fatalf("level = %q, want symbols", syms.Level)
	}
	if len(syms.Nodes) == 0 {
		t.Error("no symbols drawn at the symbol level")
	}
}

// omissionTotal reads one reason out of the rollup.
func omissionTotal(om Omissions, reason string) int {
	n := 0
	for _, o := range om.Omitted {
		if o.Reason == reason {
			n += o.Count
		}
	}
	return n
}
