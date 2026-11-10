package web

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/planchange"
)

// The contract between the JSON these handlers emit and the JavaScript that
// reads it, checked from both ends.
//
// # Why this file exists
//
// The same bug has shipped THREE times and every instance was invisible:
//
//   - the canvas builder read `c.name`; ContainerNode marshals `label`. Every
//     container label was undefined, `a.label.localeCompare(b.label)` threw, and
//     the graph explorer rendered a blank 1038x610 canvas while every control
//     around it looked correct.
//   - the overview read `f.exported`; the payload carries `exported_symbols`.
//     The repository fingerprint printed the literal string "undefined" between
//     two correct numbers.
//   - the task box read `d.scope`; the payload nests it under `d.contract`.
//     Every list came back undefined, the figures rendered 0 / 0 / 0, and
//     NOTHING ERRORED. A zero is indistinguishable from a real answer of zero,
//     which is the worst shape this class of bug takes.
//
// The handler tests could not catch any of them: the JSON was right every time.
//
// # Why the previous version of this file stopped working, and what changed
//
// It pinned specific JavaScript function names and variable aliases —
// `buildContainerGraph`, `const r = d.resolution`, `runTask`. The frontend was
// rewritten and every one of those anchors vanished, so the test that existed to
// catch renames failed because of a rename it was not about. A contract test
// coupled to the shape of one side's implementation is a test that breaks when
// that side is refactored and passes when the contract is broken.
//
// So this version checks four things, none of which depends on how the client
// is organised:
//
//  1. THE API SIDE IS FROZEN. Every JSON field name of every response type,
//     walked reflectively and compared against a golden file. A rename fails
//     loudly, with the old and new names in the diff, on the commit that makes
//     it — which is the moment the client has to be updated too.
//  2. THE CLIENT READS NOTHING THE API DOES NOT SEND. Every `x.some_field` and
//     `x['some_field']` in the shipped JavaScript must name a field some
//     response actually marshals.
//  3. EVERY SCREEN RENDERS ITS HONESTY SURFACE. A hardcoded list of fields the
//     client MUST mention — the index health verdict, the omission rollup, the
//     verify disposition, the pager, the two file counts. `run.status:
//     "degraded"` was sent by the API for the portal's whole existence and
//     rendered by nothing; this is what makes that impossible to reship.
//  4. THE CHECKER ITSELF CATCHES A RENAME. Point 2's matcher is run against a
//     synthetic client with a renamed field and must report it. A contract test
//     never seen to fail is not known to work.
//
// # The coarse instrument, stated
//
// Point 2 cannot see a field read through a variable (`obj[key]`), and it
// matches on snake_case because that is the API's naming convention and JS
// locals here are camelCase. It would not have caught `c.name` vs `c.label` on
// its own — both are legal identifiers in some payload — which is exactly why
// point 1 and point 3 exist beside it. Three coarse instruments aimed at the
// same failure catch more of it than one precise one aimed at last month's
// implementation.

var updateGolden = flag.Bool("update", false, "rewrite testdata/api-contract.golden")

// responseTypes is every payload the portal serves. A new screen adds its type
// here; forgetting to is caught by TestEveryHandlerPayloadIsInTheContract,
// which asserts every /api route's response is represented.
func responseTypes() []any {
	return []any{
		OverviewResponse{}, GraphResponse{}, BlastResponse{}, GovernanceResponse{},
		CrossStackResponse{}, ContractResponse{}, VerifyResponse{}, SymbolsResponse{},
		SourceResponse{}, IndexStateResponse{},
		AccuracyResponse{}, FactDiffResponse{}, OwnershipResponse{},
		TraceResponse{}, BenchResponse{}, EstateResponse{},
		DBResponse{}, ConfigResponse{}, DepsResponse{}, TestsResponse{},
		MetricsResponse{},
		EventsPayload{}, missed.Report{}, planchange.Plan{},
		AgentPayload{},
	}
}

// ---------------------------------------------------------------- the API side

