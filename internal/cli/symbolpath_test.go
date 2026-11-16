package cli

import (
	"strings"
	"testing"
)

// explore, callers and callees pick one of several same-named definitions with
// --path. tests and owners refused the flag and only took the file:name form,
// so the same question needed two spellings depending on the command.
func TestTestsAndOwnersAcceptPath(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, map[string]string{
		"go.mod":        "module example.com/sp\n\ngo 1.22\n",
		"a/run.go":      "package a\n\nfunc Run() int { return 1 }\n",
		"b/run.go":      "package b\n\nfunc Run() int { return 2 }\n",
		"a/run_test.go": "package a\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) { _ = Run() }\n",
	})
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := run(t, root, "index"); err != nil {
		t.Fatalf("index: %v\n%s", err, out)
	}
	if out, err := run(t, root, "resolve"); err != nil {
		t.Fatalf("resolve: %v\n%s", err, out)
	}

	out, err := run(t, root, "tests", "Run", "--path", "a/run.go")
	if err != nil {
		t.Fatalf("tests --path: %v\n%s", err, out)
	}
	if !strings.Contains(out, "TestRun") {
		t.Errorf("tests --path a/run.go did not find TestRun:\n%s", out)
	}
	if out, err := run(t, root, "owners", "Run", "--path", "b/run.go"); err != nil {
		t.Fatalf("owners --path: %v\n%s", err, out)
	}
}

func TestQualifyWithPath(t *testing.T) {
	cases := []struct{ name, path, want string }{
		{"Run", "", "Run"},
		{"Run", "a/run.go", "a/run.go:Run"},
		{"Svc.Run", "a/svc.go", "a/svc.go:Svc.Run"},
		{"a/run.go:Run", "b/run.go", "a/run.go:Run"}, // already qualified: left alone
		{"Run", "./a/run.go", "a/run.go:Run"},
	}
	for _, c := range cases {
		if got := qualifyWithPath(c.name, c.path); got != c.want {
			t.Errorf("qualifyWithPath(%q, %q) = %q, want %q", c.name, c.path, got, c.want)
		}
	}
}
