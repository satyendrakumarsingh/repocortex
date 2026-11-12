package infer

import (
	"reflect"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// legacyPackageIsDirectory, legacyReceiverIsAType and legacyMemberSeparators are
// the three switches LanguageSpec replaced, copied VERBATIM from the resolver as
// it stood before the refactor.
//
// They are the oracle for TestSpecsReproduceTheLegacySwitches, and that test is
// the only thing standing between "the table is tidier" and "the table changed
// an answer". The refactor was required to be behaviour-preserving and was
// measured end to end against a scip-go index to prove it; this keeps the
// property from drifting afterwards, when nobody is measuring.
//
// Do NOT update these to match a new table entry. If a spec field must change,
// the change has to be justified by a precision measurement, and these
// functions are what force that conversation to happen.
func legacyPackageIsDirectory(language string) bool {
	switch language {
	case "go", "java", "kotlin":
		return true
	default:
		return false
	}
}

func legacyReceiverIsAType(language string) bool {
	switch language {
	case "java", "kotlin", "typescript":
		return true
	default:
		return false
	}
}

func legacyMemberSeparators(language string) []string {
	switch language {
	case "java", "kotlin", "scala":
		return []string{".", "#", "$"}
	case "ruby", "rust", "cpp", "c", "php":
		return []string{"::", ".", "#"}
	default:
		return []string{".", "#"}
	}
}

// legacyLanguages is every language the three switches named, plus the grammars
// RepoCortex parses with a supplement or query, plus names that were in NONE of
// them — the fallback path is as much a behaviour to preserve as the explicit
// cases, and it is the one covering roughly 190 of the 206 grammars.
var legacyLanguages = []string{
	"go", "java", "kotlin", "typescript", "tsx", "javascript", "python",
	"rust", "scala", "ruby", "cpp", "c", "php",
	// Never named by any switch. These must still answer exactly as the old
	// default branch did.
	"haskell", "elixir", "zig", "ocaml", "swift", "", "GO", "Java",
}

func TestSpecsReproduceTheLegacySwitches(t *testing.T) {
	specs := NewSpecs()
	for _, lang := range legacyLanguages {
		t.Run(lang, func(t *testing.T) {
			spec := specs.For(lang)

			if got, want := spec.PackageIsDirectory, legacyPackageIsDirectory(lang); got != want {
				t.Errorf("PackageIsDirectory = %v, legacy switch says %v", got, want)
			}
			// fileSegments used to branch on packageIsDirectory directly, so
			// ImportPathNamesFile must be its exact negation for every language
			// that existed before the spec did.
			if got, want := spec.ImportPathNamesFile, !legacyPackageIsDirectory(lang); got != want {
				t.Errorf("ImportPathNamesFile = %v, legacy fileSegments implies %v", got, want)
			}
			if got, want := spec.ReceiverDenotesType, legacyReceiverIsAType(lang); got != want {
				t.Errorf("ReceiverDenotesType = %v, legacy switch says %v", got, want)
			}
			if got, want := spec.MemberSeparators, legacyMemberSeparators(lang); !reflect.DeepEqual(got, want) {
				t.Errorf("MemberSeparators = %v, legacy switch says %v", got, want)
			}
		})
	}
}

// TestSpecsPreserveTheHardcodedReceiverRules covers the constants that were not
// switches at all: the three-character floor and the this/self exclusion were
// literals inside strictReceiverMatch, receiverMatches and receiverNamesAType.
// Moving them into the spec must not have moved their VALUES.
func TestSpecsPreserveTheHardcodedReceiverRules(t *testing.T) {
	specs := NewSpecs()
	for _, lang := range legacyLanguages {
		spec := specs.For(lang)
		if spec.MinTypeNameLength != 3 {
			t.Errorf("%s: MinTypeNameLength = %d, was hardcoded 3", lang, spec.MinTypeNameLength)
		}
		if !reflect.DeepEqual(spec.SelfNames, []string{"this", "self"}) {
			t.Errorf("%s: SelfNames = %v, was hardcoded this/self", lang, spec.SelfNames)
		}
	}

	spec := specs.For("java")
	// namesAType is the floor-bearing predicate: strictReceiverMatch and
	// receiverNamesAType both refused anything under three characters.
	for _, r := range []string{"", "b", "ab", "this", "self", "THIS", "*Se"} {
		if spec.namesAType(r) {
			t.Errorf("namesAType(%q) = true, the legacy rule refused it", r)
		}
	}
	for _, r := range []string{"Builder", "*Builder", "abc"} {
		if !spec.namesAType(r) {
			t.Errorf("namesAType(%q) = false, the legacy rule accepted it", r)
		}
	}
	// usableReceiver is receiverMatches' weaker test: it refused empty and
	// self-references and applied NO length floor. `ab` must survive here even
	// though namesAType rejects it.
	for _, r := range []string{"", "this", "self", "*self"} {
		if spec.usableReceiver(r) {
			t.Errorf("usableReceiver(%q) = true, the legacy rule refused it", r)
		}
	}
	for _, r := range []string{"b", "ab", "Builder"} {
		if !spec.usableReceiver(r) {
			t.Errorf("usableReceiver(%q) = false, the legacy rule accepted it", r)
		}
	}
}

// TestSpecsRespectCandidateCap enforces the cross-package contract documented on
// MaxCandidates: internal/pipeline sizes its candidate cache at MaxCandidates+1
// rows per name, so a language asking for a higher cap would be handed a
// TRUNCATED candidate set and would resolve on partial evidence while reporting
// full confidence. Lowering the cap is fine; raising it is not.
func TestSpecsRespectCandidateCap(t *testing.T) {
	specs := NewSpecs()
	for _, lang := range append(append([]string{}, specs.Names()...), "unknown-language") {
		if cap := specs.For(lang).MaxCandidates; cap > MaxCandidates {
			t.Errorf("%s: MaxCandidates = %d exceeds the cache ceiling of %d", lang, cap, MaxCandidates)
		} else if cap <= 0 {
			t.Errorf("%s: MaxCandidates = %d would abandon every reference", lang, cap)
		}
	}
}

// TestSpecsAreInternallyConsistent is the check that replaces the reviewer who
// used to have to hold three switches in their head at once.
//
// ReceiverDenotesType without CallMethod is the contradiction that matters: it
// claims the parser rewrites receivers to types in a language whose calls never
// carry a receiver. It cannot be true, and a new-language contributor who sets
// one and forgets the other gets told here rather than in a precision report a
// week later.
func TestSpecsAreInternallyConsistent(t *testing.T) {
	specs := NewSpecs()
	for _, lang := range specs.Names() {
		spec := specs.For(lang)
		if spec.Name != lang {
			t.Errorf("%s: spec carries Name %q", lang, spec.Name)
		}
		if spec.ReceiverDenotesType && !spec.CallShapes.Permits(CallMethod) {
			t.Errorf("%s: ReceiverDenotesType with no CallMethod shape", lang)
		}
		if len(spec.MemberSeparators) == 0 {
			t.Errorf("%s: no member separators, ownsQualifiedName can never match", lang)
		}
		if spec.CallShapes == 0 {
			t.Errorf("%s: no call shapes declared", lang)
		}
	}
}

// TestFallbackCarriesTheRequestedName keeps diagnostics honest: an unknown
// language must still say which language it was.
func TestFallbackCarriesTheRequestedName(t *testing.T) {
	spec := NewSpecs().For("brainfuck")
	if spec.Name != "brainfuck" {
		t.Errorf("fallback Name = %q, want the language that was asked for", spec.Name)
	}
}

// TestSpecNamesAreSorted guards the property every listing in this package has
// to hold: no map iteration order may reach a caller. The registry digest is
// computed over resolver output, and a determinism defect of exactly this shape
// — an unordered read feeding a first-wins rule — produced three different
// digests over byte-identical source.
func TestSpecNamesAreSorted(t *testing.T) {
	specs := NewSpecs()
	first := specs.Names()
	for i := 0; i < 50; i++ {
		if got := specs.Names(); !reflect.DeepEqual(got, first) {
			t.Fatalf("Names() returned %v, previously %v", got, first)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] >= first[i] {
			t.Fatalf("Names() is not sorted: %q before %q", first[i-1], first[i])
		}
	}
}

// TestTestFileConventions covers the DESCRIPTIVE field. It is tested precisely
// because nothing in the binding path reads it: an unexercised convention is one
// that quietly rots, and the reason it is not consulted (A19's 2.4pp) is a
// reason to keep it correct, not a reason to leave it unchecked.
func TestTestFileConventions(t *testing.T) {
	specs := NewSpecs()
	tests := []struct {
		language string
		path     string
		want     bool
	}{
		{"go", "internal/resolve/infer/resolver_test.go", true},
		{"go", "internal/resolve/infer/resolver.go", false},
		{"go", "internal/parse/testdata/sample.go", true},
		{"python", "tests/test_addons.py", true},
		{"python", "scrapy/utils/test.py", false},
		{"python", "scrapy/addons_test.py", true},
		{"python", "tests/conftest.py", true},
		{"python", "scrapy/crawler.py", false},
		{"typescript", "src/app.spec.ts", true},
		{"typescript", "src/__tests__/app.ts", true},
		{"typescript", "src/app.ts", false},
		{"java", "src/test/java/com/pay/Foo.java", true},
		{"java", "src/main/java/com/pay/FooTest.java", true},
		{"java", "src/main/java/com/pay/Foo.java", false},
		// The fallback still has to answer, and must not claim a bare file is
		// test code just because the language is unknown.
		{"haskell", "src/Main.hs", false},
		{"haskell", "test/Spec.hs", true},
	}
	for _, tc := range tests {
		if got := specs.For(tc.language).IsTestFile(tc.path); got != tc.want {
			t.Errorf("%s: IsTestFile(%q) = %v, want %v", tc.language, tc.path, got, tc.want)
		}
	}
}

// TestFileSegmentsFollowTheSpec pins the behaviour ImportPathNamesFile drives,
// because it is the field most likely to be flipped by someone reasoning about
// package semantics rather than about what an import path denotes.
func TestFileSegmentsFollowTheSpec(t *testing.T) {
	specs := NewSpecs()

	// Go: the last segment is a DIRECTORY. Including the file stem cost
	// prometheus 0.4 points.
	if got, want := fileSegments(specs.For("go"), "internal/config/load.go"),
		[]string{"internal", "config"}; !reflect.DeepEqual(got, want) {
		t.Errorf("go fileSegments = %v, want %v", got, want)
	}
	// TypeScript and Python: a module IS a file, so the stem is the last
	// segment. This is what makes `from './sibling'` match sibling.ts.
	if got, want := fileSegments(specs.For("typescript"), "src/net/client.ts"),
		[]string{"src", "net", "client"}; !reflect.DeepEqual(got, want) {
		t.Errorf("typescript fileSegments = %v, want %v", got, want)
	}
	if got, want := fileSegments(specs.For("python"), "scrapy/utils/test.py"),
		[]string{"scrapy", "utils", "test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("python fileSegments = %v, want %v", got, want)
	}
}

// TestPythonResolvesThroughItsImportsNotItsNeighbours is the shape that cost
// scrapy 4.7 points of `high` precision, expressed as a test over the spec that
// now declares the answer.
//
// tests/test_addons.py imports get_crawler from scrapy.utils.test and defines
// nothing of that name. An unrelated get_crawler in a NEIGHBOURING test file
// must not win, because in Python adjacency asserts nothing about visibility.
func TestPythonResolvesThroughItsImportsNotItsNeighbours(t *testing.T) {
	idx := index(
		sym(1, "get_crawler", "scrapy/utils/test.py:get_crawler", "scrapy/utils/test.py", 10, true),
		sym(2, "get_crawler", "tests/test_spider.py:get_crawler", "tests/test_spider.py", 20, true),
	)
	fr := registry.FileRefs{
		FileID: 90, Path: "tests/test_addons.py", Language: "python",
		Imports: []registry.Import{{Path: "scrapy.utils.test"}},
		Symbols: enclose(91, "test_addon_is_loaded"),
		Refs: []registry.Reference{
			{ID: 1, Name: "get_crawler", Kind: "reference.call", Line: 58},
		},
	}
	edges, _, _ := resolveOne(t, New(idx, Options{}), fr)
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	if edges[0].DstID != 1 {
		t.Errorf("bound to %d (the neighbouring test file); the import names %d",
			edges[0].DstID, 1)
	}
}

// TestGoResolvesThroughProximityNotItsImports is the converse, and the reason
// the order is a per-language field rather than one global answer. Reversing it
// for Go cost prometheus 0.8 points.
func TestGoResolvesThroughProximityNotItsImports(t *testing.T) {
	idx := index(
		sym(1, "Load", "internal/config/helper.go:Load", "internal/config/helper.go", 10, true),
		sym(2, "Load", "internal/store/store.go:Load", "internal/store/store.go", 20, true),
	)
	fr := registry.FileRefs{
		FileID: 90, Path: "internal/config/load.go", Language: "go",
		Imports: []registry.Import{{Path: "github.com/satyendrakumarsingh/repocortex/internal/store"}},
		Symbols: enclose(91, "Run"),
		Refs: []registry.Reference{
			{ID: 1, Name: "Load", Kind: "reference.call", Line: 12},
		},
	}
	edges, _, _ := resolveOne(t, New(idx, Options{}), fr)
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1: %+v", len(edges), edges)
	}
	if edges[0].DstID != 1 {
		t.Errorf("bound to %d; an unqualified name in Go resolves in-package, so %d",
			edges[0].DstID, 1)
	}
}

