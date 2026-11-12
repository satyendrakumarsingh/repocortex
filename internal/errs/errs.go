package errs

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
)

// Kind classifies a failure into one of a closed set of classes a caller can
// act on. The zero Kind means "unclassified" and is not one of the classes;
// see KindOf.
//
// The set is deliberately small. Every kind below earns its place by having a
// caller in this repository that would behave differently for it — that is
// the bar for adding another one.
type Kind string

const (
	// KindInvalid means the input was rejected before anything happened.
	// Retrying with the same input is pointless; the user or caller has to
	// change something.
	//
	// Real instances: config.Validate rejecting accuracy.mode or
	// context.max_graph_depth, registry.BeginRun rejecting an accuracy mode,
	// ReplaceFile rejecting an edge whose placeholder has no symbol, a
	// migration filename that is not NNNN_name.sql.
	KindInvalid Kind = "invalid"

	// KindNotFound means the thing asked for is absent. In RepoCortex this is
	// usually a NORMAL state rather than a fault — a freshly initialised
	// index has no committed run — which is exactly why it needs its own
	// kind: callers fall back instead of failing.
	//
	// Real instances: registry.ErrNoCommittedRun, registry.ErrRunNotFound,
	// sql.ErrNoRows behind them, git.ErrNotRepository, a missing index file.
	KindNotFound Kind = "not_found"

	// KindConflict means the request was well-formed but the current state
	// forbids it. Distinct from KindInvalid because the input is fine: the
	// same call may succeed once the state changes, so the fix is different
	// (do something else first, not correct your arguments).
	//
	// Real instances: ReplaceFile refusing a committed run without
	// AllowCommittedRun, CommitRun on a run that is already committed,
	// AbortRun refusing to abort the committed run, duplicate migration
	// version, a SQLITE_CONSTRAINT from the invariant CHECKs.
	KindConflict Kind = "conflict"

	// KindCorrupt means stored state violates an invariant the rest of the
	// system relies on. Never retryable and never recoverable in place: the
	// index has to be rebuilt or the database replaced.
	//
	// Keeping this apart from KindInternal matters because the remedy
	// differs. An internal bug needs a fix from us; corruption needs a
	// reindex from the user, and telling them to retry would be actively
	// harmful.
	//
	// Real instances: a shipped migration edited after being applied
	// (checksum mismatch), edges claiming confidence=exact without
	// source=compiler, edges straddling two index runs, dangling edges found
	// by `repocortex doctor`, foreign_keys failing to take effect,
	// SQLITE_CORRUPT / SQLITE_NOTADB.
	KindCorrupt Kind = "corrupt"

	// KindUnsupported means the request is valid but cannot be served by this
	// build or for this input. Nothing is wrong; a capability is simply
	// absent, and the caller should degrade rather than fail.
	//
	// Real instances: no tree-sitter grammar for a file type, no tags query
	// for a grammar, a source file over MaxSourceBytes, a SQLite build
	// without ENABLE_FTS5 (search unavailable), the watched-directory limit.
	KindUnsupported Kind = "unsupported"

	// KindTransient means retrying the identical operation has a real chance
	// of succeeding. This is the kind the whole package is built around.
	//
	// SQLite is a single-writer store and RepoCortex routinely has three
	// processes on one database — an index run, the watcher and the MCP
	// server. Lock contention is expected, bounded by store.Options
	// .BusyTimeout, and resolves on its own. Everything else here does not,
	// which is why exactly one kind is retryable.
	//
	// Real instances: SQLITE_BUSY, SQLITE_LOCKED, SQLITE_PROTOCOL.
	KindTransient Kind = "transient"

	// KindCancelled means the caller asked to stop. It is not a failure and
	// must not be reported as one.
	//
	// This has teeth in RepoCortex: pipeline.Run already special-cases
	// context.Canceled so a cancelled pass is not mistaken for a broken one,
	// and `repocortex index` refuses to publish a cancelled run because a
	// partial index that looks complete is worse than no new index. A
	// cancelled operation is also NOT retryable — the caller wanted it to
	// stop.
	KindCancelled Kind = "cancelled"

	// KindInternal means an invariant in our own code was violated: a bug.
	// It is also the answer KindOf gives for any error it cannot classify,
	// so that an unclassified failure is never silently treated as benign or
	// as retryable.
	//
	// Real instances: LastInsertId failing after a successful insert, writing
	// through a closed Writer, a tree-sitter parser panic.
	KindInternal Kind = "internal"
)

