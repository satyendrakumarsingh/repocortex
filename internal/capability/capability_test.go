package capability

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// --- fakes -------------------------------------------------------------------
//
// These exist to prove the Deps seam actually works with fakes, which is the
// reason it is two interfaces rather than the concrete types. The behavioural
// assertions about traversal live in the impact and tests packages, where they
// run against the real graph — a fake would only prove the fake agrees with the
// assertion.

type fakeRegistry struct {
	mu sync.Mutex

	symbols map[string][]registry.SymbolIndexEntry
	stats   registry.ResolutionStats
	loads   int
	err     error
}

func (f *fakeRegistry) FindSymbolsByName(_ context.Context, _ int64, name, _ string, _ int) ([]registry.SymbolHit, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []registry.SymbolHit
	for _, e := range f.symbols[name] {
		out = append(out, registry.SymbolHit{
			ID: e.ID, Name: e.Name, FQN: e.FQN, Kind: e.Kind,
			FilePath: e.FilePath, Exported: e.Exported,
			Confidence: string(model.ConfidenceHigh), Source: string(model.SourceSourceCode),
		})
	}
	return out, nil
}

func (f *fakeRegistry) LoadSymbolIndex(context.Context, int64) (map[string][]registry.SymbolIndexEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	f.loads++
	f.mu.Unlock()
	return f.symbols, nil
}

func (f *fakeRegistry) ResolutionStats(context.Context, int64) (registry.ResolutionStats, error) {
	return f.stats, f.err
}

func (f *fakeRegistry) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

type fakeGraph struct{}

func (fakeGraph) Traverse(context.Context, graph.Query) (graph.Result, error) {
	return graph.Result{}, nil
}
func (fakeGraph) Neighbours(context.Context, int64, graph.Direction, []model.EdgeKind, model.Confidence) ([]graph.Neighbour, error) {
	return nil, nil
}

func entry(id int64, name, fqn, filePath string) registry.SymbolIndexEntry {
	return registry.SymbolIndexEntry{ID: id, Name: name, FQN: fqn, Kind: "function", FilePath: filePath}
}

func fakeDeps() (Deps, *fakeRegistry) {
	reg := &fakeRegistry{symbols: map[string][]registry.SymbolIndexEntry{
		"updateStatus": {entry(1, "updateStatus", "svc/status.go#updateStatus", "svc/status.go")},
		"Close":        {entry(2, "Close", "a#Close", "a.go"), entry(3, "Close", "b#Close", "b.go")},
	}}
	return NewDeps(reg, fakeGraph{}), reg
}

// --- Deps --------------------------------------------------------------------

func TestDepsWorkWithFakes(t *testing.T) {
	t.Parallel()
	deps, _ := fakeDeps()
	tgt, err := ResolveTarget(context.Background(), deps, 1, "updateStatus")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if tgt.SymbolID != 1 || tgt.FilePath != "svc/status.go" {
		t.Fatalf("resolved %+v", tgt)
	}
}

func TestDepsValidateRejectsMissingCollaborators(t *testing.T) {
	t.Parallel()
	if err := (Deps{}).Validate(); err == nil {
		t.Fatal("zero Deps validated")
	}
	if err := (Deps{Registry: &fakeRegistry{}}).Validate(); err == nil {
		t.Fatal("Deps with no Graph validated")
	}
}

func TestSymbolIndexIsLoadedOncePerRun(t *testing.T) {
	t.Parallel()
	deps, reg := fakeDeps()
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := deps.SymbolIndex(ctx, 7); err != nil {
			t.Fatalf("SymbolIndex: %v", err)
		}
	}
	if got := reg.loadCount(); got != 1 {
		t.Fatalf("loaded the symbol table %d times, want 1", got)
	}
	// A new run must invalidate: the old run's rows are deleted on commit, so
	// serving its names would be serving garbage.
	if _, err := deps.SymbolIndex(ctx, 8); err != nil {
		t.Fatalf("SymbolIndex: %v", err)
	}
	if got := reg.loadCount(); got != 2 {
		t.Fatalf("a new run id did not invalidate the cache (loads=%d)", got)
	}
}

func TestSymbolIndexCacheIsRaceFree(t *testing.T) {
	t.Parallel()
	deps, reg := fakeDeps()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := deps.SymbolIndex(context.Background(), 3); err != nil {
				t.Errorf("SymbolIndex: %v", err)
			}
		}()
	}
	wg.Wait()
	// One load, not sixteen: a cold cache under concurrent MCP calls must not
	// build sixteen 100k-entry indexes.
	if got := reg.loadCount(); got != 1 {
		t.Fatalf("concurrent cold access caused %d loads, want 1", got)
	}
}

func TestZeroDepsStillWorksWithoutCaching(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistry{symbols: map[string][]registry.SymbolIndexEntry{}}
	deps := Deps{Registry: reg, Graph: fakeGraph{}}
	for i := 0; i < 3; i++ {
		if _, err := deps.SymbolIndex(context.Background(), 1); err != nil {
			t.Fatalf("SymbolIndex: %v", err)
		}
	}
	if got := reg.loadCount(); got != 3 {
		t.Fatalf("uncached Deps loaded %d times, want 3", got)
	}
}

func TestResolveTargetAmbiguityIsNotGuessed(t *testing.T) {
	t.Parallel()
	deps, _ := fakeDeps()
	_, err := ResolveTarget(context.Background(), deps, 1, "Close")
	var amb *AmbiguousTargetError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want AmbiguousTargetError", err)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("got %d candidates", len(amb.Candidates))
	}
	if amb.Error() == "" {
		t.Fatal("ambiguity error has no message")
	}
}

