package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/multirepo"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// runSystemCLI executes `repocortex system` in-process.
//
// The command is assembled here rather than fetched out of NewRoot so the test
// exercises newSystemCmd itself and does not depend on where in the root tree
// the command happens to be registered — the same reason runDiffCLI does it.
func runSystemCLI(t *testing.T, projectRoot string, args ...string) (string, error) {
	t.Helper()

	g := &Global{}
	root := &cobra.Command{
		Use:           "repocortex",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return g.setup(cmd)
		},
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&g.JSON, "json", false, "emit machine-readable JSON on stdout")
	pf.BoolVarP(&g.Verbose, "verbose", "v", false, "verbose logging")
	pf.BoolVarP(&g.Quiet, "quiet", "q", false, "suppress all logging")
	pf.StringVar(&g.Project, "project", "", "project root")
	root.AddCommand(newSystemCmd(g))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--project", projectRoot, "--quiet", "system"}, args...))

	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// execSeed applies raw INSERTs to a freshly initialised index.
//
// Written directly rather than through the pipeline because the pipeline needs
// a real repository in a real language, and these tests are about the FEDERATION
// — which call meets which route across two databases — not about extraction.
func execSeed(t *testing.T, dbPath string, stmts []string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

// seedMember initialises a project and returns its index path.
func seedMember(t *testing.T, stmts []string) string {
	t.Helper()
	root := isolate(t)
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	dbPath := filepath.Join(root, IndexDir, IndexFile)
	execSeed(t, dbPath, stmts)
	return dbPath
}

// webIndex is the calling side: a frontend making three outbound calls, none of
// which it answers itself.
//
// The last one is deliberately into a third party. Nobody's estate is closed,
// and a report that dropped it would claim more coverage than it has.
func webIndex(t *testing.T) string {
	t.Helper()
	return seedMember(t, []string{
		`INSERT INTO index_runs(id, status, committed, started_unix, finished_unix,
		                        git_revision, accuracy_mode, producer_version,
		                        index_state, files_discovered, files_indexed)
		   VALUES(1,'ready',1,1000,2000,'8f3c1ab9de','auto','test-build','complete',1,1)`,

		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(10,1,'src/orders.ts','typescript','h1')`,

		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
		                     start_line, end_line, confidence, source)
		   VALUES(100,1,10,'function','useOrders','src/orders.ts:useOrders',
		          5,20,'high','source_code')`,

		// A literal call answered by the other repository.
		`INSERT INTO client_calls(id, index_run_id, file_id, method, path, client,
		                          src_symbol_id, start_line, dynamic, confidence, binding)
		   VALUES(1,1,10,'GET','/api/orders','fetch',100,12,0,'high','unbound')`,

		// A literal call answered by a PARAMETERISED route over there, which is
		// a tier weaker and must be graded as one.
		`INSERT INTO client_calls(id, index_run_id, file_id, method, path, client,
		                          src_symbol_id, start_line, dynamic, confidence, binding)
		   VALUES(2,1,10,'POST','/api/payments/42','axios',100,14,0,'high','unbound')`,

		// Nobody in this system answers this one.
		`INSERT INTO client_calls(id, index_run_id, file_id, method, path, client,
		                          src_symbol_id, start_line, dynamic, confidence, binding)
		   VALUES(3,1,10,'POST','https://collector.example.com/v1/traces','fetch',100,18,0,'high','unbound')`,
	})
}

// apiIndex is the answering side: three routes, one of which nobody calls.
//
// The EXPOSES edge on /api/orders is deliberately `medium` so the minimum along
// the path comes from the LAST leg rather than from the route match. A test
// where every leg is `high` would pass whether the code took a minimum or a
// maximum.
func apiIndex(t *testing.T) string {
	t.Helper()
	return seedMember(t, []string{
		`INSERT INTO index_runs(id, status, committed, started_unix, finished_unix,
		                        git_revision, accuracy_mode, producer_version,
		                        index_state, files_discovered, files_indexed)
		   VALUES(1,'ready',1,1000,2000,'a19d7e04bc','auto','test-build','complete',1,1)`,

		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(20,1,'internal/http/routes.go','go','h2')`,

		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
		                     start_line, end_line, confidence, source)
		   VALUES(200,1,20,'endpoint','GET /api/orders','internal/http/routes.go:GET /api/orders',
		          31,31,'high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
		                     start_line, end_line, confidence, source)
		   VALUES(201,1,20,'function','ListOrders','internal/http/routes.go:ListOrders',
		          40,60,'high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
		                     start_line, end_line, confidence, source)
		   VALUES(202,1,20,'endpoint','POST /api/payments/{id}','internal/http/routes.go:POST /api/payments/{id}',
		          32,32,'high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
		                     start_line, end_line, confidence, source)
		   VALUES(203,1,20,'function','Pay','internal/http/routes.go:Pay',
		          62,80,'high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
		                     start_line, end_line, confidence, source)
		   VALUES(204,1,20,'endpoint','DELETE /api/admin/purge','internal/http/routes.go:DELETE /api/admin/purge',
		          33,33,'high','source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn,
		                     start_line, end_line, confidence, source)
		   VALUES(205,1,20,'function','Purge','internal/http/routes.go:Purge',
		          82,90,'high','source_code')`,

		`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, source, detail)
		   VALUES(1,200,201,'EXPOSES','medium','internal/http/routes.go',31,'source_code','chi router')`,
		`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, source, detail)
		   VALUES(1,202,203,'EXPOSES','high','internal/http/routes.go',32,'source_code','chi router')`,
		`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence,
		                   evidence_file, evidence_line, source, detail)
		   VALUES(1,204,205,'EXPOSES','high','internal/http/routes.go',33,'source_code','chi router')`,
	})
}

// TestSystemJoinsCallToEndpointAcrossTwoIndexes is the capability itself: two
// separate databases, one answer.
func TestSystemJoinsCallToEndpointAcrossTwoIndexes(t *testing.T) {
	web, api := webIndex(t), apiIndex(t)
	here := isolate(t)

	out, err := runSystemCLI(t, here,
		"--member", "web="+web, "--member", "api="+api)
	if err != nil {
		t.Fatalf("system: %v\n%s", err, out)
	}
	t.Logf("rendered:\n%s", out)

	for _, want := range []string{
		"CROSS-REPOSITORY EDGES (2)",
		"web -> api",
		"GET /api/orders",
		"ListOrders",
		"route match",
		"the weakest leg",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report omits %q", want)
		}
	}

	rep := systemJSON(t, here, "--member", "web="+web, "--member", "api="+api)

	if len(rep.Edges) != 2 {
		t.Fatalf("want 2 cross-repo edges, got %d: %+v", len(rep.Edges), rep.Edges)
	}
	byPath := map[string]multirepo.CrossRepoEdge{}
	for _, e := range rep.Edges {
		byPath[e.Path] = e
	}

	orders, ok := byPath["/api/orders"]
	if !ok {
		t.Fatalf("the literal call did not join: %+v", rep.Edges)
	}
	if orders.FromMember != "web" || orders.ToMember != "api" {
		t.Errorf("edge crosses %s -> %s, want web -> api", orders.FromMember, orders.ToMember)
	}
	if orders.ToHandler != "ListOrders" {
		t.Errorf("handler = %q, want ListOrders", orders.ToHandler)
	}
	if orders.FromFile != "src/orders.ts" || orders.FromLine != 12 {
		t.Errorf("edge lost its call site: %s:%d", orders.FromFile, orders.FromLine)
	}
	if !rep.Complete() {
		t.Error("both members were readable; the report should call itself complete")
	}
}