// fieldPaths walks a struct type and returns every JSON field it can marshal,
// as `Type.field`, sorted.
//
// Recursive through structs, pointers, slices, arrays and map values, because a
// client reads a nested field exactly as readily as a top-level one and a
// rename three levels down renders "undefined" just as silently.
func fieldPaths(v any) []string {
	seen := map[reflect.Type]bool{}
	out := map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice ||
			t.Kind() == reflect.Array || t.Kind() == reflect.Map) {
			t = t.Elem()
		}
		if t == nil || t.Kind() != reflect.Struct {
			return
		}
		if seen[t] {
			return
		}
		seen[t] = true
		defer delete(seen, t)

		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				if !f.IsExported() {
					continue
				}
				// An embedded struct with no tag flattens into its parent.
				if f.Anonymous {
					walk(f.Type, prefix)
					continue
				}
				name = f.Name
			}
			out[prefix+"."+name] = true
			walk(f.Type, prefix+"."+name)
		}
	}
	t := reflect.TypeOf(v)
	walk(t, t.Name())

	list := make([]string, 0, len(out))
	for k := range out {
		list = append(list, k)
	}
	sort.Strings(list)
	return list
}

// contractLines is the whole frozen surface, in a stable order.
func contractLines() []string {
	var out []string
	for _, v := range responseTypes() {
		out = append(out, fieldPaths(v)...)
	}
	sort.Strings(out)
	// Dedupe: the same nested type appears under several responses and the
	// paths are already distinct by prefix, but a response embedded twice would
	// otherwise duplicate.
	uniq := out[:0]
	var last string
	for _, l := range out {
		if l != last {
			uniq = append(uniq, l)
			last = l
		}
	}
	return uniq
}

// apiFieldNames is every JSON field name the API can send, flattened — the
// universe the client's reads must live inside.
func apiFieldNames() map[string]bool {
	out := map[string]bool{}
	for _, line := range contractLines() {
		parts := strings.Split(line, ".")
		out[parts[len(parts)-1]] = true
	}
	return out
}

// TestAPIFieldContractIsFrozen is point 1.
//
// The golden file is the checked-in statement of what the portal promises. It
// is not decoration: it is the thing that makes a rename a REVIEWED event
// rather than a silent one, because the diff names the field that moved and the
// commit that moves it must also update the client.
func TestAPIFieldContractIsFrozen(t *testing.T) {
	t.Parallel()

	got := strings.Join(contractLines(), "\n") + "\n"
	path := filepath.Join("testdata", "api-contract.golden")

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("rewrote " + path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nRun: go test ./internal/web -run TestAPIFieldContractIsFrozen -update", path, err)
	}
	if string(want) == got {
		return
	}

	added, removed := diffLines(strings.Split(strings.TrimSpace(string(want)), "\n"),
		strings.Split(strings.TrimSpace(got), "\n"))
	t.Errorf("the API's field contract changed.\n\nREMOVED (the client may still be reading these; a "+
		"removed field renders as `undefined`, or as a zero, which looks like a real answer):\n  %s\n\n"+
		"ADDED (nothing renders these yet):\n  %s\n\n"+
		"If the change is intended: update the client, then re-run with -update.",
		strings.Join(removed, "\n  "), strings.Join(added, "\n  "))
}

func diffLines(want, got []string) (added, removed []string) {
	w, g := map[string]bool{}, map[string]bool{}
	for _, l := range want {
		w[l] = true
	}
	for _, l := range got {
		g[l] = true
	}
	for _, l := range got {
		if !w[l] {
			added = append(added, l)
		}
	}
	for _, l := range want {
		if !g[l] {
			removed = append(removed, l)
		}
	}
	if len(added) == 0 {
		added = []string{"(none)"}
	}
	if len(removed) == 0 {
		removed = []string{"(none)"}
	}
	return added, removed
}

