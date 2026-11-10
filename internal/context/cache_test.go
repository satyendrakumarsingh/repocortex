package context

import (
	stdctx "context"
	"sync"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestCatalogueCacheInvalidatesOnIncrementalUpdate is the test that makes the
// cache safe to have at all.
//
// A committed run is NOT immutable: incremental indexing mutates it in place,
// so caching on the run id alone would serve pre-edit answers for as long as
// the process lived — the precise staleness RepoCortex exists to remove. The
// fingerprint is what prevents that, and this proves it works against a REAL
// in-place update rather than a simulated one.
func TestCatalogueCacheInvalidatesOnIncrementalUpdate(t *testing.T) {
	ctx := stdctx.Background()
	reg, _, runID := paymentFixture(t)
	testCache := &catalogueCache{}

	before, err := testCache.get(ctx, reg, runID)
	if err != nil {
		t.Fatal(err)
	}
	nBefore := len(before.symbols)

	// A second call with nothing changed must reuse.
	again, err := testCache.get(ctx, reg, runID)
	if err != nil {
		t.Fatal(err)
	}
	if again != before {
		t.Error("unchanged run rebuilt the catalogue; the cache is not working")
	}
	if hits, _ := testCache.stats(); hits == 0 {
		t.Error("no cache hit recorded for an unchanged run")
	}

	// Now mutate the COMMITTED run in place, exactly as `cortex watch` does.
	newFile := model.File{Path: "svc/added.go", Language: "go", ContentHash: "hnew", ParseStatus: "ok"}
	syms := []model.Symbol{{
		ID: -1, Kind: model.KindFunction, Name: "BrandNewSymbol",
		FQN: "svc/added.go:BrandNewSymbol", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 5}},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}}
	if err := reg.ReplaceFile(ctx, runID, &newFile, syms, nil,
		registry.AllowCommittedRun()); err != nil {
		t.Fatalf("in-place update: %v", err)
	}

	after, err := testCache.get(ctx, reg, runID)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("STALE: the cache returned the pre-edit catalogue after an in-place update")
	}
	if len(after.symbols) <= nBefore {
		t.Errorf("rebuilt catalogue has %d symbols, want more than %d", len(after.symbols), nBefore)
	}

	var found bool
	for _, s := range after.symbols {
		if s.Name == "BrandNewSymbol" {
			found = true
		}
	}
	if !found {
		t.Error("the newly added symbol is absent from the rebuilt catalogue")
	}
}

// TestCatalogueCacheDetectsDeletion covers the other direction: a delete leaves
// the maximum id untouched, so only the row count reveals it.
func TestCatalogueCacheDetectsDeletion(t *testing.T) {
	ctx := stdctx.Background()
	reg, _, runID := paymentFixture(t)
	testCache := &catalogueCache{}

	before, err := testCache.get(ctx, reg, runID)
	if err != nil {
		t.Fatal(err)
	}

	files, err := reg.Files(ctx, runID)
	if err != nil || len(files) == 0 {
		t.Skip("harness has no files to delete")
	}
	if err := reg.DeleteFile(ctx, runID, files[0].Path, registry.AllowCommittedRun()); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, err := testCache.get(ctx, reg, runID)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("STALE: a deleted file did not invalidate the cache; only the row count reveals a delete")
	}
	if len(after.files) >= len(before.files) {
		t.Errorf("file count did not fall: %d then %d", len(before.files), len(after.files))
	}
}

