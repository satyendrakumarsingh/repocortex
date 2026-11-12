package docs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
)

// TestDeterministicRegeneration is the property that makes this artifact worth
// having: an identical index produces identical bytes, so the document can be
// committed and its diff read as a diff of the architecture.
//
// Three progressively stronger claims, because only the third one is actually
// hard. Generating twice from one Generator proves nothing about map iteration
// — Go's randomised range order would still have to be unlucky twice. A second
// Generator over the same rows is closer. Re-deriving from a SHUFFLED index is
// the real test: it fails the moment any ordering decision in this package leans
// on the order the registry happened to return rows in.
func TestDeterministicRegeneration(t *testing.T) {
	t.Parallel()
	reg, m := smallIndex(), smallModel()

	g, err := New(reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := g.Derive(context.Background(), 7, m)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	second, err := g.Derive(context.Background(), 7, m)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	docs := []struct {
		name string
		of   func(*View) string
	}{
		{"architecture", (*View).ArchitectureMarkdown},
		{"capabilities", (*View).CapabilityMarkdown},
	}

	for _, d := range docs {
		want := d.of(first)
		if got := d.of(second); got != want {
			t.Errorf("%s: two Derives from one Generator disagree\n%s", d.name, firstDiff(want, got))
		}
		// Same View rendered twice must also be identical: a renderer that
		// ranged a map would pass the check above and fail this one.
		if got := d.of(first); got != want {
			t.Errorf("%s: rendering the same View twice is not stable\n%s", d.name, firstDiff(want, got))
		}
	}

	// The load-bearing case. Seeds chosen arbitrarily and fixed, so a failure
	// reproduces exactly rather than appearing one run in twenty.
	for _, seed := range []int64{1, 2, 99} {
		v := deriveFixture(t, reg.shuffled(seed), m)
		for _, d := range docs {
			want, got := d.of(first), d.of(v)
			if got != want {
				t.Errorf("%s: output changed when the index rows arrived in a different order (seed %d)\n%s",
					d.name, seed, firstDiff(want, got))
			}
		}
	}

	// The partially-declared repository exercises the coverage, shared-data and
	// unaccounted-for derivations, none of which the fully-declared fixture
	// reaches. Each of them groups by a map key, which is exactly where a
	// map-iteration ordering bug hides.
	partialReg, partialM := unclaimedIndex(), partialModel()
	base := deriveFixture(t, partialReg, partialM).CapabilityMarkdown()
	for _, seed := range []int64{3, 7, 42} {
		got := deriveFixture(t, partialReg.shuffled(seed), partialM).CapabilityMarkdown()
		if got != base {
			t.Errorf("capabilities: partially-declared output changed with row order (seed %d)\n%s",
				seed, firstDiff(base, got))
		}
	}
}

// TestNoTimestampInBody guards the one line that would silently destroy the
// property above. A generated-on date is the most natural thing in the world to
// add to a document like this, and it would make every regeneration a diff.
func TestNoTimestampInBody(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())
	for _, doc := range []string{v.ArchitectureMarkdown(), v.CapabilityMarkdown()} {
		for _, banned := range []string{"Generated at", "generated on", "Timestamp", "UTC", "20:", "T00:00:00"} {
			if strings.Contains(doc, banned) {
				t.Errorf("document contains %q; a timestamp in the body makes every regeneration a diff", banned)
			}
		}
	}
}

// TestEveryDiagramNodeCarriesEvidence is the claim the document makes about
// itself. A node or arrow a reader cannot check is the same unverifiable
// assertion the generated-prose tools ship, and one of them is enough to make
// the whole page untrustworthy — so this asserts over every diagram at once
// rather than sampling.
func TestEveryDiagramNodeCarriesEvidence(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())

	diagrams := v.Diagrams()
	if len(diagrams) < 3 {
		t.Fatalf("expected a component diagram, at least one flow and an inheritance view, got %d", len(diagrams))
	}
	nodes := 0
	for _, d := range diagrams {
		if problems := d.Validate(); len(problems) > 0 {
			t.Errorf("diagram %q: %s", d.ID, strings.Join(problems, "; "))
		}
		nodes += len(d.Nodes)
		for _, n := range d.Nodes {
			if n.Evidence.Location() == "" {
				t.Errorf("diagram %q node %q: citation has no file to open", d.ID, n.ID)
			}
		}
		for _, e := range d.Edges {
			if e.Evidence.Location() == "" {
				t.Errorf("diagram %q edge %s->%s: citation has no file to open", d.ID, e.From, e.To)
			}
		}
	}
	if nodes == 0 {
		t.Fatal("no diagram nodes at all; the assertion above would pass vacuously")
	}

	// And the citations must survive into the rendered document, not merely
	// exist in the model.
	doc := v.ArchitectureMarkdown()
	for _, want := range []string{
		"internal/api/users.go:10",
		"internal/store/users.go:17",
		".repocortex.yml",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("rendered document does not cite %q", want)
		}
	}
}

