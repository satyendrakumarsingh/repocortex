package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The word a tier reads as exists three times: here, for the prose the server
// writes; in assets/js/tiers.mjs, for everything the client renders; and in
// app.js's TIER map, the hand-kept mirror that survives the module not
// loading. Copies of a vocabulary drift unless something holds them together,
// so the Go word for every tier must be the word each copy gives THAT id —
// membership in the set is not enough, since swapping two words keeps the set.
func TestTierWordsMatchTheClientModule(t *testing.T) {
	mod := readAsset(t, "assets/js/tiers.mjs")
	app := readAsset(t, "assets/app.js")
	for _, tier := range []model.Confidence{
		model.ConfidenceExact, model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow, model.Confidence("unknown"),
	} {
		id := string(tier)
		word := tierWord(tier)
		if strings.ContainsAny(word, "'\\") {
			t.Fatalf("tierWord(%s) = %q contains a quote and cannot be matched against the module", tier, word)
		}
		if word == id {
			t.Errorf("tierWord(%s) is the id itself; the word must be plain", tier)
		}
		re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(id) + `:\s*\{\s*word:\s*'([^']*)'`)
		m := re.FindStringSubmatch(mod)
		if m == nil {
			t.Errorf("assets/js/tiers.mjs has no TIER_WORDS entry for %s", id)
		} else if m[1] != word {
			t.Errorf("tierWord(%s) = %q, but assets/js/tiers.mjs gives that id %q", tier, word, m[1])
		}
		mirror := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(id) + `:\s*\{[^\n]*\blabel:\s*'([^']*)'`)
		if m := mirror.FindStringSubmatch(app); m == nil {
			t.Errorf("assets/app.js's TIER map has no labelled entry for %s", id)
		} else if m[1] != word {
			t.Errorf("tierWord(%s) = %q, but assets/app.js's TIER map labels that id %q", tier, word, m[1])
		}
	}
	if got := tierWord(model.Confidence("nonsense")); got != "unmeasured" {
		t.Errorf("an unknown tier reads as %q, want unmeasured", got)
	}
}

func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := assets.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
