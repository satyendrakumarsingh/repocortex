package capability

import (
	"path"
	"strings"
)

// Test classification lives in the shared package rather than in
// capability/tests because impact analysis needs it too: "does anything that
// covers this symbol exist" is an input to the risk score, and two independent
// definitions of "is a test" would let the risk score and the test report
// disagree about the same file.

// TestKind is the level a test operates at, where it can be inferred.
type TestKind string

const (
	// TestKindUnknown means the path and name gave no reliable signal. It is a
	// real answer, not a placeholder — guessing "unit" would be a claim.
	TestKindUnknown     TestKind = "unknown"
	TestKindUnit        TestKind = "unit"
	TestKindIntegration TestKind = "integration"
	TestKindE2E         TestKind = "e2e"
)

// testFileSuffixes are filename endings that mark a test file across the
// languages RepoCortex indexes. Matched on the base name, case-sensitively where
// the convention is case-sensitive (Go, Python) and case-insensitively where it
// is not reliably so.
var testFileSuffixes = []string{
	"_test.go",
	"_test.py", "_test.rb", "_test.rs", "_test.ts", "_test.js",
	"_spec.rb",
	".test.ts", ".test.tsx", ".test.js", ".test.jsx", ".test.mjs",
	".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx", ".spec.mjs",
	"Test.java", "Tests.java", "IT.java", "Test.kt", "Tests.kt",
	"Test.cs", "Tests.cs",
	"Test.php", // PHPUnit
}

// Leading conventions (test_foo.py, TestFoo.java) are handled inline in
// IsTestPath rather than as a list: the "Test" prefix is only valid when what
// follows starts a new word, so it needs a predicate, not a table.

// testDirSegments are path components that mark everything beneath them as
// test code. Deliberately short: "it" and "spec" as bare words appear in
// production trees often enough that including them would misclassify real code
// as tests, and a false "this is covered" is worse than a missed test.
//
// "benches" is Cargo's benchmark directory and nothing else; IsTestName already
// counts Go's Benchmark* functions as test code, so this is the same call.
var testDirSegments = []string{
	"__tests__", "__test__", "testdata", "tests", "test",
	"e2e", "integration-tests", "acceptance", "benches",
}

// IsTestPath reports whether a repo-relative path looks like test code.
//
// Filename convention first, then directory. Both are conventions, not proofs —
// a file named foo_test.go is not guaranteed to contain tests — which is why
// every finding derived from this carries a naming-convention provenance rather
// than being presented as fact.
func IsTestPath(p string) bool {
	if p == "" {
		return false
	}
	base := path.Base(p)
	for _, suf := range testFileSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") {
		return true
	}
	// "Test" as a prefix only counts when what follows starts a new word, so
	// TestRunner.java matches but Tester.java does not.
	if rest, ok := strings.CutPrefix(base, "Test"); ok && startsWord(rest) {
		return true
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		for _, want := range testDirSegments {
			if strings.EqualFold(seg, want) {
				return true
			}
		}
	}
	return false
}

func startsWord(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return c >= 'A' && c <= 'Z'
}

// IsTestName reports whether a symbol name follows a test-function convention.
func IsTestName(name string) bool {
	switch {
	case strings.HasPrefix(name, "Test") && startsWord(strings.TrimPrefix(name, "Test")):
		return true
	case strings.HasPrefix(name, "Benchmark") && startsWord(strings.TrimPrefix(name, "Benchmark")):
		return true
	case strings.HasPrefix(name, "Fuzz") && startsWord(strings.TrimPrefix(name, "Fuzz")):
		return true
	case strings.HasPrefix(name, "Example") && startsWord(strings.TrimPrefix(name, "Example")):
		return true
	case strings.HasPrefix(name, "test_"), strings.HasPrefix(name, "should_"):
		return true
	case strings.HasSuffix(name, "Test"), strings.HasSuffix(name, "Tests"),
		strings.HasSuffix(name, "Spec"), strings.HasSuffix(name, "_test"):
		return true
	default:
		return false
	}
}

// IsTestSymbol reports whether a symbol is test code, by any available signal.
//
// The entity kind is checked first because a parser that emitted
// model.KindTest actually decided this; path and name are fallbacks for the
// generic extraction path, which emits plain functions.
func IsTestSymbol(kind, name, filePath string) bool {
	if kind == "test" {
		return true
	}
	return IsTestPath(filePath) || IsTestName(name)
}

// ClassifyTestKind infers unit / integration / e2e from path and name.
//
// Order matters: e2e markers are checked before integration ones because
// "tests/e2e/checkout_integration_test.go" is an e2e test that mentions
// integration, and the more specific marker should win.
func ClassifyTestKind(filePath, name string) TestKind {
	if !IsTestPath(filePath) && !IsTestName(name) {
		return TestKindUnknown
	}
	hay := strings.ToLower(filePath + "/" + name)

	for _, m := range []string{"e2e", "end2end", "end_to_end", "endtoend", "cypress", "playwright", "smoke"} {
		if strings.Contains(hay, m) {
			return TestKindE2E
		}
	}
	for _, m := range []string{"integration", "/it/", "acceptance", "contract_test", "soak", "functional"} {
		if strings.Contains(hay, m) {
			return TestKindIntegration
		}
	}
	// Java's convention is a suffix, not a word, so it is checked separately
	// from the substring markers above.
	if strings.HasSuffix(path.Base(filePath), "IT.java") {
		return TestKindIntegration
	}
	return TestKindUnit
}