// TestNoDeclaredComponentsSaysSo is the refusal this package is built around.
//
// The tempting behaviour — cluster the directories, draw the boxes, call it the
// architecture — produces a diagram that describes the code's current shape and
// therefore tells a reader nothing they could not get from `ls`. This asserts
// the document says the repository declared none, and asserts specifically that
// it did NOT name any of the obvious inferred candidates.
func TestNoDeclaredComponentsSaysSo(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), arch.Model{})

	if v.ComponentsDeclared {
		t.Fatal("ComponentsDeclared is true for an empty model")
	}
	if len(v.Components) != 0 || len(v.Crossings) != 0 || len(v.Capabilities) != 0 {
		t.Fatalf("invented %d components, %d crossings and %d capabilities from an empty model",
			len(v.Components), len(v.Crossings), len(v.Capabilities))
	}

	archDoc := v.ArchitectureMarkdown()
	capDoc := v.CapabilityMarkdown()
	for _, doc := range []string{archDoc, capDoc} {
		if !strings.Contains(doc, "declares no components") {
			t.Error("document does not say the repository declares no components")
		}
		if !strings.Contains(doc, "does not infer them") {
			t.Error("document does not explain why no component model was invented")
		}
		// The package names in this fixture are the exact things a clustering
		// heuristic would have promoted to components.
		for _, invented := range []string{"| api |", "| service |", "| store |"} {
			if strings.Contains(doc, invented) {
				t.Errorf("document contains %q: a component model was inferred from the code's shape", invented)
			}
		}
	}
	if !strings.Contains(capDoc, "No capabilities can be derived") {
		t.Error("capability document does not decline to derive capabilities")
	}

	// The edge-derived sections are still rendered: they are facts from
	// indexers, and they do not need a declared model.
	if len(v.Endpoints) == 0 {
		t.Error("endpoint flows were suppressed; they come from EXPOSES edges, not from components")
	}
	if !strings.Contains(archDoc, "GET /users") {
		t.Error("architecture document lost its endpoint flows when no components were declared")
	}
	if !strings.Contains(archDoc, "implements") {
		t.Error("architecture document lost its inheritance view when no components were declared")
	}
}

// TestDerivedNotInferred checks the A26 promise from the other direction: a
// component the index knows nothing about is listed AS SUCH rather than dropped
// or described.
func TestDerivedNotInferred(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())

	var frontend *Capability
	for i := range v.Capabilities {
		if v.Capabilities[i].Component == "frontend" {
			frontend = &v.Capabilities[i]
		}
	}
	if frontend == nil {
		t.Fatal("the declared component that matched no files was dropped from the capability model")
	}
	if !frontend.Silent {
		t.Error("a component with no endpoints and no tables is not marked Silent")
	}
	doc := v.CapabilityMarkdown()
	if !strings.Contains(doc, "The graph is silent about this component") {
		t.Error("capability document does not say where the graph is silent")
	}

	// And the capabilities that ARE derived name only what the graph contains.
	var api *Capability
	for i := range v.Capabilities {
		if v.Capabilities[i].Component == "api" {
			api = &v.Capabilities[i]
		}
	}
	if api == nil {
		t.Fatal("no capability derived for the api component")
	}
	if len(api.Endpoints) != 3 {
		t.Errorf("api capability has %d endpoints, want the 3 the index declares", len(api.Endpoints))
	}
	wantTables := map[string]string{"users": "writes", "audit_log": "writes"}
	if len(api.Tables) != len(wantTables) {
		t.Fatalf("api capability has %d tables, want %d", len(api.Tables), len(wantTables))
	}
	for _, tab := range api.Tables {
		if wantTables[tab.Table] != tab.Access {
			t.Errorf("table %s: access %q, want %q", tab.Table, tab.Access, wantTables[tab.Table])
		}
		if !tab.Evidence.Cited() {
			t.Errorf("table %s carries no evidence", tab.Table)
		}
	}
}

