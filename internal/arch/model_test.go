package arch

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// glob matches one pattern against one unsplit path, mirroring what
// ComponentOf does per path. Production code splits once per path and matches
// many patterns against the result, so this shape exists only for tests.
func glob(pattern, path string) bool {
	if len(path) > maxPathBytes {
		return false
	}
	segs, ok := splitPath(path, make([]string, 0, maxPathSegments))
	if !ok {
		return false
	}
	return matchSegs(pattern, segs)
}

func TestDoubleStarSpansZeroOrMoreSegments(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// `.git/**` failing to match `.git` is a bug this repository has
		// shipped before: the directory a rule was written to claim ends up
		// claimed by nobody, and the rule silently covers nothing.
		{"trailing ** matches the directory itself", ".git/**", ".git", true},
		{"trailing ** matches the named directory", "src/**", "src", true},
		{"trailing ** matches a direct child", "src/**", "src/a.go", true},
		{"trailing ** matches a deep descendant", "src/**", "src/a/b/c.go", true},
		{"trailing ** does not match a sibling prefix", "src/**", "srcery/a.go", false},
		{"trailing ** does not match an unrelated tree", "src/**", "test/a.go", false},
		{"bare ** matches everything", "**", "a/b/c.go", true},
		{"bare ** matches a single segment", "**", "a", true},
		{"interior ** spans zero segments", "src/**/a.go", "src/a.go", true},
		{"interior ** spans one segment", "src/**/a.go", "src/x/a.go", true},
		{"interior ** spans many segments", "src/**/a.go", "src/x/y/z/a.go", true},
		{"leading ** matches at any depth", "**/testdata/**", "a/b/testdata", true},
		{"leading ** matches the root itself", "**/testdata/**", "testdata/x/y.go", true},
		{"repeated ** is the same as one **", "**/**/**/a.go", "x/y/a.go", true},
		{"** does not excuse a wrong literal", "src/**/a.go", "src/x/b.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := glob(tc.pattern, tc.path); got != tc.want {
				t.Fatalf("glob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestSingleStarNeverCrossesASlash(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"star matches within one segment", "src/*", "src/a.go", true},
		{"star does not span a segment boundary", "src/*", "src/a/b.go", false},
		{"star matches an empty remainder", "src/a*", "src/a", true},
		{"star matches a suffix pattern", "**/*.go", "internal/arch/model.go", true},
		{"star does not match a different suffix", "**/*.go", "internal/arch/model.java", false},
		{"question mark matches exactly one character", "src/?.go", "src/a.go", true},
		{"question mark does not match two characters", "src/?.go", "src/ab.go", false},
		{"question mark does not cross a slash", "src?a.go", "src/a.go", false},
		{"a wildcard-free pattern matches its exact path", "src/a.go", "src/a.go", true},
		{"a wildcard-free pattern matches nothing longer", "src/a.go", "src/a.go/b", false},
		{"a wildcard-free pattern matches nothing shorter", "src/a.go", "src", false},
		{"a literal segment must match exactly", "src/a.go", "src/A.go", false},
		{"stars inside a segment stay inside it", "src/a**b", "src/axxb", true},
		{"stars inside a segment do not span directories", "src/a**b", "src/a/x/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := glob(tc.pattern, tc.path); got != tc.want {
				t.Fatalf("glob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

func TestPathsAreNormalisedBeforeMatching(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"plain repo-relative path", "src/a.go"},
		{"dot-slash prefix", "./src/a.go"},
		{"leading slash", "/src/a.go"},
		{"doubled separators", "src//a.go"},
		{"trailing slash on a directory-like path", "src/a.go/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !glob("src/**", tc.path) {
				t.Fatalf("glob(%q, %q) = false, want true", "src/**", tc.path)
			}
		})
	}
}

func componentModel() Model {
	return Model{Components: []Component{
		{Name: "root", Include: []string{"internal/**"}},
		{Name: "store", Include: []string{"internal/store/**"}},
		{Name: "web", Include: []string{"cmd/web/**", "internal/http/**"}, Exclude: []string{"**/*_test.go"}},
		{Name: "generated", Include: []string{"**/gen/**"}},
	}}
}

func TestComponentOfPrefersTheMostSpecificInclude(t *testing.T) {
	m := componentModel()
	cases := []struct {
		name string
		path string
		want string
	}{
		{"narrower component beats the broader one", "internal/store/db.go", "store"},
		{"broader component owns what the narrow one misses", "internal/graph/paths.go", "root"},
		{"a second include pattern counts too", "internal/http/server.go", "web"},
		{"a component outside every include is unowned", "docs/spec.md", ""},
		{"an empty path is unowned", "", ""},
		{"a second component's include is matched too", "cmd/web/main.go", "web"},
		// Specificity is counted in LITERAL characters, so a rooted pattern
		// beats one that opens with a wildcard even though the second looks
		// narrower. If this flipped, `internal/**` would stop owning the tree it
		// names whenever any component mentioned a directory inside it.
		{"a rooted pattern outranks one that opens with a wildcard", "internal/gen/api.go", "root"},
		{"a wildcard-led pattern still owns what nothing else claims", "web/gen/api.go", "generated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.ComponentOf(tc.path); got != tc.want {
				t.Fatalf("ComponentOf(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestComponentOfLetsExcludeBeatInclude(t *testing.T) {
	m := Model{Components: []Component{
		{Name: "src", Include: []string{"src/**"}, Exclude: []string{"src/generated/**", "**/*_test.go"}},
		{Name: "gen", Include: []string{"src/generated/**"}},
	}}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"an included path is owned", "src/a.go", "src"},
		{"an excluded subtree falls to the component that includes it", "src/generated/api.go", "gen"},
		{"the excluded directory itself is excluded", "src/generated", "gen"},
		{"an exclude with no other owner leaves the path unowned", "src/a_test.go", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.ComponentOf(tc.path); got != tc.want {
				t.Fatalf("ComponentOf(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestComponentOfIsDeterministicWhenComponentsTie(t *testing.T) {
	// Three components claiming the same path with identical specificity. The
	// answer must depend only on the set of components, never on the order they
	// were declared in, and must not vary between runs of the same process.
	tie := []Component{
		{Name: "gamma", Include: []string{"src/**"}},
		{Name: "alpha", Include: []string{"src/**"}},
		{Name: "beta", Include: []string{"src/**"}},
	}
	const want = "alpha" // alphabetically first among equals

	perms := [][]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	}
	for _, perm := range perms {
		m := Model{}
		for _, i := range perm {
			m.Components = append(m.Components, tie[i])
		}
		// Repeated because a map-iteration dependency shows up as an
		// intermittent answer, not a wrong one.
		for i := 0; i < 200; i++ {
			if got := m.ComponentOf("src/a/b.go"); got != want {
				t.Fatalf("order %v, iteration %d: ComponentOf = %q, want %q", perm, i, got, want)
			}
		}
	}
}

func TestComponentOfPrefersTheNarrowerOfTwoNestedComponents(t *testing.T) {
	// The nested component is the one its author meant. If the broad component
	// won, carving a subsystem out of an existing tree would be impossible: the
	// new component would own nothing, and every rule naming it would pass.
	nested := Component{Name: "api-internal", Include: []string{"src/api/internal/**"}}
	broad := Component{Name: "api", Include: []string{"src/**"}}

	for _, order := range [][]Component{{broad, nested}, {nested, broad}} {
		m := Model{Components: order}
		if got := m.ComponentOf("src/api/internal/x.go"); got != "api-internal" {
			t.Errorf("declared as %q, %q: ComponentOf = %q, want %q",
				order[0].Name, order[1].Name, got, "api-internal")
		}
		if got := m.ComponentOf("src/api/handler.go"); got != "api" {
			t.Errorf("declared as %q, %q: ComponentOf = %q, want %q",
				order[0].Name, order[1].Name, got, "api")
		}
	}
}

func TestComponentOfTieBreaksOnPatternLengthBeforeName(t *testing.T) {
	// Both patterns carry nine literal characters, so specificity cannot
	// separate them; length does, and it must be consulted before the name. If
	// the name were consulted first, adding a component called "alpha" would
	// quietly steal paths from a more precisely declared one.
	m := Model{Components: []Component{
		{Name: "zulu", Include: []string{"src/**/a*.go"}},
		{Name: "alpha", Include: []string{"src/*/a*.go"}},
	}}
	if got := m.ComponentOf("src/x/ab.go"); got != "zulu" {
		t.Fatalf("ComponentOf = %q, want %q", got, "zulu")
	}
}

func ruleModel() Model {
	return Model{
		Components: []Component{
			{Name: "web", Include: []string{"web/**"}},
			{Name: "service", Include: []string{"service/**"}},
			{Name: "repo", Include: []string{"repo/**"}},
			{Name: "util", Include: []string{"util/**"}},
		},
		Rules: []Rule{
			{ID: "service-allow-list", From: []string{"service"}, AllowedTo: []string{"repo"}},
			{ID: "service-extra-allow", From: []string{"service"}, AllowedTo: []string{"util"}},
			{ID: "web-allow-list", From: []string{"web"}, AllowedTo: []string{"service"}, Severity: SeverityLow},
			{ID: "web-must-not-touch-the-database", From: []string{"web"}, Forbidden: []string{"repo"}, Severity: SeverityHigh},
		},
	}
}

func TestEvaluateAppliesRulePrecedence(t *testing.T) {
	m := ruleModel()
	cases := []struct {
		name     string
		from, to string
		want     Verdict
		wantRule string // "" means no rule decided
	}{
		{"a dependency inside one component is always allowed", "web", "web", VerdictAllowed, ""},
		{"an unconstrained component is allowed anywhere", "util", "repo", VerdictAllowed, ""},
		{"a component with no rule at all is allowed", "repo", "util", VerdictAllowed, ""},
		{"an allow-list permits what it lists", "web", "service", VerdictAllowed, "web-allow-list"},
		{"an allow-list refuses what it omits", "web", "util", VerdictNotAllowed, "web-allow-list"},
		{"forbidden beats an allow-list declared earlier", "web", "repo", VerdictForbidden, "web-must-not-touch-the-database"},
		{"a second allow-list widens the first", "service", "util", VerdictAllowed, "service-extra-allow"},
		{"the union of allow-lists still excludes the rest", "service", "web", VerdictNotAllowed, "service-allow-list"},
		{"an unowned source is not judged", "", "repo", VerdictAllowed, ""},
		{"an unowned target is not judged", "web", "", VerdictAllowed, ""},
		{"an unknown component matches no rule", "ghost", "repo", VerdictAllowed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rule := m.Evaluate(tc.from, tc.to)
			if got != tc.want {
				t.Fatalf("Evaluate(%q, %q) verdict = %q, want %q", tc.from, tc.to, got, tc.want)
			}
			switch {
			case tc.wantRule == "" && rule != nil:
				t.Fatalf("Evaluate(%q, %q) returned rule %q, want none", tc.from, tc.to, rule.ID)
			case tc.wantRule != "" && rule == nil:
				t.Fatalf("Evaluate(%q, %q) returned no rule, want %q", tc.from, tc.to, tc.wantRule)
			case tc.wantRule != "" && rule.ID != tc.wantRule:
				t.Fatalf("Evaluate(%q, %q) returned rule %q, want %q", tc.from, tc.to, rule.ID, tc.wantRule)
			}
		})
	}
}

func TestEvaluateLetsAnyForbiddingRuleWin(t *testing.T) {
	// The allowing rule is declared FIRST, so an implementation that returns on
	// the first matching rule reports "allowed" here.
	m := Model{
		Components: []Component{{Name: "a", Include: []string{"a/**"}}, {Name: "b", Include: []string{"b/**"}}},
		Rules: []Rule{
			{ID: "allow", From: []string{"a"}, AllowedTo: []string{"b"}},
			{ID: "deny", From: []string{"a"}, Forbidden: []string{"b"}},
		},
	}
	verdict, rule := m.Evaluate("a", "b")
	if verdict != VerdictForbidden {
		t.Fatalf("verdict = %q, want %q", verdict, VerdictForbidden)
	}
	if rule == nil || rule.ID != "deny" {
		t.Fatalf("rule = %v, want deny", rule)
	}
}

func TestEvaluateTreatsAForbidOnlyRuleAsNoAllowList(t *testing.T) {
	// A rule that forbids one target must not implicitly forbid every target it
	// did not mention.
	m := Model{
		Components: []Component{
			{Name: "a", Include: []string{"a/**"}},
			{Name: "b", Include: []string{"b/**"}},
			{Name: "c", Include: []string{"c/**"}},
		},
		Rules: []Rule{{ID: "deny-b", From: []string{"a"}, Forbidden: []string{"b"}}},
	}
	if v, _ := m.Evaluate("a", "c"); v != VerdictAllowed {
		t.Fatalf("Evaluate(a, c) = %q, want %q", v, VerdictAllowed)
	}
	if v, _ := m.Evaluate("a", "b"); v != VerdictForbidden {
		t.Fatalf("Evaluate(a, b) = %q, want %q", v, VerdictForbidden)
	}
}

func TestValidateAcceptsAnEmptyModel(t *testing.T) {
	// Architecture governance is opt-in: a repository that declares none must
	// still load.
	if err := (Model{}).Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateAcceptsAWellFormedModel(t *testing.T) {
	if err := ruleModel().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if err := componentModel().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateReportsEveryProblemInOneError(t *testing.T) {
	m := Model{
		Components: []Component{
			{Name: "", Include: []string{"a/**"}},                // empty name
			{Name: "dup", Include: []string{"b/**"}},             // fine
			{Name: "dup", Include: []string{"c/**"}},             // duplicate name
			{Name: "empty-include"},                              // no includes
			{Name: "bad-pattern", Include: []string{"d/**", ""}}, // empty pattern
			{Name: "long-pattern", Include: []string{strings.Repeat("x", maxPatternBytes+1)}},
			{Name: "deep-pattern", Include: []string{strings.Repeat("a/", maxPatternSegments+2) + "z"}},
		},
		Rules: []Rule{
			{ID: "no-from", AllowedTo: []string{"dup"}}, // no from
			{ID: "unknown", From: []string{"nope"}, AllowedTo: []string{"gone"}, Forbidden: []string{"vanished"}},
			{ID: "bad-severity", From: []string{"dup"}, Severity: "critical"},
			{ID: "bad-severity", From: []string{"dup"}}, // duplicate id
			{From: []string{"dup"}}, // unnamed but valid
		},
	}

	err := m.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want problems")
	}
	var ve *ValidationError
	ok := errors.As(err, &ve)
	if !ok {
		t.Fatalf("Validate() returned %T, want *ValidationError", err)
	}

	want := []string{
		`component #1: name is empty`,
		`duplicate component name "dup"`,
		`component "empty-include": no include patterns`,
		`component "bad-pattern": include pattern #2 is empty`,
		`component "long-pattern": include pattern #1 is 513 bytes`,
		`component "deep-pattern": include pattern #1 has more than 64 path segments`,
		`rule "no-from": no `,
		`rule "unknown": from references unknown component "nope"`,
		`rule "unknown": allowed_to references unknown component "gone"`,
		`rule "unknown": forbidden references unknown component "vanished"`,
		`rule "bad-severity": severity "critical"`,
		`rule "bad-severity": duplicate rule id`,
	}
	msg := err.Error()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("error is missing %q\ngot:\n%s", w, msg)
		}
	}
	if len(ve.Problems) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(ve.Problems), len(want), msg)
	}
	// An empty component name must not become a component rules can reference.
	if strings.Count(msg, `unknown component ""`) != 0 {
		t.Errorf("unexpected reference to the empty component name:\n%s", msg)
	}
}

func TestValidateDoesNotRequireRuleIDs(t *testing.T) {
	// Two rules with no ID are not duplicates of each other: the ID is optional,
	// and rejecting the second unnamed rule would be a rule about nothing.
	m := Model{
		Components: []Component{{Name: "a", Include: []string{"a/**"}}},
		Rules:      []Rule{{From: []string{"a"}}, {From: []string{"a"}}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestSeverityOrDefaultFallsBackToMedium(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unset means medium", "", SeverityMedium},
		{"high is preserved", "high", SeverityHigh},
		{"medium is preserved", "medium", SeverityMedium},
		{"low is preserved", "low", SeverityLow},
		{"case and padding are tolerated", "  HIGH ", SeverityHigh},
		{"an unrecognised value cannot escape the severity buckets", "critical", SeverityMedium},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Rule{Severity: tc.in}).SeverityOrDefault(); got != tc.want {
				t.Fatalf("SeverityOrDefault(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHostileInputNeitherPanicsNorHangs(t *testing.T) {
	// Every input here is something a .repocortex.yml in an untrusted repository
	// could contain. None of them may hang the indexer or take the process down;
	// returning "no match" is an acceptable answer for all of them.
	long := strings.Repeat("a/", 100000) + "z.go"
	deepPath := strings.Repeat("a/", maxPathSegments+50) + "z.go"

	cases := []struct {
		name    string
		pattern string
		path    string
	}{
		{"thousands of double stars", strings.Repeat("**/", 20000) + "z.go", "a/b/c/z.go"},
		{"thousands of double stars against a deep path", strings.Repeat("**/", 20000) + "z.go", deepPath},
		{"alternating wildcards and literals", strings.Repeat("**/a/", 20) + "b", strings.Repeat("a/", 100) + "c"},
		{"a segment of nothing but stars", strings.Repeat("*", 400), "abcdefghij"},
		{"stars and question marks interleaved", strings.Repeat("*?", 200), strings.Repeat("ab", 200)},
		{"a path far longer than any real one", "**/z.go", long},
		{"a pattern far longer than any real one", strings.Repeat("ab/", 300) + "z.go", "ab/z.go"},
		{"a path deeper than the segment cap", "**", deepPath},
		{"empty pattern against a real path", "", "a/b.go"},
		{"real pattern against an empty path", "**", ""},
		{"separators only", "/////", "/////"},
	}

	// A generous budget: the point is the difference between milliseconds and
	// never, not a performance assertion that would flake on a loaded machine.
	const budget = 5 * time.Second
	start := time.Now()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = glob(tc.pattern, tc.path)
		})
	}
	if elapsed := time.Since(start); elapsed > budget {
		t.Fatalf("hostile inputs took %s, over the %s budget", elapsed, budget)
	}
}

func TestHostileModelsResolveAndValidateWithoutHanging(t *testing.T) {
	var comps []Component
	for i := 0; i < 3000; i++ {
		comps = append(comps, Component{
			Name:    strings.Repeat("n", i%40+1),
			Include: []string{strings.Repeat("**/", 500) + "*.go", strings.Repeat("*", 300)},
			Exclude: []string{strings.Repeat("**/", 500) + "*_test.go"},
		})
	}
	m := Model{Components: comps, Rules: []Rule{{From: []string{"unknown"}, Severity: "nonsense"}}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Validate()
		_ = m.ComponentOf(strings.Repeat("dir/", 60) + "file.go")
		_ = m.ComponentOf(strings.Repeat("x", maxPathBytes*2))
		v, _ := m.Evaluate("unknown", "also-unknown")
		if v != VerdictAllowed {
			t.Errorf("Evaluate on an unknown pair = %q, want %q", v, VerdictAllowed)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hostile model did not resolve within 10s")
	}
}

func TestResolverAgreesWithTheModelAndIsSafeForConcurrentUse(t *testing.T) {
	m := componentModel()
	r := NewResolver(m)
	paths := []string{
		"internal/store/db.go", "internal/graph/paths.go", "internal/http/server.go",
		"internal/http/server_test.go", "cmd/web/main.go", "docs/spec.md", "",
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				for _, p := range paths {
					if got, want := r.ComponentOf(p), m.ComponentOf(p); got != want {
						t.Errorf("Resolver.ComponentOf(%q) = %q, want %q", p, got, want)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	if r.Model() == nil || len(r.Model().Components) != len(m.Components) {
		t.Fatal("Resolver.Model() did not return the underlying model")
	}
}