// TestEveryHandlerPayloadIsInTheContract catches the actual lesson of the third
// bug: a contract test only covers the payloads someone remembered to add to
// it. Every route the mux serves must have a type in responseTypes().
func TestEveryHandlerPayloadIsInTheContract(t *testing.T) {
	t.Parallel()

	known := map[string]bool{}
	for _, v := range responseTypes() {
		known[reflect.TypeOf(v).Name()] = true
	}
	// The mapping is stated rather than derived, because a route's response type
	// is not discoverable from an http.HandlerFunc. Adding a route without
	// adding it here fails below.
	routes := map[string]string{
		"/api/overview":   "OverviewResponse",
		"/api/graph":      "GraphResponse",
		"/api/blast":      "BlastResponse",
		"/api/governance": "GovernanceResponse",
		"/api/crossstack": "CrossStackResponse",
		"/api/contract":   "ContractResponse",
		"/api/verify":     "VerifyResponse",
		"/api/symbols":    "SymbolsResponse",
		"/api/source":     "SourceResponse",
		"/api/indexstate": "IndexStateResponse",
		"/api/accuracy":   "AccuracyResponse",
		"/api/diff":       "FactDiffResponse",
		"/api/who":        "OwnershipResponse",
		"/api/trace":      "TraceResponse",
		"/api/bench":      "BenchResponse",
		"/api/system":     "EstateResponse",
		"/api/db":         "DBResponse",
		"/api/config":     "ConfigResponse",
		"/api/deps":       "DepsResponse",
		"/api/tests":      "TestsResponse",
		"/api/metrics":    "MetricsResponse",
		"/api/events":     "EventsPayload",
		"/api/missed":     "Report",
		"/api/planchange": "Plan",
		"/api/agent":      "AgentPayload",
	}
	for route, typ := range routes {
		if !known[typ] {
			t.Errorf("%s serves %s, which is not in responseTypes()", route, typ)
		}
	}

	// And every route the server advertises must be in the map above. The
	// not-found handler lists them, which is the one enumeration the server
	// itself maintains.
	srv, _, _ := fixture(t)
	var body map[string]any
	get(t, srv, "/api/nope", &body)
	eps, _ := body["endpoints"].([]any)
	if len(eps) == 0 {
		t.Fatal("the not-found handler no longer lists the endpoints; this test cannot enumerate them")
	}
	for _, e := range eps {
		path, _ := e.(string)
		if path == "/api/health" {
			continue // a map literal, deliberately: it must answer before a run exists
		}
		if _, ok := routes[path]; !ok {
			t.Errorf("%s is served but has no response type in this contract test. "+
				"A payload nobody added here is a payload nothing checks.", path)
		}
	}
}

// ---------------------------------------------------------------- the JS side

// assetScripts returns every JavaScript file the portal ships: the classic
// app.js and every ES module under assets/js/, read from the EMBEDDED tree so
// this checks what the binary serves.
func assetScripts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(filepath.Join("assets"))
	if err != nil {
		t.Fatalf("read assets: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("assets", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	// The shell's modules read payload fields too — a label list today, the
	// answer line and glossary later — and a module that reads a field the API
	// renamed fails the same silent way app.js would.
	modules, err := fs.Glob(assets, "assets/js/*.mjs")
	if err != nil {
		t.Fatalf("glob assets/js: %v", err)
	}
	for _, name := range modules {
		b, err := assets.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[strings.TrimPrefix(name, "assets/")] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("the portal ships no JavaScript; this test cannot check a contract with nothing")
	}
	return out
}

// stripComments removes // and /* */ comments before matching.
//
// Without it this test read its own documentation: the comments here name
// `c.name` as the bug they prevent, and the matcher dutifully reported the
// comment as a violation. A checker that cannot tell code from prose about code
// will fail on any file that explains itself.
func stripComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, " ")
	src = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, " ")
	return src
}

// snakeReads finds every `x.some_field` and `x['some_field']` in JS source.
//
// Snake_case only. That is the API's naming convention and this client's locals
// are camelCase, so a snake_case property read is a payload read with very few
// false positives — and the few there are (CSS custom properties, dataset keys)
// are declared in jsAllowedSnake below rather than silently tolerated.
func snakeReads(src string) []string {
	src = stripComments(src)
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	dot := regexp.MustCompile(`\.([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\b`)
	for _, m := range dot.FindAllStringSubmatch(src, -1) {
		add(m[1])
	}
	idx := regexp.MustCompile(`\[\s*['"]([a-z][a-z0-9]*(?:_[a-z0-9]+)+)['"]\s*\]`)
	for _, m := range idx.FindAllStringSubmatch(src, -1) {
		add(m[1])
	}
	sort.Strings(out)
	return out
}

