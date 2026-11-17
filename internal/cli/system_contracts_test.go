package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/multirepo"
)

// These tests run `repocortex system contracts` over real index databases, one
// per member. The matching rules themselves are pinned in
// internal/multirepo/contract_test.go against hand-built sources; what is
// asserted here is the part only an end-to-end run can show — that the facts
// arrive through real SQL, that the command shares `system`'s member handling,
// and that the exit status and the JSON are what a script would rely on.

// neverStale is a --stale-after long enough that the seeded indexes, which
// record a finish time in 1970, are not called old. Tests that are about
// something other than staleness pass it so that staleness is not what they
// measure.
const neverStale = "1000000h"

// driftedWebIndex is a client whose calls have drifted from the server in
// apiIndex: one still lines up, one uses the wrong verb, one is misspelt, one
// is short a path variable and one was assembled from a configured base URL.
func driftedWebIndex(t *testing.T) string {
	t.Helper()
	insertCall := func(id, line int, method, path string, dynamic int) string {
		return fmt.Sprintf(`INSERT INTO client_calls(id, index_run_id, file_id, method, path, client,
		                          src_symbol_id, start_line, dynamic, confidence, binding)
		   VALUES(%d,1,10,'%s','%s','fetch',100,%d,%d,'high','unbound')`, id, method, path, line, dynamic)
	}
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
		          5,60,'high','source_code')`,

		insertCall(1, 12, "GET", "/api/orders", 0),            // lines up
		insertCall(2, 20, "DELETE", "/api/orders", 0),         // the route takes GET only
		insertCall(3, 28, "GET", "/api/order", 0),             // no such route
		insertCall(4, 36, "POST", "/api/payments", 0),         // the route is /api/payments/{id}
		insertCall(5, 44, "GET", "", 1),                       // `${API_BASE}/api/orders`
		insertCall(6, 52, "DELETE", "/v2/api/admin/purge", 0), // only under an unseen /v2

		// A test requesting a route from a server it started itself. Not a
		// client of this system: counted, and not examined.
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(11,1,'src/orders.test.ts','typescript','h2')`,
		`INSERT INTO client_calls(id, index_run_id, file_id, method, path, client,
		                          start_line, dynamic, confidence, binding)
		   VALUES(7,1,11,'GET','/api/fake','fetch',9,0,'high','unbound')`,
	})
}

func contractsJSON(t *testing.T, projectRoot string, args ...string) *multirepo.ContractReport {
	t.Helper()
	out, err := runSystemCLI(t, projectRoot, append([]string{"contracts", "--json"}, args...)...)
	var ec *ExitCodeError
	if err != nil && !errors.As(err, &ec) {
		t.Fatalf("system contracts --json: %v\n%s", err, out)
	}
	var rep multirepo.ContractReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("system contracts --json emitted non-JSON on stdout: %v\n%s", err, out)
	}
	return &rep
}

func contractFinding(t *testing.T, rep *multirepo.ContractReport, line int) multirepo.ContractFinding {
	t.Helper()
	for _, fd := range rep.Findings {
		if fd.Call.Line == line {
			return fd
		}
	}
	t.Fatalf("no finding for the call on line %d: %+v", line, rep.Findings)
	return multirepo.ContractFinding{}
}

