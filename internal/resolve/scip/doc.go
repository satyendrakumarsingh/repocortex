// Package scip ingests SCIP indexes produced by compiler-backed indexers and
// translates them into RepoCortex symbols and relationships.
//
// # Why this package exists
//
// Everything else in internal/resolve works from a syntax tree. A syntax tree
// can tell you that the text `svc.updateStatus()` is a call expression; it
// cannot tell you *which* updateStatus, because that requires a type for `svc`,
// which requires imports, which requires the build graph. Heuristic resolvers
// therefore top out at ConfidenceHigh, and honestly so.
//
// SCIP indexers do not guess. scip-typescript drives the TypeScript
// typechecker, scip-java runs as a compiler plugin, scip-go uses go/packages.
// Their output is what the compiler concluded, so it is the one input in the
// whole system that justifies model.ConfidenceExact.
//
// # The invariant
//
// An edge may carry ConfidenceExact only if its Evidence.Source is
// SourceCompiler. This is checked by model.Edge.Validate, by a SQL CHECK in
// migration 0001, and again by registry.validateRun before a run is published.
// This package is the only legitimate producer of exact edges, which makes it
// the place where the invariant is easiest to break. Two consequences shape the
// API:
//
//   - Relation.Edge is the single constructor for a model.Edge here. It sets
//     Confidence and Evidence.Source itself and runs Validate. No other code
//     path in this package builds a model.Edge literal.
//
//   - Nothing this package cannot prove becomes an edge. SCIP does not record
//     whether a reference is a call, so no CALLS edge is ever derived from the
//     index alone — a plausible CALLS edge stamped "exact" would be exactly the
//     lie the confidence system exists to prevent. See mapping notes on
//     Ingester.Ingest.
//
// # Calls
//
// CALLS edges ARE produced, by PromoteCalls, and the distinction from the rule
// above is the whole of its justification. A call site is two claims — "this is
// a call" and "it reaches that definition" — and they arrive from two
// independent sources: the first from RepoCortex's own tree-sitter parser, which
// recorded a call expression at a file and line, the second from the compiler
// index. PromoteCalls joins them and rewrites the REFERENCES edge at that
// position into a CALLS edge. Neither half is inferred, so the exact stamp
// still means what it says. Where the compiler resolved a call line to more
// than one target, the edge stays REFERENCES: there is no single answer, and
// choosing one is the inference this package refuses to make. calls.go states
// the argument in full; kinds.go records the objection it answers.
//
// # Memory
//
// SCIP indexes for large repositories run to hundreds of megabytes, so the
// index is never loaded whole. Ingest streams it document by document and hands
// each document's facts to a caller-supplied EmitFunc; peak heap is a function
// of the largest single document, not of the index. Collect exists for tests
// and small indexes and deliberately gives that property up.
//
// # Transactions
//
// This package performs no database writes. It returns values shaped for
// registry.ReplaceFile so the caller decides what a transaction is — ingesting
// a 200 MB index inside one transaction would hold a write lock for minutes.
//
// Note that landing a whole index is inherently two passes: a reference in one
// document points at a definition in another, so no edge can be written until
// every symbol has a rowid. That is why the package returns Relation values
// keyed by FQN rather than finished edges, and why FileFacts.Edges takes a
// lookup. See TestFactsLandInTheRegistry for the shape of the caller's loop.
package scip
