package scip

import (
	"bytes"
	"context"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"
)

func TestPathMismatchIsReportedNotSwallowed(t *testing.T) {
	facts, st := ingestFixture(t,
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()},
		serviceDoc(), ghostDoc())

	if len(facts) != 1 {
		t.Fatalf("want 1 emitted document, got %d", len(facts))
	}
	if st.Documents != 2 || st.DocumentsMatched != 1 || st.DocumentsUnmatched != 1 {
		t.Fatalf("counts: %d seen, %d matched, %d unmatched", st.Documents, st.DocumentsMatched, st.DocumentsUnmatched)
	}
	if len(st.UnmatchedPaths) != 1 || st.UnmatchedPaths[0] != "generated/ghost.go" {
		t.Fatalf("unmatched sample: %v", st.UnmatchedPaths)
	}
	if !st.PathsVerified {
		t.Fatal("PathsVerified should be true when KnownPaths is supplied")
	}
	// The whole point: the failure has to be legible without a debugger.
	if summary := st.Summary(); !contains(summary, "generated/ghost.go") || !contains(summary, "1 unmatched") {
		t.Fatalf("Summary hides the mismatch: %q", summary)
	}
}

func TestUnverifiedModeSaysSoRatherThanReportingZero(t *testing.T) {
	_, st := ingestFixture(t, Options{RepoRoot: "/build/repo"}, serviceDoc(), ghostDoc())

	if st.PathsVerified {
		t.Fatal("PathsVerified should be false without KnownPaths")
	}
	if st.DocumentsUnmatched != 0 || st.DocumentsMatched != 2 {
		t.Fatalf("unverified mode should accept every path: %+v", st)
	}
	if summary := st.Summary(); !contains(summary, "unverified") {
		t.Fatalf("Summary must flag unverified mode: %q", summary)
	}
	if st.PathStrategies[StrategyUnverified] != 2 {
		t.Fatalf("strategies: %v", st.PathStrategies)
	}
}

