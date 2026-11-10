package infer

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// arityEntry is entry() with a declared parameter count.
func arityEntry(id int64, name, file string, params model.Arity) registry.SymbolIndexEntry {
	e := entry(id, name, file)
	e.ParamCount = params
	return e
}

// twoImportedSaves is the shape A4 exists for: one name, two definitions, both
// reachable through the same import, distinguishable only by how many arguments
// the call passes. Without arity, pickImported sorts and takes the first.
func twoImportedSaves() ([]registry.SymbolIndexEntry, registry.FileRefs) {
	cands := []registry.SymbolIndexEntry{
		arityEntry(1, "Save", "internal/store/a_wide.go", model.KnownArity(3)),
		arityEntry(2, "Save", "internal/store/b_narrow.go", model.KnownArity(1)),
	}
	fr := registry.FileRefs{
		FileID: 99, Path: "internal/app/handler.go", Language: "go",
		Imports: []registry.Import{{Path: "example.com/x/internal/store"}},
	}
	return cands, fr
}

func pickWith(t *testing.T, fr registry.FileRefs, ref registry.Reference, cands []registry.SymbolIndexEntry) (candidate, bool) {
	t.Helper()
	r := New(nil, Options{})
	spec := r.specs.For(fr.Language)
	return r.pick(spec, fr, buildImportSet(spec, fr.Imports), ref, cands)
}

// TestArityPromotesWithinTheMediumTier is A4's whole claim.
//
// Two definitions of Save live in one imported package, so pickImported has
// several equally-deep matches and reports ConfidenceMedium with a
// deterministic preference. `Save(id)` passes one argument, which names one of
// them.
func TestArityPromotesWithinTheMediumTier(t *testing.T) {
	cands, fr := twoImportedSaves()
	ref := registry.Reference{Name: "Save", ArgCount: model.KnownArity(1)}

	got, ok := pickWith(t, fr, ref, cands)
	if !ok {
		t.Fatal("resolved nothing")
	}
	if got.entry.ID != 2 {
		t.Errorf("bound to %s (%s); the call passes 1 argument and only that "+
			"candidate declares 1 parameter", got.entry.FilePath, got.reason)
	}
	if got.confidence != model.ConfidenceMedium {
		t.Errorf("confidence %s, want medium: A4 reorders inside the tier that "+
			"was already going to answer, it does not promote a tier",
			got.confidence)
	}
}

// TestArityIsNeverAFilter is the property that separates A4 from A19.
//
// A19 removed candidates and cost 2.4pp of `high` — not by emitting wrong edges
// directly, but by shrinking candidate sets until references that had been
// honestly AMBIGUOUS tipped over the disambiguation thresholds and resolved
// wrongly. An arity rule written as "drop the candidates whose count differs"
// would be that change again. So when NO candidate matches the call's arity,
// the outcome must be exactly what it was without arity: the same tier, the
// same deterministic preference, nothing removed.
func TestArityIsNeverAFilter(t *testing.T) {
	cands, fr := twoImportedSaves()

	// A call passing 7 arguments contradicts both candidates. That is far more
	// likely to mean the parser miscounted, or the real target is not indexed,
	// than that either of these is right — and it is not grounds for dropping
	// anything.
	withArity, ok := pickWith(t, fr, registry.Reference{Name: "Save", ArgCount: model.KnownArity(7)}, cands)
	if !ok {
		t.Fatal("an arity that matches nothing must not stop resolution")
	}
	without, ok := pickWith(t, fr, registry.Reference{Name: "Save"}, cands)
	if !ok {
		t.Fatal("resolved nothing without arity")
	}
	if withArity.entry.ID != without.entry.ID || withArity.confidence != without.confidence {
		t.Errorf("an unmatched arity changed the answer: %d/%s vs %d/%s",
			withArity.entry.ID, withArity.confidence, without.entry.ID, without.confidence)
	}
}