// TestCapabilityModelStatesWhatItDoesNotCover is the A26 honesty requirement.
//
// A capability model derived from the declarations describes the declared part
// of the repository, and a reader has no way to tell that from a description of
// the whole repository — the two render identically. So the fraction covered is
// stated and the remainder is listed with the reason it fell out, which turns a
// silent gap into either "widen the globs" or "this really belongs to nobody".
func TestCapabilityModelStatesWhatItDoesNotCover(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, unclaimedIndex(), partialModel())

	want := Coverage{Endpoints: 3, EndpointsClaimed: 1, Tables: 3, TablesClaimed: 1}
	if v.Coverage != want {
		t.Errorf("coverage = %+v, want %+v", v.Coverage, want)
	}

	// Endpoints, in source order, each with the reason it is unaccounted for.
	gotEndpoints := map[string]string{}
	for _, u := range v.UnclaimedEndpoints {
		gotEndpoints[u.Symbol.display()] = u.Detail
		if !u.Symbol.Evidence.Cited() {
			t.Errorf("unclaimed endpoint %s carries no citation", u.Symbol.display())
		}
	}
	if len(gotEndpoints) != 2 {
		t.Fatalf("unclaimed endpoints = %v, want the 2 outside the declared component", gotEndpoints)
	}
	if got := gotEndpoints["POST /jobs"]; got != unclaimedEndpointNoComponent {
		t.Errorf("POST /jobs reason = %q, want the handler-outside reason", got)
	}
	// The distinction that matters: "nothing declares it" is not the same
	// finding as "we could not see what serves it".
	if got := gotEndpoints["GET /ping"]; got != unclaimedEndpointNoHandler {
		t.Errorf("GET /ping reason = %q, want the no-EXPOSES-edge reason", got)
	}

	gotTables := map[string]string{}
	for _, u := range v.UnclaimedTables {
		gotTables[u.Symbol.display()] = u.Detail
	}
	if got := gotTables["jobs"]; got != unclaimedTableOutside {
		t.Errorf("jobs reason = %q, want the accessed-from-outside reason", got)
	}
	if got := gotTables["archive"]; got != unclaimedTableUntouched {
		t.Errorf("archive reason = %q, want the nothing-touches-it reason", got)
	}
	if _, claimed := gotTables["orders"]; claimed {
		t.Error("orders is read by the declared component and must not be listed as unaccounted for")
	}

	doc := v.CapabilityMarkdown()
	for _, sentence := range []string{
		"account for 1 of 3 endpoints and 1 of 3 tables",
		"Not accounted for by any capability",
		"GET /ping",
		"internal/legacy/cron.go:5",
	} {
		if !strings.Contains(doc, sentence) {
			t.Errorf("capability document does not state %q", sentence)
		}
	}
}

// TestSharedTableIsNamed. Two capabilities on one table are coupled through the
// database whatever the component boundaries say, and that coupling is invisible
// in a document that only lists each capability's own tables.
func TestSharedTableIsNamed(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())

	if len(v.SharedTables) != 1 {
		t.Fatalf("shared tables = %+v, want exactly the one two capabilities reach", v.SharedTables)
	}
	st := v.SharedTables[0]
	if st.Table != "users" {
		t.Errorf("shared table = %q, want users", st.Table)
	}
	got := map[string]string{}
	for _, by := range st.By {
		got[by.Component] = by.Access
		if !by.Evidence.Cited() {
			t.Errorf("%s's access to %s carries no citation", by.Component, st.Table)
		}
	}
	if got["api"] != "writes" || got["store"] != "reads" {
		t.Errorf("accesses = %v, want api writing and store reading", got)
	}
	// audit_log is written by one capability only and must not appear: a
	// "shared" section that lists unshared tables is a section nobody reads.
	for _, s := range v.SharedTables {
		if s.Table == "audit_log" {
			t.Error("audit_log has one accessing capability and is listed as shared")
		}
	}
	if !strings.Contains(v.CapabilityMarkdown(), "## Shared data") {
		t.Error("capability document has no shared-data section")
	}
}

