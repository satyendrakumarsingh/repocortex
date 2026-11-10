package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestADRRefusesToReadOutsideTheRepository pins a reproduced exfiltration bug.
//
// `arch adr` joined the repo root to a stored path and called os.ReadFile. The
// walker refuses to FOLLOW a symlink but still RECORDS it — so the gap stays
// visible in the index — and os.ReadFile follows. A repository containing
// docs/adr/0001-x.md -> /etc/hosts therefore had that file's contents printed
// as an ADR summary, including by `--json`, which an agent reads.
//
// The threat model this breaks is the project's central one: a hostile
// repository is in scope. Cloning someone else's code and running RepoCortex on
// it must not read anything outside it.
//
// security.ValidatePath had the right contract and ZERO production callers,
// which is the only reason this was reachable — the same shape as the whole
// internal/security package once being imported by nothing.
func TestADRRefusesToReadOutsideTheRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	root := isolate(t)

	// The victim file lives outside the repository entirely.
	outside := filepath.Join(t.TempDir(), "victim-private.md")
	const canary = "TOP-SECRET-CANARY-VALUE-12345"
	if err := os.WriteFile(outside, []byte("# Secret\n\n"+canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	adrDir := filepath.Join(root, "docs", "adr")
	if err := os.MkdirAll(adrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A filename ADR discovery accepts, pointing out of the tree.
	if err := os.Symlink(outside, filepath.Join(adrDir, "0001-innocent.md")); err != nil {
		t.Skipf("cannot create symlink here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	for _, args := range [][]string{{"arch", "adr"}, {"arch", "adr", "--json"}} {
		out, err := run(t, root, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if strings.Contains(out, canary) {
			t.Fatalf("EXFILTRATION: %v read a file outside the repository:\n%s", args, out)
		}
	}
}

// TestADRStillReadsRealRecords guards the other direction: a containment check
// that rejects everything would pass the test above while breaking the feature.
func TestADRStillReadsRealRecords(t *testing.T) {
	root := isolate(t)
	adrDir := filepath.Join(root, "docs", "adr")
	if err := os.MkdirAll(adrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adrDir, "0001-use-postgres.md"),
		[]byte("# 1. Use PostgreSQL\n\n## Status\n\nAccepted\n\n## Context\n\nWe need transactions.\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	out, err := run(t, root, "arch", "adr")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Use PostgreSQL") {
		t.Errorf("a legitimate in-repository ADR was not read:\n%s", out)
	}
}
