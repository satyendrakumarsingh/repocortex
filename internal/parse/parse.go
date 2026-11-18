// Package parse turns source files into symbols and unresolved references.
//
// Two boundaries define this package:
//
//   - It never touches the database, the filesystem, or the network. It takes
//     bytes and returns facts. That makes it trivially testable and safe to run
//     across many goroutines.
//
//   - It does NOT resolve references. Tree-sitter produces a syntax tree per
//     file; it cannot tell you that `svc.updateStatus()` binds to
//     `com.pay.TransactionService#updateStatus`. Parsing emits Reference values
//     carrying the raw name and position, and the resolve layer binds them to
//     symbols with a confidence level. Conflating the two would let syntactic
//     guesses masquerade as resolved facts.
package parse

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Status describes what the parser did with a file. It says nothing about
// what was extracted: a file that parses cleanly and declares nothing is
// StatusOK, and a file full of definitions the parser stumbled through is
// StatusPartial.
//
// The values are the ones stored in files.parse_status; see
// pkg/model/parsestatus.go for the contract and for how old rows read.
type Status string

const (
	// StatusOK means the parser read the whole file and reported no syntax
	// error.
	StatusOK Status = model.ParseStatusOK

	// StatusPartial means a tree was built but it is not a clean reading of
	// the whole file: the parser reported syntax errors, or the parse stopped
	// before the end because it exceeded its budget. Result.Reason says which,
	// and what was extracted is still usable.
	StatusPartial Status = model.ParseStatusPartial

	// StatusFailed means nothing usable came out. The file is recorded so the
	// gap is visible rather than silently absent.
	StatusFailed Status = model.ParseStatusFailed

	// StatusSkipped means no tree was attempted: the file is oversized or
	// empty, or its grammar has no definition query (Result.Reason is then
	// model.ParseReasonTextOnly and the file is indexed for search alone).
	StatusSkipped Status = model.ParseStatusSkipped
)

// Reference is a use of a name that has not yet been bound to a definition.
//
// It deliberately carries no target. Binding happens in the resolve layer,
// which attaches a confidence level; a Reference is raw evidence, not a fact
// about the graph.
type Reference struct {
	// Name is the identifier as written, e.g. "updateStatus".
	Name string

	// Kind mirrors tree-sitter tag kinds, e.g. "reference.call".
	Kind string

	// Receiver is the qualifying expression when the syntax exposes one, e.g.
	// "transactionService" in transactionService.updateStatus(). Empty when
	// unqualified. The resolver uses it to infer a target type.
	Receiver string

	// ReceiverKind says what Receiver is — nothing (a bare call), a name, a
	// path, the enclosing instance, the superclass, a stated type, a local
	// value, or an expression with no name. One of the model.Receiver*
	// constants for every call reference; empty for a reference that is not a
	// call. An empty Receiver therefore means "the source wrote none" only
	// when ReceiverKind is model.ReceiverBare — the distinction A18 in
	// internal/resolve/infer was reverted for lacking.
	ReceiverKind string

	// ArgCount is how many arguments this call site passes, where the parser
	// could determine that. model.ArityUnknown for every reference that is not
	// a call, for a call spreading a slice or a sequence, and for every
	// language with no argument query.
	//
	// Unknown is the default and it means exactly that. A18 is the reason the
	// type carries the distinction rather than using 0: an empty Receiver was
	// read as "the source wrote no receiver" when it meant "the query could not
	// express one", and every chained Go call broke. A count of zero and no
	// count are the same failure one field over.
	ArgCount model.Arity

	// Value is, for a call made on a local variable the source declares
	// without a type, the start of the call whose result initialised the
	// variable: the Range.Start of that call's own Reference in this file.
	// Zero when the variable was not initialised by one plain call. The
	// resolver binds that call and reads the return type of what it reaches.
	// See returnedValue in shape.go.
	Value model.Position

	Range model.Range
}

// Import is a dependency declared by a file. The resolver uses these to narrow
// candidate targets, which is what separates a high-confidence binding from a
// bare name match.
type Import struct {
	// Path is the module/package as written, e.g. "com.pay.repository" or
	// "./notification-service".
	Path string

	// Alias is the local binding, when the language supports renaming.
	Alias string

	// Directive is the keyword that introduced the row when the import query
	// captured one (@directive): Dart's `export` and `part`, which name a file
	// and are not imports of it. A language's import fix reads it and turns it
	// into what the row stores; it is never persisted itself.
	Directive string

	Range model.Range
}

