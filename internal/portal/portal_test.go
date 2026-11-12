package portal

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
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

func get(t *testing.T, ts *httptest.Server, path string, mutate func(*http.Request)) (*http.Response, string) {
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
	return resp, string(body)
}

func TestServesThePageAndItsAssets(t *testing.T) {
	ts := serve(t)
	for path, want := range map[string]string{
		"/":        `<script type="module" src="app.js">`,
		"/app.js":  "THE PAGE HAS NO DATA ROUTES OF ITS OWN",
		"/app.css": "--accent:",
	} {
		resp, body := get(t, ts, path, nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("GET %s: status %d, missing %q", path, resp.StatusCode, want)
		}
	}
}

func TestForwardsTheToolEndpoint(t *testing.T) {
	ts := serve(t)
	for _, path := range []string{APIPrefix, APIPrefix + "/tools", APIPrefix + "/tools/find_callers"} {
		if _, body := get(t, ts, path, nil); body != "api:GET:"+path {
			t.Errorf("GET %s was not forwarded to the API: %q", path, body)
		}
	}
}

// A page on the internet must not be able to point the user's browser at this
// server by a name it controls.
func TestRefusesAForeignHost(t *testing.T) {
	ts := serve(t)
	resp, _ := get(t, ts, "/", func(r *http.Request) { r.Host = "attacker.example" })
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host got status %d, want 403", resp.StatusCode)
	}
	resp, _ = get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Host = "attacker.example:7777" })
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host reached the API: status %d", resp.StatusCode)
	}
}

// The tool endpoint takes a POST with no token, so a cross-site page must not
// be able to make one.
func TestRefusesACrossOriginRequest(t *testing.T) {
	ts := serve(t)
	resp, _ := get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-origin request got status %d, want 403", resp.StatusCode)
	}
	// Another port on this machine is another origin.
	resp, _ = get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:1") })
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("another local port got status %d, want 403", resp.StatusCode)
	}
	// The page's own origin is allowed.
	resp, _ = get(t, ts, APIPrefix+"/tools", func(r *http.Request) { r.Header.Set("Origin", ts.URL) })
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the page's own origin got status %d, want 200", resp.StatusCode)
	}
}

func TestSendsALockedDownPolicy(t *testing.T) {
	ts := serve(t)
	resp, _ := get(t, ts, "/", nil)
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy is missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("the policy allows inline or evaluated code: %s", csp)
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing no-store or nosniff: %v", resp.Header)
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
	for _, banned := range []string{".innerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(string(script), banned) {
			t.Errorf("app.js uses %s; repository content must never be parsed as markup", banned)
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
