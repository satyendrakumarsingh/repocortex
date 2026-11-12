//go:build soak

package soak

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// soakReaders is how many query goroutines run against the index while it is
// being rewritten.
//
// Four rather than one because SQLite's contention behaviour is not linear:
// with a single reader the write lock is almost never actually contended, and
// the busy-timeout path this test exists to exercise never runs. Four is also
// under store.maxOpenConns (8), so a reader waiting on a lock is genuinely
// waiting on the lock rather than on the connection pool — which would be a
// different bug wearing the same symptom.
const soakReaders = 4

// TestIndexQueryConcurrencySoak keeps incremental indexing running while
// readers query continuously, for the configured duration.
//
// Three properties are under test, and each corresponds to a promise the
// registry package makes in its own doc comment:
//
//  1. Contention is absorbed, not surfaced. `database is locked` must never
//     reach a caller: the store sets a busy timeout and the writer keeps
//     transactions to a few milliseconds precisely so that it cannot.
//
//  2. No data races. Run with -race; a Registry is documented as safe to share
//     between goroutines and this is where that claim gets exercised for real.
//
//  3. Readers never see a torn file. registry/inplace.go gives up cross-file
//     consistency during a pass but keeps per-file atomicity, so a file must
//     never be observed half-replaced.
//
// The third is the interesting one, and it is checked without needing a
// consistent snapshot across two queries: every symbol name carries its file's
// version number, so a single Symbols() call that returns V7 and V8 symbols
// for one file has caught a non-atomic replacement red-handed. The
// never-mutated control files give the complementary check — collateral damage
// to a file nobody was writing.
func TestIndexQueryConcurrencySoak(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t, *flagFiles)

	var (
		c        counters
		reads    atomic.Int64
		searches atomic.Int64
		passes   atomic.Int64
	)

	var (
		errMu  sync.Mutex
		errs   []string
		errSet = map[string]bool{}
	)
	// Distinct failures only. A lock error under contention arrives thousands
	// of times a second once it arrives at all, and a test that prints all of
	// them buries its own diagnosis.
	report := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		errMu.Lock()
		if !errSet[msg] {
			errSet[msg] = true
			errs = append(errs, msg)
		}
		errMu.Unlock()
	}

	expected := expectedStableSymbols(r.stable)

	// Warm up before baselining, so the connection pool, the grammar and the
	// prepared statements are all already paid for.
	m := newMutator(r, &c, *flagSeed)
	warm := time.Now().Add(2 * time.Second)
	for time.Now().Before(warm) {
		m.step()
	}
	if _, err := r.reconcile(ctx); err != nil {
		t.Fatalf("warm-up reconcile: %v", err)
	}

	tr := newTracker("index/query concurrency")
	deadline := time.Now().Add(budget())

	var wg sync.WaitGroup

	// Writer: mutate, then bring the index back in line. Sequential by design
	// — SQLite is a single-writer store and the registry documents a Writer as
	// unsafe for concurrent use, so a second writer here would be testing a
	// configuration RepoCortex does not ship.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			for i := 0; i < 5; i++ {
				m.step()
			}
			stats, err := r.reconcile(ctx)
			if err != nil {
				if isLockError(err) {
					report("SQLite contention escaped the writer as an error: %v", err)
				} else {
					report("incremental pass failed: %v", err)
				}
				continue
			}
			c.modified.Add(stats.Modified)
			c.created.Add(stats.Added)
			c.deleted.Add(stats.Deleted)
			passes.Add(1)
		}
	}()

	// Readers.
	for i := 0; i < soakReaders; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				runID := r.runID

				syms, err := r.reg.Symbols(ctx, runID)
				if err != nil {
					if isLockError(err) {
						report("SQLite contention escaped a reader (Symbols): %v", err)
					} else {
						report("Symbols failed: %v", err)
					}
					continue
				}
				files, err := r.reg.Files(ctx, runID)
				if err != nil {
					if isLockError(err) {
						report("SQLite contention escaped a reader (Files): %v", err)
					} else {
						report("Files failed: %v", err)
					}
					continue
				}
				reads.Add(1)

				for _, problem := range checkNoTornFiles(files, syms) {
					report("%s", problem)
				}
				for _, problem := range checkStableFiles(files, syms, expected) {
					report("%s", problem)
				}

				// SearchFiles goes through files_fts_map and the committed-run
				// join. Its own invariant is that it can only ever surface a
				// file that belongs to the committed run, so a hit on a path
				// outside the fixture would mean FTS rows outliving their file.
				hits, err := r.reg.SearchFiles(ctx, "Soak", 25)
				if err != nil {
					if isLockError(err) {
						report("SQLite contention escaped a reader (SearchFiles): %v", err)
					} else {
						report("SearchFiles failed: %v", err)
					}
					continue
				}
				searches.Add(1)
				for _, h := range hits {
					if !strings.HasSuffix(h.Path, ".go") || strings.Contains(h.Path, ".repocortex/") {
						report("search returned a path that is not an indexed fixture file: %q", h.Path)
					}
				}
			}
		}(i)
	}

	interval := budget() / 12
	if interval < time.Second {
		interval = time.Second
	}
	for time.Now().Before(deadline) {
		time.Sleep(minDuration(interval, time.Until(deadline)))
		tr.sample()
	}
	wg.Wait()

	errMu.Lock()
	found := append([]string(nil), errs...)
	errMu.Unlock()
	for _, e := range found {
		t.Error(e)
	}

	// Referential damage is cumulative and invisible through the Go read API,
	// so it is checked once at the end, in SQL. A soak run is where a cascade
	// that quietly is not firing accumulates enough orphans to be undeniable.
	for _, p := range r.integrityProblems(ctx) {
		t.Errorf("referential integrity broken after sustained incremental updates: %s", p)
	}

	tr.notef("workload     %d incremental passes, %d index reads, %d searches",
		passes.Load(), reads.Load(), searches.Load())
	tr.notef("index churn  %d added, %d modified, %d deleted (as applied)",
		c.created.Load(), c.modified.Load(), c.deleted.Load())
	tr.notef("distinct problems observed: %d", len(found))
	tr.sample()
	tr.finish(t)
}