// The interop matrix has two failure modes and they are not symmetric.
//
// Failing CLOSED (tsx, vue) refuses real bindings, so references silently
// vanish from the graph — the caller sees "no references" and believes it.
// Failing OPEN (svelte, astro, css) lets a stylesheet bind to a Go function,
// which is a wrong edge. Under this project's doctrine a wrong edge is the
// worse of the two, because a missing edge is at least honest.
func TestTSXCanBindToTypeScriptAndJavaScript(t *testing.T) {
	spec := NewSpecs().For("tsx")

	for _, dst := range []string{"a.ts", "a.tsx", "a.js", "a.vue"} {
		if !spec.bindableAcross(dst) {
			t.Errorf("tsx must be bindable to %s; typescript and javascript both "+
				"already declare tsx in their Interop", dst)
		}
	}
	if spec.bindableAcross("a.go") {
		t.Error("tsx must not be bindable to Go")
	}
}

func TestVueHasItsOwnSpecAndBindsToTheScriptLanguages(t *testing.T) {
	spec := NewSpecs().For("vue")

	if spec.Name != "vue" {
		t.Fatalf("Name = %q, want vue (a missing spec falls back and binds to nothing)", spec.Name)
	}
	for _, dst := range []string{"a.ts", "a.tsx", "a.js", "a.vue"} {
		if !spec.bindableAcross(dst) {
			t.Errorf("vue must be bindable to %s: a .vue script block imports "+
				"ordinary TS and JS modules", dst)
		}
	}
	if spec.bindableAcross("a.py") {
		t.Error("vue must not be bindable to Python")
	}
}

