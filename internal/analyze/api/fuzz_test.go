package api

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fuzzPaths cover every branch of languageOf plus one extension it rejects.
//
// The rejected one is not filler: Extract must return nothing at all for a file
// it does not claim to understand, and a scanner that started matching Express
// syntax inside a .txt file would inflate an API surface with routes from
// documentation.
var fuzzPaths = []string{
	"src/main/java/Controller.java",
	"internal/http/router.go",
	"app/routes.py",
	"app/urls.py",
	"src/routes.js",
	"src/routes.tsx",
	"src/users.controller.ts",
	"README.txt",
}

// FuzzExtract hunts fabricated endpoints.
//
// The input is source from repositories RepoCortex did not write, so a panic here
// aborts an index run over one file someone else committed. The properties
// asserted below are the ones that decide whether a reader can trust the API
// surface report: a route with no path names no endpoint, a Line that does not
// exist in the file sends a reader to nowhere, and `exact` is reserved for facts
// a compiler proved — nothing found by scanning text ever qualifies, and a route
// claiming it would outrank a fact that really was proved.
func FuzzExtract(f *testing.F) {
	for _, s := range []string{
		"@RestController\n@RequestMapping(\"/payment\")\nclass C {\n@PostMapping(\"/callback\")\npublic void handle() {}\n}",
		"@RequestMapping\npublic void all() {}",
		"mux.HandleFunc(\"/health\", healthz)",
		"r.Get(\"/users/{id}\", h.Get)",
		"mux.Handle(\"/x\", h).Methods(\"POST\")",
		"@app.get(\"/items/{id}\")\nasync def read(): pass",
		"@app.route(\"/x\", methods=[\"POST\",\"GET\"])\ndef x(): pass",
		"app.get('/a', handler)",
		"router.all(`/b`, h)",
		// Degenerate spellings that a framework accepts syntactically but that
		// name no endpoint. These are where an empty or nonsense route comes from.
		"mux.HandleFunc(\"\", h)",
		"app.get('', h)",
		"@app.get(\"\")\ndef f(): pass",
		"",
		"@@@@@@",
		"\n\n\n",
		// Django urlconf shapes, including a decorator-wrapped view.
		`path("", views.i)`,
		`path("x/", include("a"))`,
		`re_path(r"^$", v)`,
		`path("a", "b")`,
		`path("w/", cache_page(60)(views.f))`,
		// NestJS controller decorators, including degenerate and unterminated
		// spellings.
		"@Controller('u')\n@Get()\nf(){}",
		"@Controller({ path: 'u' })",
		"@Get(",
		"@All('x')",
		"@Controller(['a'])\n@Get('x')\nx(){}",
		"@Controller({\npath: 'p',\n})",
	} {
		for i := range fuzzPaths {
			f.Add(uint8(i), s)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, src string) {
		path := fuzzPaths[int(which)%len(fuzzPaths)]
		routes := Extract(path, []byte(src))
		if path == "README.txt" && len(routes) != 0 {
			t.Fatalf("routes from an unsupported extension: %#v", routes)
		}

		lines := strings.Count(src, "\n") + 1
		supported := SupportedFrameworks()
		for _, r := range routes {
			// A route the report cannot name is a row that says an endpoint
			// exists without saying which one.
			if r.Path == "" {
				t.Fatalf("empty path in %#v from %q", r, src)
			}
			// Line is what a reader clicks. Off the end of the file it points at
			// nothing, and there is no way for them to tell.
			if r.Line < 1 || r.Line > lines {
				t.Fatalf("line %d outside 1..%d in %#v from %q", r.Line, lines, r, src)
			}
			if r.Confidence == model.ConfidenceExact {
				t.Fatalf("exact confidence from a text scan: %#v from %q", r, src)
			}
			if !r.Confidence.Valid() {
				t.Fatalf("unclassifiable confidence %q in %#v", r.Confidence, r)
			}
			// An empty Method means ANY, which is a real answer. Anything else
			// must be a verb this package declares it reports, or the surface
			// report gains a method no framework registered.
			if r.Method != "" && !httpMethods[r.Method] {
				t.Fatalf("method %q is not a reported verb: %#v from %q", r.Method, r, src)
			}
			if !containsFramework(supported, r.Framework) {
				t.Fatalf("framework %q is missing from SupportedFrameworks: %#v", r.Framework, r)
			}
			// Signature is what lands in a ticket. Whitespace INSIDE a path is
			// the author's own literal and is reported as written; a signature
			// that is blank altogether names nothing.
			if sig := r.Signature(); strings.TrimSpace(sig) == "" {
				t.Fatalf("unusable signature %q from %#v", sig, r)
			}
		}
	})
}