// TestSystemContractsFindDriftAcrossTwoIndexes is the capability: two separate
// databases, a client in one and its server in the other, and each way the two
// have come apart named with a file and line on both sides.
func TestSystemContractsFindDriftAcrossTwoIndexes(t *testing.T) {
	web, api := driftedWebIndex(t), apiIndex(t)
	here := isolate(t)

	out, err := runSystemCLI(t, here, "contracts", "--stale-after", neverStale,
		"--member", "web="+web, "--member", "api="+api)
	if err != nil {
		t.Fatalf("system contracts: %v\n%s", err, out)
	}
	t.Logf("rendered:\n%s", out)

	for _, want := range []string{
		"CONTRACTS: 2 member repositories, 2 readable",
		"CALLS EXAMINED: 6",
		"METHOD MISMATCH (1)",
		"[high] web  DELETE /api/orders",
		"call   src/orders.ts:20  in useOrders  via fetch",
		"route  api  GET /api/orders  internal/http/routes.go:31  handler ListOrders",
		"PATH-VARIABLE MISMATCH (1)",
		"route  api  POST /api/payments/{id}  internal/http/routes.go:32  handler Pay",
		"NO MATCHING ROUTE (1)",
		"[high] web  GET /api/order",
		"BASE PATH UNVERIFIED (1)",
		"[low] web  DELETE /v2/api/admin/purge",
		"route  api  DELETE /api/admin/purge  internal/http/routes.go:33  handler Purge",
		"NOT COMPARED (1)",
		// The payments route is named under the path-variable finding and is
		// still unreached: the finding is that the call does not arrive there.
		"ROUTES NO INDEXED CALL REACHES (1)",
		"[low] api  POST /api/payments/{id}  internal/http/routes.go:32",
		"WHAT THIS IS NOT",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report omits %q", want)
		}
	}
	// The coverage statement comes from the extractors themselves.
	flatOut := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"Outbound calls: fetch, axios, go-net-http.", "Routes: spring,", "base URL",
		"1 further call in test files was set aside",
	} {
		if !strings.Contains(flatOut, want) {
			t.Errorf("rendered report omits %q", want)
		}
	}

	rep := contractsJSON(t, here, "--stale-after", neverStale,
		"--member", "web="+web, "--member", "api="+api)

	verb := contractFinding(t, rep, 20)
	if verb.Kind != multirepo.ContractMethodMismatch || verb.Confidence != "high" {
		t.Errorf("wrong verb: %+v", verb)
	}
	if verb.Call.Member != "web" || verb.Call.File != "src/orders.ts" || verb.Call.Caller != "useOrders" {
		t.Errorf("the call side lost its location: %+v", verb.Call)
	}
	if len(verb.Routes) != 1 || verb.Routes[0].Member != "api" ||
		verb.Routes[0].File != "internal/http/routes.go" || verb.Routes[0].Line != 31 ||
		verb.Routes[0].Handler != "ListOrders" {
		t.Errorf("the route side lost its location: %+v", verb.Routes)
	}

	if fd := contractFinding(t, rep, 28); fd.Kind != multirepo.ContractNoRoute || len(fd.Routes) != 0 {
		t.Errorf("misspelt path: %+v", fd)
	}
	if fd := contractFinding(t, rep, 36); fd.Kind != multirepo.ContractPathVariables || fd.Confidence != "medium" {
		t.Errorf("missing path variable: %+v", fd)
	}
	if fd := contractFinding(t, rep, 52); fd.Kind != multirepo.ContractBasePath || fd.Confidence != "low" {
		t.Errorf("unverified base path: %+v", fd)
	}
	if len(rep.NotCompared) != 1 || rep.NotCompared[0].Call.Line != 44 ||
		!strings.Contains(rep.NotCompared[0].Reason, "base URL") {
		t.Errorf("the configured base URL was not reported as uncompared: %+v", rep.NotCompared)
	}

	for _, fd := range rep.Findings {
		if fd.Confidence == "exact" {
			t.Errorf("a contract finding claims `exact`: %+v", fd)
		}
	}
	if !rep.Complete() || len(rep.Weakeners) != 0 {
		t.Errorf("both members were read in full: complete=%v weakeners=%v", rep.Complete(), rep.Weakeners)
	}

	// The language each member's calls were read in arrives from the index.
	for _, m := range rep.Members {
		want := map[string]string{"web": "typescript", "api": "go"}[m.Name]
		if len(m.CallsReadIn) != 1 || m.CallsReadIn[0].Language != want || len(m.CallsNotReadIn) != 0 {
			t.Errorf("%s: calls read in %+v, not read in %+v; want %s", m.Name, m.CallsReadIn, m.CallsNotReadIn, want)
		}
	}
}

