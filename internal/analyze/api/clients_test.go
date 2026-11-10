package api

import (
	"os"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Client extraction is library-specific, so the tests are too: one case per
// convention, written the way that library is actually written, plus a body of
// inputs that must NOT produce calls.
//
// The fixtures under testdata/clients are committed source rather than strings
// in this file, because the thing being tested is whether a real component's
// calls survive a scan of a real file.

func TestExtractCallsFromReactComponent(t *testing.T) {
	calls := extractFixture(t, "testdata/clients/checkout.tsx")

	// The literal cases: method and path both stated, both captured.
	assertCall(t, calls, "GET /api/transactions", "load", ClientFetch)
	assertCall(t, calls, "POST /payment/callback", "submitPayment", ClientFetch)

	// The dynamic cases: the literal prefix, and no invented remainder.
	assertCall(t, calls, "POST /payment/refund/<unresolved>", "refundPayment", ClientAxios)
	assertCall(t, calls, "GET /api/transactions/<unresolved>", "fetchTransaction", ClientFetch)
	assertCall(t, calls, "GET /api/search?q=<unresolved>", "search", ClientAxios)
	assertCall(t, calls, "GET <unresolved>", "callAnything", ClientFetch)
	assertCall(t, calls, "DELETE /api/transactions/<unresolved>", "remove", ClientAxios)

	if len(calls) != 7 {
		t.Errorf("got %d calls, want 7: %v", len(calls), callSignatures(calls))
	}
}

func TestExtractCallsFromGoClient(t *testing.T) {
	calls := extractFixture(t, "testdata/clients/gateway.go")

	assertCall(t, calls, "POST https://ledger.internal/settlements", "Settle", ClientGoNetHTTP)
	assertCall(t, calls, "GET https://ledger.internal/health", "Health", ClientGoNetHTTP)
	assertCall(t, calls, "GET <unresolved>", "Transaction", ClientGoNetHTTP)
	assertCall(t, calls, "POST /payment/refund/<unresolved>", "Refund", ClientGoNetHTTP)
	// The verb is a variable. ANY is the honest answer; POST would be a coin flip.
	assertCall(t, calls, "ANY /payment/callback", "Verb", ClientGoNetHTTP)

	for _, c := range calls {
		if strings.Contains(c.Path, "session-key") {
			t.Errorf("a cache lookup on a well-named receiver became an HTTP call: %#v", c)
		}
	}
	if len(calls) != 5 {
		t.Errorf("got %d calls, want 5: %v", len(calls), callSignatures(calls))
	}
}

// TestFileWithNoClientCallsYieldsNothing. A file that makes no outbound call
// must produce nothing at all: a fabricated call puts a dependency in an impact
// report that the code does not have, which is the client-side twin of the
// fabricated endpoint the route extractor is built to avoid.
func TestFileWithNoClientCallsYieldsNothing(t *testing.T) {
	if calls := extractFixture(t, "testdata/clients/quiet.ts"); len(calls) != 0 {
		t.Errorf("invented %v", callSignatures(calls))
	}
}

func TestExtractCallsNeverInventsTargets(t *testing.T) {
	cases := []struct{ path, src string }{
		{"log.go", `log.Printf("calling http.Get(\"/users\") now")`},
		{"doc.go", `// This client calls http.Get("https://api.example.com/users").`},
		{"cache.go", `value, ok := c.cache.Get("session-key")`},
		{"repo.go", `row := db.Get("users")`},
		{"a.ts", `console.log('called fetch("/api/x")');`},
		{"b.ts", `const refetch = () => refetch();`},
		{"c.ts", `client.fetch("/api/x");`}, // a member named fetch is not the fetch API
		{"d.ts", `api.get("/api/x");`},      // an axios instance renamed is out of scope
		{"README.md", `fetch("/api/x")`},    // not a source file
	}
	for _, c := range cases {
		if calls := ExtractCalls(c.path, []byte(c.src)); len(calls) > 0 {
			t.Errorf("%s: invented %v from %q", c.path, callSignatures(calls), c.src)
		}
	}
}

// TestDynamicTargetsAreUnresolvedNotGuessed is the whole point of the Dynamic
// flag. A path assembled at run time is a different URL on every call, and a
// consumer that matched Path against a route table would bind the call to an
// endpoint nobody proved it reaches.
func TestDynamicTargetsAreUnresolvedNotGuessed(t *testing.T) {
	cases := []struct {
		path, src, wantLiteral string
	}{
		{"a.ts", "fetch(`/api/users/${id}`)", "/api/users/"},
		{"b.ts", `fetch("/api/users/" + id)`, "/api/users/"},
		{"c.ts", `axios.get(buildUrl(id))`, ""},
		{"d.ts", `fetch(url)`, ""},
		{"e.go", `resp, _ := http.Get(fmt.Sprintf("%s/users/%s", base, id))`, ""},
		{"f.go", `req, _ := http.NewRequest("PUT", base+"/users", nil)`, ""},
	}
	for _, c := range cases {
		calls := ExtractCalls(c.path, []byte(c.src))
		if len(calls) != 1 {
			t.Fatalf("%s: want 1 call, got %v", c.path, callSignatures(calls))
		}
		got := calls[0]
		if !got.Dynamic {
			t.Errorf("%s: %q reported as a resolved target", c.path, got.Signature())
		}
		if got.Path != c.wantLiteral {
			t.Errorf("%s: literal = %q, want %q — the remainder was invented",
				c.path, got.Path, c.wantLiteral)
		}
		if got.Confidence == model.ConfidenceHigh {
			t.Errorf("%s: an assembled target claims high confidence", c.path)
		}
		if strings.Contains(got.Path, "${") || strings.Contains(got.Path, "%s") {
			t.Errorf("%s: a substitution marker leaked into the path: %q", c.path, got.Path)
		}
	}
}

// TestMethodIsOnlyReportedWhenStated. fetch's default is GET and that is a fact
// about the API; a verb the syntax never states is ANY.
func TestMethodIsOnlyReportedWhenStated(t *testing.T) {
	cases := []struct{ path, src, want string }{
		{"a.ts", `fetch("/api/users")`, "GET"},
		{"b.ts", `fetch("/api/users", { headers: h })`, "GET"},
		{"c.ts", `fetch("/api/users", { method: "POST", body: b })`, "POST"},
		{"d.ts", `axios.put("/api/users/1", body)`, "PUT"},
		{"e.go", `http.NewRequest(method, "/api/users", nil)`, ""},
		{"f.go", `http.NewRequest(http.MethodDelete, "/api/users/1", nil)`, "DELETE"},
		{"g.go", `http.Post("/api/users", "application/json", body)`, "POST"},
	}
	for _, c := range cases {
		calls := ExtractCalls(c.path, []byte(c.src))
		if len(calls) != 1 {
			t.Fatalf("%s: want 1 call, got %v", c.path, callSignatures(calls))
		}
		if calls[0].Method != c.want {
			t.Errorf("%s: method = %q, want %q", c.path, calls[0].Method, c.want)
		}
	}
}

// TestMethodIsUnknownWhenTheOptionsObjectRunsLong. Reading only a bounded
// window is a real limit, and the answer at the edge of it must be "unknown"
// rather than the default that would usually be right.
func TestMethodIsUnknownWhenTheOptionsObjectRunsLong(t *testing.T) {
	src := "fetch(\"/api/users\", {\n" + strings.Repeat("  header: 1,\n", 20) + "  method: \"POST\",\n})"
	calls := ExtractCalls("a.ts", []byte(src))
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %v", callSignatures(calls))
	}
	if calls[0].Method != "" {
		t.Errorf("method = %q, want \"\" — a verb past the window was claimed", calls[0].Method)
	}
	if calls[0].Confidence == model.ConfidenceHigh {
		t.Error("a call whose verb was not read claims high confidence")
	}
}