// TestAssembleIsCorrectAcrossCacheStates guards the property that actually
// matters to a user: the ANSWER must not depend on whether the catalogue
// happened to be cached.
func TestAssembleIsCorrectAcrossCacheStates(t *testing.T) {
	ctx := stdctx.Background()
	reg, g, runID := paymentFixture(t)
	const task = "Add idempotency to payment callback processing"

	// ONE assembler, so the second call is genuinely warm. Two assemblers
	// would make this test pass while proving nothing: both calls would be
	// cold, and a broken cache would be indistinguishable from a working one.
	asm := NewAssembler()
	cold, err := asm.Assemble(ctx, reg, g, runID, Request{Task: task})
	if err != nil {
		t.Fatal(err)
	}
	warm, err := asm.Assemble(ctx, reg, g, runID, Request{Task: task})
	if err != nil {
		t.Fatal(err)
	}

	if len(cold.Items) != len(warm.Items) {
		t.Fatalf("cold produced %d items, warm %d", len(cold.Items), len(warm.Items))
	}
	for i := range cold.Items {
		if cold.Items[i].Name != warm.Items[i].Name {
			t.Errorf("item %d: cold %q, warm %q", i, cold.Items[i].Name, warm.Items[i].Name)
		}
		if cold.Items[i].Why != warm.Items[i].Why {
			t.Errorf("item %d %q: explanation differs between cold and warm", i, cold.Items[i].Name)
		}
	}
	if cold.TokenEstimate != warm.TokenEstimate {
		t.Errorf("token estimate differs: cold %d, warm %d", cold.TokenEstimate, warm.TokenEstimate)
	}
}

// TestCatalogueCacheIsRaceFree exercises the shared-immutable assumption. A
// cached catalogue is read by many in-flight MCP requests at once; if anything
// wrote to it, -race would find it here.
func TestCatalogueCacheIsRaceFree(t *testing.T) {
	ctx := stdctx.Background()
	reg, g, runID := paymentFixture(t)

	// One assembler shared by every goroutine: the cached catalogue is the
	// thing under test, and giving each goroutine its own would remove the
	// sharing this test exists to exercise.
	asm := NewAssembler()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pack, err := asm.Assemble(ctx, reg, g, runID,
				Request{Task: "Add idempotency to payment callback processing"})
			if err != nil {
				errs <- err
				return
			}
			if len(pack.Items) == 0 {
				errs <- errEmpty
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent assemble: %v", err)
		}
	}

	// One build for many concurrent first-requests, not thirty-two. Without
	// holding the lock across the load they would all miss and all build.
	_, misses := asm.cache.stats()
	if misses > 2 {
		t.Errorf("%d cache misses across 32 concurrent requests; the loader is stampeding", misses)
	}
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errEmpty = constErr("assemble returned no items")

// TestCatalogueCacheDoesNotCrossDatabases pins a real bug that shipped for one
// commit.
//
// The fingerprint describes CONTENT within a database; it does not identify the
// database. Two projects can each hold run 1 with identical counts — that is
// the normal case for any two repositories of similar shape — so a
// content-only key served one project's catalogue for the other project's
// query. It broke two tests within minutes of being introduced, and in
// production it would silently mix two codebases.
func TestCatalogueCacheDoesNotCrossDatabases(t *testing.T) {
	ctx := stdctx.Background()
	testCache := &catalogueCache{}

	// Two INDEPENDENT stores over the same fixture: same run id, same counts,
	// same fingerprint — but different databases and different row ids.
	regA, _, runA := paymentFixture(t)
	regB, _, runB := paymentFixture(t)

	fpA, err := regA.CatalogueFingerprint(ctx, runA)
	if err != nil {
		t.Fatal(err)
	}
	fpB, err := regB.CatalogueFingerprint(ctx, runB)
	if err != nil {
		t.Fatal(err)
	}
	if fpA != fpB {
		t.Skipf("fixtures produced different fingerprints (%+v vs %+v); "+
			"this test needs a collision to be meaningful", fpA, fpB)
	}
	t.Logf("both databases fingerprint identically: %+v", fpA)

	catA, err := testCache.get(ctx, regA, runA)
	if err != nil {
		t.Fatal(err)
	}
	catB, err := testCache.get(ctx, regB, runB)
	if err != nil {
		t.Fatal(err)
	}

	if catA == catB {
		t.Fatal("CROSS-DATABASE LEAK: two separate registries with colliding " +
			"fingerprints were served the same catalogue; one project would see " +
			"another project's code")
	}
}
