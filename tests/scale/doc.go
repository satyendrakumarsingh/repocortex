// Package scale holds RepoCortex's size tests: does each query stay bounded
// when the repository behind it is large?
//
// The unit tests answer "is this correct?" on fixtures of a dozen symbols, and
// a query that is correct on a dozen symbols can still read every edge in the
// index once per file. That exact defect shipped inside a feature branch here:
// `repocortex hotspots` was right on every unit test and did not return in two
// minutes on a 19,000-edge index. Nothing but size finds that class of bug, so
// size gets its own suite.
//
// # One harness, two sizes
//
// Every test here runs in the default `go test ./...`, at a size small enough
// to finish in seconds under the race detector. The same tests take the real
// measurement when asked for a larger fixture:
//
//	go test ./tests/scale/                                    # the CI gate: 5,000 symbols
//	go test ./tests/scale/ -v -scale.symbols=250000           # a large monorepo
//	go test ./tests/scale/ -v -timeout 60m -scale.symbols=1000000 -scale.source-files=20000
//
// This is deliberately not a build tag. A tagged suite is invisible to the
// compiler, to vet and to the default run, so it rots until the night someone
// needs it; a suite that always runs small cannot.
//
// Flags, each with an environment variable so CI can set a size without
// rewriting a command line:
//
//	-scale.symbols        symbols in the synthetic graph   (REPOCORTEX_SCALE_SYMBOLS,      default 5000)
//	-scale.source-files   files in the generated source    (REPOCORTEX_SCALE_SOURCE_FILES, default 150)
//	-scale.commits        commits in the synthetic history (REPOCORTEX_SCALE_COMMITS,      default 400)
//
// Run with -v. Each test logs what it built and what it measured, because a
// scale test whose only output is PASS says nothing about how close the system
// is to its budget.
//
// # What is asserted, and why those things
//
// Three properties, in descending order of how much they matter:
//
//  1. THE ANSWER IS BOUNDED. A response to an agent must fit the host's inline
//     limit whether the index holds five thousand symbols or five million. An
//     answer that grows with the repository stops being read.
//  2. MEMORY IS BOUNDED. A streaming scan must not retain memory in proportion
//     to what it scanned. This is architectural invariant 3, and it is checked
//     by comparing retained heap before and after, never by trusting the code.
//  3. TIME GROWS NO WORSE THAN LINEARLY. Budgets are a fixed allowance plus an
//     allowance per symbol, so the same test fails at 5,000 symbols and at a
//     million for the same reason: something became quadratic.
//
// Budgets are generous on purpose. They are tripwires for a change in the
// shape of a cost, and a tight budget on a shared CI runner is a flaky test
// that teaches everyone to re-run instead of read. The measured figures are in
// docs/operations/scale-testing.md; the budgets here are several times looser.
//
// # House rules
//
//  1. Never touch the real repository. Everything is generated under
//     t.TempDir() or in memory.
//  2. Never call t.Parallel(). Heap figures are process-wide.
//  3. A budget states its two terms (fixed, per symbol) where it is declared,
//     so a reader can see what the test allows without running it.
package scale