// TestSystemContractsDoNotReportCallsThatLineUp runs the fixture `system`
// itself is tested with, where every call into the other member is answered.
// The only finding is the call that leaves the system, and it says so.
func TestSystemContractsDoNotReportCallsThatLineUp(t *testing.T) {
	web, api := webIndex(t), apiIndex(t)
	here := isolate(t)
	rep := contractsJSON(t, here, "--member", "web="+web, "--member", "api="+api)

	if rep.Summary.CallsExamined != 3 || rep.Summary.Matched != 2 {
		t.Errorf("summary = %+v, want 3 calls of which 2 match", rep.Summary)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("want only the third-party call reported, got %+v", rep.Findings)
	}
	fd := rep.Findings[0]
	if fd.Kind != multirepo.ContractNoRoute || fd.Call.Host != "collector.example.com" ||
		!strings.Contains(fd.Detail, "external") {
		t.Errorf("the call out of the system: %+v", fd)
	}
	// /api/payments/42 against /api/payments/{id} is a match, not a finding,
	// and the route it reaches is not listed as uncalled.
	if len(rep.UncalledRoutes) != 1 || rep.UncalledRoutes[0].Endpoint != "DELETE /api/admin/purge" ||
		rep.UncalledRoutes[0].Confidence != "low" {
		t.Errorf("uncalled routes = %+v, want only the purge route at low", rep.UncalledRoutes)
	}
}

// TestSystemContractsAbsentMemberWeakensFindingsAndChangesTheExitCode: with a
// member unread, "no route matches" may be false, and a CI job must not be
// able to read the report as a clean one.
func TestSystemContractsAbsentMemberWeakensFindingsAndChangesTheExitCode(t *testing.T) {
	web, api := driftedWebIndex(t), apiIndex(t)
	here := isolate(t)
	missing := filepath.Join(t.TempDir(), "billing", IndexDir, IndexFile)

	out, err := runSystemCLI(t, here, "contracts", "--stale-after", neverStale,
		"--member", "web="+web, "--member", "api="+api, "--member", "billing="+missing)

	var ec *ExitCodeError
	if !errors.As(err, &ec) || ec.Code != systemExitIncomplete || !ec.Silent {
		t.Fatalf("an unreadable member must exit %d silently, got err=%v", systemExitIncomplete, err)
	}
	flatOut := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"BILLING WAS NOT READ", "no index at", "repocortex index",
		"EVERY FINDING BELOW IS ONE TIER WEAKER", "billing could not be read", "INCOMPLETE",
		// The readable members still contributed.
		"METHOD MISMATCH (1)",
	} {
		if !strings.Contains(flatOut, want) {
			t.Errorf("rendered report omits %q:\n%s", want, out)
		}
	}

	rep := contractsJSON(t, here, "--stale-after", neverStale,
		"--member", "web="+web, "--member", "api="+api, "--member", "billing="+missing)
	if rep.Complete() {
		t.Error("a report missing a member must not call itself complete")
	}
	if len(rep.Weakeners) != 1 || !strings.Contains(rep.Weakeners[0], "billing") {
		t.Errorf("weakeners = %v, want only the unread member", rep.Weakeners)
	}
	if fd := contractFinding(t, rep, 20); fd.Confidence != "medium" || !fd.Weakened {
		t.Errorf("the high finding kept its tier with a member unread: %+v", fd)
	}
}