// TestCapabilityDependenciesAreTheCrossings: the "depends on" list is a
// projection of the crossings the architecture document already derived, not a
// second derivation that could disagree with it.
func TestCapabilityDependenciesAreTheCrossings(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())

	byName := map[string]Capability{}
	for _, c := range v.Capabilities {
		byName[c.Component] = c
	}
	api := byName["api"]
	if len(api.DependsOn) != 2 {
		t.Fatalf("api depends on %d capabilities, want 2", len(api.DependsOn))
	}
	// Violations first, exactly as the crossing order fixes them.
	if api.DependsOn[0].To != "store" || api.DependsOn[0].RuleID != "api-goes-through-service" {
		t.Errorf("api's first dependency = %+v, want the forbidden one on store", api.DependsOn[0])
	}
	if api.DependsOn[1].To != "service" || api.DependsOn[1].Verdict != "allowed" {
		t.Errorf("api's second dependency = %+v, want the allowed one on service", api.DependsOn[1])
	}

	// Every link must be one of the crossings, with the same evidence.
	crossings := map[string]Crossing{}
	for _, cr := range v.Crossings {
		crossings[cr.From+"->"+cr.To] = cr
	}
	for _, c := range v.Capabilities {
		for _, d := range c.DependsOn {
			cr, ok := crossings[c.Component+"->"+d.To]
			if !ok {
				t.Errorf("%s depends on %s, which is not a derived crossing", c.Component, d.To)
				continue
			}
			if d.Count != cr.Count || d.Evidence != cr.Evidence || d.Verdict != cr.Verdict {
				t.Errorf("%s -> %s disagrees with the crossing: %+v vs %+v", c.Component, d.To, d, cr)
			}
		}
	}

	// A component the index has only dependencies for is not "silent".
	if byName["service"].Silent {
		t.Error("service is called by api and calls store; the graph is not silent about it")
	}
	if !byName["frontend"].Silent {
		t.Error("frontend matches nothing at all and should still be marked silent")
	}
}

// TestUndeclaredRepositoryStillListsWhatTheGraphKnows.
//
// The refusal to name capabilities without declarations is right, and it used to
// end the document there — which threw away the endpoints and tables the
// indexers DID find. Those are facts with citations; what is missing is a name
// for the capability each one serves. The document must publish the first and
// withhold only the second.
func TestUndeclaredRepositoryStillListsWhatTheGraphKnows(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), arch.Model{})

	if len(v.Capabilities) != 0 || len(v.SharedTables) != 0 {
		t.Fatalf("invented %d capabilities and %d shared tables from an empty model",
			len(v.Capabilities), len(v.SharedTables))
	}
	if len(v.UnclaimedEndpoints) != 3 || len(v.UnclaimedTables) != 2 {
		t.Fatalf("unclaimed = %d endpoints, %d tables; want every one in the index",
			len(v.UnclaimedEndpoints), len(v.UnclaimedTables))
	}
	want := Coverage{Endpoints: 3, EndpointsClaimed: 0, Tables: 2, TablesClaimed: 0}
	if v.Coverage != want {
		t.Errorf("coverage = %+v, want %+v", v.Coverage, want)
	}

	doc := v.CapabilityMarkdown()
	for _, sentence := range []string{
		"No capabilities can be derived",
		"What the graph knows anyway",
		"GET /users",
		"users",
		"internal/api/users.go:10",
	} {
		if !strings.Contains(doc, sentence) {
			t.Errorf("capability document does not contain %q", sentence)
		}
	}
	// And it still must not name a capability after a package.
	for _, invented := range []string{"| api |", "| service |", "| store |"} {
		if strings.Contains(doc, invented) {
			t.Errorf("document contains %q: a capability was named from the code's shape", invented)
		}
	}
}