// SubjectStem strips a test file's test markers to recover the base name of the
// file it is presumed to cover: status_test.go -> "status", TestStatus.java ->
// "Status", Status.spec.ts -> "Status".
//
// Returns "" when the path is not a recognised test file, so a caller cannot
// accidentally treat an ordinary file as its own subject.
func SubjectStem(p string) string {
	base := path.Base(p)
	if !IsTestPath(p) {
		return ""
	}
	// Longest suffixes first so ".spec.tsx" is not shortened by a ".tsx" rule.
	for _, suf := range []string{
		"_test.go", "_test.py", "_test.rb", "_test.rs", "_test.ts", "_test.js", "_spec.rb",
		".test.tsx", ".test.jsx", ".test.mjs", ".test.ts", ".test.js",
		".spec.tsx", ".spec.jsx", ".spec.mjs", ".spec.ts", ".spec.js",
		"Tests.java", "Test.java", "IT.java", "Tests.kt", "Test.kt", "Tests.cs", "Test.cs",
	} {
		if stem, ok := strings.CutSuffix(base, suf); ok && stem != "" {
			return stem
		}
	}
	if stem, ok := strings.CutPrefix(base, "test_"); ok {
		return strings.TrimSuffix(stem, path.Ext(stem))
	}
	if stem, ok := strings.CutPrefix(base, "Test"); ok && startsWord(stem) {
		return strings.TrimSuffix(stem, path.Ext(stem))
	}
	return ""
}

// FileStem is the base name of a path with its extension removed.
func FileStem(p string) string {
	base := path.Base(p)
	return strings.TrimSuffix(base, path.Ext(base))
}

// IsSiblingTestFile reports whether testPath is the conventional test file for
// sourcePath: same directory, and its subject stem is the source file's stem.
//
// This is stricter than "a test file in the same directory", and deliberately
// so. Plain co-location claims that every test beside a file covers that file,
// which in a package with twenty files and one test is wrong nineteen times.
// Requiring the stems to correspond turns co-location from a guess into a
// convention with a name — and a symbol whose file has no sibling test file
// then honestly reports having no co-located coverage.
func IsSiblingTestFile(sourcePath, testPath string) bool {
	if sourcePath == "" || testPath == "" || sourcePath == testPath {
		return false
	}
	if path.Dir(sourcePath) != path.Dir(testPath) {
		return false
	}
	stem := SubjectStem(testPath)
	return stem != "" && strings.EqualFold(stem, FileStem(sourcePath))
}

// ConventionalTestSuffix reports whether what follows a generated test-name
// prefix keeps the match conventional. Pass it to SymbolIndex.WithPrefix.
//
// Empty means an exact hit (TestUpdateStatus for updateStatus); a leading "_"
// covers the subtest convention (TestUpdateStatus_NotFound). Anything else is
// rejected, which costs recall — it misses TestUpdateStatusReturnsError — and
// buys precision: without the rule, target `Update` would claim
// `TestUpdateStatus` as its test. Recall lost here is recovered by the
// call-graph signal whenever a real call edge exists, and the call-graph signal
// is the one worth trusting anyway.
//
// Trailing digits were allowed at first, for the TestUpdateStatus2 variant
// convention. They came out after the 100k-symbol benchmark showed what the
// rule actually does: any identifier ending in a digit swallows its longer
// neighbours, so `Sha25` would claim TestSha256 and `v1` would claim TestV10.
// On the benchmark fixture that single rule turned 2 naming hits into 50. A
// numbered-variant test is rare; digit-suffixed identifiers are not.
func ConventionalTestSuffix(rest string) bool {
	return rest == "" || rest[0] == '_'
}

// upperFirst capitalises the first ASCII letter, for building TestFoo from foo.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	c := s[0]
	if c >= 'a' && c <= 'z' {
		return string(c-32) + s[1:]
	}
	return s
}

// camelToSnake converts updateStatus to update_status, for the Python and Ruby
// test-name conventions. Runs of capitals are kept together so HTTPServer
// becomes http_server rather than h_t_t_p_server.
func camelToSnake(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			prevLower := i > 0 && s[i-1] >= 'a' && s[i-1] <= 'z'
			nextLower := i+1 < len(s) && s[i+1] >= 'a' && s[i+1] <= 'z'
			if i > 0 && (prevLower || nextLower) {
				b.WriteByte('_')
			}
			b.WriteByte(c + 32)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// TestNameCandidates returns the conventional test-function names for a symbol,
// as (prefix, exact) pairs where exact says whether the generated name is a
// complete convention rather than a prefix to be extended.
//
// Returned as prefixes so a single sorted-key binary search finds both
// TestUpdateStatus and TestUpdateStatus_NotFound, rather than the caller having
// to enumerate every subtest suffix that might exist.
func TestNameCandidates(symbolName string) []string {
	if symbolName == "" {
		return nil
	}
	upper := upperFirst(symbolName)
	snake := camelToSnake(symbolName)

	cands := []string{
		"Test" + upper,      // Go, C#
		"Test" + symbolName, // Go, when the symbol is already exported
		"test_" + snake,     // Python, Ruby
		"test_" + symbolName,
		"should_" + snake,
		upper + "Test",  // Java, JUnit class
		upper + "Tests", // .NET, JUnit
		upper + "Spec",  // RSpec, Jasmine
		symbolName + "Test",
		symbolName + "Spec",
	}
	return dedupeStrings(cands)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