// TestSystemContractsAStaleMemberIsSaidAndWeakensFindings. The seeded indexes
// finished in 1970, so under the default threshold both are decades stale —
// which is exactly the case where "this route does not exist" may be about a
// route list that has since changed.
func TestSystemContractsAStaleMemberIsSaidAndWeakensFindings(t *testing.T) {
	web, api := driftedWebIndex(t), apiIndex(t)
	here := isolate(t)

	rep := contractsJSON(t, here, "--member", "web="+web, "--member", "api="+api)
	if !rep.Complete() {
		t.Error("a stale member was read; it must not be reported as unreadable")
	}
	for _, m := range rep.Members {
		if !m.Stale {
			t.Errorf("%s finished indexing in 1970 and is not marked stale", m.Name)
		}
	}
	if len(rep.Weakeners) != 2 {
		t.Errorf("weakeners = %v, want one per stale member", rep.Weakeners)
	}
	if fd := contractFinding(t, rep, 20); fd.Confidence != "medium" || !fd.Weakened {
		t.Errorf("a finding against a stale route list kept its tier: %+v", fd)
	}

	out, err := runSystemCLI(t, here, "contracts", "--member", "web="+web, "--member", "api="+api)
	if err != nil {
		t.Fatalf("a stale member is a warning, not a failure: %v", err)
	}
	flatOut := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"EVERY FINDING BELOW IS ONE TIER WEAKER", "day(s) ago", "(one tier weaker: see above)"} {
		if !strings.Contains(flatOut, want) {
			t.Errorf("rendered report omits %q:\n%s", want, out)
		}
	}
}