// TestUnknownArityIsNotAMismatch pins the reason model.Arity exists.
//
// A count of zero and no count must not share a representation. The languages
// where they would be confused — a Python method's implicit `self`, a
// TypeScript optional parameter, a variadic signature — are exactly the
// languages where an arity mismatch is a parser artefact rather than evidence.
// Unknown on either side must be inert.
func TestUnknownArityIsNotAMismatch(t *testing.T) {
	// Candidate 2 declares one parameter; candidate 1's count is unknown.
	cands := []registry.SymbolIndexEntry{
		arityEntry(1, "Save", "internal/store/a_wide.go", model.ArityUnknown),
		arityEntry(2, "Save", "internal/store/b_narrow.go", model.KnownArity(1)),
	}
	_, fr := twoImportedSaves()

	// An unknown ARGUMENT count must express no preference at all, so the
	// deterministic order still decides.
	got, ok := pickWith(t, fr, registry.Reference{Name: "Save"}, cands)
	if !ok {
		t.Fatal("resolved nothing")
	}
	sorted := append([]registry.SymbolIndexEntry(nil), cands...)
	sortCandidates(sorted)
	if got.entry.ID != sorted[0].ID {
		t.Errorf("unknown argument count changed the deterministic order: got %d, want %d",
			got.entry.ID, sorted[0].ID)
	}

	// An unknown PARAMETER count must not be read as zero: a call passing no
	// arguments must not select candidate 1 just because nothing was recorded
	// for it.
	got, ok = pickWith(t, fr, registry.Reference{Name: "Save", ArgCount: model.KnownArity(0)}, cands)
	if !ok {
		t.Fatal("resolved nothing")
	}
	if got.entry.ID == 1 && got.reason != sortedReason {
		t.Errorf("an unknown parameter count was treated as zero: bound to %d (%s)",
			got.entry.ID, got.reason)
	}
}

// sortedReason is what pickImported says when nothing but the deterministic
// order decided. Named so the assertion above reads as the claim it is making.
const sortedReason = "multiple matches among imports"

// TestArityDoesNotFireWhenSeveralCandidatesShareIt.
//
// A preference is only worth expressing when it points somewhere. Two
// candidates of the same arity are exactly as ambiguous as they were, so the
// existing deterministic order must still decide and the reason must still say
// it was a preference.
func TestArityDoesNotFireWhenSeveralCandidatesShareIt(t *testing.T) {
	cands := []registry.SymbolIndexEntry{
		arityEntry(1, "Save", "internal/store/a.go", model.KnownArity(1)),
		arityEntry(2, "Save", "internal/store/b.go", model.KnownArity(1)),
	}
	_, fr := twoImportedSaves()
	got, ok := pickWith(t, fr, registry.Reference{Name: "Save", ArgCount: model.KnownArity(1)}, cands)
	if !ok {
		t.Fatal("resolved nothing")
	}
	if got.reason != sortedReason {
		t.Errorf("reason %q: two candidates of the same arity are still ambiguous, "+
			"so the deterministic preference must be what answered", got.reason)
	}
}

// TestArityCannotReachTheHighTier is the safety property that made A4 worth
// attempting at all, and it is structural rather than statistical.
//
// `high` measures 97.8% on this repository and A3 was reverted for moving it by
// 0.3pp. preferByArity is called from three places, every one of them a branch
// that has ALREADY chosen a candidate and is about to report ConfidenceMedium.
// A single candidate in an imported package returns High before arity is ever
// consulted, and it must keep doing so even when its arity disagrees with the
// call.
func TestArityCannotReachTheHighTier(t *testing.T) {
	cands := []registry.SymbolIndexEntry{
		arityEntry(1, "Save", "internal/store/a.go", model.KnownArity(3)),
	}
	fr := registry.FileRefs{
		FileID: 99, Path: "internal/app/handler.go", Language: "go",
		Imports: []registry.Import{{Path: "example.com/x/internal/store"}},
	}
	got, ok := pickWith(t, fr, registry.Reference{Name: "Save", ArgCount: model.KnownArity(1)}, cands)
	if !ok {
		t.Fatal("a lone imported candidate must still resolve")
	}
	if got.confidence != model.ConfidenceHigh {
		t.Errorf("confidence %s, want high: an arity disagreement is not grounds "+
			"for demoting an otherwise unique match", got.confidence)
	}
}

// TestPreferByArityRequiresExactlyOne exercises the helper directly, including
// the empty and no-match cases the tiers above cannot easily reach.
func TestPreferByArityRequiresExactlyOne(t *testing.T) {
	cands := []registry.SymbolIndexEntry{
		arityEntry(1, "f", "a.go", model.KnownArity(0)),
		arityEntry(2, "f", "b.go", model.KnownArity(2)),
		arityEntry(3, "f", "c.go", model.ArityUnknown),
	}
	for _, tc := range []struct {
		name   string
		ref    registry.Reference
		wantID int64
		wantOK bool
	}{
		{"unknown argument count", registry.Reference{}, 0, false},
		{"zero arguments selects the niladic one", registry.Reference{ArgCount: model.KnownArity(0)}, 1, true},
		{"two arguments selects the binary one", registry.Reference{ArgCount: model.KnownArity(2)}, 2, true},
		{"no candidate of that arity", registry.Reference{ArgCount: model.KnownArity(5)}, 0, false},
	} {
		got, ok := preferByArity(tc.ref, cands)
		if ok != tc.wantOK {
			t.Errorf("%s: ok=%v, want %v", tc.name, ok, tc.wantOK)
			continue
		}
		if ok && got.ID != tc.wantID {
			t.Errorf("%s: selected %d, want %d", tc.name, got.ID, tc.wantID)
		}
	}
}
