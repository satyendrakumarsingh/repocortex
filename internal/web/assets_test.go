package web

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssetsAreTextNotBinary guards a property that has nothing to do with the
// browser and everything to do with the next person to read this code.
//
// app.js used raw U+0000 and U+0001 bytes as composite-key separators. The
// strings were correct and the browser did not care - but the file became
// `data` to file(1), and grep returns NOTHING on a binary file. Searching a
// 9,957-line asset for any symbol came back empty, which reads as "the code is
// not there" rather than "your tool declined to look".
//
// The escape sequences produce identical strings at runtime.
func TestAssetsAreTextNotBinary(t *testing.T) {
	for _, name := range []string{
		"assets/app.js", "assets/app.css", "assets/index.html",
		"assets/js/main.mjs", "assets/js/nav.mjs",
		"assets/js/tiers.mjs", "assets/js/glossary.mjs", "assets/js/answer.mjs",
		"assets/js/codemap.mjs",
	} {
		b, err := assets.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, c := range b {
			if c == 0x00 || c < 0x09 || (c > 0x0d && c < 0x20) {
				line := 1 + strings.Count(string(b[:i]), "\n")
				t.Errorf("%s:%d holds control byte %#x - this makes the file binary to grep; "+
					"write it as an escape sequence instead", name, line, c)
			}
		}
	}
}

/*
TestDistinguishingLabel executes the one pure function behind "labels name what
differs" — the map and the card browser both name a node by the shortest
'/'-aligned tail of its path that no sibling also ends in: the last segment
unless a twin forces it to grow.

It is a Go test that runs JavaScript for the same reason canvas_test.go is: the
package ships no JS test runner, and a Go test that grepped for the function
would pass on a function that returned the wrong thing. The function is cut out
of the embedded app.js between two marker comments so it is the served code
that is proved, not a copy; a missing marker fails loudly rather than checking
nothing. Without node on PATH the test skips and says what went unverified.
*/
func TestDistinguishingLabel(t *testing.T) {
	t.Parallel()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH: distinguishingLabel in assets/app.js is NOT verified by this run. " +
			"Install any Node runtime and re-run to gate on it.")
	}

	body, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatalf("read embedded assets/app.js: %v", err)
	}
	const openMark, closeMark = "// >>> distinguishingLabel", "// <<< distinguishingLabel"
	src := string(body)
	from := strings.Index(src, openMark)
	to := strings.Index(src, closeMark)
	if from < 0 || to < 0 || to < from {
		t.Fatalf("assets/app.js does not carry distinguishingLabel between the markers %q and %q", openMark, closeMark)
	}
	fn := src[from+len(openMark) : to]

	type tc struct {
		Name     string   `json:"name"`
		Path     string   `json:"path"`
		Siblings []string `json:"siblings"`
		Want     string   `json:"want"`
	}
	cases := []tc{
		{"siblings sharing a long prefix, first", "a/b/c/x", []string{"a/b/c/x", "a/b/c/y"}, "x"},
		{"siblings sharing a long prefix, second", "a/b/c/y", []string{"a/b/c/x", "a/b/c/y"}, "y"},
		{"a deeper path is still named by its last segment", "a/b/c/x/inner", []string{"a/b/c/x/inner", "a/b/c/y"}, "inner"},
		{"siblings sharing nothing are named by their last segment", "p/q", []string{"p/q", "r/s"}, "q"},
		{"a single sibling is named by its last segment", "a/b/c", []string{"a/b/c"}, "c"},
		{"identical paths fall back to the last segment", "a/b", []string{"a/b", "a/b"}, "b"},
		{"a sibling that merely extends the path is no twin, shorter", "a/b", []string{"a/b", "a/b/c/d"}, "b"},
		{"a sibling that merely extends the path is no twin, longer", "a/b/c/d", []string{"a/b", "a/b/c/d"}, "d"},
		{"no siblings at all", "a/b", []string{}, "b"},
		{"twins grow until the tails differ, main", "src/main/java/com/pay/controller",
			[]string{"src/main/java/com/pay/controller", "src/test/java/com/pay/controller"}, "main/java/com/pay/controller"},
		{"twins grow until the tails differ, test", "src/test/java/com/pay/controller",
			[]string{"src/main/java/com/pay/controller", "src/test/java/com/pay/controller"}, "test/java/com/pay/controller"},
		{"a path that is a suffix of a sibling grows to its full path", "a/b", []string{"a/b", "x/a/b"}, "a/b"},
		{"the sibling it is a suffix of grows to its full path too", "x/a/b", []string{"a/b", "x/a/b"}, "x/a/b"},
	}
	arg, err := json.Marshal(cases)
	if err != nil {
		t.Fatalf("encode cases: %v", err)
	}

	harness := fn + `
const cases = JSON.parse(process.argv[2]);
process.stdout.write(JSON.stringify(cases.map((c) => distinguishingLabel(c.path, c.siblings))));
`
	script := filepath.Join(t.TempDir(), "distinguishing_label.js")
	if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
		t.Fatalf("stage harness: %v", err)
	}
	out, err := exec.Command(node, script, string(arg)).Output()
	if err != nil {
		var stderr []byte
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = exitErr.Stderr
		}
		t.Fatalf("node failed: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode harness output %q: %v", out, err)
	}
	if len(got) != len(cases) {
		t.Fatalf("harness returned %d results for %d cases", len(got), len(cases))
	}
	for i, c := range cases {
		if got[i] != c.Want {
			t.Errorf("%s: distinguishingLabel(%q, %q) = %q, want %q", c.Name, c.Path, c.Siblings, got[i], c.Want)
		}
	}
}