// Error is an error carrying a Kind, the operation that produced it, and
// structured context.
//
// It is an ordinary wrapper: Unwrap returns the cause, so errors.Is and
// errors.As reach straight through it in both directions and every sentinel
// comparison that worked before still works.
//
// Not safe for concurrent mutation. Build it, then treat it as immutable.
type Error struct {
	// Kind classifies the failure. A zero Kind makes this wrapper
	// transparent to KindOf, which then keeps looking at the cause — useful
	// for attaching Op or Fields without asserting a classification.
	Kind Kind

	// Op names the operation, in "package.Function" form ("registry.CommitRun").
	// It prefixes the message.
	Op string

	// Err is the wrapped cause. May be nil only for errors built by New,
	// which supplies its own.
	Err error

	// Fields is machine-readable context: values a caller can act on without
	// parsing the message. Read the guidance in the package comment before
	// putting anything here — it must never carry secrets, config values or
	// source content. Fields is never rendered by Error, so a mistake cannot
	// leak into output on its own.
	Fields map[string]any
}

// Error renders "op: cause". Kind and Fields are deliberately absent: the
// prose is what a user reads, and it is meant to look exactly like the
// fmt.Errorf messages this package is migrating from, so adopting a kind
// never changes CLI output.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := "<nil cause>" // Wrap(kind, op, nil) is a bug; make it visible.
	if e.Err != nil {
		msg = e.Err.Error()
	}
	if e.Op == "" {
		return msg
	}
	return e.Op + ": " + msg
}

// Unwrap returns the cause, making *Error transparent to errors.Is/errors.As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is reports whether target is an *Error describing the same class of
// failure. It exists so callers already written against errors.Is can match
// on a kind:
//
//	errors.Is(err, &errs.Error{Kind: errs.KindTransient})
//
// Op participates only when the target sets it, so the target acts as a
// prototype: an empty field means "any". The idiomatic form is
// errs.Is(err, errs.KindTransient); this is the compatibility path.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || e == nil || t == nil {
		return false
	}
	if t.Kind != "" && t.Kind != e.Kind {
		return false
	}
	if t.Op != "" && t.Op != e.Op {
		return false
	}
	return true
}

// WithField attaches one piece of machine-readable context and returns the
// receiver, so it chains onto New or Wrap. The map is allocated lazily, on
// the failure path only.
//
// See the package comment for what may and may not go in here.
func (e *Error) WithField(name string, value any) *Error {
	if e == nil {
		return nil
	}
	if e.Fields == nil {
		e.Fields = make(map[string]any, 4)
	}
	e.Fields[name] = value
	return e
}

// New builds a classified error with no cause.
func New(kind Kind, op string, msg string) *Error {
	return &Error{Kind: kind, Op: op, Err: errors.New(msg)}
}

// Wrap classifies an existing error.
//
// Wrap never returns nil, even when err is nil. That is deliberate: a nil
// *Error returned through an `error` result is the classic typed-nil trap —
// it compares non-nil at the call site and every `if err != nil` downstream
// fires on a failure that never happened. Wrap belongs on the failure path
// only, and wrapping nil renders as "op: <nil cause>" so the mistake is
// visible rather than silent.
func Wrap(kind Kind, op string, err error) *Error {
	return &Error{Kind: kind, Op: op, Err: err}
}

