package errs

// SQLite result-code classification.
//
// This is the one place in RepoCortex that knows what a driver error means, and
// it is what makes "should I retry this?" answerable without matching on
// message text.
//
// # Why there is no import of modernc.org/sqlite here
//
// The driver returns *sqlite.Error, whose only useful method is Code() int —
// the raw SQLite result code. That is all we need, so this package matches
// the SHAPE rather than the type:
//
//	err.(interface{ Code() int })
//
// Two reasons. First, it keeps errs a leaf package: every package that wants
// to classify an error would otherwise pull in the driver and, through it,
// modernc.org/libc — a heavy dependency for pkg/model or internal/parse to
// acquire just to name a failure kind. Second, it means a future move to a
// different SQLite driver (or a second backend behind the store interface)
// costs nothing here, as long as it exposes result codes the same way.
//
// The cost of duck typing is that an unrelated error type with a Code() int
// method would be misread. Nothing in RepoCortex's dependency tree has one, and
// the codes below are narrow enough that a stray match almost certainly falls
// through to "".
//
// # This works on typed errors, not strings
//
// Worth stating plainly because it was the open question: modernc.org/sqlite
// does surface busy and locked as typed errors carrying the result code, so
// no string matching is involved. database/sql passes driver errors through
// unwrapped, so the code is still reachable at the leaf of the chain.
//
// The exception is errors SQLite reports as plain text rather than a result
// code — store.SchemaVersion already has to do strings.Contains(err,
// "no such table") for one of them. Those stay unclassified, which is the
// honest answer: KindOf reports KindInternal rather than guessing.

// SQLite primary result codes. Duplicated as untyped constants rather than
// imported so this file has no dependencies; they are part of SQLite's
// permanently stable ABI and cannot change.
const (
	sqliteBusy       = 5  // SQLITE_BUSY
	sqliteLocked     = 6  // SQLITE_LOCKED
	sqliteInterrupt  = 9  // SQLITE_INTERRUPT
	sqliteCorrupt    = 11 // SQLITE_CORRUPT
	sqliteProtocol   = 15 // SQLITE_PROTOCOL
	sqliteConstraint = 19 // SQLITE_CONSTRAINT
	sqliteNotADB     = 26 // SQLITE_NOTADB
)

// classifySQLite maps a driver error to a Kind, or "" if it is not a SQLite
// error or carries a code we do not classify.
func classifySQLite(err error) Kind {
	c, ok := err.(interface{ Code() int })
	if !ok {
		return ""
	}
	// SQLite packs an extended code into the high bytes (SQLITE_BUSY_SNAPSHOT
	// is 5 | 2<<8). The low byte is the primary code, and every distinction
	// we care about lives there.
	switch c.Code() & 0xff {
	case sqliteBusy, sqliteLocked, sqliteProtocol:
		// Contention, not damage. RepoCortex routinely has an index run, the
		// watcher and the MCP server on one database; SQLite is a single
		// writer, so these are expected and they clear. SQLITE_PROTOCOL is
		// the locking-protocol variant of the same situation.
		return KindTransient

	case sqliteInterrupt:
		// sqlite3_interrupt, which is how a cancelled context stops a query
		// in flight. Reporting it as a failure would make a user pressing
		// Ctrl-C look like a broken index.
		return KindCancelled

	case sqliteCorrupt, sqliteNotADB:
		// The file on disk is not a usable database. No retry helps; the
		// index has to be rebuilt.
		return KindCorrupt

	case sqliteConstraint:
		// A CHECK or unique-index violation. In RepoCortex these are not
		// incidental: the exact-implies-compiler CHECK and the partial
		// unique index over committed runs are how the graph invariants are
		// enforced in SQL. Hitting one means the requested state conflicts
		// with the state that exists.
		return KindConflict
	}
	return ""
}