// TestSystemEdgeCarriesTheMinimumAndIsNeverExact is the honesty rule the whole
// product is differentiated on, asserted end to end rather than on the engine.
func TestSystemEdgeCarriesTheMinimumAndIsNeverExact(t *testing.T) {
	web, api := webIndex(t), apiIndex(t)
	here := isolate(t)
	rep := systemJSON(t, here, "--member", "web="+web, "--member", "api="+api)

	for _, e := range rep.Edges {
		if e.Confidence == "exact" {
			t.Errorf("a cross-repo edge claims `exact`: %+v", e)
		}
		if len(e.Legs) == 0 {
			t.Fatalf("edge carries no legs, so its confidence cannot be audited: %+v", e)
		}
		min := e.Legs[0].Confidence
		for _, l := range e.Legs {
			if l.Confidence.Rank() < min.Rank() {
				min = l.Confidence
			}
		}
		if e.Confidence != min {
			t.Errorf("edge confidence %q is not the minimum leg %q: %+v", e.Confidence, min, e.Legs)
		}
	}
	for _, tbl := range rep.SharedTables {
		if tbl.Confidence == "exact" {
			t.Errorf("a shared table claims `exact`: %+v", tbl)
		}
	}

	byPath := map[string]multirepo.CrossRepoEdge{}
	for _, e := range rep.Edges {
		byPath[e.Path] = e
	}
	// call=high, route match=high, recorded EXPOSES edge=medium.
	if got := byPath["/api/orders"].Confidence; got != "medium" {
		t.Errorf("literal match with a medium EXPOSES leg = %q, want medium (legs: %+v)",
			got, byPath["/api/orders"].Legs)
	}
	// call=high, route match through a wildcard=medium, EXPOSES=high.
	if got := byPath["/api/payments/42"].Confidence; got != "medium" {
		t.Errorf("wildcard route match = %q, want medium (legs: %+v)",
			got, byPath["/api/payments/42"].Legs)
	}
	if got := byPath["/api/payments/42"].Match; got != multirepo.MatchPathParameter {
		t.Errorf("match kind = %q, want %q", got, multirepo.MatchPathParameter)
	}
}

