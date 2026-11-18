package registry

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// oldFileCallerPaths is the query EnsureFileCallers was before it became one
// pass: the paths of the files with a pending reference that no definition
// covers. Kept here as the oracle.
func oldFileCallerPaths(t testing.TB, ctx context.Context, reg *Registry, runID int64) []string {
	t.Helper()
	rows, err := reg.db.QueryContext(ctx, `
		SELECT f.path FROM files f
		 WHERE f.index_run_id = ?
		   AND EXISTS (
		       SELECT 1 FROM symbol_references sr
		        WHERE sr.file_id = f.id AND sr.resolution = 'pending' AND sr.start_line > 0
		          AND NOT EXISTS (
		              SELECT 1 FROM symbols s INDEXED BY idx_symbols_file
		               WHERE s.file_id = f.id AND s.kind <> 'file'
		                 AND s.start_line <= sr.start_line
		                 AND MAX(s.end_line, s.start_line) >= sr.start_line))
		 ORDER BY f.path`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func randomFileCallerRun(t testing.TB, files int, seed int64) (*Registry, int64) {
	return sizedFileCallerRun(t, files, 12, 15, seed)
}

func sizedFileCallerRun(t testing.TB, files, maxSyms, maxRefs int, seed int64) (*Registry, int64) {
	return tiledFileCallerRun(t, files, maxSyms, maxRefs, seed, false)
}

// tiledFileCallerRun with covered set tiles each file with definitions and
// puts every reference inside one, as in a Perl file of subs: the case where
// no reference is outside, so nothing short-circuits.
func tiledFileCallerRun(t testing.TB, files, maxSyms, maxRefs int, seed int64, covered bool) (*Registry, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := New(db)
	run, err := reg.BeginRun(ctx, "auto", "fc")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("d%d/f%d.pl", i%7, i)
		fid, err := w.AddFile(ctx, &model.File{Path: path, Language: "perl", ContentHash: fmt.Sprintf("h%d", i), ParseStatus: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		nSyms := rng.Intn(maxSyms) + 1
		for s := 0; s < nSyms; s++ {
			start := 1 + rng.Intn(maxSyms*10)
			end := 0 // unknown span: covers its own line only
			switch {
			case covered:
				start, end = 1+s*10, 9+s*10
			case rng.Intn(3) == 0:
				end = start + rng.Intn(30)
			case rng.Intn(2) == 0:
				end = start + rng.Intn(3)
			}
			if _, err := w.AddSymbol(ctx, &model.Symbol{
				FileID: fid, Kind: model.KindFunction, Name: fmt.Sprintf("s%d", s), FQN: fmt.Sprintf("%s:s%d_%d", path, s, start),
				Range:      model.Range{Start: model.Position{Line: start}, End: model.Position{Line: end}},
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
			}); err != nil {
				t.Fatal(err)
			}
		}
		refLine := func() int {
			if covered {
				return 1 + rng.Intn(nSyms)*10 + rng.Intn(9)
			}
			return rng.Intn(maxSyms * 12)
		}
		for r := 0; r < rng.Intn(maxRefs); r++ {
			if err := w.AddReference(ctx, fid, Reference{
				Name: "x", Kind: "reference.call", Line: refLine(), ArgCount: model.ArityUnknown,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return reg, run.ID
}

// TestEnsureFileCallersMatchesTheQuery: the single pass creates a caller for
// exactly the files the nested query selected, over random corpora with nested,
// overlapping and unknown-span definitions and references on lines 0 and 1.
func TestEnsureFileCallersMatchesTheQuery(t *testing.T) {
	ctx := context.Background()
	for seed := int64(1); seed <= 5; seed++ {
		reg, runID := randomFileCallerRun(t, 300, seed)
		want := oldFileCallerPaths(t, ctx, reg, runID)
		n, err := reg.EnsureFileCallers(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := reg.FileCallerIDs(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for p := range ids {
			got = append(got, p)
		}
		sort.Strings(got)
		if n != len(want) || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("seed %d: created %d callers %v, the query selected %d %v", seed, n, got, len(want), want)
		}
		if again, err := reg.EnsureFileCallers(ctx, runID); err != nil || again != 0 {
			t.Fatalf("seed %d: a second call created %d (err %v), want 0", seed, again, err)
		}
	}
}

func BenchmarkEnsureFileCallers(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		reg, runID := tiledFileCallerRun(b, 300, 600, 1600, 7, true)
		b.StartTimer()
		if _, err := reg.EnsureFileCallers(ctx, runID); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFileCallerQueryBefore times the nested query the pass replaced, for
// comparison with BenchmarkEnsureFileCallers (which also does the inserts).
func BenchmarkFileCallerQueryBefore(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		reg, runID := tiledFileCallerRun(b, 300, 600, 1600, 7, true)
		b.StartTimer()
		oldFileCallerPaths(b, ctx, reg, runID)
	}
}
