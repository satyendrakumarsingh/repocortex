package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Options configures a store. The zero value is usable for tests via
// OpenMemory; on-disk stores should set Path.
type Options struct {
	// Path is the SQLite file. Empty means a private in-memory database.
	Path string

	// ReadOnly opens without attempting migration. Used by query paths that
	// must never mutate the index.
	ReadOnly bool

	// BusyTimeout bounds how long a writer waits on a lock before failing.
	// Defaults to 5s. The watcher and MCP server can contend, and a hung
	// query is worse than a clear error.
	BusyTimeout time.Duration
}

// DB wraps a *sql.DB with RepoCortex's connection policy applied.
type DB struct {
	*sql.DB
	path string
}

// Path reports the on-disk location, or "" for in-memory stores.
func (d *DB) Path() string { return d.path }

// Open opens (creating if needed) a store and applies connection pragmas.
//
// It does not migrate; call Migrate explicitly so callers decide when schema
// changes happen. `repocortex init` migrates, query paths do not.
func Open(ctx context.Context, opts Options) (*DB, error) {
	dsn, path := buildDSN(opts)

	if path != "" {
		// 0o750: the index is a structural map of private source code and has
		// no reason to be world-readable.
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("create index directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// modernc's driver serialises access per connection. Indexing is a single
	// writer with many concurrent readers, which WAL handles well.
	db.SetMaxOpenConns(maxOpenConns(opts))
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	if err := verifyPragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{DB: db, path: path}, nil
}

func maxOpenConns(opts Options) int {
	if opts.Path == "" {
		// A shared in-memory DB must use one connection: each new connection
		// to ":memory:" would otherwise get its own empty database.
		return 1
	}
	return 8
}

// memCounter names in-memory databases uniquely.
//
// This is not cosmetic. `file::memory:?cache=shared` is a SINGLE database
// shared by every connection in the process, so two Open calls would silently
// hand back the same store: one test's writes would appear in another's reads.
// Naming each one (`file:repocortex-mem-N?mode=memory&cache=shared`) gives real
// isolation while still allowing several connections to reach the same
// database when needed.
var memCounter atomic.Uint64

// connectionPragmas are applied in the DSN so they take effect on EVERY
// connection the pool opens.
//
// This is not a style preference; it is a correctness requirement that cost us
// a real data-integrity bug.
//
// SQLite pragmas are PER-CONNECTION. An earlier version issued
// `db.ExecContext(ctx, "PRAGMA foreign_keys = ON")`, which borrows one
// connection from the pool, configures it, and returns it. The other seven ran
// with foreign keys OFF, so ON DELETE CASCADE silently did not fire — and the
// verification query that followed happened to reuse the connection just
// configured, so the check could never detect it.
//
// A 30-second soak with concurrent readers produced 1,010 dangling symbols
// (a symbol whose file row is gone — exactly the failure spec §44 requires us
// to prevent) and 505 orphaned FTS rows that later abort indexing with a UNIQUE
// constraint violation once FTS5 recycles a rowid. Sampling 64 connections
// showed 62 with foreign keys off.
//
// Anything that must hold for all queries belongs here, never in an Exec.
func connectionPragmas(opts Options) []string {
	return []string{
		// Referential integrity. The graph invariant gate depends on it.
		"foreign_keys(1)",
		"busy_timeout(" + itoa(busyMillis(opts)) + ")",
		"synchronous(NORMAL)",
		"temp_store(MEMORY)",
		"cache_size(-64000)",
	}
}

func buildDSN(opts Options) (dsn, path string) {
	params := make([]string, 0, 8)
	for _, p := range connectionPragmas(opts) {
		params = append(params, "_pragma="+p)
	}

	if opts.Path == "" {
		name := fmt.Sprintf("repocortex-mem-%d", memCounter.Add(1))
		return "file:" + name + "?mode=memory&cache=shared&" + strings.Join(params, "&"), ""
	}

	path = opts.Path
	if !opts.ReadOnly {
		// WAL is a database-level setting, but issuing it per connection is
		// harmless and guarantees it regardless of which connection opens first.
		params = append(params, "_pragma=journal_mode(WAL)")
	} else {
		params = append(params, "mode=ro")
	}
	return "file:" + path + "?" + strings.Join(params, "&"), path
}

func busyMillis(opts Options) int {
	d := opts.BusyTimeout
	if d <= 0 {
		d = 10 * time.Second
	}
	return int(d / time.Millisecond)
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

// verifyPragmas confirms the DSN settings actually took effect.
//
// It samples CONCURRENTLY and across more goroutines than the pool holds, which
// is the only way to observe a per-connection setting that was applied to just
// one connection. Checking once on a single borrowed connection is what let the
// original bug through — the check reused the connection it had configured.
func verifyPragmas(ctx context.Context, db *sql.DB) error {
	const samples = 32

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		bad int
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
				bad++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if bad > 0 {
		return fmt.Errorf("foreign_keys is off on %d of %d sampled connections; "+
			"ON DELETE CASCADE would silently not fire and the graph would accumulate "+
			"dangling rows", bad, samples)
	}
	return nil
}

// OpenMemory is a convenience for tests: an in-memory store, fully migrated.
func OpenMemory(ctx context.Context) (*DB, error) {
	db, err := Open(ctx, Options{})
	if err != nil {
		return nil, err
	}
	if _, err := Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// VerifyFTS5 confirms the FTS5 module is present in this build.
//
// modernc.org/sqlite enables FTS5 via a codegen flag but does not document it.
// Undocumented means unpromised, so RepoCortex checks at startup rather than
// discovering it as a mangled search result later.
func VerifyFTS5(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return fmt.Errorf("read compile_options: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var opt string
		if err := rows.Scan(&opt); err != nil {
			return err
		}
		if opt == "ENABLE_FTS5" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return fmt.Errorf("SQLite build lacks FTS5 (ENABLE_FTS5 absent); " +
		"search is unavailable — this indicates an unexpected modernc.org/sqlite build")
}
