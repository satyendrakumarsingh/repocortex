package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// dropLatestMigration rewinds an index one schema version, simulating a
// database written by the previous release of repocortex.
//
// It drops the objects the newest migration created and deletes its
// schema_migrations row, which is what an older binary's index actually looks
// like — as opposed to a fresh database, which would hide the bug entirely.
func dropLatestMigration(t *testing.T, dbPath string) int {
	t.Helper()

	migrations, err := store.LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 2 {
		t.Skip("need at least two migrations to rewind one")
	}
	latest := migrations[len(migrations)-1]

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Undo whatever the latest migration created. CREATE INDEX, CREATE TABLE and
	// ALTER TABLE ... ADD COLUMN are handled; a migration doing anything else
	// must extend this helper rather than silently rewinding nothing.
	//
	// ADD COLUMN was added when 0008 became the latest migration and this helper
	// failed on it. Note the failure mode it was already designed against: the
	// loop counts what it undid and FAILS when that is zero, so a migration it
	// cannot rewind stops the test instead of leaving it to pass against a
	// database that was never actually rewound. That is why this surfaced as a
	// clear message rather than as two upgrade tests quietly asserting nothing.
	var undid int
	for _, line := range strings.Split(latest.SQL, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		var drop string
		switch {
		case strings.EqualFold(fields[0], "CREATE") && strings.EqualFold(fields[1], "INDEX"):
			drop = "DROP INDEX IF EXISTS " + fields[2]
		case strings.EqualFold(fields[0], "CREATE") && strings.EqualFold(fields[1], "TABLE"):
			drop = "DROP TABLE IF EXISTS " + fields[2]
		case strings.EqualFold(fields[0], "ALTER") && strings.EqualFold(fields[1], "TABLE"):
			// ALTER TABLE <table> ADD [COLUMN] <name> ...
			col, ok := addedColumn(fields)
			if !ok {
				continue
			}
			drop = "ALTER TABLE " + fields[2] + " DROP COLUMN " + col
		default:
			continue
		}
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
		undid++
	}
	if undid == 0 {
		t.Fatalf("migration %04d_%s created nothing this helper knows how to "+
			"drop; extend dropLatestMigration", latest.Version, latest.Name)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, latest.Version); err != nil {
		t.Fatal(err)
	}
	return latest.Version
}

func schemaVersionOf(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestUpgradeAppliesPendingMigrations pins the upgrade path.
//
// store.Migrate was called only by `repocortex init`. Every other command opened
// the database and used it as-is, so upgrading the binary left an existing user
// on their old schema permanently: migration 0005's foreign-key indexes were
// never created, and before that, a v2 index met a build expecting
// table_accesses and failed with a raw driver error from deep inside a query.
//
// Anyone who ran `repocortex init` once and then upgraded was affected, which is
// every user who keeps the tool longer than one release.
func TestUpgradeAppliesPendingMigrations(t *testing.T) {
	root := isolate(t)

	if err := os.WriteFile(filepath.Join(root, "a.go"),
		[]byte("package a\n\nfunc A() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	dbPath := filepath.Join(root, ".repocortex", "index.db")
	current := schemaVersionOf(t, dbPath)
	rewound := dropLatestMigration(t, dbPath)
	if got := schemaVersionOf(t, dbPath); got != rewound-1 {
		t.Fatalf("rewind failed: schema is v%d, wanted v%d", got, rewound-1)
	}

	// A writable command must upgrade in place.
	if out, err := run(t, root, "index"); err != nil {
		t.Fatalf("index on an outdated schema failed: %v\n%s", err, out)
	}
	if got := schemaVersionOf(t, dbPath); got != current {
		t.Fatalf("index did not upgrade the schema: still v%d, wanted v%d", got, current)
	}
}

// TestOutdatedSchemaIsReportedNotCrashed pins the read-only path.
//
// Queries open the database read-only and so cannot migrate. Before this, they
// failed wherever the missing object was first touched, with a message naming a
// table the user has never heard of and no indication that the fix is one
// command. The mismatch is now asserted at open.
func TestOutdatedSchemaIsReportedNotCrashed(t *testing.T) {
	root := isolate(t)

	if err := os.WriteFile(filepath.Join(root, "a.go"),
		[]byte("package a\n\nfunc A() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	dbPath := filepath.Join(root, ".repocortex", "index.db")
	dropLatestMigration(t, dbPath)

	out, err := run(t, root, "symbol", "A")
	if err == nil {
		t.Fatalf("a query against an outdated schema succeeded; expected a refusal:\n%s", out)
	}
	// The message has to name the fix, not just the problem.
	if !strings.Contains(err.Error()+out, "repocortex index") {
		t.Errorf("error does not tell the user how to fix it: %v\n%s", err, out)
	}

	// status is what someone runs to diagnose, so it must still work and say
	// what is wrong rather than refusing alongside everything else.
	st, err := run(t, root, "status")
	if err != nil {
		t.Fatalf("status refused to run on an outdated index: %v\n%s", err, st)
	}
	if !strings.Contains(st, "OUTDATED") {
		t.Errorf("status did not report the outdated schema:\n%s", st)
	}
}

// addedColumn extracts the column name from an `ALTER TABLE t ADD [COLUMN] c …`
// statement, or reports that the statement adds no column.
//
// Deliberately narrow: it recognises the one form the migrations in this
// repository use and refuses anything else, so an ALTER that renames or drops
// something is left to trip the "created nothing this helper knows how to drop"
// guard rather than being half-rewound.
func addedColumn(fields []string) (string, bool) {
	for i := 3; i < len(fields)-1; i++ {
		if !strings.EqualFold(fields[i], "ADD") {
			continue
		}
		next := fields[i+1]
		if strings.EqualFold(next, "COLUMN") {
			if i+2 >= len(fields) {
				return "", false
			}
			next = fields[i+2]
		}
		name := strings.Trim(next, `"'`+"`;")
		if name == "" {
			return "", false
		}
		return name, true
	}
	return "", false
}
