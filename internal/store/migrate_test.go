package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func fixedClock() func() int64 { return func() int64 { return 1_700_000_000 } }

func TestLoadMigrationsOrderedAndChecksummed(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded — embed directive broken")
	}
	for i, m := range ms {
		if i > 0 && m.Version <= ms[i-1].Version {
			t.Fatalf("migrations out of order: %d after %d", m.Version, ms[i-1].Version)
		}
		if m.Checksum == "" || len(m.Checksum) != 64 {
			t.Fatalf("migration %d: bad checksum %q", m.Version, m.Checksum)
		}
		if m.SQL == "" {
			t.Fatalf("migration %d: empty SQL", m.Version)
		}
	}
}

func TestMigrateFreshInstall(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	applied, err := Migrate(ctx, db.DB, fixedClock())
	if err != nil {
		t.Fatalf("fresh migrate: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("fresh install applied nothing")
	}

	v, err := SchemaVersion(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if v != applied[len(applied)-1] {
		t.Fatalf("SchemaVersion = %d, want %d", v, applied[len(applied)-1])
	}

	for _, table := range []string{"index_runs", "files", "symbols", "edges", "files_fts", "files_fts_map"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("table %q missing after migration", table)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	first, err := Migrate(ctx, db.DB, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Migrate(ctx, db.DB, fixedClock())
	if err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second migrate applied %v, want none", second)
	}
	if len(first) == 0 {
		t.Fatal("first migrate applied nothing")
	}
}

// TestMigrateDetectsEditedMigration guards a subtle, high-cost failure: someone
// edits a shipped migration, and two installs silently end up with different
// schemas.
func TestMigrateDetectsEditedMigration(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := Migrate(ctx, db.DB, fixedClock()); err != nil {
		t.Fatal(err)
	}
	// Simulate an edit by corrupting the recorded checksum.
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}

	_, err = Migrate(ctx, db.DB, fixedClock())
	if err == nil {
		t.Fatal("expected checksum mismatch to be rejected")
	}
	t.Logf("correctly rejected: %v", err)
}

// TestMigrateInterrupted proves an interrupted migration leaves no partial
// schema. Each migration is transactional, so a failure must roll back
// completely and re-running must succeed.
func TestMigrateInterrupted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}

	// Force failure partway: pre-create a table that 0001 also creates, so the
	// migration errors mid-statement.
	if _, err := db.ExecContext(ctx, `CREATE TABLE symbols (bogus INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, db.DB, fixedClock()); err == nil {
		t.Fatal("expected migration to fail against a conflicting table")
	}

	// Nothing should have been recorded, and the partial schema must be absent.
	v, err := SchemaVersion(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("schema version = %d after failed migration, want 0", v)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name = 'index_runs'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("index_runs survived a rolled-back migration — transaction boundary is wrong")
	}

	// Remove the conflict; a retry must now fully succeed.
	if _, err := db.ExecContext(ctx, `DROP TABLE symbols`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, db.DB, fixedClock()); err != nil {
		t.Fatalf("retry after interruption failed: %v", err)
	}
	if v, _ := SchemaVersion(ctx, db.DB); v == 0 {
		t.Fatal("retry did not apply migrations")
	}
	db.Close()
}

func TestSchemaVersionOnVirginDB(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	v, err := SchemaVersion(ctx, db.DB)
	if err != nil {
		t.Fatalf("SchemaVersion on virgin db: %v", err)
	}
	if v != 0 {
		t.Fatalf("virgin schema version = %d, want 0", v)
	}
}

func TestVerifyFTS5(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := VerifyFTS5(ctx, db.DB); err != nil {
		t.Fatalf("FTS5 unavailable — the search layer cannot work: %v", err)
	}
}

// TestExactRequiresCompilerSource proves the SQL CHECK enforces the same
// invariant as model.Edge.Validate. Defence in depth: a buggy writer must not
// be able to record a guess as compiler-proven.
func TestExactRequiresCompilerSource(t *testing.T) {
	ctx := context.Background()
	db, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	seedRunFileSymbols(t, db.DB)

	_, err = db.ExecContext(ctx, `
		INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, source)
		VALUES(1, 1, 2, 'CALLS', 'exact', 'inferred')`)
	if err == nil {
		t.Fatal("SQL CHECK allowed confidence=exact with source=inferred")
	}
	t.Logf("correctly rejected by CHECK: %v", err)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, source)
		VALUES(1, 1, 2, 'CALLS', 'exact', 'compiler')`); err != nil {
		t.Fatalf("legitimate exact edge rejected: %v", err)
	}
}

