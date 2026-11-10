package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// indexedProject returns a project root with one Go file, initialised, indexed.
func indexedProject(t *testing.T) string {
	t.Helper()
	root := isolate(t)
	src := "package main\n\nfunc a() { b() }\n\nfunc b() {}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return root
}

// TestResolveReportsAFailedSCIPAutoImport: a corrupt index.scip must be named
// in the output. Before, the failure was discarded and the user could not tell
// "upgrade skipped" from "no SCIP index present".
func TestResolveReportsAFailedSCIPAutoImport(t *testing.T) {
	root := indexedProject(t)
	if err := os.WriteFile(filepath.Join(root, "index.scip"), []byte("not a scip index"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "resolve")
	if err != nil {
		t.Fatalf("a bad SCIP index must not fail resolve: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NOT imported") || !strings.Contains(out, "index.scip") {
		t.Errorf("output does not report the failed import:\n%s", out)
	}

	jsonOut, err := run(t, root, "--json", "resolve")
	if err != nil {
		t.Fatalf("resolve --json: %v\n%s", err, jsonOut)
	}
	if !strings.Contains(jsonOut, `"scip"`) || !strings.Contains(jsonOut, `"error"`) {
		t.Errorf("JSON output has no scip error field:\n%s", jsonOut)
	}
}

func TestResolveNoSCIPSkipsAutoImport(t *testing.T) {
	root := indexedProject(t)
	if err := os.WriteFile(filepath.Join(root, "index.scip"), []byte("not a scip index"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "resolve", "--no-scip")
	if err != nil {
		t.Fatalf("resolve --no-scip: %v\n%s", err, out)
	}
	if strings.Contains(out, "SCIP:") {
		t.Errorf("--no-scip still attempted an import:\n%s", out)
	}
}

func TestResolveWithoutSCIPIndexSaysNothing(t *testing.T) {
	root := indexedProject(t)
	out, err := run(t, root, "resolve")
	if err != nil {
		t.Fatalf("resolve: %v\n%s", err, out)
	}
	if strings.Contains(out, "SCIP:") {
		t.Errorf("no index.scip present, yet output mentions SCIP:\n%s", out)
	}
}