// TestCallerIsTheInnermostDeclaration. "the repository calls the ledger" is
// nearly worthless; "Client.Settle calls the ledger" answers a real question.
func TestCallerIsTheInnermostDeclaration(t *testing.T) {
	src := `func outer() {
	helper := func() {}
	_ = helper
}

func Settle() {
	http.Get("/a")
}

func init() {
	http.Get("/b")
}
`
	calls := ExtractCalls("client.go", []byte(src))
	assertCall(t, calls, "GET /a", "Settle", ClientGoNetHTTP)
	assertCall(t, calls, "GET /b", "init", ClientGoNetHTTP)

	// A call outside every declaration belongs to no symbol, and saying so is
	// better than attaching it to whatever was declared last.
	top := ExtractCalls("top.ts", []byte("function unrelated() {}\nfetch(\"/a\");\n"))
	if len(top) != 1 {
		t.Fatalf("want 1 call, got %v", callSignatures(top))
	}
	if top[0].Caller != "" {
		t.Errorf("caller = %q for a top-level call, want none", top[0].Caller)
	}
}

func TestExtractCallsIgnoresUnsupportedLanguages(t *testing.T) {
	// Java and Python declare routes here; their clients are not covered, and
	// an empty result is what SupportedClients promises.
	for _, path := range []string{"C.java", "s.py", "a.rb", "b.php", "c.txt", "d.md", "e"} {
		if c := ExtractCalls(path, []byte(`fetch("/x"); axios.get("/y"); http.Get("/z")`)); c != nil {
			t.Errorf("%s produced calls from an uncovered language: %v", path, callSignatures(c))
		}
	}
}

