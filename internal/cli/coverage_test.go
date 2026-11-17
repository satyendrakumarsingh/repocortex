package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoverageImportCmd(t *testing.T) {
	tmpDir := t.TempDir()
	covFile := filepath.Join(tmpDir, "coverage.out")
	covContent := `mode: set
github.com/example/pkg/service.go:10.2,15.10 2 1
github.com/example/pkg/service.go:16.2,20.10 1 0
`
	if err := os.WriteFile(covFile, []byte(covContent), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	g := &Global{}
	cmd := newCoverageCmd(g)
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"import", covFile, "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute coverage import: %v", err)
	}
}

// svc/payment.go in the query fixture: Settle is lines 6-8, Refund 10-12.
const paymentProfile = `mode: set
example.com/shop/svc/payment.go:6.40,8.2 2 3
example.com/shop/svc/payment.go:10.40,12.2 2 0
example.com/shop/elsewhere/gone.go:1.1,3.2 2 1
`

func TestCoverageImportStoresPerSymbolCoverage(t *testing.T) {
	root := queryFixture(t)
	prof := filepath.Join(t.TempDir(), "cover.out")
	if err := os.WriteFile(prof, []byte(paymentProfile), 0o600); err != nil {
		t.Fatal(err)
	}

	// --dry-run keeps the old behaviour: print, store nothing.
	if out, err := run(t, root, "coverage", "import", prof, "--dry-run"); err != nil || strings.Contains(out, "Stored:") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	out, err := run(t, root, "tool", "call", "find_tests", "symbol=Refund")
	if err != nil || strings.Contains(out, "Coverage:") {
		t.Fatalf("a dry run stored coverage, or find_tests failed: %v\n%s", err, out)
	}

	out, err = run(t, root, "coverage", "import", prof)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	flat := collapse(out)
	for _, want := range []string{
		"Stored: 2 symbol(s) now have coverage; 1 instrumented file(s) matched an indexed file, 1 matched none.",
		"no indexed file: example.com/shop/elsewhere/gone.go",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	// Refund never ran: find_tests says so.
	out, err = run(t, root, "tool", "call", "find_tests", "symbol=Refund")
	if err != nil {
		t.Fatalf("find_tests: %v\n%s", err, out)
	}
	if !strings.Contains(collapse(out), "no test executed Refund in the coverage imported") {
		t.Errorf("find_tests did not use the stored coverage:\n%s", out)
	}

	// A second import replaces the first, and a missing index is refused.
	if out, err := run(t, root, "coverage", "import", prof); err != nil {
		t.Fatalf("second import: %v\n%s", err, out)
	}
	if _, err := run(t, isolate(t), "coverage", "import", prof); err == nil {
		t.Error("import without an index should fail")
	}
}