// FuzzExtractCalls hunts fabricated dependencies.
//
// The same reasoning as FuzzExtract, pointed the other way. A call is read as
// "this symbol depends on that endpoint", so the properties that decide whether
// it can be trusted are: a Line a reader can click, a confidence that never
// claims a compiler proved it, a verb this package declares it reports, and —
// the one that only exists on this side — a Path that is either literal or
// flagged Dynamic. A resolved-looking path that was actually assembled would
// bind a component to an endpoint nobody proved it calls.
func FuzzExtractCalls(f *testing.F) {
	for _, s := range []string{
		`fetch("/api/users")`,
		"fetch(`/api/users/${id}`)",
		`fetch("/api/users", { method: "POST" })`,
		"fetch(\"/api/users\", {\n  method: \"PATCH\",\n})",
		`axios.get("/api/users")`,
		`axios.delete("/api/users/" + id)`,
		`const load = async () => fetch("/a")`,
		`http.NewRequest(http.MethodPost, "https://x/y", nil)`,
		`http.NewRequestWithContext(ctx, method, "/y", nil)`,
		`http.Get("https://x/y")`,
		`c.httpClient.Post("/y", "application/json", b)`,
		// Degenerate and adversarial spellings: an empty target, a call inside a
		// string, a receiver that only looks like a client.
		`fetch("")`,
		`axios.get('')`,
		`console.log('fetch("/api/x")')`,
		`c.cache.Get("session-key")`,
		`fetch(`,
		"",
		"``````",
		"\n\n\n",
	} {
		for i := range fuzzPaths {
			f.Add(uint8(i), s)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, src string) {
		path := fuzzPaths[int(which)%len(fuzzPaths)]
		calls := ExtractCalls(path, []byte(src))
		switch path {
		case "README.txt", "src/main/java/Controller.java", "app/routes.py", "app/urls.py":
			if len(calls) != 0 {
				t.Fatalf("calls from a language SupportedClients excludes: %#v", calls)
			}
		}

		lines := strings.Count(src, "\n") + 1
		supported := SupportedClients()
		for _, c := range calls {
			// A call with neither a literal target nor the Dynamic flag says a
			// request goes somewhere, and names nowhere, without admitting it.
			if c.Path == "" && !c.Dynamic {
				t.Fatalf("target-less call reported as resolved: %#v from %q", c, src)
			}
			if c.Line < 1 || c.Line > lines {
				t.Fatalf("line %d outside 1..%d in %#v from %q", c.Line, lines, c, src)
			}
			if c.Confidence == model.ConfidenceExact {
				t.Fatalf("exact confidence from a text scan: %#v from %q", c, src)
			}
			if !c.Confidence.Valid() {
				t.Fatalf("unclassifiable confidence %q in %#v", c.Confidence, c)
			}
			// An assembled target must never outrank a literal one, or a report
			// sorted by confidence puts guesses at the top.
			if c.Dynamic && c.Confidence.AtLeast(model.ConfidenceHigh) {
				t.Fatalf("assembled target claims %q: %#v from %q", c.Confidence, c, src)
			}
			if c.Method != "" && !httpMethods[c.Method] {
				t.Fatalf("method %q is not a reported verb: %#v from %q", c.Method, c, src)
			}
			if !containsClient(supported, c.Client) {
				t.Fatalf("client %q is missing from SupportedClients: %#v", c.Client, c)
			}
			// Signature is what lands in a ticket. A blank one names nothing.
			if sig := c.Signature(); strings.TrimSpace(sig) == "" {
				t.Fatalf("unusable signature %q from %#v", sig, c)
			}
		}
	})
}

func containsClient(all []Client, want Client) bool {
	for _, c := range all {
		if c == want {
			return true
		}
	}
	return false
}

func containsFramework(all []Framework, want Framework) bool {
	for _, f := range all {
		if f == want {
			return true
		}
	}
	return false
}