// TestSystemReportsWhatItCouldNotJoin: a dropped call is an invisible gap, and
// an invisible gap in a federated view reads as completeness.
func TestSystemReportsWhatItCouldNotJoin(t *testing.T) {
	web, api := webIndex(t), apiIndex(t)
	here := isolate(t)

	out, err := runSystemCLI(t, here, "--member", "web="+web, "--member", "api="+api)
	if err != nil {
		t.Fatalf("system: %v\n%s", err, out)
	}
	for _, want := range []string{
		"COULD NOT JOIN",
		"outbound calls matching no known endpoint: 1",
		"collector.example.com",
		"endpoints no member of this system calls: 1",
		"DELETE /api/admin/purge",
		"WHAT THIS IS NOT",
		"HEURISTIC",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report omits %q:\n%s", want, out)
		}
	}

	rep := systemJSON(t, here, "--member", "web="+web, "--member", "api="+api)
	if len(rep.Unjoined.Calls) != 1 {
		t.Fatalf("want the third-party call reported, got %+v", rep.Unjoined.Calls)
	}
	if rep.Unjoined.Calls[0].Reason == "" {
		t.Error("an unjoined call must say why")
	}
	if len(rep.Unjoined.Endpoints) != 1 || rep.Unjoined.Endpoints[0].Endpoint != "DELETE /api/admin/purge" {
		t.Errorf("want the uncalled route reported, got %+v", rep.Unjoined.Endpoints)
	}
}

// TestSystemAbsentMemberIsReportedAndChangesTheExitCode.
//
// The failure this guards against is the quiet one: a member whose index was
// never built contributes nothing, and without this the report would be a
// smaller system rendered as a complete one.
func TestSystemAbsentMemberIsReportedAndChangesTheExitCode(t *testing.T) {
	web, api := webIndex(t), apiIndex(t)
	here := isolate(t)
	missing := filepath.Join(t.TempDir(), "billing", IndexDir, IndexFile)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("fixture path exists: %v", err)
	}

	out, err := runSystemCLI(t, here,
		"--member", "web="+web, "--member", "api="+api, "--member", "billing="+missing)
	t.Logf("rendered:\n%s", out)

	var ec *ExitCodeError
	if !errors.As(err, &ec) {
		t.Fatalf("an unreadable member must change the exit status, got err=%v", err)
	}
	if ec.Code != systemExitIncomplete {
		t.Errorf("exit code = %d, want %d", ec.Code, systemExitIncomplete)
	}
	if !ec.Silent {
		t.Error("the report is already on stdout; the error must not print a second verdict")
	}

	// The report is still produced — a partial answer beats none — and it says
	// what is missing, where, and how to fix it.
	for _, want := range []string{
		"BILLING IS MISSING FROM THIS GRAPH",
		"no index at",
		"repocortex index",
		"floor",
		"INCOMPLETE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report omits %q:\n%s", want, out)
		}
	}
	// And the members that DID open still contributed.
	if !strings.Contains(out, "CROSS-REPOSITORY EDGES (2)") {
		t.Errorf("one missing member suppressed the other two:\n%s", out)
	}

	rep := systemJSON(t, here,
		"--member", "web="+web, "--member", "api="+api, "--member", "billing="+missing)
	if rep.Complete() {
		t.Error("a report missing a member must not call itself complete")
	}
	if len(rep.Unjoined.UnavailableMembers) != 1 || rep.Unjoined.UnavailableMembers[0] != "billing" {
		t.Errorf("unavailable members = %v", rep.Unjoined.UnavailableMembers)
	}
}

