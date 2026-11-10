package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestForeignKeysOnEveryConnection is a regression test for a data-integrity
// bug that shipped and was caught only by soak testing.
//
// SQLite pragmas are per-connection. The original code ran
// `db.ExecContext(ctx, "PRAGMA foreign_keys = ON")`, which borrows ONE
// connection from the pool, sets the pragma on it, and returns it. The rest of
// the pool ran with foreign keys off, so ON DELETE CASCADE silently did not
// fire — and the verification that followed reused the connection it had just
// configured, so it always passed.
//
// Measured impact before the fix: 62 of 64 sampled connections had foreign keys
// off, and a 30-second concurrent soak produced 1,010 dangling symbols and 505
// orphaned FTS rows.
//
// The test samples far more goroutines than the pool holds, because a
// single-connection check is exactly what failed to catch it.
func TestForeignKeysOnEveryConnection(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"on_disk", Options{Path: filepath.Join(t.TempDir(), "fk.db")}},
		{"in_memory", Options{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(ctx, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			const samples = 64
			var (
				wg  sync.WaitGroup
				mu  sync.Mutex
				off int
			)
			for i := 0; i < samples; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var fk int
					if err := db.QueryRowContext(ctx,
						`SELECT foreign_keys FROM pragma_foreign_keys`).Scan(&fk); err != nil {
						return
					}
					if fk != 1 {
						mu.Lock()
						off++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()

			if off > 0 {
				t.Fatalf("%d/%d connections have foreign_keys OFF; cascades would silently not fire", off, samples)
			}
		})
	}
}

// TestCascadeFiresUnderConcurrency is the behavioural counterpart: it proves
// the pragma is not merely reported as on, but actually enforced, on whichever
// connection a delete happens to land on.
func TestCascadeFiresUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "cascade.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		t.Fatal(err)
	}

	// Seed several runs, then delete them from many goroutines so the deletes
	// spread across the pool.
	const runs = 24
	for i := 1; i <= runs; i++ {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO index_runs(id, status, started_unix) VALUES(?, 'indexing', 0)`, i); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO files(id, index_run_id, path, content_hash) VALUES(?,?,?,'h')`,
			i, i, "f.go"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO symbols(index_run_id, file_id, kind, name, fqn, confidence, source)
			 VALUES(?,?, 'function', 'A', 'pkg.A', 'high', 'source_code')`, i, i); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 1; i <= runs; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, _ = db.ExecContext(ctx, `DELETE FROM index_runs WHERE id = ?`, id)
		}(i)
	}
	wg.Wait()

	for _, table := range []string{"files", "symbols"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d orphaned rows after cascade delete; foreign keys were off on some connection", table, n)
		}
	}
}