// The fail-open case. Absence from knownLanguages means bindableAcross returns
// true for every destination, so these three currently bind to Go and Python.
func TestMarkupAndStyleLanguagesDoNotBindToBackendLanguages(t *testing.T) {
	specs := NewSpecs()
	for _, lang := range []string{"svelte", "astro", "css"} {
		t.Run(lang, func(t *testing.T) {
			spec := specs.For(lang)
			for _, dst := range []string{"a.go", "a.py", "a.java"} {
				if spec.bindableAcross(dst) {
					t.Errorf("%s must not be bindable to %s", lang, dst)
				}
			}
		})
	}
}

// The structural guard. Without it, adding a language to knownLanguages and
// forgetting its spec reproduces exactly the vue bug: in the table, no opinion,
// binds to nothing.
func TestEveryKnownLanguageHasASpec(t *testing.T) {
	specs := NewSpecs()
	for lang := range knownLanguages {
		if got := specs.For(lang).Name; got != lang {
			t.Errorf("%q is in knownLanguages but has no spec (For returned %q); "+
				"it will take the fallback and bind only to itself", lang, got)
		}
	}
}

// Interop must be mutual. typescript listing tsx while tsx omits typescript is
// precisely the shipped bug.
func TestInteropIsSymmetric(t *testing.T) {
	specs := NewSpecs()
	for _, name := range specs.Names() {
		spec := specs.For(name)
		for _, other := range spec.Interop {
			if other == name {
				continue
			}
			back := specs.For(other)
			if back.Name != other {
				t.Errorf("%s declares interop with %q, which has no spec", name, other)
				continue
			}
			if !back.interopSet()[name] {
				t.Errorf("%s declares interop with %s, but %s does not declare it back",
					name, other, other)
			}
		}
	}
}

