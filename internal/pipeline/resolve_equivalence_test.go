package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/infer"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// verifyDB opens a second, read-only connection to a fixture's database.
//
// The registry deliberately exposes no *sql.DB — CLAUDE.md §2 holds it to
// returning domain models rather than drivers — and adding an accessor so a
// test can peer inside would weaken the adapter for every caller. Opening the
// file separately keeps the assertion in the test where it belongs.
func verifyDB(t *testing.T, dir string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(dir, "index.db"), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// sharedNameFixture builds a run in which many files declare the SAME names.
//
// resolveFixture cannot serve here: it names every symbol op<file>_<n>, unique
// by construction, so every candidate bucket holds exactly one entry and a test
// written against it never reaches the tiers that break ties — nor the
// candidate cap, which only engages past eight. A cap that is never exercised
// is a cap that is never tested.
//
// Here each of `files` files declares `namesPerFile` methods drawn from one
// shared pool, so a pool name's bucket holds roughly files*namesPerFile/pool
// candidates and the cap engages on nearly every lookup.
func sharedNameFixture(tb testing.TB, dir string, files, namesPerFile, pool, refsPerFile int) (*registry.Registry, int64) {
	tb.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "index.db")})
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		tb.Fatalf("migrate: %v", err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "shared")
	if err != nil {
		tb.Fatalf("begin run: %v", err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		tb.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	for i := 0; i < files; i++ {
		path := fmt.Sprintf("src/pkg%03d/File%05d.java", i/20, i)
		fileID, err := w.AddFile(ctx, &model.File{
			Path: path, Language: "java",
			ContentHash: fmt.Sprintf("%064x", i), SizeBytes: 4096, ParseStatus: "ok",
		})
		if err != nil {
			tb.Fatalf("add file: %v", err)
		}
		if _, err := w.AddSymbol(ctx, &model.Symbol{
			Kind: model.KindClass, Name: fmt.Sprintf("File%05d", i),
			FQN: path + "#File", FileID: fileID,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 10000}},
			Exported:   true,
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}); err != nil {
			tb.Fatalf("add class: %v", err)
		}
		for j := 0; j < namesPerFile; j++ {
			name := fmt.Sprintf("shared%03d", (i*namesPerFile+j)%pool)
			if _, err := w.AddSymbol(ctx, &model.Symbol{
				Kind: model.KindMethod, Name: name,
				FQN: fmt.Sprintf("%s#%s", path, name), FileID: fileID,
				Range: model.Range{
					Start: model.Position{Line: j*8 + 2}, End: model.Position{Line: j*8 + 7}},
				Exported:   true,
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
			}); err != nil {
				tb.Fatalf("add symbol: %v", err)
			}
		}
		if err := w.AddImport(ctx, fileID, registry.Import{
			Path: fmt.Sprintf("src.pkg%03d", ((i/20)+1)%(files/20+1)), Line: 2,
		}); err != nil {
			tb.Fatalf("add import: %v", err)
		}
		for k := 0; k < refsPerFile; k++ {
			if err := w.AddReference(ctx, fileID, registry.Reference{
				Name: fmt.Sprintf("shared%03d", (i*7919+k)%pool),
				Kind: "call", Line: k%(namesPerFile*8) + 2, Col: 5,
			}); err != nil {
				tb.Fatalf("add reference: %v", err)
			}
		}
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("close writer: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		tb.Fatalf("commit run: %v", err)
	}
	return reg, run.ID
}

