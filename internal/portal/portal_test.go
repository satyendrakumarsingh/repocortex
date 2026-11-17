package portal

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

func serve(t *testing.T) *httptest.Server {
	t.Helper()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api:"+r.Method+":"+r.URL.Path)
	})
	ts := httptest.NewServer(Handler(api))
	t.Cleanup(ts.Close)
	return ts
}

// reply is what a test needs from a response. The body is read and closed inside
// get, so no caller is handed a response it would have to close.
type reply struct {
	status int
	header http.Header
	body   string
}

func get(t *testing.T, ts *httptest.Server, path string, mutate func(*http.Request)) reply {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func TestServesThePageAndItsAssets(t *testing.T) {
	ts := serve(t)
	for path, want := range map[string]string{
		"/":        `<script type="module" src="app.js">`,
		"/app.js":  "THE PAGE HAS NO DATA ROUTES OF ITS OWN",
		"/app.css": "--accent:",
	} {
		resp := get(t, ts, path, nil)
		if resp.status != http.StatusOK || !strings.Contains(resp.body, want) {
			t.Errorf("GET %s: status %d, missing %q", path, resp.status, want)
		}
	}
}

func TestForwardsTheToolEndpoint(t *testing.T) {
	ts := serve(t)
	for _, path := range []string{APIPrefix, APIPrefix + "/tools", APIPrefix + "/tools/find_callers"} {
		if body := get(t, ts, path, nil).body; body != "api:GET:"+path {
			t.Errorf("GET %s was not forwarded to the API: %q", path, body)
		}
	}
}

// A page on the internet must not be able to point the user's browser at this
// server by a name it controls.
func TestRefusesAForeignHost(t *testing.T) {
	ts := serve(t)
	resp := get(t, ts, "/", func(r *http.Request) { r.Host = "attacker.example" })
	if resp.status != http.StatusForbidden {
		t.Errorf("a foreign Host got status %d, want 403", resp.status)
	}
	resp = get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Host = "attacker.example:7777" })
	if resp.status != http.StatusForbidden {
		t.Errorf("a foreign Host reached the API: status %d", resp.status)
	}
}

// The tool endpoint takes a POST with no token, so a cross-site page must not
// be able to make one.
func TestRefusesACrossOriginRequest(t *testing.T) {
	ts := serve(t)
	resp := get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if resp.status != http.StatusForbidden {
		t.Errorf("a cross-origin request got status %d, want 403", resp.status)
	}
	// Another port on this machine is another origin.
	resp = get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:1") })
	if resp.status != http.StatusForbidden {
		t.Errorf("another local port got status %d, want 403", resp.status)
	}
	// The page's own origin is allowed.
	resp = get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Header.Set("Origin", ts.URL) })
	if resp.status != http.StatusOK {
		t.Errorf("the page's own origin got status %d, want 200", resp.status)
	}
}

func TestSendsALockedDownPolicy(t *testing.T) {
	ts := serve(t)
	resp := get(t, ts, "/", nil)
	csp := resp.header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy is missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("the policy allows inline or evaluated code: %s", csp)
	}
	if resp.header.Get("Cache-Control") != "no-store" || resp.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing no-store or nosniff: %v", resp.header)
	}
}

// The policy forbids inline script and style, so the page must contain none:
// a page that needs them would render broken, silently.
func TestThePageNeedsNothingThePolicyForbids(t *testing.T) {
	page, err := os.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]`).MatchString(html) {
		t.Error("index.html contains an inline script")
	}
	if strings.Contains(html, "<style") || regexp.MustCompile(`\sstyle="`).MatchString(html) {
		t.Error("index.html contains inline style")
	}
	if regexp.MustCompile(`\son[a-z]+="`).MatchString(html) {
		t.Error("index.html contains an inline event handler")
	}
	if regexp.MustCompile(`(?i)(src|href)="https?://`).MatchString(html) {
		t.Error("index.html loads something from another origin")
	}

	script, err := os.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	// Repository text reaches the page as text nodes only.
	for _, banned := range []string{".innerHTML", ".outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(string(script), banned) {
			t.Errorf("app.js uses %s; repository content must never be parsed as markup", banned)
		}
	}
	// A style attribute is inline style however it is written, and the policy
	// drops it without a word. Dimensions go through the element's style
	// object, which the policy permits.
	for _, banned := range []string{"setAttribute('style'", `setAttribute("style"`, ".cssText", "style:"} {
		if strings.Contains(string(script), banned) {
			t.Errorf("app.js sets inline style with %s; the policy discards it silently", banned)
		}
	}
}

