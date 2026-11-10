// Package security is the last thing that runs before indexed content leaves
// the machine.
//
// RepoCortex indexes private source and serves excerpts of it to AI coding agents
// over MCP. That transport is the whole reason this package exists: a line of
// code returned to an agent is a line of code sent to a third party. Spec §24
// and §57 draw the line at values — existence and location may be reported,
// the credential itself may not:
//
//	exists: true, source: Kubernetes Secret, key: DB_PASSWORD   ✅
//	value: hunter2                                              ❌
//
// THE ERROR MODES ARE NOT SYMMETRIC. A false positive redacts a line of code:
// the agent loses a little context and someone files an annoyed issue. A false
// negative ships a live credential to a model provider's logs, and no release
// fixes that — the credential has to be rotated, assuming anyone notices. Every
// judgement call in this package therefore leans toward redacting, and every
// place where it leans the other way (to keep the tool usable) says why.
//
// Two structural decisions follow from the same reasoning:
//
//   - Only the standard library. A supply-chain compromise of a dependency in
//     the redaction path would be invisible and total. stdlib regexp is also
//     RE2: linear time, no catastrophic backtracking, so hostile input cannot
//     turn redaction into a denial of service. FuzzRedact holds us to that.
//
//   - Everything compiles once, at package init. Redact sits on the hot path
//     of every MCP response; a regexp compiled per call would be a performance
//     bug that only shows up under the load we care about.
//
// The package covers four concerns:
//
//	Detect / Redact    content — replace secret values, keep structure
//	IsSensitivePath    paths whose entire content is a credential
//	ValidatePath       path arguments arriving from an agent (traversal)
//	ContainedPath      the same, errs-typed and stricter — prefer it in NEW code
//	OpenContained      ContainedPath plus a re-checked open, closing the TOCTOU
//
// ValidatePath and ContainedPath are not interchangeable: ContainedPath
// requires the path to exist, refuses a symlinked final component and refuses
// a literal "..". containment.go's package comment lists every difference.
// Existing callers stay on ValidatePath because they match its sentinel
// errors with errors.Is.
package security
