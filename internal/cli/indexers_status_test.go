package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIndexersListShowsBlockedSteps pins a defect in which a safety gate was
// invisible.
//
// `indexers list` chose its STATUS column from MissingTools, NeedsProvision and
// Installed.Preexisting, and defaulted to "ready". A step the planner had
// BLOCKED matched none of those, so it printed as ready — and the step that
// blocks is the one that matters: indexing Java runs the repository's own Maven
// or Gradle build, which executes arbitrary code from the repository, so it is
// withheld pending an explicit opt-in.
//
// The table announced that indexer as ready to use. A safety gate the interface
// does not show is one a user works around by accident.
//
// Asserted on the rendered output rather than on the switch, because the defect
// was that a real state had no rendering — a test on the branch would have had
// to know the branch exists.
func TestIndexersListShowsBlockedSteps(t *testing.T) {
	root := isolate(t)
	// A pom.xml with no build tooling present is the shape that reproduces it:
	// the planner detects a Java project, blocks it, and has no missing-tool to
	// report instead.
	if err := os.WriteFile(filepath.Join(root, "pom.xml"),
		[]byte("<project><modelVersion>4.0.0</modelVersion></project>"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "indexers", "list")
	if err != nil {
		t.Fatalf("indexers list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "java") {
		t.Skipf("no java project detected here; nothing to assert:\n%s", out)
	}

	// Either it is genuinely blocked and says so, or it is genuinely usable.
	// What it must never do is report a blocked step as ready.
	if strings.Contains(out, "blocked") {
		if !strings.Contains(out, "arbitrary code") && !strings.Contains(out, "needs") {
			t.Errorf("status says blocked but gives no reason:\n%s", out)
		}
		return
	}
	if strings.Contains(out, "ready") && !strings.Contains(out, "found on PATH") {
		t.Errorf("bare \"ready\" for a java step: a blocked step is being reported "+
			"as usable, which is the defect this test exists for:\n%s", out)
	}
}