// jsAllowedSnake is the closed list of snake_case names that are NOT payload
// fields. Kept short and explicit: every entry is a place the client legitimately
// uses the API's naming style for something else, and an unexplained addition
// here is how a real miss gets waved through.
var jsAllowedSnake = map[string]string{
	"orphan_table":      "key in Report.by_kind map representing KindOrphanTable blindspots",
	"unmapped_endpoint": "key in Report.by_kind map representing KindUnmappedEndpoint blindspots",
	"unresolved_call":   "key in Report.by_kind map representing KindUnresolvedCall blindspots",
	"untested_critical": "key in Report.by_kind map representing KindUntestedCritical blindspots",
}

// TestJSReadsOnlyFieldsTheAPISends is point 2.
func TestJSReadsOnlyFieldsTheAPISends(t *testing.T) {
	t.Parallel()

	universe := apiFieldNames()
	for name, script := range assetScripts(t) {
		for _, field := range snakeReads(script) {
			if universe[field] {
				continue
			}
			if why, ok := jsAllowedSnake[field]; ok {
				t.Logf("%s reads .%s, allowed: %s", name, field, why)
				continue
			}
			t.Errorf("%s reads .%s, which no response in this package marshals. "+
				"That read yields undefined — which renders as the literal string \"undefined\", or "+
				"as a zero that looks like a real answer. If the field was renamed, rename it here too; "+
				"if it is genuinely not a payload field, add it to jsAllowedSnake with a reason.",
				name, field)
		}
	}
}

// TestClientRendersTheHonestySurface is point 3.
//
// # Why a hardcoded list is the right instrument here
//
// Everything else in this file checks that what the client reads EXISTS. This
// checks that what MUST be read IS read, and there is no way to derive that from
// either side: a payload field nothing renders serialises perfectly and looks
// correct in every handler test. `run.status: "degraded"` was returned by
// /api/health and /api/overview on every request for the portal's whole
// existence; the freshness card printed `current` and `complete` badges from
// other fields; grep for "degraded" in the client returned nothing. A degraded
// run presented as a healthy one everywhere a reader actually looks.
//
// Each entry below is a defect that reached a user. Removing one is a decision
// to let that defect back in, and it should be as visible as this list makes it.
func TestClientRendersTheHonestySurface(t *testing.T) {
	t.Parallel()

	required := []struct {
		field string
		why   string
	}{
		{"index_health", "a degraded run presented as a healthy one on every screen; the verdict is " +
			"served structured precisely so a client cannot render it as silence"},
		{"trustworthy", "the boolean a banner gates on, so a client comparing status strings cannot get " +
			"the comparison wrong"},

		{"omitted", "the per-view statement of what was left out — the rule this whole package exists " +
			"to enforce"},
		{"totals", "the per-reason rollup that replaced a hoisted count rendering the same 8,015 edges " +
			"as two separate omissions"},

		{"exit_code", "the verify verdict a CI-minded reader screenshots"},
		{"unverified", "`pass` and `unverified` must never render the same; this is the boolean that " +
			"says which one this is"},
		{"answered", "a question that ran over the repository and engaged none of the changed files is " +
			"asked and unanswered, and that combination used to render as a green PASS"},

		{"next_offset", "399 of 411 routes were unreachable because paging state lived in a sentence " +
			"inside an omission and the client parsed it with a regex"},
		{"has_next", "the pager's own statement that there is more, rather than an inference"},

		{"files_with_symbols", "one component, two file counts, both labelled `files`, on two screens " +
			"in one session"},
		{"files_matched", "the other half of the same fact; showing one without the other is how the " +
			"disagreement started"},

		{"node_budget", "a cap that drops nodes without saying so produces a picture that is wrong in " +
			"the one way a reader cannot detect: it looks complete"},
		{"clusters", "community assignment is computed server-side and deterministically; a client that " +
			"ignores it re-derives a different partition"},

		{"confidence", "the tier every edge is drawn at — the product's whole visual language"},
		{"evidence", "an edge without a checkable file:line is an assertion, which is what every " +
			"competitor ships"},
	}

	scripts := assetScripts(t)
	joined := ""
	for _, src := range scripts {
		joined += stripComments(src) + "\n"
	}

	universe := apiFieldNames()
	for _, req := range required {
		if !universe[req.field] {
			t.Errorf("the API no longer sends %q, which the portal is required to render: %s",
				req.field, req.why)
			continue
		}
		if !strings.Contains(joined, req.field) {
			t.Errorf("the client never mentions %q. It is required because: %s", req.field, req.why)
		}
	}
}

