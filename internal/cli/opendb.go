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

// openPlaceholder creates an empty, migrated schema at path, for a server
// that has to exist before the project's index does. See serve_noindex.go.
//
// It is here, and not beside its one caller, because this file is the only
// place in the package allowed to open the store: the schema version is
// asserted at every door or it is asserted at none. The path is always in a
// temporary directory the caller removes, never in the project. Unlike
// openWritable it logs nothing, since "upgraded index schema" would describe
// an index the user does not have.
func openPlaceholder(ctx stdctx.Context, path string) (*store.DB, error) {
	db, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx, db.DB, time.Now().Unix); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// stdSQLNamed is sql.Named, kept here so query commands bind a named
// parameter without importing database/sql themselves: this file is the one
// door to the store in this package.
func stdSQLNamed(name string, value any) any { return sql.Named(name, value) }

// openWritableCurrent opens the index for writing WITHOUT migrating it, and
// refuses one whose schema is not this build's.
//
// For a long-running process that writes on the user's behalf in the
// background (serve's edit check): upgrading the schema there would change
// the user's index in the middle of an agent's request, unannounced. The
// mismatch is returned for the caller to report, and 'repocortex migrate'
// stays the one place an upgrade happens.
func openWritableCurrent(ctx stdctx.Context, path string) (*store.DB, error) {
	db, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		return nil, err
	}
	if err := store.CheckVersion(ctx, db.DB); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
