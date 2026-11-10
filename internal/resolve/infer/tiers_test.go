package infer

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func entry(id int64, name, file string) registry.SymbolIndexEntry {
	return registry.SymbolIndexEntry{
		ID: id, Name: name, FQN: file + ":" + name,
		FilePath: file, Kind: "function", Exported: true,
	}
}

// TestImportBeatsProximityWhereAModuleIsAFile pins the largest single source of
// wrong high-confidence edges found against compiler ground truth.
//
// `tests/test_addons.py` imports get_crawler from scrapy.utils.test, defines
// nothing of that name, and calls it. Directory proximity matched an unrelated
// get_crawler in a NEIGHBOURING test file and returned it as high confidence —
// 144 edges from that one name, roughly 45% of every high-confidence
// disagreement in scrapy, and 87.1% -> 91.8% precision once fixed.
//
// An import is a declaration. Adjacency is an inference. In a language where
// each file is its own module, adjacency means nothing at all.
func TestImportBeatsProximityWhereAModuleIsAFile(t *testing.T) {
	r := New(nil, Options{})
	cands := []registry.SymbolIndexEntry{
		entry(1, "get_crawler", "tests/test_extension_throttle.py"), // same directory
		entry(2, "get_crawler", "scrapy/utils/test.py"),             // imported
	}
	fr := registry.FileRefs{
		FileID: 99, Path: "tests/test_addons.py", Language: "python",
		Imports: []registry.Import{{Path: "scrapy.utils.test"}},
	}
	got, ok := r.pick(NewSpecs().For(fr.Language), fr, buildImportSet(NewSpecs().For(fr.Language), fr.Imports),
		registry.Reference{Name: "get_crawler"}, cands)
	if !ok {
		t.Fatal("resolved nothing")
	}
	if got.entry.FilePath != "scrapy/utils/test.py" {
		t.Errorf("bound to %s (%s); the file imports scrapy.utils.test and the "+
			"other candidate is merely adjacent", got.entry.FilePath, got.reason)
	}
}

// TestProximityBeatsImportWhereADirectoryIsThePackage pins the opposite order,
// which is equally load-bearing and was measured the same way.
//
// In Go an unqualified name resolves IN the package; the imported one would
// have to be written pkg.Foo(). Putting imports first cost prometheus 0.8
// points of high-confidence precision, so the ORDER is language-dependent
// rather than one rule for everything.
func TestProximityBeatsImportWhereADirectoryIsThePackage(t *testing.T) {
	r := New(nil, Options{})
	cands := []registry.SymbolIndexEntry{
		entry(1, "Helper", "internal/server/helper.go"), // same package
		entry(2, "Helper", "internal/util/helper.go"),   // imported
	}
	fr := registry.FileRefs{
		FileID: 99, Path: "internal/server/server.go", Language: "go",
		Imports: []registry.Import{{Path: "github.com/satyendrakumarsingh/repocortex/internal/util"}},
	}
	got, ok := r.pick(NewSpecs().For(fr.Language), fr, buildImportSet(NewSpecs().For(fr.Language), fr.Imports),
		registry.Reference{Name: "Helper"}, cands)
	if !ok {
		t.Fatal("resolved nothing")
	}
	if got.entry.FilePath != "internal/server/helper.go" {
		t.Errorf("bound to %s (%s); an unqualified name in Go resolves in its own "+
			"package", got.entry.FilePath, got.reason)
	}
}

// TestAdjacencyWithoutAnImportIsNotHighConfidence pins the honesty half.
//
// Where the language requires an import, a neighbouring file is still the best
// guess available — but it is a guess, and the label has to say so.
func TestAdjacencyWithoutAnImportIsNotHighConfidence(t *testing.T) {
	r := New(nil, Options{})
	fr := registry.FileRefs{
		FileID: 99, Path: "app/views.py", Language: "python",
	}
	got, ok := r.pick(NewSpecs().For(fr.Language), fr, buildImportSet(NewSpecs().For(fr.Language), nil),
		registry.Reference{Name: "helper"},
		[]registry.SymbolIndexEntry{entry(1, "helper", "app/utils.py")})
	if !ok {
		t.Fatal("resolved nothing")
	}
	if got.confidence == "high" {
		t.Errorf("adjacency without an import reported as high confidence (%s)", got.reason)
	}
}

// TestRelativeImportOfAFileIsMatched pins why TypeScript improved.
//
// `import { X } from './sibling'` names a FILE. Matching only directory
// segments missed every relative import, so the import tier found nothing and
// proximity answered instead.
func TestRelativeImportOfAFileIsMatched(t *testing.T) {
	set := buildImportSet(NewSpecs().For("typescript"), []registry.Import{{Path: "./sibling"}})
	if _, ok := set.covers("packages/core/sibling.ts"); !ok {
		t.Error("a relative import of a sibling file is not matched")
	}
	// And Go is not loosened by it. There an import path names a DIRECTORY, so
	// the file's own name must not be consulted at all: matching it would make
	// `import ".../config"` claim every config.go in the repository, which cost
	// prometheus 0.4 points of precision when it was enabled for every
	// language.
	//
	// A6 closed the looseness this comment used to record as a known limit.
	// Segment matching was a bag of words: any segment of the file's path equal
	// to any segment of any import, so `internal/server/config.go` matched an
	// import of `cortex/internal/config` on the shared `internal` segment.
	// Matching is now anchored at the END of the path, which is what an import
	// path asserts. See importSet.covers.
	goSet := buildImportSet(NewSpecs().For("go"), []registry.Import{{Path: "github.com/satyendrakumarsingh/repocortex/internal/config"}})
	if _, ok := goSet.covers("cmd/server/config.go"); ok {
		t.Error("a Go import matched a file solely because it shares the import's " +
			"last segment as a FILE name")
	}
	if _, ok := goSet.covers("internal/config/load.go"); !ok {
		t.Error("a Go import no longer matches a file in the imported directory")
	}
	if _, ok := goSet.covers("internal/server/config.go"); ok {
		t.Error("a Go import matched a file that merely shares the mid-path segment " +
			"`internal`; an import path names what its TRAILING segments name")
	}
	// Anchoring must not cost the prefix tolerance the two alphabets require:
	// a Java import names a package that a source tree buries under src/main/java.
	javaSet := buildImportSet(NewSpecs().For("java"), []registry.Import{{Path: "com.pay.repository"}})
	if _, ok := javaSet.covers("src/main/java/com/pay/repository/OrderRepo.java"); !ok {
		t.Error("anchoring at the end broke the differing-prefix case an import " +
			"path and a source path always have")
	}
}