func TestForeignKeysCascade(t *testing.T) {
	ctx := context.Background()
	db, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	seedRunFileSymbols(t, db.DB)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, source)
		VALUES(1, 1, 2, 'CALLS', 'high', 'source_code')`); err != nil {
		t.Fatal(err)
	}

	// Deleting the run must cascade to files, symbols and edges. Without this
	// a discarded staging run would leak rows into every later query.
	if _, err := db.ExecContext(ctx, `DELETE FROM index_runs WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"files", "symbols", "edges"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s has %d orphaned rows after run delete; cascade is broken", table, n)
		}
	}
}

func seedRunFileSymbols(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO index_runs(id, status, started_unix) VALUES(1, 'indexing', 0)`,
		`INSERT INTO files(id, index_run_id, path, content_hash) VALUES(1, 1, 'a.go', 'h1')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		 VALUES(1, 1, 1, 'function', 'A', 'pkg.A', 'high', 'source_code')`,
		`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
		 VALUES(2, 1, 1, 'function', 'B', 'pkg.B', 'high', 'source_code')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %.40q: %v", s, err)
		}
	}
}

// TestMigrateExistingIndexGainsCompletenessColumns is the upgrade path for
// migrations 0006 and 0007, and it is the one that matters: a fresh install
// exercises the whole schema at once, whereas a user upgrading a binary has an
// index full of rows written by a build that had never heard of these columns.
//
// Two things must hold for that user. Their existing run must survive the
// ALTER — losing a published index on upgrade is the worst outcome available —
// and it must come back reading as `complete`, because a run written before
// completeness was measured made no claim either way, and relabelling every
// existing index as suspect would be a louder lie than the silence it replaces.
func TestMigrateExistingIndexGainsCompletenessColumns(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Bring the database up to the schema as it shipped BEFORE this work, by
	// applying every migration below the first new one, then populate it the
	// way a real index is populated.
	const firstNew = 6
	all, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, migrationTableDDL); err != nil {
		t.Fatal(err)
	}
	var old []Migration
	for _, m := range all {
		if m.Version < firstNew {
			old = append(old, m)
		}
	}
	if len(old) == 0 {
		t.Fatalf("no migrations below %d; the version numbering has changed", firstNew)
	}
	for _, m := range old {
		if err := applyOne(ctx, db.DB, m, fixedClock()); err != nil {
			t.Fatalf("apply legacy migration %04d: %v", m.Version, err)
		}
	}
	seedRunFileSymbols(t, db.DB)
	if _, err := db.ExecContext(ctx,
		`UPDATE index_runs SET status='ready', committed=1, files_indexed=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	applied, err := Migrate(ctx, db.DB, fixedClock())
	if err != nil {
		t.Fatalf("upgrade an existing index: %v", err)
	}
	if len(applied) == 0 {
		t.Fatal("upgrade applied nothing; the existing index never gained the new columns")
	}

	// The published run is still published, and still counted.
	var status string
	var committed, indexed, discovered, unaccounted int64
	var state string
	if err := db.QueryRowContext(ctx, `
		SELECT status, committed, files_indexed, files_discovered, index_state, files_unaccounted
		  FROM index_runs WHERE id = 1`).
		Scan(&status, &committed, &indexed, &discovered, &state, &unaccounted); err != nil {
		t.Fatalf("read upgraded run: %v", err)
	}
	if status != "ready" || committed != 1 || indexed != 1 {
		t.Fatalf("upgrade disturbed the published run: status=%q committed=%d indexed=%d",
			status, committed, indexed)
	}
	if state != "complete" {
		t.Errorf("index_state on an upgraded run = %q, want %q — a run that never "+
			"measured coverage must not be relabelled as suspect", state, "complete")
	}
	if discovered != 0 || unaccounted != 0 {
		t.Errorf("files_discovered=%d files_unaccounted=%d, want 0/0 — the absence of a "+
			"measurement must stay visible rather than being invented", discovered, unaccounted)
	}

	// The CHECK arrived with the column, not merely the column.
	if _, err := db.ExecContext(ctx,
		`UPDATE index_runs SET index_state = 'mostly' WHERE id = 1`); err == nil {
		t.Error("index_state accepted a value outside (complete, partial); the CHECK did not survive ADD COLUMN")
	}

	// Migration 0007's table exists on the upgraded database too, with its
	// foreign keys cascading from the run that already existed.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO client_calls(index_run_id, file_id, method, path, client, src_symbol_id, start_line, confidence)
		VALUES(1, 1, 'GET', '/api/orders', 'fetch', 1, 7, 'high')`); err != nil {
		t.Fatalf("client_calls unusable after upgrade: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM index_runs WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var orphans int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM client_calls`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("client_calls left %d orphaned rows after the run was deleted", orphans)
	}
}
