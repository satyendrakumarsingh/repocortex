// Package errs adds machine-checkable failure kinds to RepoCortex's errors.
//
// # What this package is for, and what it is not for
//
// Go already has an error framework: wrapping with %w, errors.Is and
// errors.As. That framework answers "which specific error is this?" perfectly
// well, and this package does not replace it — an *Error unwraps like any
// other wrapper, so every existing errors.Is against a sentinel keeps working
// through one.
//
// What wrapping does NOT answer is "what CLASS of failure is this, and what
// should I do about it?" A caller holding an error from the registry cannot
// tell a SQLite lock it should retry from a corrupt index it must never
// retry, except by matching on message text. That is the gap this package
// closes, and it is the only gap it closes.
//
// So the rule for migration is narrow:
//
//   - Use a Kind when a CALLER WILL BRANCH ON IT. Retry logic, exit codes,
//     MCP error responses, "reindex required" advice, distinguishing a
//     cancelled run from a failed one.
//
//   - Otherwise keep the plain fmt.Errorf("...: %w", err) you already have.
//     The vast majority of RepoCortex's wraps are internal breadcrumbs that
//     nothing branches on. Converting them buys nothing and costs an
//     allocation and a line of noise each.
//
// A useful test: if you cannot name the caller that would switch on the kind,
// do not add one.
//
// Kinds belong at package boundaries — the exported function a different
// package calls — not on every internal helper. A kind applied deep inside a
// package tends to describe an implementation detail rather than something
// the caller can act on, and the outermost annotation wins anyway (see
// KindOf).
//
// # Retryability is the point
//
// IsRetryable is the single most valuable thing here. SQLite lock contention
// between the indexer, the watcher and the MCP server is normal and resolves
// on its own; a corrupt index, a rejected config value and a run that is
// already committed never will. Before this package the only way to tell them
// apart was strings.Contains, which is a bug waiting for a message reword.
//
// # Fields must never carry secrets or source content
//
// Fields exists so a caller can act on a failure without parsing its message:
// a run id to retry, a path to re-walk, a migration version to report. It is
// NOT a debug dump.
//
// Safe to put in Fields: identifiers (run ids, symbol ids), counts, repo-
// relative paths, enum values that are already public vocabulary ("exact",
// "fast"), schema versions, SQLite result codes.
//
// Never put in Fields: file contents or any excerpt of them, config VALUES
// (a rejected value may be a token or an internal hostname — record the
// config KEY instead), environment variables, absolute paths outside the
// repo, credentials, or anything from .cortex.yml verbatim. Repository
// config is attacker-controlled; see internal/config.
//
// This is enforced structurally as far as it can be: Error.Error never
// renders Fields, so a mistake here cannot leak into CLI output or a log line
// by accident. A caller that wants to log fields must reach for them
// deliberately.
//
// # What this package deliberately does not have
//
// No error codes for humans, no stack traces, no severity, no HTTP status
// mapping, no registry of error constructors per package. Each of those has
// been left out because nothing in RepoCortex branches on it today, and a
// taxonomy that grows a kind per call site stops being machine-checkable.
// The set of kinds is meant to stay closed.
package errs
