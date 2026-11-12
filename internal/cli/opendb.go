package cli

import (
	stdctx "context"
	"database/sql"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// openWritable opens the index for a command that may modify it, bringing the
// schema up to date first.
//
// Migrations used to run only in `repocortex init`. Every other command opened
// the database and used it as-is, so upgrading the binary left existing users
// on their old schema forever: new tables were missing and new indexes were
// never created. The failure surfaced far from its cause — a raw
// "no such table: table_accesses" from inside a query — so the schema is now
// asserted at the single point where the database is opened.
//
// Migrate is idempotent and checksum-verified, so this is a no-op once the
// index is current.
func openWritable(ctx stdctx.Context, g *Global, path string) (*store.DB, error) {
	db, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		return nil, err
	}
	applied, err := store.Migrate(ctx, db.DB, time.Now().Unix)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if len(applied) > 0 {
		g.Log().Info("upgraded index schema", "migrations", applied)
	}
	return db, nil
}

// openReadOnly opens the index for a command that only queries it.
//
// Read-only commands deliberately do not migrate: a query must never mutate
// the user's index, and the database is opened read-only so it could not
// anyway. They report the mismatch and name the fix instead.
func openReadOnly(ctx stdctx.Context, path string) (*store.DB, error) {
	db, err := store.Open(ctx, store.Options{Path: path, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if err := store.CheckVersion(ctx, db.DB); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// stdSQLNamed is sql.Named, kept here so query commands bind a named
// parameter without importing database/sql themselves: this file is the one
// door to the store in this package.
func stdSQLNamed(name string, value any) any { return sql.Named(name, value) }