// TestContractCheckerDetectsARename is point 4: the test that tests the test.
//
// A contract test never seen to fail is not known to work. This runs the same
// matcher TestJSReadsOnlyFieldsTheAPISends uses against a synthetic client that
// reads a renamed field, and fails if the matcher lets it through.
func TestContractCheckerDetectsARename(t *testing.T) {
	t.Parallel()

	universe := apiFieldNames()

	// A real field, spelled the way the API spells it, must pass.
	for _, ok := range []string{
		`const n = c.files_with_symbols;`,
		`row.push(d['exit_code']);`,
	} {
		for _, field := range snakeReads(ok) {
			if !universe[field] {
				t.Errorf("the matcher rejected %q, which the API does send", field)
			}
		}
	}

	// The same fields, renamed the way every one of the three shipped bugs was.
	for _, renamed := range []struct{ src, field string }{
		// Sentinels must name fields the API does NOT send: the case proves the
		// checker catches a read of a field that does not exist. `file_count`
		// stopped working as a sentinel when the deps screen began serving a
		// field by that name — a real field cannot stand in for a renamed one.
		{`const n = c.file_tally;`, "file_tally"},
		{`row.push(d['exit_status']);`, "exit_status"},
		{`banner(meta.health_status);`, "health_status"},
	} {
		found := false
		for _, field := range snakeReads(renamed.src) {
			if field == renamed.field {
				found = true
				if universe[field] {
					t.Errorf("%q is in the API universe, so this case does not exercise a rename", field)
				}
			}
		}
		if !found {
			t.Errorf("the matcher did not even SEE the read in %q. It would not have caught the "+
				"rename, and a checker that cannot fail is not a check.", renamed.src)
		}
	}

	// And the comment stripper must not hide a real read, nor surface a fake
	// one. Both directions have bitten this file before.
	mixed := "// we used to read c.old_name here\nconst v = c.new_name;"
	if got := snakeReads(mixed); len(got) != 1 || got[0] != "new_name" {
		t.Errorf("comment handling is wrong: read %v from a line with one real access and one in prose", got)
	}
}

// TestNoLiteralUndefinedReachesTheUser pins the symptom rather than the cause.
// A field mismatch renders the string "undefined" into the page, and that is
// what a reader sees — a number they cannot distinguish from a real one.
func TestNoLiteralUndefinedReachesTheUser(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)
	for _, path := range []string{
		"/api/overview", "/api/graph", "/api/governance",
		"/api/crossstack", "/api/contract", "/api/verify", "/api/symbols",
		"/api/graph?level=files&container=internal/api",
		"/api/crossstack?offset=1",
	} {
		var rawBody json.RawMessage
		rec := get(t, srv, path, &rawBody)
		if rec.Code != 200 {
			continue
		}
		if strings.Contains(string(rawBody), `"undefined"`) {
			t.Errorf("%s serialises the literal string \"undefined\"", path)
		}
	}
}

// TestEveryOmissionCarriesAHumanLabel: the portal rendered `low_confidence_hidden:`
// and `row_budget:` as UI labels — raw snake_case JSON identifiers shown to
// people, twice on one card with two unrelated meanings. The words are the API's
// job now, so this asserts every reason has them.
func TestEveryOmissionCarriesAHumanLabel(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	for _, path := range []string{"/api/overview", "/api/graph", "/api/governance", "/api/crossstack"} {
		body := raw(t, srv, path)
		om, ok := body["omissions"].(map[string]any)
		if !ok {
			t.Errorf("%s has no omissions block", path)
			continue
		}
		for _, key := range []string{"omitted", "totals"} {
			list, _ := om[key].([]any)
			for _, item := range list {
				o, _ := item.(map[string]any)
				label, _ := o["label"].(string)
				reason, _ := o["reason"].(string)
				if strings.TrimSpace(label) == "" {
					t.Errorf("%s: omissions.%s entry %q carries no label", path, key, reason)
				}
				if label == reason {
					t.Errorf("%s: omissions.%s entry labels itself with its own machine key %q",
						path, key, reason)
				}
			}
		}
	}
}

var _ = fmt.Sprint // kept for the error paths above