// TestResolvedTierLanguagesDeclareTheirTestFiles covers the DESCRIPTIVE field
// for the languages added after the measured three. Like TestTestFileConventions
// above, it exists precisely because nothing in the binding path reads
// TestFiles — `repocortex verify` classifies through capability.IsTestPath, not
// through the spec — and an unexercised convention is one that quietly rots.
// The language's convention is recorded here so that the two classifiers
// (this one and capability.IsTestPath) can be compared, and so a spec that
// answers "no" for every file of a language it claims to know is caught.
//
// A fallback is detected by membership in Names(), not by Name: For copies the
// requested name onto the fallback so diagnostics can say what was asked for
// (TestFallbackCarriesTheRequestedName), which makes Name useless for telling
// a declared spec from a default one.
func TestResolvedTierLanguagesDeclareTheirTestFiles(t *testing.T) {
	specs := NewSpecs()
	declared := map[string]bool{}
	for _, name := range specs.Names() {
		declared[name] = true
	}
	cases := map[string][]string{
		"ruby": {"spec/models/order_spec.rb", "test/models/order_test.rb"},
		"rust": {"tests/integration.rs", "benches/bench.rs"},
		"php":  {"tests/OrderTest.php", "tests/Feature/OrderTest.php"},
		"c_sharp": {
			"tests/OrderTests.cs",
			// Matches on the `Tests.cs` suffix; the directory `Order.Tests`
			// is one segment and is not `tests`, see the negative case.
			"Order.Tests/OrderTests.cs",
		},
	}
	// Production files the convention must NOT claim. Without these a spec
	// that matched everything would pass the positive cases above.
	//
	// `Order.Tests/Order.cs` is the one that pins the splitter: a directory
	// segment is what sits between slashes, so a `.Tests` project directory
	// does not yield a `tests` segment. It used to, because Matches borrowed
	// pathSegments, which splits on `.` as well for import-path matching, and
	// this classifier then disagreed with the one verify uses
	// (capability.IsTestPath), which splits on `/` only. Whether a `.Tests`
	// project should count is a convention to declare, not a side effect of a
	// splitter written for something else.
	notTests := map[string][]string{
		"ruby":    {"app/models/order.rb"},
		"rust":    {"src/order.rs"},
		"php":     {"src/Order.php"},
		"c_sharp": {"src/Orders/Order.cs", "Order.Tests/Order.cs"},
	}
	for lang, files := range cases {
		if !declared[lang] {
			t.Errorf("%s: fell back to the default spec; a resolved-tier language must declare one", lang)
		}
		spec := specs.For(lang)
		for _, f := range files {
			if !spec.TestFiles.Matches(f) {
				t.Errorf("%s: %s is a test file by the language's own convention and the spec says it is not", lang, f)
			}
		}
	}
	for lang, files := range notTests {
		spec := specs.For(lang)
		for _, f := range files {
			if spec.TestFiles.Matches(f) {
				t.Errorf("%s: %s is not a test file by the language's own convention and the spec says it is", lang, f)
			}
		}
	}
}

// TestCSharpBindsOnlyToItself: C# has no module system in common with any
// other language in the table, so a binding across that line is never right.
//
// The last check is the one that makes the first two safe to declare: a spec
// named by the grammar (`c_sharp`) with a non-empty Interop refuses every
// destination whose language it does not list, and languageOfPath used to
// classify a .cs file as "csharp". Together those would have refused every
// C#-to-C# binding, silently, as a missing edge.
func TestCSharpBindsOnlyToItself(t *testing.T) {
	spec := NewSpecs().For("c_sharp")
	if got, want := spec.Interop, []string{"c_sharp"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Interop = %v, want %v", got, want)
	}
	if spec.PackageIsDirectory {
		t.Error("PackageIsDirectory = true; a C# namespace is declared, not derived from the directory")
	}
	if !spec.bindableAcross("src/Orders/Order.cs") {
		t.Error("c_sharp must be bindable to a .cs file; languageOfPath and the spec disagree on the language's name")
	}
	for _, dst := range []string{"a.go", "a.py", "a.ts", "a.java"} {
		if spec.bindableAcross(dst) {
			t.Errorf("c_sharp must not be bindable to %s", dst)
		}
	}
}
