package pipeline

import "fmt"

// Completeness is a different guarantee from atomicity, and the index needs
// both.
//
// The two-phase swap in internal/registry proves a run is published
// all-or-nothing: readers see the previous revision or the new one, never a
// half-built index. What it cannot prove is that the new revision covers
// everything the walker found. A run that discovered 1,200 files, processed
// 1,163 of them and published those 1,163 atomically is, by every check the
// registry applies, a healthy `ready` run. It is also missing 37 files, and
// every query against it will answer confidently and wrongly about them.
//
// That failure has been observed in the wild under load, with no error
// returned, which is exactly what makes it dangerous: a silent shortfall is
// indistinguishable from a repository that genuinely has fewer files.
//
// The invariant that closes the gap:
//
//	FilesIndexed + FilesSkipped + ParseFailures == FilesDiscovered
//
// Every discovered file must leave the pipeline through exactly one of three
// terminal outcomes. Note that skipped and failed are ACCOUNTED FOR, not
// shortfall: a file that is too large, has no grammar, or is malformed is a
// known, recorded gap that `cortex status` already reports. Shortfall is the
// fourth category — a file that was discovered and then left the run through
// none of the three doors.

// IndexState names how much of what a run discovered the run actually covered.
//
// It is orthogonal to model.IndexStatus, which describes the run's lifecycle
// (indexing / ready / degraded / failed). A run can be `ready` and partial, and
// that combination is precisely the one worth warning about.
type IndexState string

const (
	// IndexStateComplete means every discovered file was accounted for.
	IndexStateComplete IndexState = "complete"

	// IndexStatePartial means the run did not cover everything it discovered.
	// The index is still internally consistent and still worth publishing —
	// it just must not be presented as a full picture of the repository.
	IndexStatePartial IndexState = "partial"
)

// Reconciliation is the completeness proof for one run.
//
// It is computed once, after the worker pool has been joined, as pure
// arithmetic over the final counter values. That is deliberate: it adds
// nothing to the hot path, needs no lock and no extra atomic, and its result
// cannot depend on worker scheduling — for any run that terminates normally
// the same tree yields the same reconciliation at any pool width.
type Reconciliation struct {
	// Discovered is what the walker handed the pipeline.
	Discovered int64

	// Indexed, Skipped and Errored are the three terminal outcomes. Skipped
	// and Errored are accounted for, not shortfall.
	Indexed int64
	Skipped int64
	Errored int64

	// Accounted is Indexed + Skipped + Errored.
	Accounted int64

	// Unaccounted is Discovered - Accounted: files the walker found that
	// reached none of the three terminal outcomes. Zero on a healthy run.
	//
	// A negative value means a file was counted twice, which is a different
	// bug with the same consequence — the numbers cannot be trusted — so it
	// also marks the run partial.
	Unaccounted int64

	// State is Complete only when Unaccounted is exactly zero.
	State IndexState

	// Cancelled records that the run was cut short by its context — the caller
	// cancelling, a deadline expiring, or a sink failure aborting the pool.
	//
	// It separates the two kinds of shortfall. A cancelled run is expected to
	// be short — the user pressed Ctrl-C — and the count varies with
	// scheduling because work in flight is abandoned. A shortfall WITHOUT
	// cancellation is the silent-loss bug this check exists to catch, and its
	// count is deterministic.
	Cancelled bool
}

// Complete reports whether the run covered everything it discovered.
func (r Reconciliation) Complete() bool { return r.State == IndexStateComplete }

// Warning is the human- and agent-readable statement of the shortfall, empty
// when the run is complete.
//
// It names the count, because "the index may be incomplete" is not actionable
// and "37 of 1200 discovered files are missing" is.
func (r Reconciliation) Warning() string {
	switch {
	case r.Complete():
		return ""
	case r.Unaccounted < 0:
		return fmt.Sprintf(
			"index run is partial: accounting is inconsistent, %d files were counted "+
				"against %d discovered; the published index cannot be trusted to cover the repository",
			r.Accounted, r.Discovered)
	case r.Cancelled:
		return fmt.Sprintf(
			"index run is partial: cancelled with %d of %d discovered files unprocessed; "+
				"the published index does not cover the whole repository",
			r.Unaccounted, r.Discovered)
	default:
		return fmt.Sprintf(
			"index run is partial: %d of %d discovered files were neither indexed, "+
				"skipped nor recorded as failed; the run was published atomically but "+
				"does not cover the whole repository",
			r.Unaccounted, r.Discovered)
	}
}

// String renders the reconciliation compactly for logs.
func (r Reconciliation) String() string {
	return fmt.Sprintf("%s(discovered=%d indexed=%d skipped=%d errored=%d unaccounted=%d)",
		r.State, r.Discovered, r.Indexed, r.Skipped, r.Errored, r.Unaccounted)
}

// reconcile derives the completeness proof from a run's final counts.
//
// Pure function, no receiver, no state: it is the whole check, and it is
// testable without a filesystem or a worker pool.
func reconcile(s Stats, cancelled bool) Reconciliation {
	r := Reconciliation{
		Discovered: s.FilesDiscovered,
		Indexed:    s.FilesIndexed,
		Skipped:    s.FilesSkipped,
		Errored:    s.ParseFailures,
		Cancelled:  cancelled,
	}
	r.Accounted = r.Indexed + r.Skipped + r.Errored
	r.Unaccounted = r.Discovered - r.Accounted
	if r.Unaccounted == 0 {
		r.State = IndexStateComplete
	} else {
		r.State = IndexStatePartial
	}
	return r
}
