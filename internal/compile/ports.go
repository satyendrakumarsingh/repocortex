package compile

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
)

// The ports this compiler depends on.
//
// A contract built from the context pack alone knows what the task is ABOUT and
// nothing about the system it lands in. Two facts the repository already states
// are missing from it, and both change what an agent should do:
//
//   - the DECLARED ARCHITECTURE. "Reporting must not write the payments table"
//     is a rule someone wrote down; a contract that omits it hands the agent a
//     brief that is silent on the one boundary the change is most likely to
//     cross. It is not an invention — the rule exists in .cortex.yml and the
//     constraint cites it by id.
//   - the IMPACT of the change. Scope answers "what does this task touch";
//     blast radius answers "what does touching it reach". A contract that lists
//     the handler and not its four callers has bounded the edit and not the
//     consequence.
//
// Both arrive through interfaces declared HERE, at the consumer, per CLAUDE.md
// §1 — and here that is not only style. internal/capability/architecture and
// internal/capability/impact both reach the SQLite driver transitively, and
// tests/architecture/layering_test.go holds this package to the rule that a
// reporting package must not know the database exists. So these ports carry
// this package's own small types rather than the capabilities' report structs,
// and the adapter that maps one onto the other lives with the caller that
// already has a database handle.
//
// internal/arch is the exception, and deliberately: it is pure declarative
// data — path globs, component names, rule ids — with no I/O anywhere in it,
// and restating arch.Rule as a near-identical local struct would create a
// second vocabulary for the same rule that a reader would then have to
// reconcile against the config file they wrote.
//
// Both ports are OPTIONAL. A zero Ports produces exactly the contract this
// package produced before they existed, and the fact that a source was not
// consulted is reported as an Unknown rather than left to look like an absence
// of findings.
type Ports struct {
	// Architecture supplies the declared component model. Nil means no
	// architecture was declared, or the caller chose not to consult it.
	Architecture ArchitectureView

	// Impact supplies the blast radius of a change to a scope symbol. Nil
	// means the caller chose not to consult it.
	Impact ImpactView
}

// ArchitectureView is the declared architecture, as this package needs it.
//
// Two methods: which component owns a path, and what rules exist. It does NOT
// expose evaluation — deciding whether an edge is a violation is
// internal/capability/architecture's job over the real graph, and a contract is
// written before any edge exists to judge.
type ArchitectureView interface {
	// ComponentOf names the declared component owning a repo-relative path, or
	// "" when no component claims it.
	ComponentOf(path string) string

	// Rules returns the declared rules in declaration order. The order is
	// load-bearing: constraints are emitted in it, so the contract does not
	// reshuffle when a rule's severity changes.
	Rules() []arch.Rule
}

// ImpactView answers "what would a change to this symbol reach?".
//
// One method, and it takes a symbol name because that is what a contract has:
// scope entries name symbols, and resolving a name to an id is the adapter's
// problem, not the compiler's.
type ImpactView interface {
	// Reach reports the blast radius of a change to target.
	//
	// A target the analysis cannot resolve is NOT an error: it is a zero Reach
	// with Resolved false, which the contract reports as a named gap. An error
	// means the analysis itself failed, and compilation fails with it rather
	// than emitting a contract that silently lost a source.
	Reach(ctx context.Context, target string) (Reach, error)
}

// Reach is one impact analysis, in the terms a contract can state.
//
// Deliberately a projection of internal/capability/impact.Report rather than a
// copy of it: everything here is something the CONTRACT says, and the parts of
// that report which exist to justify a risk score to a human reading the impact
// command have no place in a brief handed to an agent.
type Reach struct {
	// Resolved is false when the analysis could not find the target in the
	// index. Reported, never silently treated as "nothing is affected".
	Resolved bool

	// Target is the name the analysis resolved to, which may be an FQN where
	// the caller supplied a bare name.
	Target string

	// Callers are the symbols a change to Target could affect, each carrying
	// the confidence of the PATH that reached it.
	Callers []Reached

	// Floor is the confidence floor the walk ran with, and Hidden counts the
	// symbols an identical unfloored walk would have reached. Carried because a
	// blast radius is only interpretable alongside the filter that produced it:
	// "4 callers" and "4 callers, 12 hidden below `medium`" are different
	// statements about the same change.
	Floor  string
	Hidden int

	// Truncated says the walk stopped at a bound with work outstanding.
	Truncated bool

	// Tests are the tests the analysis found covering Target, each with the
	// signal that found it. A test found by a proven call chain and one found
	// because its name looks right are not the same claim and are not merged.
	Tests []TestRef
}

// Reached is one symbol a change to a scope entry would affect.
type Reached struct {
	Symbol string `json:"symbol"`
	Path   string `json:"path"`
	Kind   string `json:"kind,omitempty"`

	// Depth is hops from the target; 1 is a direct caller.
	Depth int `json:"depth"`

	// Confidence is the MINIMUM confidence along the path that reached this
	// symbol, carried through unchanged. A depth-3 reach across a medium edge
	// is medium however strong the last hop was.
	Confidence string `json:"confidence,omitempty"`

	IsTest bool `json:"is_test,omitempty"`

	// Evidence is the source location proving the relationship, where the
	// analysis had one. Empty for indirect reach, which has no single location
	// that justifies it.
	Evidence string `json:"evidence,omitempty"`

	// Target is the scope symbol whose analysis reached this one. Set by
	// Compile, not by the port: the port is asked one target at a time and has
	// no reason to repeat back what it was asked.
	Target string `json:"target,omitempty"`
}

// TestRef is one test the analysis says covers a scope symbol.
type TestRef struct {
	Name string `json:"name"`
	Path string `json:"path"`

	// Signal is HOW the test was found: a proven call chain, a naming
	// convention, or co-location. Never dropped — a contract that says "covered
	// by TestFoo" without saying that the only evidence is the name has made a
	// guess look like a fact.
	Signal     string `json:"signal,omitempty"`
	Confidence string `json:"confidence,omitempty"`

	// Covers is the scope symbol this test was found for. Set by Compile.
	Covers string `json:"covers,omitempty"`
}
