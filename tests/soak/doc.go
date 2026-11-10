// Package soak holds RepoCortex's endurance tests: the failure modes that only
// become visible after a process has been running for a long time.
//
// Everything else in the test suite answers "is this correct?" in a few
// seconds. These answer a different question — "is this still correct after an
// hour, and did it cost more memory, more goroutines or more watch descriptors
// than it did at the start?" A watcher that leaks one goroutine per deleted
// directory passes every unit test in the repository and dies on a developer's
// machine after an afternoon of branch switching. That gap is what lives here.
//
// # Running them
//
// Every test file in this package carries the `soak` build tag, so the suite is
// invisible to `go test ./...` and to CI's per-PR gate. That is deliberate: a
// slow test in the default path is a test people learn to skip, and a skipped
// test protects nothing. Ask for it by name:
//
//	go test -tags soak -timeout 2h ./tests/soak/... -soak.duration=1h
//
// Locally, the default duration is short enough to run before a commit:
//
//	go test -tags soak -race -v ./tests/soak/...
//
// `-soak.duration` is the budget for EACH test, not for the suite, so the wall
// time of a full run is roughly the duration multiplied by the number of tests
// (currently four). Set `-timeout` accordingly — Go's default 10m test timeout
// will kill anything longer.
//
// Flags, each with a `CORTEX_SOAK_*` environment variable equivalent so CI
// can configure a run without rewriting the command line:
//
//	-soak.duration   per-test budget                (CORTEX_SOAK_DURATION, default 30s)
//	-soak.files      synthetic repository size      (CORTEX_SOAK_FILES,    default 200)
//	-soak.interval   pause between mutations        (CORTEX_SOAK_INTERVAL, default 3ms)
//	-soak.seed       mutation stream seed           (CORTEX_SOAK_SEED,     default 1)
//
// Run with `-v`. Every test logs a resource report — sampled goroutine counts,
// heap trend, iterations, events — because a soak test whose only output is
// PASS teaches nothing about how close the system is to the cliff.
//
// # What is covered
//
//   - TestWatcherEndurance          — sustained file churn through the watcher,
//     with convergence checked after quiesce.
//   - TestWatcherDirectoryChurn     — repeated directory create/delete, checking
//     that watch descriptors are released.
//   - TestIndexQueryConcurrencySoak — continuous incremental indexing under
//     concurrent readers.
//   - TestRepeatedFullIndexStability — the same tree indexed N times, digest and
//     resource footprint compared.
//
// # House rules for anything added here
//
//  1. Never touch the real repository. Everything runs against a synthetic tree
//     under t.TempDir().
//  2. Never call t.Parallel(). Goroutine counts are process-wide, so a second
//     test running alongside makes the leak check meaningless.
//  3. Sample only after settling — see settle() in harness_test.go. Go's GC is
//     lazy and a goroutine that has returned is still counted for a moment, so
//     an unsettled sample produces exactly the flaky failure that gets a soak
//     suite deleted.
//  4. On failure, print the delta and the whole sampled series. "Leaked" is not
//     a bug report; "goroutines went 24 → 31 → 58 → 91 over four minutes" is.
package soak
