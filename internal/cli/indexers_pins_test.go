package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
)

// TestIndexersPinsIsAStableCacheKey: CI hashes this output to key its indexer
// cache, so it must be a pure function of the binary — same bytes every run,
// one line per pin, a fixed number of fields.
func TestIndexersPinsIsAStableCacheKey(t *testing.T) {
	root := isolate(t)
	first, err := run(t, root, "indexers", "pins")
	if err != nil {
		t.Fatalf("indexers pins: %v\n%s", err, first)
	}
	second, err := run(t, root, "indexers", "pins")
	if err != nil {
		t.Fatalf("indexers pins (second run): %v\n%s", err, second)
	}
	if first != second {
		t.Fatalf("output differs between runs:\n%s\n---\n%s", first, second)
	}
	lines := strings.Split(strings.TrimSpace(first), "\n")
	if len(lines) != len(indexer.Pins()) {
		t.Fatalf("got %d lines for %d pins:\n%s", len(lines), len(indexer.Pins()), first)
	}
	for _, p := range indexer.Pins() {
		if !strings.Contains(first, string(p.Indexer)+"\t"+p.Version+"\t") {
			t.Errorf("pin %s %s missing from:\n%s", p.Indexer, p.Version, first)
		}
	}
	for _, l := range lines {
		if n := len(strings.Split(l, "\t")); n != 5 {
			t.Errorf("line has %d fields, want 5: %q", n, l)
		}
	}
}

func TestPrepareIndexerOutDir(t *testing.T) {
	base := t.TempDir()
	nonEmpty := filepath.Join(base, "full")
	if err := os.MkdirAll(nonEmpty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "old.scip"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	aFile := filepath.Join(base, "file")
	if err := os.WriteFile(aFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "absent is created", in: filepath.Join(base, "new", "scip")},
		{name: "empty is accepted", in: empty},
		{name: "non-empty is refused", in: nonEmpty, wantErr: "not empty"},
		{name: "a file is refused", in: aFile, wantErr: "not a directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prepareIndexerOutDir(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareIndexerOutDir: %v", err)
			}
			if !filepath.IsAbs(got) {
				t.Errorf("returned %q, want an absolute path", got)
			}
			if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
				t.Errorf("%s is not a directory after prepare: %v", got, err)
			}
		})
	}
}

func TestIndexersInstallAcceptsOutDir(t *testing.T) {
	root := isolate(t) // no project markers: nothing to install, nothing run
	out, err := run(t, root, "indexers", "install", "--yes", "--out-dir", filepath.Join(t.TempDir(), "scip"))
	if err != nil {
		t.Fatalf("indexers install --out-dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Nothing to install") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

// TestIndexersInstallRefusesANonEmptyOutDir: with a project to index, a stale
// index in --out-dir must stop the run before any indexer is planned or run.
func TestIndexersInstallRefusesANonEmptyOutDir(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "old.scip"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := run(t, root, "indexers", "install", "--yes", "--out-dir", out)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v, want one containing %q\n%s", err, "not empty", got)
	}
}