// expectedStableSymbols is the symbol name every control file must always
// carry. The control group is never written after the initial index, so
// anything missing or extra here is damage caused by a concurrent write to a
// different file.
func expectedStableSymbols(stable []string) map[string]string {
	out := make(map[string]string, len(stable))
	for _, rel := range stable {
		out[rel] = fmt.Sprintf("S%dV0", idOf(rel))
	}
	return out
}

// checkNoTornFiles looks for a file whose symbols disagree about which version
// of the file they came from.
//
// This needs only the single Symbols() query, which SQLite evaluates as one
// statement, so the observation is genuinely point-in-time. A file replaced
// non-atomically — old symbols deleted and new ones inserted outside one
// transaction — is the only way two versions can coexist under one file_id.
func checkNoTornFiles(files []model.File, syms []model.Symbol) []string {
	path := make(map[int64]string, len(files))
	for _, f := range files {
		path[f.ID] = f.Path
	}

	seen := map[int64]string{} // file id -> first version string observed
	var problems []string
	for _, s := range syms {
		v, ok := symbolVersion(s.Name)
		if !ok {
			continue
		}
		prev, had := seen[s.FileID]
		if !had {
			seen[s.FileID] = v
			continue
		}
		if prev != v {
			problems = append(problems, fmt.Sprintf(
				"torn read: file %q (id %d) exposed symbols from two versions at once (V%s and V%s); "+
					"a replacement was not atomic", path[s.FileID], s.FileID, prev, v))
		}
	}
	return problems
}

// checkStableFiles verifies the control group.
//
// A missing control file, or one whose defining symbol has disappeared, means
// a concurrent DeleteFile or ReplaceFile touched rows belonging to a file it
// was not asked to touch. Both queries are used here, but the control group is
// immutable for the whole run so there is no snapshot problem to reason about.
func checkStableFiles(files []model.File, syms []model.Symbol, expected map[string]string) []string {
	byID := make(map[int64]string, len(files))
	present := make(map[string]bool, len(files))
	for _, f := range files {
		byID[f.ID] = f.Path
		present[f.Path] = true
	}
	have := make(map[string]bool, len(syms))
	for _, s := range syms {
		if p, ok := byID[s.FileID]; ok {
			have[p+"\x00"+s.Name] = true
		}
	}

	var problems []string
	for rel, sym := range expected {
		if !present[rel] {
			problems = append(problems, fmt.Sprintf(
				"never-mutated file %q vanished from the index while other files were being rewritten", rel))
			continue
		}
		if !have[rel+"\x00"+sym] {
			problems = append(problems, fmt.Sprintf(
				"never-mutated file %q lost its symbol %q while other files were being rewritten", rel, sym))
		}
	}
	return problems
}

// symbolVersion pulls the fixture version out of a symbol name. Symbols that
// do not carry one (anything the parser derives that is not a fixture
// declaration) are ignored rather than guessed at.
func symbolVersion(name string) (string, bool) {
	mm := versionRe.FindStringSubmatch(name)
	if mm == nil {
		return "", false
	}
	if _, err := strconv.Atoi(mm[1]); err != nil {
		return "", false
	}
	return mm[1], true
}
