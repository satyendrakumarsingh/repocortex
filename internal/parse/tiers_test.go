package parse

import "testing"

func TestTierForGoIsTyped(t *testing.T) {
	if got := TierFor("go"); got != TierTyped {
		t.Fatalf("go: want TierTyped, got %v", got)
	}
}

// TestTierForPythonIsTyped pins the one language that reaches the typed tier
// without an arity query, and why it does.
//
// Python discriminates by the receiver's CLASS — read from the source by the
// scope analysis in pyscope.go — which is a second way to earn the tier, not
// an exception to it. The promotion was made on a measurement against
// scip-python over four repositories, recorded in docs/accuracy.md; a Python
// arity query was measured in the same run, decided nothing, and was removed.
func TestTierForPythonIsTyped(t *testing.T) {
	if got := TierFor("python"); got != TierTyped {
		t.Fatalf("python: want TierTyped, got %v", got)
	}
	if !HasScopeAnalysis("python") {
		t.Fatal("python is typed without a scope analysis to earn it")
	}
	if _, hasArity := arityDialectFor("python"); hasArity {
		t.Error("python carries an arity dialect; docs/accuracy.md records it as measured and removed")
	}
}

// TestTypedTierIsEarnedByDiscrimination: every typed language has one of the
// two things that tell candidates of one name apart, and no other language has
// both the queries and one of them. A language cannot be typed by being listed.
func TestTypedTierIsEarnedByDiscrimination(t *testing.T) {
	for lang, tier := range Tiers() {
		_, hasArity := arityDialectFor(lang)
		earned := ImportQuery(lang) != "" && HasMeasuredReceivers(lang) &&
			(hasArity || HasScopeAnalysis(lang))
		if (tier == TierTyped) != earned {
			t.Errorf("%s: tier %v, but imports+receiver+(arity or scope analysis) = %v", lang, tier, earned)
		}
	}
	// The scope analysis is not a label: it is absent wherever the parser has
	// no walk to back it.
	for _, lang := range []string{"go", "java", "typescript", "ruby", "not-a-language"} {
		if HasScopeAnalysis(lang) {
			t.Errorf("%s: reports a scope analysis it does not have", lang)
		}
	}
}

func TestTierForGrammarOnlyLanguageIsParsed(t *testing.T) {
	// nix has a grammar but no import, inheritance, receiver or arity query.
	if got := TierFor("nix"); got != TierParsed {
		t.Fatalf("nix: want TierParsed, got %v", got)
	}
}

func TestTierForUnknownLanguageIsParsed(t *testing.T) {
	if got := TierFor("not-a-language"); got != TierParsed {
		t.Fatalf("unknown: want TierParsed, got %v", got)
	}
}

func TestTierStringsAreStable(t *testing.T) {
	cases := map[Tier]string{
		TierParsed:   "parsed",
		TierResolved: "resolved",
		TierTyped:    "typed",
	}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Errorf("Tier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

func TestNewlyResolvedLanguages(t *testing.T) {
	for _, lang := range []string{"ruby", "php", "c_sharp", "kotlin", "scala", "c", "cpp"} {
		if ImportQuery(lang) == "" {
			t.Errorf("%s: no import query", lang)
		}
		if got := TierFor(lang); got < TierResolved {
			t.Errorf("%s: want at least TierResolved, got %v", lang, got)
		}
	}
}

// A single-file component has no import query of its own: its script is read
// as TypeScript or JavaScript, so it is resolved when that language is.
func TestSingleFileComponentsAreResolved(t *testing.T) {
	for _, lang := range []string{"vue", "svelte", "apex", "solidity"} {
		if got := TierFor(lang); got != TierResolved {
			t.Errorf("%s: want TierResolved, got %v", lang, got)
		}
	}
}

// Haskell, F#, OCaml, Perl and Zig read their imports, and so bind calls across files.
func TestModuleLanguagesAreResolved(t *testing.T) {
	for _, lang := range []string{"haskell", "fsharp", "ocaml", "perl", "zig"} {
		if ImportQuery(lang) == "" {
			t.Errorf("%s: no import query", lang)
		}
		if got := TierFor(lang); got != TierResolved {
			t.Errorf("%s: want TierResolved, got %v", lang, got)
		}
	}
}

// MATLAB, Fortran, CUDA, GLSL, HLSL and CMake read their imports (an `import`, a
// `use`, an `#include`, an `include()`), and so bind calls across files.
func TestFileAndModuleLanguagesAreResolved(t *testing.T) {
	for _, lang := range []string{"matlab", "fortran", "cuda", "glsl", "hlsl", "cmake"} {
		if ImportQuery(lang) == "" {
			t.Errorf("%s: no import query", lang)
		}
		if got := TierFor(lang); got != TierResolved {
			t.Errorf("%s: want TierResolved, got %v", lang, got)
		}
	}
}

func TestTiersCoversEveryGrammar(t *testing.T) {
	all := PureGoBackend().Languages()
	tiers := Tiers()
	if len(tiers) != len(all) {
		t.Fatalf("Tiers() has %d entries, backend reports %d grammars", len(tiers), len(all))
	}
	for _, lang := range all {
		if _, ok := tiers[lang]; !ok {
			t.Errorf("Tiers() is missing %q", lang)
		}
	}
}
