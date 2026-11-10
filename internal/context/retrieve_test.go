package context

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestLexicalSeedPicksTheSymbolTheTaskNames pins WHICH symbol a lexically
// matched file contributes to the walk.
//
// This is a contract, not a preference. Expansion radiates from the seed, so
// seeding a file's first incidental helper instead of the function the task is
// about spends the walk's depth budget going the wrong way out of the right
// file. The original code took whichever unit came first, which made the choice
// depend on nothing the task said.
//
// The self bench cannot tell the two apart — it measured identical at 2/5 —
// which is exactly why the rule is pinned here instead of resting on a number
// that did not move.
func TestLexicalSeedPicksTheSymbolTheTaskNames(t *testing.T) {
	const fileID = int64(1)
	c := &catalogue{
		symbols: map[int64]model.Symbol{
			10: {ID: 10, Name: "init", FileID: fileID},
			11: {ID: 11, Name: "bufferPool", FileID: fileID},
			12: {ID: 12, Name: "RedactSecrets", FileID: fileID},
		},
		unitsByFile: map[int64][]int64{fileID: {10, 11, 12}},
	}
	in := Intent{Terms: []Term{{Text: "redact", Weight: 1, Stem: "redact"}}}
	th := DefaultTuning().Thresholds
	hits := map[int64]*lexicalHit{fileID: {fileID: fileID, terms: []string{"redact", "secret"}, weight: 2}}

	got := addLexicalSeeds(c, in, nil, hits, th)
	if len(got) != 1 {
		t.Fatalf("expected exactly one seed from one matching file, got %d", len(got))
	}
	if got[0].symbolID != 12 {
		t.Errorf("seeded symbol %d (%q); the task names RedactSecrets, and seeding %q sends the walk out of the right file in the wrong direction",
			got[0].symbolID, c.symbols[got[0].symbolID].Name, c.symbols[got[0].symbolID].Name)
	}
}