// TestSystemUncommittedMemberIsDistinguishedFromAnAbsentOne. The two look
// identical to somebody staring at a member with no edges, and each has a
// different fix.
func TestSystemUncommittedMemberIsDistinguishedFromAnAbsentOne(t *testing.T) {
	web := webIndex(t)
	empty := seedMember(t, nil) // initialised schema, nothing indexed
	here := isolate(t)

	out, _ := runSystemCLI(t, here, "--member", "web="+web, "--member", "empty="+empty)
	t.Logf("rendered:\n%s", out)
	if !strings.Contains(out, "none is committed") {
		t.Errorf("an initialised-but-empty index must be distinguished from a missing one:\n%s", out)
	}
	if strings.Contains(out, "no index at "+empty) {
		t.Errorf("an existing index was reported as absent:\n%s", out)
	}
}

// TestSystemMemberProvenanceIsReported: an index written by another build holds
// edges this build would not produce, and nothing on disk announces that.
func TestSystemMemberProvenanceIsReported(t *testing.T) {
	web := webIndex(t)
	old := seedMember(t, []string{
		`INSERT INTO index_runs(id, status, committed, started_unix, finished_unix,
		                        git_revision, accuracy_mode, producer_version,
		                        index_state, files_discovered, files_indexed,
		                        files_unaccounted)
		   VALUES(1,'degraded',1,1,2,'','fast','0.9.0-ancient','partial',10,3,7)`,
	})
	here := isolate(t)

	rep := systemJSON(t, here, "--member", "web="+web, "--member", "old="+old)

	if !rep.Provenance.MixedProducers {
		t.Error("two producer builds across the estate must be reported as mixed")
	}
	var warnings string
	for _, m := range rep.Members {
		if m.Name == "old" {
			warnings = strings.Join(m.Warnings, " | ")
		}
	}
	for _, want := range []string{"degraded", "partial", "no commit", "0.9.0-ancient"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("member warnings omit %q: %s", want, warnings)
		}
	}

	out, _ := runSystemCLI(t, here, "--member", "web="+web, "--member", "old="+old)
	if !strings.Contains(out, "PROVENANCE") {
		t.Errorf("the provenance section is missing:\n%s", out)
	}
}

// TestSystemEstateFileIsEquivalentToFlags. The file exists so a team can commit
// its member list; if it disagreed with the flags it would be a second source
// of truth rather than a convenience.
func TestSystemEstateFileIsEquivalentToFlags(t *testing.T) {
	web, api := webIndex(t), apiIndex(t)
	here := isolate(t)

	estate := filepath.Join(t.TempDir(), "repocortex.system.yml")
	body := fmt.Sprintf("members:\n  - name: web\n    index: %s\n  - name: api\n    index: %s\n", web, api)
	if err := os.WriteFile(estate, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	viaFile, err := runSystemCLI(t, here, "--members", estate)
	if err != nil {
		t.Fatalf("system --members: %v\n%s", err, viaFile)
	}
	viaFlags, err := runSystemCLI(t, here, "--member", "api="+api, "--member", "web="+web)
	if err != nil {
		t.Fatalf("system --member: %v\n%s", err, viaFlags)
	}
	if viaFile != viaFlags {
		t.Errorf("the estate file and the flags disagree:\n--- file ---\n%s\n--- flags ---\n%s",
			viaFile, viaFlags)
	}
}

// TestSystemRefusesDegenerateInvocations. Each of these has no unambiguous
// reading, and picking one silently is how a tool answers a question nobody
// asked.
func TestSystemRefusesDegenerateInvocations(t *testing.T) {
	web := webIndex(t)
	here := isolate(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no members", nil, "at least two members"},
		{"one member", []string{"--member", "web=" + web}, "at least two members"},
		{
			"duplicate names",
			[]string{"--member", "web=" + web, "--member", "web=" + web},
			"named twice",
		},
		{
			"malformed member",
			[]string{"--member", "web", "--member", "api=" + web},
			"is not name=path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runSystemCLI(t, here, tc.args...)
			if err == nil {
				t.Fatalf("expected a refusal, got a report:\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
			if strings.Contains(out, "CROSS-REPOSITORY EDGES") {
				t.Errorf("a refused invocation still rendered a report:\n%s", out)
			}
		})
	}
}

// systemJSON runs the command with --json and decodes it, asserting on the way
// that stdout carried nothing but the document a script would parse.
func systemJSON(t *testing.T, projectRoot string, args ...string) *multirepo.Report {
	t.Helper()
	out, err := runSystemCLI(t, projectRoot, append(args, "--json")...)
	var ec *ExitCodeError
	if err != nil && !errors.As(err, &ec) {
		t.Fatalf("system --json: %v\n%s", err, out)
	}
	var rep multirepo.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("system --json emitted non-JSON on stdout: %v\n%s", err, out)
	}
	return &rep
}
