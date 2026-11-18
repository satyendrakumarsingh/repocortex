package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func surfaceProject(t *testing.T) string {
	t.Helper()
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		"a.go":   "package m\n\n// hello world main()\nfunc Dup() {}\n",
		"b.go":   "package m\n\nfunc Dup2() { Dup() }\n",
	})
	for _, args := range [][]string{{"init"}, {"index"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return root
}

// execute runs the CLI the way main does and returns stdout, stderr, exit code.
func execute(root string, args ...string) (string, string, int) {
	cmd := NewRoot()
	cmd.SetArgs(append([]string{"--project", root, "--quiet"}, args...))
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	code := executeRoot(context.Background(), cmd, &out, &errb)
	return out.String(), errb.String(), code
}

// Raw FTS5 syntax is never handed to the engine: the CLI builds the query as
// search_code does.
func TestSearchTakesPunctuationAsWords(t *testing.T) {
	root := surfaceProject(t)
	for _, q := range []string{"a.go", "main()", "hello-world", "x:y", "AND", `"`} {
		if out, err := run(t, root, "search", q); err != nil &&
			(strings.Contains(err.Error(), "fts5") || strings.Contains(err.Error(), "no such column") || strings.Contains(err.Error(), "syntax")) {
			t.Errorf("search %q leaked an engine error: %v\n%s", q, err, out)
		}
	}
	out, err := run(t, root, "search", "main()")
	if err != nil || !strings.Contains(out, "a.go") {
		t.Errorf("search main() should find a.go: %v\n%s", err, out)
	}
}

func TestSearchJSONUsesSnakeCaseKeys(t *testing.T) {
	root := surfaceProject(t)
	out, err := run(t, root, "--json", "search", "hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"FileID"`, `"Path"`, `"Score"`} {
		if strings.Contains(out, bad) {
			t.Errorf("Go field name %s leaked:\n%s", bad, out)
		}
	}
	for _, want := range []string{`"file_id"`, `"path"`, `"score"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing key %s:\n%s", want, out)
		}
	}
}

func TestLimitMustBePositive(t *testing.T) {
	root := surfaceProject(t)
	for _, cmd := range []string{"search", "symbol"} {
		for _, n := range []string{"0", "-1"} {
			if out, err := run(t, root, cmd, "Dup", "-n", n); err == nil {
				t.Errorf("%s -n %s was accepted:\n%s", cmd, n, out)
			}
		}
		if _, err := run(t, root, cmd, "Dup", "-n", "1"); err != nil {
			t.Errorf("%s -n 1: %v", cmd, err)
		}
	}
}

func TestJSONErrorsGoToStdout(t *testing.T) {
	root := surfaceProject(t)
	for _, args := range [][]string{
		{"callers", "nonexistent"},
		{"callers", "a.*b[("},
		{"callers", strings.Repeat("x", 10000)},
		{"search", ""},
		{"symbol", "Dup", "-n", "0"},
	} {
		out, errText, code := execute(root, append([]string{"--json"}, args...)...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1", args[:1], code)
		}
		var v struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil || v.Error.Message == "" || v.Error.Code == "" {
			t.Errorf("%v: stdout is not a JSON error object: %q (%v)", args[:1], out, err)
		}
		if !strings.Contains(errText, "error:") {
			t.Errorf("%v: stderr lost the text error: %q", args[:1], errText)
		}
	}
}

func TestSymbolNotFoundJSONHasEmptyList(t *testing.T) {
	root := surfaceProject(t)
	out, err := run(t, root, "--json", "symbol", "nonexist")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"symbols": []`) {
		t.Errorf("symbols must be [] not null:\n%s", out)
	}
}