func TestResolveTargetFallsBackToFQN(t *testing.T) {
	t.Parallel()
	deps, _ := fakeDeps()
	tgt, err := ResolveTarget(context.Background(), deps, 1, "svc/status.go#updateStatus")
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if tgt.SymbolID != 1 {
		t.Fatalf("resolved to %d, want 1", tgt.SymbolID)
	}
}

func TestResolveTargetEmptyAndMissing(t *testing.T) {
	t.Parallel()
	deps, _ := fakeDeps()
	for _, q := range []string{"", "   ", "nope"} {
		if _, err := ResolveTarget(context.Background(), deps, 1, q); !errors.Is(err, ErrTargetNotFound) {
			t.Errorf("ResolveTarget(%q) err = %v, want ErrTargetNotFound", q, err)
		}
	}
}

// --- SymbolIndex -------------------------------------------------------------

func TestSymbolIndexPrefixSearchRespectsTheAcceptFilter(t *testing.T) {
	t.Parallel()
	idx := buildSymbolIndex(1, map[string][]registry.SymbolIndexEntry{
		"TestUpdate":        {entry(1, "TestUpdate", "a#TestUpdate", "a_test.go")},
		"TestUpdate_NotOK":  {entry(2, "TestUpdate_NotOK", "a#2", "a_test.go")},
		"TestUpdateStatus":  {entry(3, "TestUpdateStatus", "a#3", "a_test.go")},
		"TestUpdate2":       {entry(4, "TestUpdate2", "a#4", "a_test.go")},
		"UnrelatedFunction": {entry(5, "UnrelatedFunction", "a#5", "a.go")},
	})

	got := map[string]bool{}
	for _, s := range idx.WithPrefix("TestUpdate", ConventionalTestSuffix) {
		got[s.Name] = true
	}
	for _, want := range []string{"TestUpdate", "TestUpdate_NotOK"} {
		if !got[want] {
			t.Errorf("%s not matched", want)
		}
	}
	if got["TestUpdateStatus"] {
		t.Error("TestUpdateStatus matched: an unconstrained prefix would let `Update` claim it")
	}
	if got["TestUpdate2"] {
		t.Error("TestUpdate2 matched: a trailing digit lets `Sha25` claim TestSha256")
	}
	if len(idx.WithPrefix("", ConventionalTestSuffix)) != 0 {
		t.Error("empty prefix matched something")
	}
}

func TestSymbolIndexLookups(t *testing.T) {
	t.Parallel()
	idx := buildSymbolIndex(9, map[string][]registry.SymbolIndexEntry{
		"a": {entry(1, "a", "pkg/a.go#a", "pkg/a.go")},
		"b": {entry(2, "b", "pkg/b.go#b", "pkg/b.go")},
		"c": {entry(3, "c", "other/c.go#c", "other/c.go")},
	})
	if idx.Len() != 3 || idx.RunID() != 9 {
		t.Fatalf("len=%d run=%d", idx.Len(), idx.RunID())
	}
	if s, ok := idx.Lookup(2); !ok || s.Name != "b" {
		t.Fatalf("Lookup(2) = %+v %v", s, ok)
	}
	if _, ok := idx.Lookup(404); ok {
		t.Fatal("Lookup of an unknown id succeeded")
	}
	if s, ok := idx.ByFQN("pkg/a.go#a"); !ok || s.ID != 1 {
		t.Fatalf("ByFQN = %+v %v", s, ok)
	}
	if got := idx.InDir("pkg"); len(got) != 2 {
		t.Fatalf("InDir(pkg) = %d symbols, want 2", len(got))
	}
	if got := idx.ByName("c"); len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("ByName(c) = %+v", got)
	}
}

// --- Completeness ------------------------------------------------------------

func TestCompletenessAlwaysCarriesBothMandatoryCaveats(t *testing.T) {
	t.Parallel()
	cases := []registry.ResolutionStats{
		{},
		{Total: 100, Resolved: 100},
		{Total: 100, Resolved: 10, Unresolved: 80, Ambiguous: 5, Pending: 5},
	}
	for _, stats := range cases {
		c := NewCompleteness(stats, 1234, false, "")
		if !c.LowerBound {
			t.Fatal("LowerBound false")
		}
		if !c.HasCaveat(LowerBoundCaveat) || !c.HasCaveat(DynamicDispatchCaveat) {
			t.Fatalf("mandatory caveat missing for %+v: %v", stats, c.Caveats)
		}
		if c.IndexedSymbols != 1234 {
			t.Fatalf("IndexedSymbols = %d", c.IndexedSymbols)
		}
	}
}

// TestZeroReferencesDoesNotReadAsFullyResolved: 0/0 must not render as a
// reassuring 100%.
func TestZeroReferencesDoesNotReadAsFullyResolved(t *testing.T) {
	t.Parallel()
	c := NewCompleteness(registry.ResolutionStats{}, 0, false, "")
	var explained bool
	for _, cav := range c.Caveats {
		if contains(cav, "no references at all") {
			explained = true
		}
		if contains(cav, "100.0%") {
			t.Fatalf("a 0/0 run rendered as a resolution percentage: %q", cav)
		}
	}
	if !explained {
		t.Fatalf("empty resolution stats not explained: %v", c.Caveats)
	}
}

func TestCompletenessRecordsTruncation(t *testing.T) {
	t.Parallel()
	c := NewCompleteness(registry.ResolutionStats{Total: 10, Resolved: 10}, 5, true, "node limit 25 reached")
	if !c.Truncated || c.TruncatedReason == "" {
		t.Fatal("truncation not recorded")
	}
	var found bool
	for _, cav := range c.Caveats {
		if contains(cav, "TRUNCATED") && contains(cav, "node limit 25 reached") {
			found = true
		}
	}
	if !found {
		t.Fatalf("truncation reason absent from caveats: %v", c.Caveats)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