// KindOf reports the class of err.
//
// It walks the chain from the outside in and returns the first classification
// it finds. Outermost wins on purpose: the code closest to the caller has the
// most context about what the failure means to that caller, so a deliberate
// re-classification is respected rather than overridden by a driver detail
// five layers down.
//
// A non-nil error with nothing to classify reports KindInternal — an
// unclassified failure is a failure we do not understand, and the safe
// reading of that is "a bug", never "benign" or "retryable".
//
// KindOf(nil) reports the zero Kind. It does not allocate.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	if k := classify(err); k != "" {
		return k
	}
	return KindInternal
}

// Is reports whether err is classified as kind. Safe on nil, which is never
// of any kind.
func Is(err error, kind Kind) bool {
	return err != nil && KindOf(err) == kind
}

// IsRetryable reports whether retrying the operation unchanged could
// plausibly succeed.
//
// Exactly one kind qualifies. That is the claim this package makes, and
// keeping it a single equality rather than a set is what stops the retry
// predicate from quietly widening: a caller that retries a KindConflict spins
// forever against unchanged state, and one that retries a KindCorrupt hammers
// a database that needs rebuilding. KindCancelled is excluded too — the
// caller asked to stop.
//
// Safe on nil. Does not allocate.
func IsRetryable(err error) bool {
	return err != nil && classify(err) == KindTransient
}

// classify walks the chain and returns the first kind found, or "" if the
// chain carries no classification at all.
//
// Hand-rolled rather than built on errors.As because this runs on every
// error a retry loop inspects: a type switch and an interface assertion cost
// nothing and allocate nothing, whereas errors.As goes through reflection.
func classify(err error) Kind {
	for err != nil {
		//nolint:errorlint // Deliberate: this function IS the chain walk. It
		// unwraps step by step below, so a type switch at each level is
		// correct and avoids errors.As's reflection on a hot retry path.
		switch e := err.(type) {
		case *Error:
			if e == nil {
				// A typed-nil *Error travelling inside a non-nil error
				// interface is the trap Wrap exists to prevent. It is a bug
				// wherever it came from, so say so rather than panicking.
				return KindInternal
			}
			if e.Kind != "" {
				return e.Kind
			}
			// A transparent *Error: keep looking at the cause.
			err = e.Err
			continue

		case interface{ Unwrap() []error }:
			// fmt.Errorf with several %w verbs, or errors.Join. Take the
			// first branch that carries a classification; there is no
			// principled way to merge two different kinds, and picking the
			// first keeps the result stable for a given error value.
			for _, sub := range e.Unwrap() {
				if k := classify(sub); k != "" {
					return k
				}
			}
			return ""
		}

		if k := classifyLeaf(err); k != "" {
			return k
		}

		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return ""
		}
		err = u.Unwrap()
	}
	return ""
}

// classifyLeaf classifies errors that come from outside RepoCortex and will
// never carry a Kind of their own.
func classifyLeaf(err error) Kind {
	// errors.Is, not ==. A syscall.Errno(ENOENT) matches fs.ErrNotExist only
	// through its Is method — it neither equals the sentinel nor unwraps to it —
	// so equality would classify a missing file as an internal fault.
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// Requirement: a cancelled or timed-out operation must never be
		// reported as an internal fault. pipeline.Run and `repocortex index`
		// both already depend on telling these apart from real failures.
		return KindCancelled
	case errors.Is(err, sql.ErrNoRows):
		// The registry turns this into ErrRunNotFound / ErrNoCommittedRun in
		// the paths it owns, but it also leaks from ad-hoc QueryRow calls.
		return KindNotFound
	case errors.Is(err, fs.ErrNotExist):
		return KindNotFound
	}

	if k := classifySQLite(err); k != "" {
		return k
	}

	// Some stdlib leaves answer to a sentinel without being equal to it —
	// syscall.Errno reports ENOENT as fs.ErrNotExist through this hook, and
	// it is how errors.Is finds it too. Probing the hook directly keeps the
	// walk faithful without paying for a second full traversal per sentinel.
	if x, ok := err.(interface{ Is(error) bool }); ok {
		switch {
		case x.Is(fs.ErrNotExist):
			return KindNotFound
		case x.Is(context.Canceled), x.Is(context.DeadlineExceeded):
			return KindCancelled
		}
	}
	return ""
}
