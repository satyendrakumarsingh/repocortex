package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// callFixture builds a run holding endpoint nodes and the calls that target
// them, and returns the run id plus a lookup from endpoint signature to symbol
// id so assertions can name the route they expect rather than a rowid.
type callFixture struct {
	runID     int64
	endpoints map[string]int64
	callerID  int64
}

// routes and calls are declared as literals so each test reads as the situation
// it pins: which routes exist, what the client asked for.
func newCallFixture(t *testing.T, r *Registry, routes []string, calls []ClientCall) callFixture {
	t.Helper()
	ctx := context.Background()

	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	// One backend file per route, because two services in one repository may
	// legitimately declare the same path and the binder must see them as two
	// candidates rather than one.
	fx := callFixture{runID: run.ID, endpoints: map[string]int64{}}
	for i, sig := range routes {
		path := "server/route" + string(rune('a'+i)) + ".go"
		fileID, err := w.AddFile(ctx, &model.File{Path: path, Language: "go", ContentHash: path})
		if err != nil {
			t.Fatal(err)
		}
		sym := model.Symbol{
			Kind: model.KindEndpoint, Name: sig, FQN: path + "#" + sig,
			FileID: fileID, Range: model.Range{Start: model.Position{Line: 1}},
			Exported: true, Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		id, err := w.AddSymbol(ctx, &sym)
		if err != nil {
			t.Fatal(err)
		}
		fx.endpoints[sig] = id
	}

	// One frontend file holding one caller, which every staged call is
	// attributed to.
	clientFileID, err := w.AddFile(ctx, &model.File{
		Path: "web/orders.ts", Language: "typescript", ContentHash: "web",
	})
	if err != nil {
		t.Fatal(err)
	}
	caller := model.Symbol{
		Kind: model.KindFunction, Name: "loadOrder", FQN: "web/orders.ts:loadOrder",
		FileID: clientFileID, Range: model.Range{Start: model.Position{Line: 5}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	callerID, err := w.AddSymbol(ctx, &caller)
	if err != nil {
		t.Fatal(err)
	}
	fx.callerID = callerID

	for _, c := range calls {
		c.SrcID = callerID
		if c.Confidence == "" {
			c.Confidence = model.ConfidenceHigh
		}
		if c.Client == "" {
			c.Client = "fetch"
		}
		if err := w.AddClientCall(ctx, clientFileID, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return fx
}

func consumesEdges(t *testing.T, r *Registry, runID int64) []model.Edge {
	t.Helper()
	all, err := r.Edges(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Edge
	for _, e := range all {
		if e.Kind == model.EdgeConsumes {
			out = append(out, e)
		}
	}
	return out
}

// TestLiteralCallBindsToItsRoute is A14's happy path: a call written out in
// full, one route that answers it, one CONSUMES edge from the calling
// declaration to the endpoint node.
func TestLiteralCallBindsToItsRoute(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	fx := newCallFixture(t, r,
		[]string{"GET /api/orders", "POST /api/payments"},
		[]ClientCall{{Method: "GET", Path: "/api/orders", Line: 6}},
	)

	st, err := r.BindClientCalls(ctx, fx.runID)
	if err != nil {
		t.Fatalf("BindClientCalls: %v", err)
	}
	if st.Total != 1 || st.Bound != 1 || st.Unbound != 0 {
		t.Fatalf("stats = %+v, want 1 total / 1 bound / 0 unbound", st)
	}

	edges := consumesEdges(t, r, fx.runID)
	if len(edges) != 1 {
		t.Fatalf("got %d CONSUMES edges, want 1", len(edges))
	}
	e := edges[0]
	if e.SrcID != fx.callerID {
		t.Errorf("edge src = %d, want the calling declaration %d", e.SrcID, fx.callerID)
	}
	if want := fx.endpoints["GET /api/orders"]; e.DstID != want {
		t.Errorf("edge dst = %d, want the endpoint node %d", e.DstID, want)
	}
	// A fully literal match invents nothing, so the edge is as strong as the
	// extraction and no stronger.
	if e.Confidence != model.ConfidenceHigh {
		t.Errorf("confidence = %q, want %q for a literal exact match", e.Confidence, model.ConfidenceHigh)
	}
	if e.Evidence.FilePath != "web/orders.ts" || e.Evidence.Range.Start.Line != 6 {
		t.Errorf("evidence points at %s:%d, want web/orders.ts:6",
			e.Evidence.FilePath, e.Evidence.Range.Start.Line)
	}

	// Idempotent: a second pass rebinds and writes no duplicate.
	if _, err := r.BindClientCalls(ctx, fx.runID); err != nil {
		t.Fatal(err)
	}
	if n := len(consumesEdges(t, r, fx.runID)); n != 1 {
		t.Fatalf("re-binding produced %d edges, want 1 — the pass is not idempotent", n)
	}
}

// TestAmbiguousCallBindsNothing is the rule that matters more than the
// plumbing. Two routes qualify equally, so the call is recorded unbound: an
// edge to the wrong endpoint sends someone to edit a handler nothing calls,
// and no edge at least says so.
func TestAmbiguousCallBindsNothing(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	// Two services in one repository, each parameterising the same path with a
	// different parameter name. Canonicalised, both are GET /api/orders/*.
	fx := newCallFixture(t, r,
		[]string{"GET /api/orders/{id}", "GET /api/orders/:orderId"},
		[]ClientCall{{Method: "GET", Path: "/api/orders/42", Line: 9}},
	)

	st, err := r.BindClientCalls(ctx, fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Bound != 0 {
		t.Fatalf("bound %d call(s) with two qualifying routes; ambiguity must refuse", st.Bound)
	}
	if st.Unbound != 1 || st.Ambiguous != 1 {
		t.Fatalf("stats = %+v, want 1 unbound of which 1 ambiguous", st)
	}
	if n := len(consumesEdges(t, r, fx.runID)); n != 0 {
		t.Fatalf("got %d CONSUMES edges from an ambiguous call, want 0", n)
	}

	// Unbound is a recorded fact, not silence: the row says so and names the
	// target, so "which APIs do we call that we cannot resolve" is answerable.
	report, err := r.ClientCallReport(ctx, fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || report[0].Binding != "unbound" {
		t.Fatalf("report = %+v, want one row recorded unbound", report)
	}
	if len(st.UnboundPaths) != 1 || st.UnboundPaths[0] != "GET /api/orders/42" {
		t.Errorf("unbound sample = %v, want the target named", st.UnboundPaths)
	}
}

// TestCallBindingEvidenceTiers pins the confidence ladder and the
// canonicalisation that feeds it, one row per situation.
func TestCallBindingEvidenceTiers(t *testing.T) {
	tests := []struct {
		name       string
		routes     []string
		call       ClientCall
		wantRoute  string // "" means nothing should bind
		wantConf   model.Confidence
		wantEdges  int
		wantAmbig  int64
		wantDynamc int64
	}{
		{
			name:      "wildcard absorbs one segment and drops one tier",
			routes:    []string{"GET /api/orders/{id}"},
			call:      ClientCall{Method: "GET", Path: "/api/orders/42", Line: 3},
			wantRoute: "GET /api/orders/{id}",
			wantConf:  model.ConfidenceMedium,
			wantEdges: 1,
		},
		{
			name:      "host, query and trailing slash are matching noise",
			routes:    []string{"GET /api/orders"},
			call:      ClientCall{Method: "GET", Path: "https://svc.example.com/API/Orders/?page=2", Line: 3},
			wantRoute: "GET /api/orders",
			wantConf:  model.ConfidenceHigh,
			wantEdges: 1,
		},
		{
			name:      "a literal route beats the parameterised one that also matches",
			routes:    []string{"GET /api/orders/summary", "GET /api/orders/{id}"},
			call:      ClientCall{Method: "GET", Path: "/api/orders/summary", Line: 3},
			wantRoute: "GET /api/orders/summary",
			wantConf:  model.ConfidenceHigh,
			wantEdges: 1,
		},
		{
			name:      "a wildcard absorbs exactly one segment, never the remainder",
			routes:    []string{"GET /api/{tenant}/orders"},
			call:      ClientCall{Method: "GET", Path: "/api/acme/eu/orders", Line: 3},
			wantRoute: "",
		},
		{
			name:      "the verb has to agree",
			routes:    []string{"POST /api/orders"},
			call:      ClientCall{Method: "GET", Path: "/api/orders", Line: 3},
			wantRoute: "",
		},
		{
			name:      "a route stating no verb answers any verb",
			routes:    []string{"ANY /health"},
			call:      ClientCall{Method: "GET", Path: "/health", Line: 3},
			wantRoute: "ANY /health",
			wantConf:  model.ConfidenceHigh,
			wantEdges: 1,
		},
		{
			name:       "a dynamic call with one prefix candidate binds at low",
			routes:     []string{"GET /api/orders/{id}", "POST /api/payments"},
			call:       ClientCall{Method: "GET", Path: "/api/orders/", Line: 3, Dynamic: true},
			wantRoute:  "GET /api/orders/{id}",
			wantConf:   model.ConfidenceLow,
			wantEdges:  1,
			wantDynamc: 1,
		},
		{
			name:       "a dynamic call with several prefix candidates binds nothing",
			routes:     []string{"GET /api/orders/{id}", "GET /api/orders/{id}/items"},
			call:       ClientCall{Method: "GET", Path: "/api/orders/", Line: 3, Dynamic: true},
			wantRoute:  "",
			wantAmbig:  1,
			wantDynamc: 1,
		},
		{
			name:       "a dynamic call with no literal prefix is staged and never bound",
			routes:     []string{"GET /api/orders/{id}"},
			call:       ClientCall{Method: "GET", Path: "", Line: 3, Dynamic: true},
			wantRoute:  "",
			wantDynamc: 1,
		},
		{
			name:      "an unknown target stays recorded rather than bound",
			routes:    []string{"GET /api/orders"},
			call:      ClientCall{Method: "GET", Path: "/v2/customers", Line: 3},
			wantRoute: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, _ := newTestRegistry(t)
			fx := newCallFixture(t, r, tc.routes, []ClientCall{tc.call})

			st, err := r.BindClientCalls(ctx, fx.runID)
			if err != nil {
				t.Fatal(err)
			}
			if st.Total != 1 {
				t.Fatalf("staged %d calls, want 1", st.Total)
			}
			if st.Dynamic != tc.wantDynamc {
				t.Errorf("dynamic = %d, want %d", st.Dynamic, tc.wantDynamc)
			}
			if st.Ambiguous != tc.wantAmbig {
				t.Errorf("ambiguous = %d, want %d", st.Ambiguous, tc.wantAmbig)
			}

			edges := consumesEdges(t, r, fx.runID)
			if tc.wantRoute == "" {
				if st.Bound != 0 || len(edges) != 0 {
					t.Fatalf("bound=%d edges=%d, want nothing bound", st.Bound, len(edges))
				}
				return
			}
			if st.Bound != 1 {
				t.Fatalf("bound = %d, want 1", st.Bound)
			}
			if len(edges) != tc.wantEdges {
				t.Fatalf("got %d CONSUMES edges, want %d", len(edges), tc.wantEdges)
			}
			if want := fx.endpoints[tc.wantRoute]; edges[0].DstID != want {
				t.Errorf("bound to symbol %d, want %s (%d)", edges[0].DstID, tc.wantRoute, want)
			}
			if edges[0].Confidence != tc.wantConf {
				t.Errorf("confidence = %q, want %q", edges[0].Confidence, tc.wantConf)
			}
		})
	}
}

// TestBoundCallOutsideAnyDeclarationWritesNoEdge covers the case that has an
// endpoint but no source node: a module-level prefetch. The call binds, so the
// report can show what it targets, but there is no function for an edge to
// leave from and naming the file instead would answer "which function calls
// this endpoint" with a filename.
func TestBoundCallOutsideAnyDeclarationWritesNoEdge(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	fx := newCallFixture(t, r, []string{"GET /api/orders"}, nil)

	// Re-stage one call with no source symbol, through the incremental path so
	// that path is covered too.
	var fileID int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT id FROM files WHERE index_run_id = ? AND path = 'web/orders.ts'`,
		fx.runID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceFileCalls(ctx, fx.runID, fileID, []ClientCall{
		{Method: "GET", Path: "/api/orders", Line: 1, Confidence: model.ConfidenceHigh},
	}); err != nil {
		t.Fatal(err)
	}

	st, err := r.BindClientCalls(ctx, fx.runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Bound != 1 {
		t.Fatalf("bound = %d, want 1", st.Bound)
	}
	if n := len(consumesEdges(t, r, fx.runID)); n != 0 {
		t.Fatalf("got %d edges from a call with no enclosing declaration, want 0", n)
	}
}
