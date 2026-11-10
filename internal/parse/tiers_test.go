package parse

import "testing"

func TestTierForGoIsTyped(t *testing.T) {
	if got := TierFor("go"); got != TierTyped {
		t.Fatalf("go: want TierTyped, got %v", got)
	}
}

func TestTierForPythonIsAtLeastResolved(t *testing.T) {
	if got := TierFor("python"); got < TierResolved {
		t.Fatalf("python: want at least TierResolved, got %v", got)
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
