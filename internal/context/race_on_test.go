//go:build race

package context

// See race_off_test.go for why the latency gate needs to know.
const raceEnabled = true
