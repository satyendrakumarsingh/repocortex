package registry

// In-place updates to the committed run.
//
// By default a committed run is immutable: ReplaceFile and NewWriter both
// refuse it, so the two-phase swap is the only way new content becomes visible.
// That is the right default, and full indexing keeps it.
//
// Incremental indexing needs something different, and the reason is arithmetic.
// The alternative — seed a fresh staging run from the committed one, apply the
// changes, then swap — copies every unchanged row on every update. At the
// measured write throughput (~35k rows/s) a 50k-file repository costs seconds
// per pass, so a file watcher reacting to a single keystroke-save would spend
// its time copying rows that did not change. That defeats the entire purpose of
// incremental indexing, which spec §12 defines as scaling with the number of
// changed files rather than repository size.
//
// So in-place update is supported, but only when the caller asks for it by
// name. What you keep and what you give up:
//
//   KEEP: per-file atomicity. Each ReplaceFile or DeleteFile is a single
//   transaction, so a reader observes a file either entirely before or
//   entirely after the change. The half-replaced file the guard warns about
//   cannot happen — SQLite prevents it.
//
//   GIVE UP: cross-file consistency during a pass. An incremental update
//   touching twenty files is twenty transactions, so a reader running
//   concurrently may see some updated and some not. The graph stays
//   internally valid throughout; it simply reflects a moment in the middle of
//   the pass.
//
// That trade is acceptable here because the index describes a working tree
// that is itself being edited. A developer who saves a file while an agent is
// querying does not expect the index to represent an instant that never
// existed on disk — they expect it to catch up quickly.
//
// Full indexing must never use this. It builds a staging run and swaps, which
// is why `cortex index` remains all-or-nothing and an interrupted full index
// leaves the previous revision untouched.

// AllowCommittedRun permits mutating the currently committed run.
//
// Required for incremental updates. Read the package comment above before
// using it: it exchanges cross-file consistency for update cost, deliberately.
func AllowCommittedRun() ReplaceOption {
	return func(o *replaceOpts) { o.allowCommitted = true }
}