// TestLongerImportSuffixWins pins the preference A6 added.
//
// Two imports can both cover a candidate. The one that shares more of the
// trailing path is the one that named it, and treating the two as equally good
// reduced a decidable case to "several matches, preferred deterministically".
func TestLongerImportSuffixWins(t *testing.T) {
	r := New(nil, Options{})
	cands := []registry.SymbolIndexEntry{
		entry(1, "New", "internal/store/new.go"),
		entry(2, "New", "internal/pay/store/new.go"),
	}
	fr := registry.FileRefs{
		FileID: 99, Path: "cmd/app/main.go", Language: "go",
		Imports: []registry.Import{
			{Path: "github.com/satyendrakumarsingh/repocortex/internal/store"},
			{Path: "github.com/satyendrakumarsingh/repocortex/internal/pay/store"},
		},
	}
	got, ok := r.pick(NewSpecs().For(fr.Language), fr, buildImportSet(NewSpecs().For(fr.Language), fr.Imports),
		registry.Reference{Name: "New"}, cands)
	if !ok {
		t.Fatal("resolved nothing")
	}
	if got.entry.ID != 2 {
		t.Errorf("bound to %s (%s); internal/pay/store matches three trailing "+
			"segments and internal/store only one", got.entry.FilePath, got.reason)
	}
	if got.confidence != model.ConfidenceHigh {
		t.Errorf("confidence = %s, want high — the deeper suffix is a unique match",
			got.confidence)
	}
}

// TestCapDropsAreCounted pins A8: a reference abandoned because its name has
// more definitions than the cap will weigh must be countable, not merely folded
// into Ambiguous.
//
// The two failures Ambiguous conflates want different fixes. "Two candidates
// and no evidence" is a disambiguation problem; "this name has 200 definitions"
// was never weighable at all. A single number cannot tell a user which one
// their repository has, and `cortex accuracy` now publishes a recall figure
// that a cap drop is a named component of.
func TestCapDropsAreCounted(t *testing.T) {
	// One name, more definitions than any spec will weigh, and a Go receiver —
	// which cannot rescue it, because ReceiverDenotesType is false for Go.
	cands := make([]registry.SymbolIndexEntry, 0, MaxCandidates+2)
	symbols := map[string][]registry.SymbolIndexEntry{}
	for i := 0; i <= MaxCandidates+1; i++ {
		cands = append(cands, entry(int64(i+1), "Save", "internal/pkg"+string(rune('a'+i))+"/store.go"))
	}
	symbols["Save"] = cands

	r := New(symbols, Options{})
	var stats Stats
	fr := registry.FileRefs{
		FileID: 99, Path: "internal/app/service.go", Language: "go",
		Refs: []registry.Reference{{ID: 1, Name: "Save", Kind: "call", Line: 3, Receiver: "repo"}},
	}
	if _, _, err := r.ResolveFile(t.Context(), fr, &stats); err != nil {
		t.Fatalf("ResolveFile: %v", err)
	}
	if stats.Ambiguous != 1 {
		t.Fatalf("Ambiguous = %d, want 1", stats.Ambiguous)
	}
	if stats.CapExceeded != 1 || stats.CapDropped != 1 {
		t.Errorf("CapExceeded/CapDropped = %d/%d, want 1/1 — the cap drop is invisible inside Ambiguous",
			stats.CapExceeded, stats.CapDropped)
	}
}

// TestOrdinaryAmbiguityIsNotCountedAsACapDrop is the other half of A8: a
// reference with a handful of candidates and nothing to choose between them is
// genuinely ambiguous and must NOT be attributed to the cap, or the counter
// would just restate Ambiguous under a new name.
func TestOrdinaryAmbiguityIsNotCountedAsACapDrop(t *testing.T) {
	symbols := map[string][]registry.SymbolIndexEntry{"Save": {
		entry(1, "Save", "internal/a/store.go"),
		entry(2, "Save", "internal/b/store.go"),
	}}
	r := New(symbols, Options{})
	var stats Stats
	fr := registry.FileRefs{
		FileID: 99, Path: "internal/app/service.go", Language: "go",
		Refs: []registry.Reference{{ID: 1, Name: "Save", Kind: "call", Line: 3}},
	}
	if _, _, err := r.ResolveFile(t.Context(), fr, &stats); err != nil {
		t.Fatalf("ResolveFile: %v", err)
	}
	if stats.Ambiguous != 1 {
		t.Fatalf("Ambiguous = %d, want 1", stats.Ambiguous)
	}
	if stats.CapExceeded != 0 || stats.CapDropped != 0 {
		t.Errorf("CapExceeded/CapDropped = %d/%d, want 0/0 — two candidates is not a cap drop",
			stats.CapExceeded, stats.CapDropped)
	}
}
