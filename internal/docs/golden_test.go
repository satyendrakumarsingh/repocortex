package docs

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update rewrites the golden files. Run with:
//
//	go test ./internal/docs/ -update
var update = flag.Bool("update", false, "rewrite the golden documents")

// TestGoldenDocuments pins the exact bytes of both documents for the fixture.
//
// The determinism test proves the output does not change between two runs of the
// same binary. This proves it does not change between two BUILDS — a reordered
// switch statement, a renamed field, a different Go version's map growth
// behaviour all show up here as a diff, which is precisely the class of change
// that would quietly break a committed ARCHITECTURE.md in someone's repository.
//
// It is also the only place a reviewer can read the artifact this package exists
// to produce, which is worth as much as the assertion.
func TestGoldenDocuments(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, smallIndex(), smallModel())

	cases := []struct {
		file string
		got  string
	}{
		{"architecture.golden.md", v.ArchitectureMarkdown()},
		{"capabilities.golden.md", v.CapabilityMarkdown()},
	}
	for _, c := range cases {
		path := filepath.Join("testdata", c.file)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatalf("mkdir testdata: %v", err)
			}
			if err := os.WriteFile(path, []byte(c.got), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run `go test ./internal/docs/ -update` to create it): %v", path, err)
		}
		if string(want) != c.got {
			t.Errorf("%s is out of date\n%s\n\nRun `go test ./internal/docs/ -update` if the change is intended.",
				c.file, firstDiff(string(want), c.got))
		}
	}
}
