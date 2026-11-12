// Package store owns RepoCortex's persistence: schema migration, connection
// setup, and the storage interface the rest of the system codes against.
//
// The storage backend is deliberately behind an interface. SQLite is the
// local-first default, but nothing above this package may assume it.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is a single versioned schema change.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string // sha256 of SQL, used to detect edits to shipped migrations
}

// LoadMigrations reads and orders the embedded migrations.
//
// Filenames must be NNNN_name.sql. Version numbers must be unique and are
// applied in ascending order.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		numStr, name, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: want NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(numStr)
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version %q: %w", e.Name(), numStr, err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// shortSum truncates a checksum for display without assuming its length.
// A corrupt or truncated schema_migrations row must produce a clear error,
// never a panic.
func shortSum(s string) string {
	if s == "" {
		return "<empty>"
	}
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

const migrationTableDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL,
    checksum    TEXT    NOT NULL,
    applied_unix INTEGER NOT NULL
)`

// Migrate brings db up to the latest schema version.
//
// Each migration runs inside its own transaction, so an interrupted run leaves
// the database at the last fully-applied version rather than in a partial
// state. Re-running simply resumes.
//
// Previously-applied migrations are checksum-verified. A shipped migration that
// has been edited is a correctness hazard — two installs would silently have
// different schemas — so it is a hard error, not a warning.
func Migrate(ctx context.Context, db *sql.DB, now func() int64) (applied []int, err error) {
	if _, err := db.ExecContext(ctx, migrationTableDDL); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	migrations, err := LoadMigrations()
	if err != nil {
		return nil, err
	}

	type record struct {
		name     string
		checksum string
	}
	existing := map[int]record{}
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var r record
		if err := rows.Scan(&v, &r.name, &r.checksum); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		existing[v] = r
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	for _, m := range migrations {
		if prev, done := existing[m.Version]; done {
			if prev.checksum != m.Checksum {
				return applied, fmt.Errorf(
					"migration %04d_%s was modified after being applied (recorded %s, embedded %s); "+
						"shipped migrations are immutable — add a new migration instead",
					m.Version, m.Name, shortSum(prev.checksum), shortSum(m.Checksum))
			}
			continue
		}

		if err := applyOne(ctx, db, m, now); err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

func applyOne(ctx context.Context, db *sql.DB, m Migration, now func() int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %04d: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("apply migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, checksum, applied_unix) VALUES(?,?,?,?)`,
		m.Version, m.Name, m.Checksum, now(),
	); err != nil {
		return fmt.Errorf("record migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %04d_%s: %w", m.Version, m.Name, err)
	}
	return nil
}

// SchemaVersion reports the highest applied migration version, or 0 if the
// database has never been migrated.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

// LatestVersion reports the highest migration version embedded in this build.
func LatestVersion() (int, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return 0, err
	}
	if len(migrations) == 0 {
		return 0, nil
	}
	// LoadMigrations sorts ascending.
	return migrations[len(migrations)-1].Version, nil
}

// ErrSchemaOutdated reports an index written by an older build than this one.
//
// Read-only commands cannot migrate — the database is opened read-only by
// design, so a query never mutates the user's index. Without this check they
// instead fail deep inside a query with a raw driver error ("no such table:
// table_accesses"), which tells the user nothing about the real cause or the
// fix. See ErrSchemaOutdated.Error for the message they get instead.
type ErrSchemaOutdated struct {
	Found  int
	Wanted int
}

func (e *ErrSchemaOutdated) Error() string {
	return fmt.Sprintf(
		"index schema is v%d but this build of repocortex expects v%d — "+
			"run 'repocortex index' to upgrade it",
		e.Found, e.Wanted)
}

// CheckVersion verifies db is at the schema version this build expects.
//
// A database ahead of this build is also an error: an older repocortex cannot
// know what a newer migration changed, and guessing risks silent corruption.
func CheckVersion(ctx context.Context, db *sql.DB) error {
	found, err := SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	wanted, err := LatestVersion()
	if err != nil {
		return err
	}
	if found != wanted {
		return &ErrSchemaOutdated{Found: found, Wanted: wanted}
	}
	return nil
}