// TestSystemContractsSayWhenAMembersCallsAreNotExtracted: a Java member makes
// calls nothing here reads. The report must say that, rather than show a
// member with zero calls and leave the reader to conclude it makes none.
func TestSystemContractsSayWhenAMembersCallsAreNotExtracted(t *testing.T) {
	api := apiIndex(t)
	javaSvc := seedMember(t, []string{
		`INSERT INTO index_runs(id, status, committed, started_unix, finished_unix,
		                        git_revision, accuracy_mode, producer_version,
		                        index_state, files_discovered, files_indexed)
		   VALUES(1,'ready',1,1000,2000,'77aa77aa77','auto','test-build','complete',2,2)`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(10,1,'src/main/java/Billing.java','java','h1')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(11,1,'src/main/resources/application.yml','yaml','h2')`,
	})
	here := isolate(t)

	out, err := runSystemCLI(t, here, "contracts", "--member", "billing="+javaSvc, "--member", "api="+api)
	if err != nil {
		t.Fatalf("system contracts: %v\n%s", err, out)
	}
	flatOut := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"CALLS NOT READ IN",
		"outbound calls are not extracted from java (1 file)",
		"No language in this member has client-call extraction at all",
		"Calls made from billing are partly or wholly unread",
	} {
		if !strings.Contains(flatOut, want) {
			t.Errorf("rendered report omits %q:\n%s", want, out)
		}
	}
	if strings.Contains(flatOut, "not extracted from yaml") {
		t.Errorf("a data language was reported as an uncovered one:\n%s", out)
	}
}

// TestSystemContractsTakeTheMembersExactlyAsSystemDoes. The estate file, the
// flags and the refusals are one piece of code; if the subcommand read them
// differently the two reports would describe two different systems.
func TestSystemContractsTakeTheMembersExactlyAsSystemDoes(t *testing.T) {
	web, api := driftedWebIndex(t), apiIndex(t)
	here := isolate(t)

	estate := filepath.Join(t.TempDir(), "repocortex.system.yml")
	body := fmt.Sprintf("members:\n  - name: web\n    index: %s\n  - name: api\n    index: %s\n", web, api)
	if err := os.WriteFile(estate, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	viaFile, err := runSystemCLI(t, here, "contracts", "--members", estate)
	if err != nil {
		t.Fatalf("system contracts --members: %v\n%s", err, viaFile)
	}
	viaFlags, err := runSystemCLI(t, here, "contracts", "--member", "api="+api, "--member", "web="+web)
	if err != nil {
		t.Fatalf("system contracts --member: %v\n%s", err, viaFlags)
	}
	if viaFile != viaFlags {
		t.Errorf("the estate file and the flags disagree:\n--- file ---\n%s\n--- flags ---\n%s", viaFile, viaFlags)
	}

	// The flags are accepted on either side of the subcommand's name.
	before, err := runSystemCLI(t, here, "--member", "api="+api, "--member", "web="+web, "contracts")
	if err != nil || before != viaFlags {
		t.Errorf("member flags before the subcommand: err=%v\n%s", err, before)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"one member", []string{"contracts", "--member", "web=" + web}, "at least two members"},
		{"duplicate names", []string{"contracts", "--member", "web=" + web, "--member", "web=" + api}, "named twice"},
		{"zero limit", []string{"contracts", "--member", "web=" + web, "--member", "api=" + api, "--limit", "0"}, "--limit must be positive"},
		{"stray argument", []string{"contracts", "extra", "--member", "web=" + web, "--member", "api=" + api}, "unknown command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runSystemCLI(t, here, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal mentioning %q, got err=%v\n%s", tc.want, err, out)
			}
			if strings.Contains(out, "CALLS EXAMINED") {
				t.Errorf("a refused invocation still rendered a report:\n%s", out)
			}
		})
	}
}

// TestSystemContractsLimitCapsEachSectionAndSaysSo.
func TestSystemContractsLimitCapsEachSectionAndSaysSo(t *testing.T) {
	stmts := []string{
		`INSERT INTO index_runs(id, status, committed, started_unix, finished_unix,
		                        git_revision, accuracy_mode, producer_version,
		                        index_state, files_discovered, files_indexed)
		   VALUES(1,'ready',1,1000,2000,'8f3c1ab9de','auto','test-build','complete',1,1)`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash)
		   VALUES(10,1,'src/many.ts','typescript','h1')`,
	}
	for i := 1; i <= 5; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO client_calls(id, index_run_id, file_id, method, path, client,
		                          start_line, dynamic, confidence, binding)
		   VALUES(%d,1,10,'GET','/nowhere/%d','fetch',%d,0,'high','unbound')`, i, i, i*10))
	}
	web, api := seedMember(t, stmts), apiIndex(t)
	here := isolate(t)

	out, err := runSystemCLI(t, here, "contracts", "-n", "2", "--member", "web="+web, "--member", "api="+api)
	if err != nil {
		t.Fatalf("system contracts -n 2: %v\n%s", err, out)
	}
	for _, want := range []string{"NO MATCHING ROUTE (5)", "3 more not shown (raise --limit)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report omits %q:\n%s", want, out)
		}
	}
	rep := contractsJSON(t, here, "--limit", "2", "--member", "web="+web, "--member", "api="+api)
	if len(rep.Findings) != 2 || rep.Truncated["no_matching_route"] != 3 || rep.Summary.NoMatchingRoute != 5 {
		t.Errorf("findings %d, truncated %v, total %d; want 2, 3 and 5",
			len(rep.Findings), rep.Truncated, rep.Summary.NoMatchingRoute)
	}
}

// TestSystemContractsIsDocumented: a subcommand nobody can find in the
// reference is a subcommand nobody runs.
func TestSystemContractsIsDocumented(t *testing.T) {
	for _, doc := range []string{"CLI.md", filepath.Join("reference", "cli.md")} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "docs", doc))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range []string{
			"### `system contracts`", "--limit",
			"method_mismatch", "path_variable_mismatch", "no_matching_route", "base_path_unverified",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("docs/%s does not mention %q", doc, want)
			}
		}
	}
}

// docs/reference/cli.md is docs/CLI.md one directory deeper, so the same text
// with every relative link given a "../" (scripts/sync-cli-reference.sh). The
// two were edited by hand and drifted: the copy had lost Next.js routes and
// ActiveMQ events. Edit docs/CLI.md and run the script.
func TestTheCLIReferenceCopyMatches(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "CLI.md"))
	if err != nil {
		t.Fatal(err)
	}
	copyText, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	want := link.ReplaceAllStringFunc(string(src), func(m string) string {
		target := link.FindStringSubmatch(m)[1]
		for _, keep := range []string{"http:", "https:", "mailto:", "#", "/"} {
			if strings.HasPrefix(target, keep) {
				return m
			}
		}
		return "](../" + target + ")"
	})
	if string(copyText) != want {
		t.Error("docs/reference/cli.md is not docs/CLI.md with its links moved a directory down; " +
			"edit docs/CLI.md and run scripts/sync-cli-reference.sh")
	}
}