// TestSubProjectPrefixFromProjectRoot is the monorepo case: the indexer ran in
// services/billing, so it calls a file src/Foo.java that RepoCortex knows as
// services/billing/src/Foo.java.
func TestSubProjectPrefixFromProjectRoot(t *testing.T) {
	doc := &scippb.Document{
		RelativePath: "src/main/java/Foo.java",
		Language:     "java",
		Occurrences:  []*scippb.Occurrence{def("scip-java maven a b Foo#", rng(0, 6, 0, 9), rng(0, 0, 9, 1))},
	}
	data := indexBytes(t, metadata("file:///home/ci/work/repo/services/billing"), doc)

	facts, st, err := Collect(context.Background(), bytes.NewReader(data), Options{
		RepoRoot:   "/home/ci/work/repo",
		KnownPaths: []string{"services/billing/src/main/java/Foo.java"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(facts) != 1 || facts[0].Path != "services/billing/src/main/java/Foo.java" {
		t.Fatalf("prefix not applied: %+v", facts)
	}
	if st.PathStrategies[StrategyProjectRoot] != 1 {
		t.Fatalf("want project-root strategy, got %v", st.PathStrategies)
	}
}

// TestSuffixMatchRescuesForeignProjectRoot covers the common CI case where the
// index was built in a container: project_root is /build/src, which has no
// relationship to the caller's checkout, so the prefix cannot be derived.
func TestSuffixMatchRescuesForeignProjectRoot(t *testing.T) {
	doc := &scippb.Document{
		RelativePath: "main/java/Foo.java",
		Occurrences:  []*scippb.Occurrence{def("scip-java maven a b Foo#", rng(0, 6, 0, 9), rng(0, 0, 9, 1))},
	}
	data := indexBytes(t, metadata("file:///build/src"), doc)

	facts, st, err := Collect(context.Background(), bytes.NewReader(data), Options{
		RepoRoot:   "/home/dev/repo",
		KnownPaths: []string{"services/billing/src/main/java/Foo.java"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(facts) != 1 || facts[0].Path != "services/billing/src/main/java/Foo.java" {
		t.Fatalf("suffix match failed: %+v", facts)
	}
	if st.PathStrategies[StrategySuffix] != 1 {
		t.Fatalf("want suffix strategy, got %v", st.PathStrategies)
	}
}

func TestAmbiguousSuffixIsRefusedNotGuessed(t *testing.T) {
	doc := &scippb.Document{
		RelativePath: "main/java/Foo.java",
		Occurrences:  []*scippb.Occurrence{def("scip-java maven a b Foo#", rng(0, 6, 0, 9), rng(0, 0, 9, 1))},
	}
	data := indexBytes(t, metadata("file:///build/src"), doc)

	facts, st, err := Collect(context.Background(), bytes.NewReader(data), Options{
		RepoRoot: "/home/dev/repo",
		KnownPaths: []string{
			"services/billing/src/main/java/Foo.java",
			"services/orders/src/main/java/Foo.java",
		},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(facts) != 0 || st.DocumentsUnmatched != 1 {
		t.Fatalf("ambiguous suffix must not be resolved to one of the candidates: %+v", facts)
	}
}

func TestExplicitPrefixDoesNotFallBack(t *testing.T) {
	doc := &scippb.Document{
		RelativePath: "src/Foo.java",
		Occurrences:  []*scippb.Occurrence{def("scip-java maven a b Foo#", rng(0, 6, 0, 9), rng(0, 0, 9, 1))},
	}
	data := indexBytes(t, metadata("file:///build/src"), doc)

	// The caller named a prefix that is wrong. Quietly matching src/Foo.java by
	// another strategy would hide their mistake behind a working index.
	_, st, err := Collect(context.Background(), bytes.NewReader(data), Options{
		RepoRoot:   "/home/dev/repo",
		PathPrefix: "services/wrong",
		KnownPaths: []string{"src/Foo.java"},
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if st.DocumentsUnmatched != 1 {
		t.Fatalf("explicit prefix must not fall back: %+v", st)
	}
}

// TestHostileDocumentPathsAreRejected proves the attack fails. A .scip file
// arrives with the repository, so a hostile one can name a document outside the
// tree; the path is stored and later handed to an agent as somewhere to read.
func TestHostileDocumentPathsAreRejected(t *testing.T) {
	hostile := []string{
		"../../../etc/passwd",
		"/etc/shadow",
		"a/../../../../etc/passwd",
		"C:/Windows/System32/config/SAM",
		"",
		".",
	}
	for _, p := range hostile {
		t.Run(p, func(t *testing.T) {
			// Unverified mode is the dangerous one: there is no file list to
			// reject the path for us.
			doc := &scippb.Document{RelativePath: p}
			data := indexBytes(t, metadata("file:///build/repo"), doc)
			facts, st, err := Collect(context.Background(), bytes.NewReader(data), Options{RepoRoot: "/build/repo"})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(facts) != 0 {
				t.Fatalf("hostile path %q was accepted as %q", p, facts[0].Path)
			}
			if st.DocumentsUnmatched != 1 {
				t.Fatalf("hostile path %q was not counted as unmatched", p)
			}
		})
	}
}

func TestNormalizeProjectRoot(t *testing.T) {
	cases := map[string]string{
		"file:///home/ci/repo":     "/home/ci/repo",
		"file:///home/ci/repo/":    "/home/ci/repo",
		"/home/ci/repo":            "/home/ci/repo",
		"file:///C:/src/repo":      "C:/src/repo",
		"file:///home/ci/my%20app": "/home/ci/my app",
		"":                         "",
	}
	for in, want := range cases {
		if got := normalizeProjectRoot(in); got != want {
			t.Errorf("normalizeProjectRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDerivePrefix(t *testing.T) {
	cases := []struct{ repo, project, want string }{
		{"/repo", "file:///repo", ""},
		{"/repo", "file:///repo/services/billing", "services/billing"},
		{"/repo", "file:///elsewhere/thing", ""},
		{"", "file:///repo/x", ""},
		{"/repo", "", ""},
	}
	for _, c := range cases {
		if got := derivePrefix(normalizePath(c.repo), c.project); got != c.want {
			t.Errorf("derivePrefix(%q, %q) = %q, want %q", c.repo, c.project, got, c.want)
		}
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