// TestFlowFollowsTheGraph checks the endpoint→handler→service→table shape is the
// graph's and not a template's.
func TestFlowFollowsTheGraph(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())

	byRoute := map[string]EndpointFlow{}
	for _, f := range v.Endpoints {
		byRoute[f.Endpoint.Name] = f
	}

	list, ok := byRoute["GET /users"]
	if !ok {
		t.Fatal("GET /users has no flow")
	}
	if list.Handler == nil || list.Handler.FQN != "api.ListUsers" {
		t.Fatalf("GET /users handler = %v, want api.ListUsers", list.Handler)
	}
	var gotCalls []string
	for _, c := range list.Calls {
		gotCalls = append(gotCalls, c.Symbol.FQN)
	}
	want := []string{"service.UserService.List", "store.UserRepo.Query"}
	if strings.Join(gotCalls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v (breadth-first, two hops)", gotCalls, want)
	}
	if len(list.Tables) != 1 || list.Tables[0].Table != "users" || list.Tables[0].Access != "reads" {
		t.Errorf("tables = %+v, want one read of users", list.Tables)
	}

	// An endpoint with no EXPOSES edge is shown, not hidden, and says why.
	orphan, ok := byRoute["GET /health"]
	if !ok {
		t.Fatal("an endpoint with no EXPOSES edge was dropped")
	}
	if orphan.Handler != nil {
		t.Error("a handler was invented for an endpoint with no EXPOSES edge")
	}
	d := v.FlowDiagram(orphan)
	if !strings.Contains(d.Note, "no EXPOSES edge") {
		t.Errorf("flow diagram does not explain the missing handler: %q", d.Note)
	}
	if len(d.Edges) != 0 {
		t.Errorf("flow diagram drew %d edges for an endpoint with no handler", len(d.Edges))
	}
}

// TestViolationIsVisible: a diagram that renders a forbidden dependency exactly
// like a permitted one has hidden the only part of it anybody needed.
func TestViolationIsVisible(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())

	var found bool
	for _, cr := range v.Crossings {
		if cr.From == "api" && cr.To == "store" {
			found = true
			if cr.Verdict != string(arch.VerdictForbidden) {
				t.Errorf("api -> store verdict = %q, want forbidden", cr.Verdict)
			}
			if cr.RuleID != "api-goes-through-service" {
				t.Errorf("violation cites rule %q", cr.RuleID)
			}
			if cr.Weakest != "low" {
				t.Errorf("weakest confidence = %q, want low: the crossing rests on low-confidence edges", cr.Weakest)
			}
		}
	}
	if !found {
		t.Fatal("the api -> store crossing was not derived")
	}
	mermaid := v.ComponentDiagram().Mermaid()
	if !strings.Contains(mermaid, "-.->") {
		t.Error("the forbidden dependency is drawn with the same arrow as a permitted one")
	}
	if !strings.Contains(mermaid, "api-goes-through-service") {
		t.Error("the diagram does not name the rule the dependency breaks")
	}
}

// TestPortErrorsAreWrapped: CLAUDE.md §4.3.
func TestPortErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("registry exploded")
	g, err := New(&fakeRegistry{err: sentinel})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = g.Derive(context.Background(), 7, smallModel())
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not wrap the port's: %v", err)
	}
	if !strings.Contains(err.Error(), "run 7") {
		t.Errorf("error does not say which run failed: %v", err)
	}
}

// TestContextCancellation: CLAUDE.md §4.1 — honour cancellation, do not just
// accept a ctx and ignore it.
func TestContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g, err := New(smallIndex())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := g.Derive(ctx, 7, smallModel()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Derive on a cancelled context returned %v, want context.Canceled", err)
	}
}

// TestConstructorRejectsBadInput covers the option plumbing.
func TestConstructorRejectsBadInput(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Error("New accepted a nil registry")
	}
	if _, err := New(smallIndex(), WithConfigPath("")); err == nil {
		t.Error("New accepted an empty config path, leaving component nodes with nothing to cite")
	}
	if _, err := New(smallIndex(), WithLimits(Limits{MaxComponents: -1})); err == nil {
		t.Error("New accepted a negative cap")
	}
	// A partial Limits literal must keep the other defaults rather than
	// silently emptying every other section.
	g, err := New(smallIndex(), WithLimits(Limits{MaxComponents: 2}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if g.limits.MaxEndpointFlows != DefaultLimits().MaxEndpointFlows {
		t.Errorf("a zero field overrode the default: MaxEndpointFlows = %d", g.limits.MaxEndpointFlows)
	}
}

// firstDiff reports the first line where two documents differ, which is far more
// useful in a failure than two thousand lines of Markdown.
func firstDiff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) && i < len(g); i++ {
		if w[i] != g[i] {
			return "first difference at line " + itoa(i+1) + ":\n  want: " + w[i] + "\n  got:  " + g[i]
		}
	}
	return "documents differ in length: want " + itoa(len(w)) + " lines, got " + itoa(len(g))
}