// TestCallConfidenceIsNeverExact. Exact is reserved for facts a compiler
// proved, and no compiler checked that this string is a URL anyone requests.
func TestCallConfidenceIsNeverExact(t *testing.T) {
	srcs := map[string]string{
		"a.ts": `fetch("/a"); axios.post("/b", x);`,
		"a.go": `http.Get("/a")`,
	}
	for path, src := range srcs {
		calls := ExtractCalls(path, []byte(src))
		if len(calls) == 0 {
			t.Fatalf("%s: nothing extracted", path)
		}
		for _, c := range calls {
			if c.Confidence == model.ConfidenceExact {
				t.Errorf("%s: %q claims exact confidence", path, c.Signature())
			}
			if !c.Confidence.Valid() {
				t.Errorf("%s: %q carries no usable confidence", path, c.Signature())
			}
		}
	}
}

func TestExtractCallsIsSafeOnHostileInput(t *testing.T) {
	for _, src := range []string{
		strings.Repeat("fetch(\"", 10000),
		strings.Repeat("axios.get(`${", 10000),
		strings.Repeat("http.NewRequest(", 10000),
		"\x00\x01\x02",
		strings.Repeat("\n", 100000),
		"fetch(" + strings.Repeat("(", 5000),
		"client.Get(" + strings.Repeat("\"", 5000),
	} {
		for _, path := range []string{"a.go", "a.ts", "a.tsx", "a.js"} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic on %s with %.30q: %v", path, src, r)
					}
				}()
				_ = ExtractCalls(path, []byte(src))
			}()
		}
	}
}

// ---------------------------------------------------------------- helpers

func extractFixture(t *testing.T, path string) []Call {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return ExtractCalls(path, src)
}

func assertCall(t *testing.T, calls []Call, signature, caller string, client Client) {
	t.Helper()
	for _, c := range calls {
		if c.Signature() != signature {
			continue
		}
		if c.Caller != caller {
			t.Errorf("%s: caller = %q, want %q", signature, c.Caller, caller)
		}
		if c.Client != client {
			t.Errorf("%s: client = %q, want %q", signature, c.Client, client)
		}
		return
	}
	t.Errorf("call %q not found; got %v", signature, callSignatures(calls))
}

func callSignatures(calls []Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Signature()+" in "+c.Caller)
	}
	return out
}
