package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSCIPImportRejectsBadInputBeforeTouchingTheIndex.
//
// Each of these is a mistake a user makes on the way to a working import, and
// each has a different fix. Reporting them as one generic parse failure — or
// worse, opening the database and mutating it before noticing — is how a
// five-second correction becomes an afternoon.
func TestSCIPImportRejectsBadInputBeforeTouchingTheIndex(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.scip")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, path, want string }{
		{"missing", filepath.Join(dir, "nope.scip"), "read SCIP index"},
		{"directory", dir, "is a directory"},
		{"empty", empty, "is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, root, "scip", "import", c.path)
			if err == nil {
				t.Fatal("accepted bad input")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

// TestSCIPImportRequiresACommittedIndex: importing before indexing must name
// the missing step rather than reporting an empty graph.
func TestSCIPImportRequiresACommittedIndex(t *testing.T) {
	root := isolate(t)
	if _, err := run(t, root, "init"); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(t.TempDir(), "i.scip")
	if err := os.WriteFile(index, []byte("not-a-real-index-but-non-empty"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := run(t, root, "scip", "import", index)
	if err == nil {
		t.Fatal("import succeeded with nothing indexed")
	}
	if !strings.Contains(err.Error(), "cortex index") {
		t.Errorf("error should name the missing step; got %q", err)
	}
}

// TestSCIPImportIsRegistered guards the wiring. A command that exists in a file
// but is never added to the root is invisible, which is a mistake that compiles
// and passes every unit test in the package.
func TestSCIPImportIsRegistered(t *testing.T) {
	root := isolate(t)
	out, err := run(t, root, "scip", "--help")
	if err != nil {
		t.Fatalf("scip --help: %v\n%s", err, out)
	}
	if !strings.Contains(out, "import") {
		t.Errorf("scip has no import subcommand:\n%s", out)
	}
}
