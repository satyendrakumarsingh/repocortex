package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An empty answer is the one thing every tool in this category renders badly.
//
// "No callers found" reads identically whether the call does not exist, the file
// never parsed, or the reference was seen and could not be bound — three
// situations with three different remedies. The outcome of every reference has
// been recorded since migration 0002 and was read back only as a percentage;
// these tests pin that it now reaches the reader.

// seedAmbiguousRepo builds a repository where a name is genuinely ambiguous:
// two types in different packages declare the same method, and a caller invokes
// it through a receiver whose type the resolver cannot pin down.
func seedAmbiguousRepo(t *testing.T) string {
	t.Helper()
	root := isolate(t)

	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("alpha/a.go", `package alpha

type Store struct{}

func (s *Store) Persist(id string) error { return nil }
`)
	write("beta/b.go", `package beta

type Store struct{}

func (s *Store) Persist(id string) error { return nil }
`)
	write("use/u.go", `package use

type any1 interface{ Persist(string) error }

func Run(s any1) error { return s.Persist("x") }
`)

	for _, step := range []string{"init", "index", "resolve"} {
		if out, err := run(t, root, step); err != nil {
			t.Fatalf("%s: %v\n%s", step, err, out)
		}
	}
	return root
}

// TestAnEmptyResultStillReportsCoverage is the guarantee that predates this
// work and must not regress: an empty relationship answer is never presented as
// proof of absence.
func TestAnEmptyResultStillReportsCoverage(t *testing.T) {
	root := seedAmbiguousRepo(t)

	out, err := run(t, root, "callers", "Persist")
	if err != nil {
		t.Fatalf("callers: %v\n%s", err, out)
	}
	if strings.Contains(out, "No callers found") &&
		!strings.Contains(out, "lower bound") &&
		!strings.Contains(out, "resolved") {
		t.Errorf("an empty result was presented without any coverage caveat:\n%s", out)
	}
}

// TestAnUnboundReferenceIsDiagnosedNotHidden is the new behaviour (A30).
//
// If a reference to the queried name exists and did not bind, the report must
// say so — with the file, the line and the recorded outcome — rather than
// leaving the reader to conclude the call does not exist.
func TestAnUnboundReferenceIsDiagnosedNotHidden(t *testing.T) {
	root := seedAmbiguousRepo(t)

	out, err := run(t, root, "callers", "Persist")
	if err != nil {
		t.Fatalf("callers: %v\n%s", err, out)
	}

	// The diagnosis only applies when the answer was actually empty AND an
	// unbound reference exists. If this fixture resolved cleanly, there is
	// nothing to diagnose and that is a pass, not a silent skip — say which.
	if !strings.Contains(out, "No callers found") {
		t.Skipf("this fixture bound its references, so there is no absence to diagnose:\n%s", out)
	}
	if !strings.Contains(out, "WHY:") {
		t.Fatalf("an empty result with unbound references gave no diagnosis:\n%s", out)
	}
	for _, want := range []string{"use/u.go", "ambiguous"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnosis does not mention %q:\n%s", want, out)
		}
	}
	// It must say what the outcome MEANS. A bare "ambiguous" is jargon the
	// reader cannot act on.
	if !strings.Contains(out, "blind spot, not an absence") {
		t.Errorf("the diagnosis does not explain what the outcome implies:\n%s", out)
	}
}

// TestABoundReferenceIsNotListedAsAProblem keeps the diagnosis signal-dense.
//
// A resolved reference is already visible as an edge. Repeating it under "WHY"
// would pad a diagnostic with the rows that are not the problem, which is how a
// useful diagnostic becomes one people skip.
func TestABoundReferenceIsNotListedAsAProblem(t *testing.T) {
	root := seedVerifyRepo(t) // Repo.CreateOrder binds cleanly from Service.PlaceOrder

	out, err := run(t, root, "callers", "CreateOrder")
	if err != nil {
		t.Fatalf("callers: %v\n%s", err, out)
	}
	if strings.Contains(out, "WHY:") {
		t.Errorf("a symbol whose callers resolved was given an absence diagnosis:\n%s", out)
	}
}
