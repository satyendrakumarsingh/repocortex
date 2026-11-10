// Package diffgraph compares two committed index runs and reports what CHANGED
// ABOUT THE CODE'S FACTS, not about its lines.
//
// A line diff says "these 40 lines moved". This package says: this endpoint now
// writes `refunds`; this symbol lost its last caller; this CALLS edge fell from
// `exact` to `high` because the compiler stopped proving it; this table gained a
// writer. Those are answers a reviewer can act on without reading the patch.
//
// # Why this is sound here and nowhere else
//
// A graph delta between two runs is only meaningful if the two runs are
// themselves reproducible. Otherwise "this edge disappeared" may mean the code
// changed, or may mean the second run's worker pool interleaved differently.
//
// RepoCortex already proves the stronger property. `registry.RegistryDigest` is a
// sha256 over one run's content that is stable across 1/2/4/8 workers and
// sensitive to a single added symbol (docs/architecture.md, "The determinism
// digest"). Three of its rules are what make this package possible:
//
//   - Order independence — rows are read under an explicit total ORDER BY over
//     content columns, never by rowid.
//   - No synthetic identity — autoincrement ids are excluded, and edges
//     reference their endpoints by (file path, fqn, start line, start col)
//     rather than by src_id/dst_id, which are allocation artefacts that
//     legitimately differ between two identical runs.
//   - No timestamps — content is covered by content_hash and size instead.
//
// This package inherits rule two exactly: every identity below is content-based
// and no comparison ever touches a row id. A delta therefore means "the code
// changed" rather than "the scheduler differed" — a guarantee an LLM-backed
// competitor, whose two passes over the same repository do not even agree with
// each other, cannot offer.
//
// # Where the comparison key deliberately DIVERGES from the digest key
//
// The digest and the diff want opposite things from position.
//
// The digest hashes (path, fqn, start line, start col) because its job is to
// flip on ANY content change — a symbol that moved is a different digest, and
// that is correct for a reproducibility gate.
//
// The diff's job is the opposite: a symbol that moved down ten lines because
// somebody added an import is THE SAME SYMBOL. If the diff called it
// removed-and-added, a one-line import would produce a thousand-line report and
// the feature would be noise. So position is demoted from identity to
// attribute:
//
//	symbol identity = (file path, fqn, kind [, ordinal])
//	symbol attribute = start/end position, signature, exported, confidence
//
//	edge identity = (src symbol identity, dst symbol identity, edge kind)
//	edge attribute = confidence, evidence sites
//
// A position change on a matched key is reported as Moved. A confidence change
// on a matched edge key is reported as a transition with a direction, because
// `exact -> high` means a compiler stopped proving something and is worth
// surfacing on its own — it is invisible to every line-based tool.
//
// Ordinal exists only for the rare case where one file legitimately holds
// several symbols sharing (fqn, kind); it is assigned in position order so a
// uniform shift still matches. Its known limit is recorded on SymbolKey.
//
// # Honesty about what could not be compared
//
// A missing edge is not always a code change. If either run was `degraded`, or
// a file's parse status was not `ok`, or the two runs were indexed in different
// accuracy modes (a run without a compiler index cannot produce `exact` at
// all), then an absence may be an indexing gap. Report.Caveats states this
// explicitly and individual changes carry Uncertain. Saying so is the
// difference between a diff and a guess.
//
// # Bounding
//
// A diff across distant commits can be enormous. Every section is sorted by a
// total content order and then capped; Report.Truncations records exactly how
// many entries each section omitted, so a bounded report never silently lies
// about its own completeness.
//
// # Where the two runs come from
//
// registry.CommitRun publishes a run and DELETES the rows of the one it
// replaces, so a single index database holds exactly one committed run. The two
// sides of a real `cortex diff <ref>..<ref>` therefore normally live in two
// index files — the working index and a second index built at the other ref —
// which is why Compare takes a Side (source + run id) per end rather than one
// source and two ids. Two runs inside one store work equally well; nothing here
// assumes either arrangement.
//
// # Determinism of the output itself
//
// Identical inputs produce a byte-identical Report and a byte-identical
// Render. No ordering decision anywhere in this package iterates a Go map;
// maps are used for lookup only, and every emitted sequence is derived from an
// explicitly sorted slice.
package diffgraph
