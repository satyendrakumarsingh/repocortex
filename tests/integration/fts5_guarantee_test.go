// Package integration holds tests that exercise real dependencies.
//
// This file is a DEPENDENCY GUARANTEE test, not a feature test.
//
// modernc.org/sqlite enables FTS5 through a codegen flag (-DSQLITE_ENABLE_FTS5)
// but does not document it. Undocumented means unpromised: a future release
// could drop it and every search in RepoCortex would silently break. This test
// fails loudly on that day, in CI, on every platform RepoCortex ships to.
//
// Do not delete or skip it without replacing the search backend.
package integration

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/search"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

func mustOpen(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestFTS5IsAvailable(t *testing.T) {
	db := mustOpen(t)
	if err := store.VerifyFTS5(context.Background(), db.DB); err != nil {
		t.Fatalf("FTS5 GUARANTEE BROKEN — search cannot work: %v", err)
	}
	var ver string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	t.Logf("sqlite %s with FTS5", ver)
}

// TestFTS5SearchPipeline exercises the exact query shape the search layer uses:
// insert into files + files_fts with an expanded identifier column, then rank
// with bm25 and extract a snippet from the ORIGINAL text.
func TestFTS5SearchPipeline(t *testing.T) {
	ctx := context.Background()
	db := mustOpen(t)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO index_runs(id, status, committed, started_unix) VALUES(1,'ready',1,0)`); err != nil {
		t.Fatal(err)
	}

	seed := []struct{ path, content string }{
		{"src/payment/CallbackController.java",
			"public class CallbackController { void handleCallback(String txnId) { transactionService.updateStatus(txnId); } }"},
		{"src/payment/TransactionService.java",
			"public class TransactionService { void updateStatus(String id) { repository.save(id); } }"},
		{"src/notify/NotificationService.java",
			"public class NotificationService { void send(String txnId) { client.post(txnId); } }"},
		{"src/report/MarketingReport.java",
			"public class MarketingReport { void render() { } }"},
	}
	for i, s := range seed {
		id := int64(i + 1)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO files(id, index_run_id, path, content_hash, language)
			 VALUES(?,1,?,?, 'java')`, id, s.path, "hash"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO files_fts(rowid, path, content, expanded) VALUES(?,?,?,?)`,
			id, s.path, s.content, search.ExpandIdentifiers(s.path+" "+s.content)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO files_fts_map(fts_rowid, file_id) VALUES(?,?)`, id, id); err != nil {
			t.Fatal(err)
		}
	}

	// The critical case: a bare word that only ever appears as a camelCase
	// fragment. Without identifier expansion this returns nothing.
	t.Run("bare_word_matches_camelcase_fragment", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, `
			SELECT f.path, bm25(files_fts, 1.0, 1.0, 2.0) AS score
			FROM files_fts JOIN files f ON f.id = files_fts.rowid
			WHERE files_fts MATCH ? ORDER BY score`, "callback")
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		defer rows.Close()

		var paths []string
		for rows.Next() {
			var p string
			var sc float64
			if err := rows.Scan(&p, &sc); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, p)
		}
		if len(paths) == 0 {
			t.Fatal("'callback' matched nothing — identifier expansion regressed")
		}
		if paths[0] != "src/payment/CallbackController.java" {
			t.Errorf("top hit = %q, want CallbackController", paths[0])
		}
		if len(paths) == len(seed) {
			t.Error("'callback' matched every file — no discrimination")
		}
	})

	t.Run("snippet_returns_original_source", func(t *testing.T) {
		var snip string
		if err := db.QueryRowContext(ctx, `
			SELECT snippet(files_fts, 1, '[', ']', '...', 10)
			FROM files_fts WHERE files_fts MATCH ? LIMIT 1`, "updatestatus").Scan(&snip); err != nil {
			t.Fatalf("snippet: %v", err)
		}
		if snip == "" {
			t.Fatal("snippet() returned empty; bounded source ranges are unavailable")
		}
		t.Logf("snippet=%q", snip)
	})

	t.Run("prefix_query", func(t *testing.T) {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM files_fts WHERE files_fts MATCH ?`, "notif*").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("prefix 'notif*' matched %d files, want 1", n)
		}
	})
}

// TestPragmasHold verifies the connection settings the storage layer promises.
func TestPragmasHold(t *testing.T) {
	ctx := context.Background()
	db := mustOpen(t)

	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1 — the graph invariant gate depends on it", fk)
	}
}
