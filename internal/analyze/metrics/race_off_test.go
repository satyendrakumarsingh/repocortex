//go:build !race

package metrics

// raceEnabled reports whether this test binary was built with -race.
//
// The cycle-budget test asserts a 2s wall clock because that is what "the
// metrics screen stays interactive" means. Under the race detector the same
// work costs 2-3x, so the assertion would either flake on a slow runner or
// have to be widened to a number that no longer says anything about the
// screen. The budget's CORRECTNESS — that it truncates, deterministically —
// still runs under -race; only the wall-clock comparison is skipped there.
const raceEnabled = false
