// Package context assembles the minimum sufficient engineering context for a
// plain-English task.
//
// # The requirement is not "retrieve everything relevant"
//
// An agent has a finite context window, and every token spent on a file it did
// not need is a token it cannot spend on thinking. The objective this package
// optimises is therefore USEFUL ENGINEERING UNDERSTANDING PER TOKEN, not
// recall. Returning fifty files is a failure even when the right five are among
// them: the agent still has to read fifty, and the noise degrades its
// reasoning. Spec §43's golden test encodes exactly that — the assertion that
// carries the weight is the one about what must be ABSENT.
//
// # Why lexical retrieval alone cannot work
//
// The bundled payment fixture contains the trap in miniature. The file
// unrelated/marketing/MarketingReport.java carries the comment "Unrelated to
// payments", so for the task "Add idempotency to payment callback processing"
// full-text search ranks it ABOVE CallbackController — the word "payment"
// appears more prominently in a file that has nothing to do with payments. Any
// engine that scores by lexical similarity returns it.
//
// What separates the two is not vocabulary but structure: CallbackController
// sits on a call path, MarketingReport shares no dependency edge with anything
// the task names. So graph connectivity is an ADMISSION GATE here, not merely a
// ranking signal (see admit in rank.go). Lexical evidence alone admits a
// candidate only when it is corroborated — several distinct task terms, not one
// word in a comment.
//
// # Determinism
//
// No LLM calls, no network, no clocks in the ranking path. The same task
// against the same index produces a byte-identical Pack, because an agent that
// gets a different answer on a retry cannot build a cache, a test, or trust.
// Every map in this package is iterated through a sorted key slice for that
// reason.
//
// # Honesty about what was left out
//
// Every Item carries a non-empty Why, and notable rejections are reported in
// Pack.Excluded with the reason. "We can tell you what we did not send you, and
// why" is the difference between a bounded answer and a silently partial one —
// the same principle the graph package applies to truncated traversals.
//
// # Query shape
//
// One Assemble call issues a bounded, fixed number of queries: two full-table
// reads (files, symbols) that build the in-memory catalogue, one full-text
// query per task term, one query per BFS level inside graph.Traverse, and at
// most Budget.MaxFiles content reads for the units that survive ranking. There
// is deliberately no per-symbol or per-edge query anywhere: an N+1 over a
// 94,000-symbol registry is how an interactive tool stops being interactive.
//
// # Measured performance
//
// From bench_test.go, 160 samples across four tasks, Apple M-series, in-memory
// store, CGO_ENABLED=0, without the race detector:
//
//	repository size            p50      p95      p99
//	payment fixture (41 syms)  1.5ms    2.5ms    4.6ms
//	synthetic 10,000 symbols   94ms     105ms    128ms
//	synthetic 94,000 symbols   852ms    1.34s    1.54s
//
// The 2s target holds at every size, but the margin at 94,000 symbols is thin
// and the reason is known rather than mysterious: BenchmarkLoadCatalogue shows
// the two full-table reads account for 79ms of the 138ms spent at 10,000
// symbols, and they scale linearly while everything after them is bounded by
// the budget. If this ever regresses, the catalogue is the thing to fix — by
// caching it per index revision, which is safe because a committed run is
// immutable — and not the ranking.
//
// Timing is not asserted under -race. Instrumentation costs 5-20x, so a budget
// that passed there would have to be loose enough to mean nothing; see
// race_off_test.go.
package context
