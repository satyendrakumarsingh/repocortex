//go:build !race

package context

// raceEnabled reports whether this test binary was built with -race.
//
// The latency gate consults it because a number measured under the race
// detector is not the number the spec target is about: instrumentation costs
// roughly 5-20x, and asserting a 2s budget against it would either fail on a
// perfectly good build or force the budget up to a value that no longer means
// anything. The correctness tests still run under -race, which is what -race is
// for; the timing tests run on the uninstrumented binary, which is what ships.
const raceEnabled = false