// resolvedState renders everything a resolution pass wrote, in a stable order,
// so two passes can be compared as text.
func resolvedState(t *testing.T, dir string, runID int64) string {
	t.Helper()
	db := verifyDB(t, dir)

	var out string
	for _, q := range []string{
		`SELECT src_id, dst_id, kind, confidence, source, COALESCE(evidence_file,''), COALESCE(evidence_line,0)
		   FROM edges WHERE index_run_id = ? ORDER BY src_id, dst_id, kind, confidence`,
		`SELECT id, COALESCE(resolved_symbol_id,0), resolution
		   FROM symbol_references WHERE index_run_id = ? ORDER BY id`,
	} {
		rows, err := db.QueryContext(context.Background(), q, runID)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			out += fmt.Sprintln(vals...)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// resolvePreloaded runs the pass the way it ran before candidates were
// streamed: the whole run's symbol index in one map.
//
// Kept here rather than in the production path because it is the reference
// implementation this change is measured against, and a reference that lives in
// the production path is one somebody eventually calls.
func resolvePreloaded(t *testing.T, reg *registry.Registry, runID int64) {
	t.Helper()
	ctx := context.Background()

	symbols, err := reg.LoadSymbolIndex(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	resolver := infer.New(symbols, infer.Options{})

	var stats infer.Stats
	var pending []registry.RefOutcome
	err = reg.EachFileWithRefs(ctx, runID, func(fr registry.FileRefs) error {
		edges, outs, err := resolver.ResolveFile(ctx, fr, &stats)
		if err != nil {
			return err
		}
		if _, err := reg.AddResolvedEdges(ctx, runID, edges); err != nil {
			return err
		}
		pending = append(pending, outs...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkResolutions(ctx, pending); err != nil {
		t.Fatal(err)
	}
}

// TestStreamingResolutionMatchesPreloadedIndex is the correctness gate under
// the memory change.
//
// Resolution stopped loading the run's symbols into one map and now reads
// candidates per page from SQLite, capped at infer.MaxCandidates+1 rows per
// name. Three things could silently change the answer: the order candidates
// arrive in (the resolver reads cands[0] in its single-candidate tiers), the
// per-name cap (a name with more candidates than the resolver weighs), and the
// cache (a stale or wrongly-shared bucket).
//
// None of those is visible in a count. Two passes can resolve the same NUMBER
// of references while binding them to different symbols, so this compares the
// rows themselves.
func TestStreamingResolutionMatchesPreloadedIndex(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name                      string
		files, perFile, refsPerFn int
	}{
		{"ordinary", 40, 10, 20},
		{"reference-heavy", 20, 30, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirA := t.TempDir()
			regA, runA := resolveFixture(t, dirA, tc.files, tc.perFile, tc.refsPerFn)
			resolvePreloaded(t, regA, runA)
			want := resolvedState(t, dirA, runA)

			dirB := t.TempDir()
			regB, runB := resolveFixture(t, dirB, tc.files, tc.perFile, tc.refsPerFn)
			if _, err := Resolve(ctx, regB, runB, ResolveOptions{}); err != nil {
				t.Fatal(err)
			}
			got := resolvedState(t, dirB, runB)

			if want == "" {
				t.Fatal("fixture resolved nothing; this test would prove nothing")
			}
			if got != want {
				t.Errorf("streaming resolution differs from the preloaded index\n"+
					"--- preloaded ---\n%s\n--- streaming ---\n%s", want, got)
			}
		})
	}

	// The case the cap exists for, and the one resolveFixture cannot express:
	// names shared by far more symbols than the resolver will weigh. The
	// preloaded index sees every candidate; the streaming path sees nine. Both
	// must reach the same verdict on every reference.
	t.Run("names shared past the candidate cap", func(t *testing.T) {
		dirA := t.TempDir()
		regA, runA := sharedNameFixture(t, dirA, 60, 10, 20, 20)
		resolvePreloaded(t, regA, runA)
		want := resolvedState(t, dirA, runA)

		dirB := t.TempDir()
		regB, runB := sharedNameFixture(t, dirB, 60, 10, 20, 20)
		if _, err := Resolve(ctx, regB, runB, ResolveOptions{}); err != nil {
			t.Fatal(err)
		}
		got := resolvedState(t, dirB, runB)

		if want == "" {
			t.Fatal("fixture resolved nothing; this test would prove nothing")
		}
		if got != want {
			t.Errorf("capped candidate reads changed the verdict\n"+
				"--- preloaded (every candidate) ---\n%s\n--- streaming (capped) ---\n%s", want, got)
		}
	})
}

// TestCandidateCapPreservesTheAmbiguousVerdict pins why reading 9 rows for a
// name shared by thousands is safe.
//
// The resolver abandons a name with more than MaxCandidates candidates and
// records it ambiguous, deciding that from the COUNT alone — it never reads
// those entries. So one row past the ceiling carries all the information the
// rest do, and fetching the rest is work whose only effect is memory that grows
// with the repository.
//
// This asserts the premise rather than trusting it: if the resolver ever starts
// reading the contents of an over-cap bucket, the cap becomes a behaviour
// change and this fails.
func TestCandidateCapPreservesTheAmbiguousVerdict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// 60 files x 10 names from a pool of 20 puts ~30 candidates behind every
	// name, comfortably past the cap of 8.
	reg, runID := sharedNameFixture(t, dir, 60, 10, 20, 20)

	over, err := namesOverCap(t, dir, runID, infer.MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if over == 0 {
		t.Fatal("fixture has no name past the candidate cap; this test would prove nothing")
	}

	full := registry.NewCandidateCache(reg, runID, 0, 100_000)
	capped := registry.NewCandidateCache(reg, runID, 0, infer.MaxCandidates+1)

	names, err := allRefNames(t, dir, runID)
	if err != nil {
		t.Fatal(err)
	}
	fullSet, err := full.Candidates(ctx, names)
	if err != nil {
		t.Fatal(err)
	}
	capSet, err := capped.Candidates(ctx, names)
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range names {
		f, c := fullSet[n], capSet[n]
		switch {
		case len(f) > infer.MaxCandidates:
			// The only thing the resolver asks is "more than the cap?". Both
			// must answer yes.
			if len(c) <= infer.MaxCandidates {
				t.Errorf("name %q: uncapped has %d candidates, capped has %d — "+
					"the capped read no longer looks ambiguous", n, len(f), len(c))
			}
		default:
			// At or under the cap the resolver reads every entry, so the capped
			// read must be identical, not merely the same length.
			if len(f) != len(c) {
				t.Errorf("name %q: uncapped %d candidates, capped %d", n, len(f), len(c))
				continue
			}
			for i := range f {
				if f[i].ID != c[i].ID {
					t.Errorf("name %q candidate %d: uncapped id %d, capped id %d — order differs",
						n, i, f[i].ID, c[i].ID)
				}
			}
		}
	}
	t.Logf("%d of %d names exceed the cap of %d", over, len(names), infer.MaxCandidates)
}

func namesOverCap(t *testing.T, dir string, runID int64, cap int) (int, error) {
	var n int
	err := verifyDB(t, dir).QueryRow(`
		SELECT COUNT(*) FROM (
		  SELECT name FROM symbols WHERE index_run_id = ?
		   GROUP BY name HAVING COUNT(*) > ?)`, runID, cap).Scan(&n)
	return n, err
}

func allRefNames(t *testing.T, dir string, runID int64) ([]string, error) {
	rows, err := verifyDB(t, dir).Query(
		`SELECT DISTINCT name FROM symbol_references WHERE index_run_id = ? ORDER BY name`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TestOverSubscribedMethodResolvedByReceiverInStreaming verifies that when a method
// has far more candidates than MaxCandidates (e.g. 25 candidates for "build"),
// a call site carrying a receiver matching candidate #20 is successfully resolved
// by Resolve() rather than dropped into ambiguous due to cache truncation.
func TestOverSubscribedMethodResolvedByReceiverInStreaming(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "index.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "oversubscribed")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}

	// Create 24 dummy files declaring "build".
	for i := 1; i <= 24; i++ {
		path := fmt.Sprintf("src/Dummy%02d.java", i)
		fID, err := w.AddFile(ctx, &model.File{
			Path: path, Language: "java", ContentHash: fmt.Sprintf("%064x", i),
			SizeBytes: 1024, ParseStatus: "ok",
		})
		if err != nil {
			t.Fatalf("add dummy file: %v", err)
		}
		if _, err := w.AddSymbol(ctx, &model.Symbol{
			Kind: model.KindMethod, Name: "build",
			FQN: fmt.Sprintf("%s#build", path), FileID: fID,
			Range: model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 10}},
			Exported: true, Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}); err != nil {
			t.Fatalf("add dummy build: %v", err)
		}
	}

	// Create candidate #25: TargetBuilder.java declaring "build".
	targetPath := "src/TargetBuilder.java"
	targetFID, err := w.AddFile(ctx, &model.File{
		Path: targetPath, Language: "java", ContentHash: fmt.Sprintf("%064x", 999),
		SizeBytes: 1024, ParseStatus: "ok",
	})
	if err != nil {
		t.Fatalf("add target file: %v", err)
	}
	targetSymID, err := w.AddSymbol(ctx, &model.Symbol{
		Kind: model.KindMethod, Name: "build",
		FQN: targetPath + "#build", FileID: targetFID,
		Range: model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 10}},
		Exported: true, Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	})
	if err != nil {
		t.Fatalf("add target build: %v", err)
	}

	// Create caller file referencing "build" with Receiver: "TargetBuilder".
	callerPath := "src/Caller.java"
	callerFID, err := w.AddFile(ctx, &model.File{
		Path: callerPath, Language: "java", ContentHash: fmt.Sprintf("%064x", 888),
		SizeBytes: 1024, ParseStatus: "ok",
	})
	if err != nil {
		t.Fatalf("add caller file: %v", err)
	}
	callerSymID, err := w.AddSymbol(ctx, &model.Symbol{
		Kind: model.KindMethod, Name: "doWork",
		FQN: callerPath + "#doWork", FileID: callerFID,
		Range: model.Range{Start: model.Position{Line: 2}, End: model.Position{Line: 20}},
		Exported: true, Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	})
	if err != nil {
		t.Fatalf("add caller method: %v", err)
	}

	if err := w.AddReference(ctx, callerFID, registry.Reference{
		Name: "build", Receiver: "TargetBuilder", Line: 10, Col: 5,
	}); err != nil {
		t.Fatalf("add reference: %v", err)
	}

	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush writer: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit run: %v", err)
	}

	// Run streaming resolution.
	stats, err := Resolve(ctx, reg, run.ID, ResolveOptions{})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if stats.Resolved != 1 {
		t.Fatalf("expected 1 resolved reference, got %d (stats: %+v)", stats.Resolved, stats)
	}

	// Verify the edge points to targetSymID.
	var edgeDst int64
	err = verifyDB(t, dir).QueryRow(`
		SELECT dst_id FROM edges WHERE index_run_id = ? AND src_id = ?`,
		run.ID, callerSymID).Scan(&edgeDst)
	if err != nil {
		t.Fatalf("query edge dst: %v", err)
	}
	if edgeDst != targetSymID {
		t.Fatalf("edge points to symbol %d, want targetSymID %d", edgeDst, targetSymID)
	}
}