// ---------------------------------------------------------------- pages

func asset(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("assets", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Every address the page writes into a link must be one its router answers.
// A link to a page that does not exist falls through to Home, which looks
// like a working link and is a dead end.
func TestEveryAddressThePageLinksToIsRouted(t *testing.T) {
	script := asset(t, "app.js")
	routed := map[string]bool{"home": true} // the router's fallback
	for _, m := range regexp.MustCompile(`head === '([a-z]+)'`).FindAllStringSubmatch(script, -1) {
		routed[m[1]] = true
	}
	// The pages this portal is made of. Losing one from the router is a
	// regression no other test would notice.
	for _, page := range []string{"s", "f", "r", "t", "search", "explore", "change", "health", "agents"} {
		if !routed[page] {
			t.Errorf("the router has no page for #/%s", page)
		}
	}
	linked := regexp.MustCompile("#/([a-z]+)")
	for _, name := range []string{"app.js", "index.html"} {
		for _, m := range linked.FindAllStringSubmatch(asset(t, name), -1) {
			if !routed[m[1]] {
				t.Errorf("%s links to #/%s, which the router does not answer", name, m[1])
			}
		}
	}
}

// The page has no data routes of its own, so every figure on it is a tool's
// answer. A tool the page calls by a name the server does not register would
// draw as a failure on a live page and nowhere else; this reads the names the
// server registers and checks each call against them.
func TestThePageCallsOnlyToolsTheServerRegisters(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join("..", "mcp", "*.go"))
	if err != nil || len(sources) == 0 {
		t.Fatalf("cannot read the tool registrations in internal/mcp: %v", err)
	}
	registered := map[string]bool{}
	name := regexp.MustCompile(`Name:\s+"([a-z]+_[a-z_]+)"`)
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range name.FindAllStringSubmatch(string(b), -1) {
			registered[m[1]] = true
		}
	}
	if len(registered) < 10 {
		t.Fatalf("found only %d registered tools; the registrations have moved and this test must follow them", len(registered))
	}

	script := asset(t, "app.js")
	called := map[string]bool{}
	for _, pattern := range []string{
		`\b(?:ask|call|statusRow)\('([a-z_]+)'`, // a direct call
		`liveSection\([^,]+, '([a-z_]+)'`,       // a section that loads one tool
		`tool: '([a-z_]+)'`,                     // a tab of the symbol page
		`commandFor\('([a-z_]+)'`,               // a command shown to the reader
	} {
		for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(script, -1) {
			called[m[1]] = true
		}
	}
	if len(called) < 10 {
		t.Fatalf("found only %d tool calls in app.js; the way it calls tools has changed and this test must follow it", len(called))
	}
	var missing []string
	for tool := range called {
		if !registered[tool] {
			missing = append(missing, tool)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("app.js calls tools the server does not register: %s", strings.Join(missing, ", "))
	}
	// The file, route and table pages each rest on these. If one is renamed
	// the page must be changed with it.
	for _, tool := range []string{"get_file_outline", "list_hotspots", "list_dead_code", "list_api_endpoints",
		"list_db_tables", "find_references", "find_callees", "find_tests", "trace_call_path"} {
		if !called[tool] {
			t.Errorf("app.js no longer calls %s, which a file, route or table page is built on", tool)
		}
	}
}

// ---------------------------------------------------------------- stylesheet

// A rule that names a token no theme defines falls back to nothing: text in
// the browser's default colour on the portal's ground, unchecked by the
// contrast test. And a literal colour below the theme blocks is a colour only
// one theme was designed for.
func TestEveryColourIsADefinedToken(t *testing.T) {
	css := asset(t, "app.css")
	defined := map[string]bool{"display": true, "body": true, "mono": true} // the three type stacks
	for name := range themes(t)[0] {
		defined[name] = true
	}
	for _, m := range regexp.MustCompile(`var\(--([a-z0-9-]+)\)`).FindAllStringSubmatch(css, -1) {
		if !defined[m[1]] {
			t.Errorf("app.css uses var(--%s), which no theme defines", m[1])
		}
	}
	rules := css[strings.LastIndex(css, "color-scheme: dark"):]
	if hex := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`).FindString(rules); hex != "" {
		t.Errorf("app.css uses the literal colour %s in a rule; rules use tokens so both themes are covered", hex)
	}
}

// Motion is only ever a response to waiting, and a reader who asked for less
// of it gets none.
func TestMotionIsSwitchedOffOnRequest(t *testing.T) {
	css := asset(t, "app.css")
	moving := regexp.MustCompile(`(?m)^([.#][^{}\n]+)\{[^}]*\b(?:animation|transition):`).FindAllStringSubmatch(css, -1)
	if len(moving) == 0 {
		return
	}
	at := strings.Index(css, "@media (prefers-reduced-motion: reduce)")
	if at < 0 {
		t.Fatal("app.css animates and has no prefers-reduced-motion rule")
	}
	reduced := css[at:]
	for _, m := range moving {
		selector := strings.TrimSpace(m[1])
		if !strings.Contains(reduced, selector) {
			t.Errorf("%s moves and is not stilled under prefers-reduced-motion", selector)
		}
	}
}

// ---------------------------------------------------------------- contrast

var tokenLine = regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})`)

// themes returns the token blocks of app.css in order: light, dark (system),
// dark (chosen).
func themes(t *testing.T) []map[string]string {
	t.Helper()
	css, err := os.ReadFile("assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, block := range strings.Split(string(css), "--ground:")[1:] {
		end := strings.Index(block, "}")
		tokens := map[string]string{}
		for _, m := range tokenLine.FindAllStringSubmatch("--ground:"+block[:end], -1) {
			tokens[m[1]] = m[2]
		}
		out = append(out, tokens)
	}
	if len(out) != 3 {
		t.Fatalf("expected three theme blocks in app.css, found %d", len(out))
	}
	return out
}

func luminance(hex string) float64 {
	channel := func(s string) float64 {
		v, _ := strconv.ParseUint(s, 16, 8)
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(hex[1:3]) + 0.7152*channel(hex[3:5]) + 0.0722*channel(hex[5:7])
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Both themes are checked against WCAG AA on every surface a colour actually
// lands on: 4.5:1 for text, 3:1 for the edge of a control. A theme chosen by
// eye passes on the designer's monitor and fails on a projector.
func TestThemesMeetContrast(t *testing.T) {
	pairs := []struct {
		fg, bg string
		min    float64
		what   string
	}{
		{"ink", "sheet", 7, "body text on a sheet"},
		{"ink", "ground", 7, "body text on the page"},
		{"ink-2", "sheet", 4.5, "secondary text on a sheet"},
		{"ink-2", "ground", 4.5, "secondary text on the page"},
		{"ink-2", "well", 4.5, "secondary text in a well"},
		{"ink-3", "sheet", 4.5, "quiet text on a sheet"},
		{"ink-3", "ground", 4.5, "quiet text on the page"},
		{"accent", "sheet", 4.5, "a link on a sheet"},
		{"accent", "ground", 4.5, "a link on the page"},
		{"accent", "accent-soft", 4.5, "the current section's label"},
		{"on-accent", "accent", 4.5, "a primary button's label"},
		{"on-accent", "accent-hi", 4.5, "a hovered primary button's label"},
		{"caution", "sheet", 4.5, "a caution on a sheet"},
		{"good", "sheet", 4.5, "a healthy state on a sheet"},
		{"bad", "ground", 4.5, "an error heading"},
		{"line-strong", "sheet", 3, "a control's edge on a sheet"},
		{"line-strong", "ground", 3, "a control's edge on the page"},
		{"ink", "well", 7, "a command in a well"},
		// The file, route and table pages.
		{"accent", "well", 4.5, "a linked name inside a block of code"},
		{"accent-hi", "well", 4.5, "a hovered link inside a block of code"},
		{"accent-hi", "ground", 4.5, "a hovered link on the page"},
	}
	names := []string{"light", "dark (system)", "dark (chosen)"}
	for i, theme := range themes(t) {
		for _, p := range pairs {
			fg, bg := theme[p.fg], theme[p.bg]
			if fg == "" || bg == "" {
				t.Errorf("%s: token --%s or --%s is not defined", names[i], p.fg, p.bg)
				continue
			}
			if got := contrast(fg, bg); got < p.min {
				t.Errorf("%s: %s is %.2f:1 (--%s %s on --%s %s), below %.1f:1",
					names[i], p.what, got, p.fg, fg, p.bg, bg, p.min)
			}
		}
	}
}

// The dark palette is written twice, once for the system setting and once for
// an explicit choice. They must be the same palette.
func TestTheTwoDarkBlocksAreIdentical(t *testing.T) {
	all := themes(t)
	system, chosen := all[1], all[2]
	if len(system) != len(chosen) {
		t.Fatalf("the dark blocks define %d and %d tokens", len(system), len(chosen))
	}
	for name, value := range system {
		if chosen[name] != value {
			t.Errorf("--%s is %s under the system setting and %s when chosen", name, value, chosen[name])
		}
	}
	if len(all[0]) != len(system) {
		t.Errorf("light defines %d tokens and dark %d; a token missing from one theme falls back to the other's", len(all[0]), len(system))
	}
}

func TestLoopbackHostAndSameOrigin(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "localhost:7777": true, "127.0.0.1:7777": true, "[::1]:7777": true, "LOCALHOST": true,
		"example.com": false, "127.0.0.1.evil.example": false, "10.0.0.5:7777": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
	for _, tc := range []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:7777", "127.0.0.1:7777", true},
		{"http://localhost:7777", "localhost:7777", true},
		{"http://127.0.0.1:1", "127.0.0.1:7777", false},
		{"https://evil.example", "127.0.0.1:7777", false},
		{"null", "127.0.0.1:7777", false},
	} {
		if got := sameOrigin(tc.origin, tc.host); got != tc.want {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", tc.origin, tc.host, got, tc.want)
		}
	}
}

// The pages are files; nothing is meant to be sent to them.
func TestRefusesAWriteToAPage(t *testing.T) {
	ts := serve(t)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST / got status %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want the methods a page accepts", got)
	}
}

// ---------------------------------------------------------------- symbol verdict and tables

// The symbol page reads the counts and the risk out of analyze_impact's
// summary. When the pattern missed, the page printed the tool's whole first
// line as its heading: on a real repository, eighty words where one sentence
// belongs, because an incomplete answer puts a clause between the counts and
// the risk. The pattern is taken from the page itself and run against every
// shape the tool writes.
func TestTheReachSentenceReadsEveryShapeTheToolWrites(t *testing.T) {
	js := asset(t, "app.js")
	m := regexp.MustCompile(`const REACH_SENTENCE = /(.+)/;`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js no longer defines REACH_SENTENCE")
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("REACH_SENTENCE must stay in the syntax Go shares, so this test can run it: %v", err)
	}
	cases := []struct {
		summary                string
		direct, indirect, risk string
		orHigher               bool
	}{
		{"updateStatus (a/T.java:18): 2 direct, 2 indirect. Risk low (score 0).", "2", "2", "low", false},
		{"INCOMPLETE — f: its name appears in at least 1 other place(s) that are not recorded as calls. " +
			"f (a.py:140): 45 direct, 535 indirect; at least 6 more symbol(s) at low confidence are excluded; " +
			"pass min_confidence=low. Risk moderate or higher (score 45; the analysis was cut at depth 3).",
			"45", "535", "moderate", true},
		{"Run (x.go:9): 3 direct, 1 indirect, 2 reached by interface dispatch. Risk high (score 70).", "3", "1", "high", false},
		{"g (y.ts:4): 0 direct, 0 indirect. Risk unknown.", "0", "0", "unknown", false},
	}
	for _, c := range cases {
		got := re.FindStringSubmatch(c.summary)
		if got == nil {
			t.Errorf("no match for %q", c.summary)
			continue
		}
		if got[1] != c.direct || got[2] != c.indirect || got[3] != c.risk || (got[4] != "") != c.orHigher {
			t.Errorf("%q read as %q direct, %q indirect, risk %q, or-higher %q", c.summary, got[1], got[2], got[3], got[4])
		}
	}
}

// Whatever the index can vouch for, the heading is a sentence the page builds;
// the tool's own explanation goes in the fold under it.
func TestTheSymbolHeadingIsNeverTheToolsFirstLine(t *testing.T) {
	js := asset(t, "app.js")
	start := strings.Index(js, "// The one-sentence answer")
	end := strings.Index(js, "const tabs = h('div', { class: 'tabs'")
	if start < 0 || end < start {
		t.Fatal("cannot find the symbol verdict in app.js")
	}
	block := js[start:end]
	for _, bad := range []string{"textContent = r.summary", "textContent = reach.value.summary", "textContent = a.summary"} {
		if strings.Contains(block, bad) {
			t.Errorf("the symbol heading is set from the tool's summary again (%s)", bad)
		}
	}
	if !strings.Contains(block, "caveatFor(") {
		t.Error("an answer the index cannot vouch for no longer says why under the heading")
	}
}

// A table must fit the frame it is drawn in. A path that could not wrap, or a
// qualified name kept on one line, pushed the last columns past the edge,
// where a scrollbar macOS hides made them look cut off.
func TestTablesBreakNamesAndPathsAtTheirSeparators(t *testing.T) {
	js, css := asset(t, "app.js"), asset(t, "app.css")
	for _, want := range []string{
		"href: fileHref(path) }, breakablePath(",
		"h('a', { class: 'name', href }, breakableName(name))",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js: %q is missing", want)
		}
	}
	if regexp.MustCompile(`td \.name\s*\{[^}]*white-space:\s*nowrap`).MatchString(css) {
		t.Error("app.css keeps a name in a table on one line again")
	}
	if !regexp.MustCompile(`td \.why\s*\{[^}]*overflow-wrap:\s*anywhere`).MatchString(css) {
		t.Error("app.css: the explanation column may no longer break a quoted path")
	}
}

// A caller can be the top level of a file: a script's statements, a module's
// import-time calls. The index names it registry.FileCallerName(path), which
// is no symbol's name, so the page links it to the file instead of to a symbol
// page that would find nothing.
func TestATopLevelCallerLinksToItsFile(t *testing.T) {
	js := asset(t, "app.js")
	m := regexp.MustCompile(`const TOP_LEVEL = /(.+)/;`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js no longer defines TOP_LEVEL")
	}
	re, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatalf("TOP_LEVEL must stay in the syntax Go shares, so this test can run it: %v", err)
	}
	for _, p := range []string{"scripts/deploy.py", "web/app (old).js", "main.go"} {
		got := re.FindStringSubmatch(registry.FileCallerName(p))
		if got == nil || got[1] != p {
			t.Errorf("TOP_LEVEL read %q as %q, want the path %q", registry.FileCallerName(p), got, p)
		}
	}
	if re.MatchString("deploy_release") {
		t.Error("TOP_LEVEL matches an ordinary name")
	}
	for _, fn := range []string{"function symbolLink(", "function hrefFor("} {
		i := strings.Index(js, fn)
		if i < 0 {
			t.Fatalf("app.js no longer defines %s", fn)
		}
		body := js[i : i+strings.Index(js[i:], "\n}")]
		if !strings.Contains(body, "TOP_LEVEL.exec(name)") || !strings.Contains(body, "fileHref(top[1])") {
			t.Errorf("%s no longer links a top-level caller to its file", fn)
		}
	}
}
