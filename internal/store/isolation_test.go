package store

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestMemoryStoresAreIsolated guards against a defect that shipped in the first
// draft of this package and was caught during M1 integration.
//
// `file::memory:?cache=shared` is ONE database shared by every connection in
// the process. Two OpenMemory calls therefore returned handles to the same
// store, and one test's writes were visible to another's reads. That produces
// order-dependent, intermittent failures that look like anything except a DSN
// problem, so the invariant is pinned here rather than left to review.
func TestMemoryStoresAreIsolated(t *testing.T) {
	ctx := context.Background()

	a, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	b, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := a.ExecContext(ctx,
		`INSERT INTO index_runs(id, status, started_unix) VALUES(42, 'indexing', 0)`); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := b.QueryRowContext(ctx,
		`SELECT count(*) FROM index_runs WHERE id = 42`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("store B sees %d row(s) written to store A; in-memory stores share a database", n)
	}

	// And the reverse direction, so isolation is not accidentally one-way.
	if _, err := b.ExecContext(ctx,
		`INSERT INTO index_runs(id, status, started_unix) VALUES(99, 'indexing', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := a.QueryRowContext(ctx,
		`SELECT count(*) FROM index_runs WHERE id = 99`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("store A sees %d row(s) written to store B", n)
	}
}

// TestMemoryDSNsAreUnique checks the mechanism directly, so a regression is
// diagnosable without inferring it from cross-contamination.
func TestMemoryDSNsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		dsn, path := buildDSN(Options{})
		if path != "" {
			t.Fatalf("in-memory store reported a path %q", path)
		}
		if !strings.Contains(dsn, "mode=memory") {
			t.Fatalf("DSN %q is not an in-memory DSN", dsn)
		}
		if seen[dsn] {
			t.Fatalf("duplicate in-memory DSN %q after %d opens", dsn, i)
		}
		seen[dsn] = true
	}
}

// TestConcurrentMemoryStores is the shape that made the original bug dangerous:
// parallel tests each opening their own store.
func TestConcurrentMemoryStores(t *testing.T) {
	ctx := context.Background()
	const n = 16

	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			db, err := OpenMemory(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer db.Close()

			if _, err := db.ExecContext(ctx,
				`INSERT INTO index_runs(id, status, started_unix) VALUES(?, 'indexing', 0)`,
				id+1); err != nil {
				errs <- err
				return
			}
			var count int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM index_runs`).Scan(&count); err != nil {
				errs <- err
				return
			}
			if count != 1 {
				errs <- errCount(id, count)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent isolation failure: %v", err)
		}
	}
}

type countErr struct {
	id, got int
}

func (e countErr) Error() string {
	return "goroutine " + itoa(e.id) + " saw " + itoa(e.got) + " rows, want 1 (stores are shared)"
}

func errCount(id, got int) error { return countErr{id: id, got: got} }