// Result is everything one file yielded.
type Result struct {
	Language   string
	Status     Status
	Symbols    []model.Symbol
	References []Reference
	Imports    []Import

	// Reason is why Status is not StatusOK, as a stable token.
	// model.ParseReasonNone for a clean parse.
	Reason model.ParseReason

	// ErrorNodes and MissingTokens count what the parser reported when
	// Reason is model.ParseReasonSyntaxErrors: ERROR nodes (text it could not
	// fit into the grammar) and MISSING nodes (tokens it had to invent to
	// continue). FirstErrorLine is the 1-based line of the earliest of them.
	ErrorNodes     int
	MissingTokens  int
	FirstErrorLine int

	// ParsedBytes is how far into the source the parser got when Reason is
	// model.ParseReasonBudgetExceeded. Definitions past it are absent.
	ParsedBytes int

	// Qualifiers counts the tagged identifiers that were dropped because they
	// are the receiver or qualifier of a call, not a call: `reader` in
	// `reader.peek()`. Reported so the drop is counted rather than silent.
	Qualifiers int

	// MacrosMasked is how many unknown-macro tokens were overwritten with
	// spaces before the tree that produced this result was parsed, for a C,
	// C++ or Objective-C file whose masked parse the guard in cmacro.go kept.
	// Zero for every other file, and for every file the plain parse served.
	MacrosMasked int

	// BranchesBlanked is how many conditional-compilation branches (the text
	// after an `#else`, an `#elif`, or an `#if` that was not kept) were
	// overwritten with spaces before the tree that produced this result was
	// parsed, for a C, C++, Objective-C or C# file whose masked parse the
	// guard in cond.go and cmacro.go kept. A definition that exists only in
	// such a branch is not in Symbols. Zero for every other file.
	BranchesBlanked int

	// ConstructsMasked is how many spans of a language's own unreadable
	// construct were overwritten before the tree that produced this result was
	// parsed, for an F# or Groovy file whose masked parse the guard in
	// cmacro.go kept: a statically resolved member constraint call in F#, a
	// one-line doc comment in Groovy. Zero for every other file.
	ConstructsMasked int

	// RecoveredByShape is how many definitions were read by line shape from
	// text the grammar left in ERROR nodes (see fallback.go). Each is a
	// symbol of confidence medium. Zero for every file the grammar read.
	RecoveredByShape int

	// Err explains a status other than ok. Its text starts with Reason, so it
	// can be stored as files.parse_error unchanged. It is diagnostic text for
	// the file's record, never a reason to abort the indexing run.
	Err error
}

// TextIndexable reports whether the pipeline should put this file's text in
// the full-text index.
func (r *Result) TextIndexable() bool {
	if r == nil {
		return false
	}
	msg := ""
	if r.Err != nil {
		msg = r.Err.Error()
	}
	return model.TextIndexed(string(r.Status), msg)
}

// Parser extracts facts from one language's source.
//
// Implementations must be safe for concurrent use by multiple goroutines.
// The underlying tree-sitter parser is NOT concurrency-safe, so implementations
// are responsible for pooling or per-goroutine state.
type Parser interface {
	// Language is the canonical language name, matching the grammar registry.
	Language() string

	// Parse extracts facts from src. It must honour ctx cancellation, and it
	// must never panic: a malformed or hostile file has to degrade to
	// StatusFailed for that file alone. Spec §13 requires parser failures to
	// be isolated to the affected file.
	Parse(ctx context.Context, path string, src []byte) (*Result, error)
}

// Registry resolves a file to the parser that should handle it.
type Registry interface {
	// ForFile returns a parser for path, or nil when no grammar applies.
	//
	// It decides from the file NAME alone, and a name is not enough for an
	// extension several languages share: `.m` is MATLAB or Objective-C, `.h`
	// is C or C++. A caller that has the file's content detects the language
	// first (fs.Walker does, for every file it discovers) and asks ForLanguage.
	ForFile(path string) Parser

	// ForLanguage returns the parser for a grammar by name, or nil when the
	// name is empty or no grammar has it.
	ForLanguage(language string) Parser

	// Languages lists every language the registry can handle.
	Languages() []string
}
