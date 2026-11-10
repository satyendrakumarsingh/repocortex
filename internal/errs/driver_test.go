// This file drives the real modernc.org/sqlite driver, so it lives in the
// external test package: internal/errs must stay a leaf, and nothing in the
// non-test build may acquire the driver dependency.
//
// It exists because the SQLite classification is the load-bearing claim of
// this package, and a claim about how a third-party driver reports errors is
// worth nothing until it has been observed. CONTRIBUTING: "a gate that has
// only ever been seen passing proves nothing."
package errs_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"

	_ "modernc.org/sqlite"
)

// openContending returns two independent handles on the same database file
// with busy_timeout disabled, so a write lock held by one is reported to the
// other immediately instead of after a wait.
func openContending(t *testing.T, path string) (*sql.DB, *sql.DB) {
	t.Helper()
	// busy_timeout(0) makes the contention deterministic. Production uses 5s
	// (store.Options.BusyTimeout) precisely so this is rare — but when the
	// timeout does expire, this is the error that comes back.
	dsn := "file:" + path + "?_pragma=busy_timeout(0)"
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		// One connection per handle keeps the lock on the connection the
		// explicit BEGIN was issued on.
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	return open(), open()
}

// TestRealSQLiteBusyIsTransient is the test that grounds IsRetryable.
//
// Lock contention between an index run, the watcher and the MCP server is the
// one failure RepoCortex should retry rather than surface, and this proves the
// driver reports it in a form this package can recognise without matching on
// message text.
func TestRealSQLiteBusyIsTransient(t *testing.T) {
	writer, other := openContending(t, filepath.Join(t.TempDir(), "busy.db"))

	if _, err := writer.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// BEGIN IMMEDIATE takes the write lock now rather than on first write.
	if _, err := writer.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	defer func() { _, _ = writer.Exec(`ROLLBACK`) }()

	_, err := other.Exec(`INSERT INTO t VALUES (1)`)
	if err == nil {
		t.Fatal("expected a lock error from the second connection")
	}

	if got := errs.KindOf(err); got != errs.KindTransient {
		t.Fatalf("KindOf(%v) = %q, want %q", err, got, errs.KindTransient)
	}
	if !errs.IsRetryable(err) {
		t.Fatalf("IsRetryable(%v) = false; lock contention must be retryable", err)
	}

	// And it survives the plain wrapping the rest of the codebase applies.
	wrapped := errs.Wrap(errs.KindTransient, "registry.CommitRun", err)
	if !errs.IsRetryable(wrapped) {
		t.Error("wrapping lost the classification")
	}
}

// TestRealSQLiteConstraintIsConflict covers the other half of the driver
// mapping: the SQL CHECKs that back the graph invariants surface as
// SQLITE_CONSTRAINT, and a caller must not mistake one for something to retry.
func TestRealSQLiteConstraintIsConflict(t *testing.T) {
	db, _ := openContending(t, filepath.Join(t.TempDir(), "constraint.db"))

	// Shaped after migration 0001's exact-implies-compiler CHECK.
	if _, err := db.Exec(`CREATE TABLE edges (
		confidence TEXT NOT NULL,
		source     TEXT NOT NULL,
		CHECK (confidence <> 'exact' OR source = 'compiler'))`); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := db.Exec(`INSERT INTO edges VALUES ('exact', 'heuristic')`)
	if err == nil {
		t.Fatal("the CHECK did not fire; test premise broken")
	}
	if got := errs.KindOf(err); got != errs.KindConflict {
		t.Fatalf("KindOf(%v) = %q, want %q", err, got, errs.KindConflict)
	}
	if errs.IsRetryable(err) {
		t.Error("a constraint violation must not be retryable")
	}
}

// TestRealSQLiteNotADatabaseIsCorrupt proves the corrupt path is reachable
// from the driver too, so `cortex doctor` can tell a user to rebuild rather
// than retry.
func TestRealSQLiteNotADatabaseIsCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(path, []byte("this is not a SQLite database at all\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`SELECT count(*) FROM sqlite_master`)
	if err == nil {
		t.Fatal("expected a not-a-database error")
	}
	if got := errs.KindOf(err); got != errs.KindCorrupt {
		t.Fatalf("KindOf(%v) = %q, want %q", err, got, errs.KindCorrupt)
	}
	if errs.IsRetryable(err) {
		t.Error("a corrupt database must never be retryable")
	}
}
