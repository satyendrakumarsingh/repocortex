package infer

import (
	"path"
	"sort"
	"strings"
)

// # LanguageSpec — the binding constraint, made declarative
//
// RepoCortex PARSES 206 grammars and RESOLVES four. The gap is not a missing
// algorithm; it is that everything resolution knows about a language was
// scattered through the resolver as switches on a language name:
//
//	func packageIsDirectory(language string) bool {
//	    switch language { case "go", "java", "kotlin": return true; ... }
//	}
//	func receiverIsAType(language string) bool {
//	    switch language { case "java", "kotlin", "typescript": return true; ... }
//	}
//	func memberSeparators(language string) []string { ... }
//
// Three switches, three different language sets, three separate places to
// forget. A language absent from all of them does not fail loudly — it silently
// takes every default, and the defaults were chosen for the languages that were
// already there. `scala` is in the separator switch and in neither of the other
// two; `tsx` is in none of them while `typescript`, the same grammar plus JSX,
// is in one. Neither of those is a decision anyone made.
//
// A LanguageSpec is that knowledge stated ONCE, per language, as data. Adding a
// language becomes one table entry whose every field a reviewer can see, rather
// than an archaeology exercise across the resolver. The switches above are gone;
// what replaces them is a struct where "we never thought about Ruby" is visible
// as a value rather than invisible as an absent case.
//
// # What a spec may and may not do
//
// A spec describes the LANGUAGE. It is not a place to put tuning knobs, and in
// particular it is not a place to add candidate filters. Two such filters were
// written into this resolver and both were reverted on measurement — see "Why
// candidates are not filtered before the tiers" in resolver.go. The rule that
// came out of them applies to every field here:
//
//	A filter that removes candidates is not automatically safe, because
//	removing candidates also removes the AMBIGUITY that was protecting the
//	caller.
//
// So two fields below — CallShapes and TestFiles — are DESCRIPTIVE. They record
// facts about the language that a resolution author needs and that no reader
// should have to re-derive, and nothing in the binding path consults them. That
// is deliberate, it is not an oversight, and turning either into a filter is a
// change that must be justified by an end-to-end precision measurement and not
// by the soundness of the fact itself. Each field's own comment says why.

// CallShape is one syntactic form a call can take in a language.
//
// DESCRIPTIVE. Recorded so a new language's author states what its call syntax
// admits, and so the A18 trap is visible at the point someone would fall into
// it, rather than discovered again from first principles.
//
// A18 was exactly this reasoning applied one step too far: "in Go and Rust a
// bare call cannot name a method, so method candidates can be dropped when
// ref.Receiver is empty". The premise is TRUE of both languages. The guard
// still broke Go outright, because an empty Receiver does not mean the source
// wrote no receiver — internal/parse's receiverQueries capture only a SIMPLE
// IDENTIFIER receiver, so `s.repo.Save(id)` arrives with an empty one.
//
// The fact and the filter are therefore separated on purpose. This type carries
// the fact. Nothing reads it to exclude a candidate.
//
// internal/parse has since learned to distinguish "no receiver in the source"
// from "a receiver the query could not express" — every call reference states
// its shape (model.ReceiverBare and its siblings) — and the rules that act on
// a call's shape are in shaped.go, driven by CallTraits. This type stays what
// it was: a description.
type CallShape uint8

const (
	// CallBare is `foo()` — a call with no qualifying expression.
	CallBare CallShape = 1 << iota

	// CallMethod is `recv.foo()` — a call on a value, where the receiver
	// denotes an instance whose type owns the member.
	CallMethod

	// CallQualified is `pkg.foo()` — a call qualified by a namespace, module or
	// package rather than by a value.
	CallQualified

	// CallStatic is `Type.foo()` or `Type::foo()` — a call qualified by a TYPE,
	// reaching a member that belongs to the type rather than to an instance.
	CallStatic
)

// Permits reports whether the language admits a call shape.
func (s CallShape) Permits(shape CallShape) bool { return s&shape != 0 }

// TestFiles describes how a language's ecosystem names its test code.
//
// DESCRIPTIVE, and this one has a measured reason to stay that way. A19 read
// exactly this convention and used it to drop test-file candidates when the
// referencing file was production code. Measured on prometheus against a
// scip-go index it cost 2.4pp of `high` precision — not by emitting wrong edges
// directly, but by shrinking candidate sets until 426 references that had been
// honestly AMBIGUOUS tipped over the disambiguation thresholds and resolved, 362
// of them wrongly.
//
// The convention is still worth stating: it is real, it differs per language,
// and a resolution author who needs it (a report, a coverage question, a future
// tier that ADDS evidence rather than removing candidates) should not have to
// invent it per language. What it must not become is a filter on the candidate
// set without an end-to-end measurement saying so.
type TestFiles struct {
	// Suffixes match the file's base name including extension —
	// "_test.go", ".spec.ts".
	Suffixes []string

	// Prefixes match the file's base name — "test_" for Python.
	Prefixes []string

	// BaseNames match the base name exactly — "conftest.py".
	BaseNames []string

	// Dirs are path segments that make everything beneath them test code —
	// "testdata", "__tests__", "src/test" written as "test".
	Dirs []string
}

// Matches reports whether a path is test code by this language's convention.
//
// Case-insensitive, because the conventions themselves are (Java's TestFoo and
// FooTest, TypeScript's .Spec.ts in some codebases) and because a case-sensitive
// answer here would differ between macOS and Linux checkouts of the same tree.
func (t TestFiles) Matches(filePath string) bool {
	if filePath == "" {
		return false
	}
	base := strings.ToLower(path.Base(filePath))
	for _, s := range t.Suffixes {
		if strings.HasSuffix(base, strings.ToLower(s)) {
			return true
		}
	}
	for _, p := range t.Prefixes {
		if strings.HasPrefix(base, strings.ToLower(p)) {
			return true
		}
	}
	for _, b := range t.BaseNames {
		if base == strings.ToLower(b) {
			return true
		}
	}
	if len(t.Dirs) == 0 {
		return false
	}
	// Compared against the DIRECTORY only. A file called `tests.go` is not test
	// code; a file under `tests/` is. A segment is what sits between slashes:
	// pathSegments is NOT used here, because it also splits on `.` for
	// import-path matching, and through it a C# `Order.Tests/` project
	// directory yielded a `tests` segment that no convention in this table
	// declares — and that capability.IsTestPath, which verify reads, does not
	// see. The two classifiers must agree on the same path.
	for _, seg := range strings.Split(strings.ToLower(path.Dir(filePath)), "/") {
		for _, d := range t.Dirs {
			if seg == strings.ToLower(d) {
				return true
			}
		}
	}
	return false
}

// LanguageSpec is everything resolution needs to know about one language.
//
// A value, not an interface: every question here is answered by data, and an
// interface would invite a language to answer one of them with code that the
// table cannot show a reviewer. Copied freely; the slices inside are shared and
// MUST NOT be mutated by anything holding a copy.
type LanguageSpec struct {
	// Name is the grammar name this spec was selected for, so a diagnostic can
	// say which language produced a decision. For the fallback it is the name
	// that was asked for, not "".
	Name string

	// Interop names the languages whose definitions this one may bind to,
	// itself included. Empty means "only itself" for a language listed in
	// knownLanguages; for any other language bindableAcross treats it as
	// unclassified and keeps the candidate.
	//
	// A call resolves through a MODULE SYSTEM, and two languages that do not
	// share one cannot see each other's definitions no matter how well the
	// names match. Measured on a polyglot repository: a Vue component calling
	// `readiness.exceptions(...)` bound to a PYTHON function named `exceptions`
	// in another service, at `high` confidence, and surfaced as an architecture
	// violation — "the warehouse UI reaches into inventory-service" — which is
	// the kind of finding somebody acts on. 62 python->go edges at `high` came
	// from the same gap.
	//
	// The set is not "same language": a Vue SFC legitimately imports
	// TypeScript, and 1,076 such edges on that repository are correct. What is
	// never correct is a binding across two languages with no shared resolution
	// mechanism at all.
	Interop []string

	// PackageIsDirectory reports whether files in one directory are members of
	// one package, visible to each other WITHOUT an import.
	//
	// This is the single highest-leverage field in the struct, because it
	// decides the order of the import tier and the same-directory tier, and
	// getting that order wrong is the largest source of wrong high-confidence
	// edges this resolver has produced. Both orders were measured:
	//
	//   - true (Go, Java, Kotlin): an unqualified `Foo()` means THIS package's
	//     Foo; the imported one would have to be written `pkg.Foo()`. Proximity
	//     must win. Putting imports first cost prometheus 0.8pp.
	//   - false (Python, TypeScript, JavaScript): each file is its own module
	//     and adjacency asserts nothing. Imports must win. Putting proximity
	//     first cost scrapy 4.7pp — one name, `get_crawler`, produced 144 wrong
	//     high-confidence edges on its own.
	//
	// It also sets the confidence pickSameDir reports: where the directory IS
	// the package a neighbour is genuinely in scope (High); elsewhere proximity
	// is a preference and is labelled as one (Medium).
	PackageIsDirectory bool

	// ImportPathNamesFile reports whether an import path's last segment names a
	// FILE rather than a directory.
	//
	// `import { X } from './sibling'` resolves to sibling.ts and
	// `from .helpers import X` to helpers.py, so the file's own stem is part of
	// what an import can match. In Go and Java the last segment is a DIRECTORY
	// and a file's own name means nothing to an import — matching on it there
	// only invents coincidences, and enabling it for Go cost prometheus 0.4pp.
	//
	// Distinct from PackageIsDirectory even though, until C#, every language in
	// the table answered them oppositely. They are different questions — one is about
	// VISIBILITY between neighbours, the other about what an import path
	// DENOTES — and a language can have a directory-package whose imports still
	// name files. Keeping them separate is what stops the next such language
	// from silently inheriting the wrong one of the two.
	ImportPathNamesFile bool

	// ReceiverDenotesType reports whether a captured receiver has already been
	// resolved to the TYPE it refers to, rather than left as the identifier that
	// names it.
	//
	// True only where internal/parse has a varTypeQuery, because that is what
	// rewrites `b.build()` into `Builder.build()`. Where it is false the
	// receiver is a VARIABLE name, and treating it as a type is the mis-
	// inference A16 laundered into facts: Go receivers are `s`, `c`, `r`, and
	// rescuing an over-subscribed name on one cost prometheus 1.7pp while
	// gaining nothing.
	//
	// Gates the two paths that treat a receiver as a type outright:
	// narrowByReceiver (the rescue from MaxCandidates) and deferInherited (the
	// ancestry walk, which asks "what does this type extend").
	ReceiverDenotesType bool

	// QualifiedMembers reports that this language's qualified names STATE the
	// class a member belongs to — `path/to/file.py:Account.save` — because
	// internal/parse derived them from where each definition sits.
	//
	// It is the precondition for every rule that treats a receiver as a class
	// without guessing, and it changes three things at once:
	//
	//   - ownership is read off the qualified name, compared EXACTLY. Elsewhere
	//     the comparison folds case, which is what lets `transactionService`
	//     name a TransactionService. In a dynamically typed language that
	//     convention is the guess this package must not make: `settings.get()`
	//     says nothing about a class called Settings. A receiver matches an
	//     owner here only when it IS the class's name — because the source
	//     wrote the class (`Settings.get()`), or because the parser replaced a
	//     variable with the class the source states for it;
	//   - a receiver in ClassSelfNames denotes the class the call is written
	//     in, which the enclosing symbol's qualified name gives exactly;
	//   - a method the receiver's class does not declare is looked for on its
	//     ancestors before any weaker tier is believed. See static.go.
	//
	// False for every language whose qualified names are `path:name`. Setting it
	// without the parser support produces a language where nothing is ever
	// owned, which is safe and useless.
	QualifiedMembers bool

	// ClassSelfNames are the receiver tokens that denote the instance or class
	// a method is written in: `self` and `cls` in Python. Read only where
	// QualifiedMembers holds. Distinct from SelfNames, which says a token
	// carries NO information; these say exactly which class it is.
	ClassSelfNames []string

	// SuperName is the receiver token for a call on the superclass — `super`,
	// as internal/parse records `super().method()`. Read only where
	// QualifiedMembers holds; empty elsewhere.
	SuperName string

	// PackageInitStem is the file stem that makes a file its DIRECTORY's module
	// rather than a module of its own: `__init__` in Python, where
	// `shop/models/__init__.py` is what `import shop.models` names.
	//
	// Without it such a file's segments end in `__init__`, which no import path
	// ever writes, so nothing defined in a package's own module could be
	// reached through the import tier at all — and in a library whose public
	// classes live in `__init__.py` files that is most of the API. Empty means
	// every file is named by its own stem.
	PackageInitStem string

	// ProximityIsEvidence reports whether a definition in the same directory is
	// worth binding to when the file did NOT import it.
	//
	// Where a directory is a package this is not the question — a neighbour is
	// in scope, and pickSameDir reports High. Everywhere else the same-directory
	// tier is a preference reported at Medium, and for most languages nobody has
	// measured whether that preference is ever right, so it stays on.
	//
	// For Python it was measured, against scip-python 0.6.6 on three
	// repositories: 1 edge right of 194 on scrapy, 0 of 11 on requests, 0 of 8
	// on httpx. That is not weak evidence, it is none: a Python name a file
	// neither defines nor imports cannot denote a neighbour's definition, and a
	// method called on an unknown object is not made likelier by which
	// directory declares one of that name. So Python turns the tier off, and a
	// reference only it could bind is recorded ambiguous, which is what it is.
	ProximityIsEvidence bool

	// ModuleDirs names the directories whose CHILDREN are modules: SwiftPM's
	// `Sources` and `Tests`, where `Sources/Core/Models/Order.swift` is a file
	// of the target `Core` and sees every other file of `Sources/Core/**`
	// without an import. Empty for every other language, whose package is the
	// directory a file is in.
	//
	// A Swift import names a module and no file, and a module is a TREE of
	// directories, so the directory test everywhere else would call the one
	// file beside a call its package and the one a folder down a stranger.
	// packageOf is the one place that says which: with it a module's files
	// are neighbours (High, as in Go and Java), an `import Core` covers every
	// file under Sources/Core, and a name defined in a module that was not
	// imported is not mistaken for one that was. Outside a Sources/ or Tests/
	// tree (an Xcode project lays targets out as it likes) the package is the
	// directory, which is what the language does with no better evidence.
	ModuleDirs []string

	// FoldNames reports that the language does not tell two spellings of a name
	// apart by case: PowerShell and CMake command names, Fortran procedure names, where `write-log` calls
	// `Write-Log` (variables are not folded). Candidate lookup is by exact name, so a call written in
	// another case finds nothing; with this set it falls back to the spellings
	// the repository defines (Resolver.foldCandidates).
	FoldNames bool

	// ReExports reports that a file's import rows include `export` and `part`
	// directives, which bring a library's names in without an import: a Dart
	// barrel's `export 'src/x.dart'`, a library's `part 'x.g.dart'`. See
	// reexport.go.
	ReExports bool

	// NamespaceDirs names the only directories whose files share a namespace
	// with no import: an R package's `R/`, where every file's functions are the
	// package's and every other file of the directory calls them by name. Empty
	// for every other language, where the directory is the package (or, with
	// PackageIsDirectory unset, no directory is).
	//
	// R has both kinds of directory side by side. A script directory loads
	// nothing it does not `source()`, and a neighbour is no more in scope than
	// any other file; an `R/` directory is a namespace. A language-wide
	// PackageIsDirectory would call the first kind a package and bind a call of
	// `filter(df, x)` to whichever sibling script happens to define `filter`.
	// With NamespaceDirs set, packageOf names the directory for a file inside one
	// of them and the FILE ITSELF for any other, so that a package's files are
	// neighbours (High, as in Go) and nothing else is.
	NamespaceDirs []string

	// TypesNeedImport reports that a type declared in a neighbouring file is in
	// scope only when the file imports it: a Dart library is one file, and
	// `Item` in lib/models/item.dart is invisible to lib/models/cart.dart
	// until `import 'item.dart'` says so. Where it is set, the type lookup does
	// not take a same-directory type on adjacency alone but asks the imports,
	// so two directories' `Item` are told apart by which one the file imports.
	// Unset everywhere else, whose measured behaviour it would change.
	TypesNeedImport bool

	// MemberSeparators is what the language writes between a type and one of its
	// members, MOST SPECIFIC FIRST.
	//
	// A slice, ordered and fixed, because resolution must not depend on map
	// iteration order — the whole package is written that way and the registry
	// digest depends on it.
	//
	// `#` appears in every entry because SCIP-shaped qualified names use it to
	// separate a type from a member, and compiler-backed symbols sit in the same
	// table as inferred ones.
	MemberSeparators []string

	// SelfNames are receiver tokens that denote the enclosing instance rather
	// than a named type. A receiver of `this` or `self` tells the resolver
	// nothing the same-file tier has not already established.
	SelfNames []string

	// MinTypeNameLength is the floor below which a receiver token is treated as
	// a coincidence rather than a type name.
	//
	// At repository scale a one- or two-character identifier collides with real
	// type names by accident: `s.Save()` naming a file s.go is not evidence.
	// Applied where a receiver is used as a TYPE, and deliberately NOT applied
	// in receiverMatches, where a short receiver still has to clear the exact
	// whole-segment ownership test before it matches anything.
	MinTypeNameLength int

	// MaxCandidates is the point past which a name is too common to resolve in
	// this language.
	//
	// Bounded above by the package-level MaxCandidates, which is a CROSS-PACKAGE
	// CONTRACT: internal/pipeline sizes its candidate cache at MaxCandidates+1
	// rows per name, so a spec asking for more would be served a truncated set
	// and would silently resolve on partial evidence. TestSpecsRespectCandidateCap
	// enforces the bound.
	MaxCandidates int

	// CallShapes is the set of call syntaxes the language admits. DESCRIPTIVE —
	// see CallShape, and A18, for why nothing in the binding path reads it.
	CallShapes CallShape

	// TestFiles is the language's test-code naming convention. DESCRIPTIVE —
	// see TestFiles, and A19, for why nothing in the binding path reads it.
	TestFiles TestFiles

	// Calls is how this language's calls reach definitions: which languages it
	// can call into, whether a bare name may be a member, how its imports bind
	// names. Filled from callTraitsByLanguage for every spec, the fallback
	// included, so it is never the zero value for a named language. See
	// calltraits.go.
	Calls CallTraits
}

// namesAType reports whether a receiver token may be treated as the NAME OF A
// TYPE — long enough to not be a coincidence, and not a self-reference.
//
// Used by the two paths that need the receiver to BE a type: the rescue from
// the candidate cap, and the ancestry walk.
func (s LanguageSpec) namesAType(receiver string) bool {
	r := strings.TrimPrefix(receiver, "*")
	if len(r) < s.MinTypeNameLength {
		return false
	}
	return !s.isSelfName(r)
}

// usableReceiver reports whether a receiver token carries any information at
// all: non-empty, and not a self-reference.
//
// Deliberately WITHOUT the length floor namesAType applies. receiverMatches
// still has to clear an exact whole-segment ownership test, which a short
// receiver almost never does on its own, and adding a floor here would be a new
// filter — the class of change this package has twice measured to be harmful.
func (s LanguageSpec) usableReceiver(receiver string) bool {
	r := strings.TrimPrefix(receiver, "*")
	return r != "" && r != opaqueReceiver && !s.isSelfName(r)
}

// packageOf is the directory that is the package of a file: the file's own
// directory, or for a language with ModuleDirs the module directory that
// holds it. See LanguageSpec.ModuleDirs.
func (s LanguageSpec) packageOf(filePath string) string {
	if len(s.NamespaceDirs) > 0 {
		dir := path.Dir(filePath)
		if !containsString(s.NamespaceDirs, path.Base(dir)) {
			return filePath
		}
		return dir
	}
	return s.moduleDirOf(filePath)
}

// moduleDirOf is the directory that names a file's module in an import path: its
// own, or for a language with ModuleDirs the module directory that holds it.
// Unlike packageOf it is never the file itself, because an import path is
// compared with the file's directory and stem.
func (s LanguageSpec) moduleDirOf(filePath string) string {
	dir := path.Dir(filePath)
	if len(s.ModuleDirs) == 0 {
		return dir
	}
	segs := strings.Split(dir, "/")
	for i, seg := range segs {
		for _, m := range s.ModuleDirs {
			if seg == m && i+1 < len(segs) {
				return strings.Join(segs[:i+2], "/")
			}
		}
	}
	return dir
}

// testSees reports whether a test file reaches a definition without naming it,
// because the test framework puts it in scope: for R, testthat attaches the
// package under test (every file of the `R/` beside the tests' directory) and
// sources the `helper*` and `setup*` files that sit with the tests.
//
// False for every language but one with NamespaceDirs, where nothing of the
// kind is modelled and a test file is a file like any other. It is one more
// way for a call to be in scope, asked only where a call has already named
// something the repository defines.
func (s LanguageSpec) testSees(testPath, candPath string) bool {
	if len(s.NamespaceDirs) == 0 || !s.TestFiles.Matches(testPath) {
		return false
	}
	testDir, candDir := path.Dir(testPath), path.Dir(candPath)
	if candDir == testDir {
		base := strings.ToLower(path.Base(candPath))
		return strings.HasPrefix(base, "helper") || strings.HasPrefix(base, "setup")
	}
	segs := strings.Split(testDir, "/")
	for i, seg := range segs {
		if seg == "tests" {
			return candDir == path.Join(append(append([]string{}, segs[:i]...), s.NamespaceDirs[0])...)
		}
	}
	return false
}

func (s LanguageSpec) isSelfName(r string) bool {
	for _, self := range s.SelfNames {
		if strings.EqualFold(r, self) {
			return true
		}
	}
	return false
}

// IsTestFile reports whether a path is test code by this language's convention.
// Not a candidate filter (see the TestFiles doc); testhelper.go applies it to a
// binding a name-only rule has already decided on.
func (s LanguageSpec) IsTestFile(filePath string) bool { return s.TestFiles.Matches(filePath) }

// Specs selects the LanguageSpec for a language — the Factory over the
// LanguageSpec strategies.
//
// A struct built by NewSpecs, held by the Resolver, and NEVER a package-level
// registry: CLAUDE.md §4.2 forbids global mutable state, and a registry other
// packages could register into would also make resolution depend on init order,
// which is exactly the class of defect that produced three different registry
// digests over byte-identical source.
//
// The table inside is written once by NewSpecs and only read afterwards, so a
// *Specs is safe for concurrent use.
type Specs struct {
	byName   map[string]LanguageSpec
	fallback LanguageSpec

	// generic holds the identifiers a bare name match says nothing about. See
	// genericNames and Specs.IsGenericName. A map on the struct rather than a
	// package-level var, for the reason the doc above gives.
	generic map[string]struct{}
}

// genericNames is the set of identifiers so common that "this name occurs
// exactly once in the repository" is not evidence about them.
//
// # A20 — what this is for, and the one tier it touches
//
// The resolver's last tier binds a name that occurs EXACTLY ONCE anywhere in
// the repository, at ConfidenceLow, reasoning that once is meaningfully
// different from eight times. For a distinctive name that holds. For `get` it
// does not: a repository having exactly one function called `get` says nothing
// about whether a given `get(...)` meant that one, because `get` is what
// everything is called — including the hundreds of definitions in dependencies
// that were never indexed and so never became candidates. The uniqueness is an
// artefact of the index's boundary, not a property of the program.
//
// This is deliberately NOT a candidate filter. It refuses one binding at the
// weakest tier, which pick() reaches only after the same-file, receiver, import
// and same-directory tiers have all declined. It therefore cannot promote,
// demote or redirect any high- or medium-confidence edge, which is what makes
// it a different shape of change from A19 — the reverted guard that shrank the
// candidate SET and thereby tipped references across the disambiguation
// thresholds. Nothing here changes what pick() has to choose between; it
// changes only whether the last-resort guess is published.
//
// The list is short and fixed on purpose. It is not an attempt to enumerate
// every common name — a long list would start deleting real edges for names
// that merely feel generic. It is the class the tier is demonstrably wrong
// about: bare verbs, bare accessors, and identifiers that are types or
// keywords in some language and function names in another.
//
// Lowercased, and compared case-insensitively, because `Get` and `get` are the
// same claim.
func genericNames() []string {
	return []string{
		"add", "all", "any", "append", "apply", "build", "call", "check",
		"clear", "close", "compare", "contains", "convert", "copy", "count",
		"create", "delete", "do", "each", "empty", "equal", "equals", "error",
		"execute", "exists", "fetch", "filter", "find", "first", "flush",
		"format", "get", "handle", "has", "id", "index", "init", "insert",
		"is", "item", "items", "key", "keys", "last", "len", "length", "list",
		"load", "log", "main", "make", "map", "match", "name", "new", "next",
		"of", "ok", "open", "parse", "pop", "print", "process", "push", "put",
		"query", "read", "remove", "render", "reset", "result", "run", "save",
		"send", "set", "size", "sort", "start", "stop", "string", "test",
		"to", "type", "update", "validate", "value", "values", "wait", "write",
	}
}

// IsGenericName reports whether a name is one the last-resort uniqueness tier
// must not bind on. See genericNames.
func (s *Specs) IsGenericName(name string) bool {
	if name == "" {
		return false
	}
	_, ok := s.generic[strings.ToLower(name)]
	return ok
}

// NewSpecs builds the language table.
//
// Every entry states every field. There is no "inherit the defaults and
// override one" shorthand on purpose: the defect this whole file exists to fix
// is a language quietly taking a default that was chosen for a different
// language, and a table where each row is complete is one a reviewer can check
// by reading a row.
//
// The shared slices below are LOCALS, not package-level vars. CLAUDE.md §4.2
// forbids global mutable state, and a package-level `[]string` is mutable
// however firmly its comment says otherwise — one stray append reaching it
// would change resolution for every language at once, silently, at a distance.
// Building them per call costs a handful of allocations once per Resolver.
func NewSpecs() *Specs {
	// What every language in the table calls the enclosing instance.
	//
	// Both tokens are listed for every language, including the ones that use
	// only one. `this` is not a Go keyword and `self` is not a Java one, so
	// listing both costs nothing and removes a per-language decision with no
	// upside — a Go variable named `self` is a self-reference by intent
	// whatever the compiler thinks.
	commonSelfNames := []string{"this", "self"}

	sepDefault := []string{".", "#"}
	// `$` separates a nested or anonymous class from its outer class in JVM
	// binary names.
	sepJVM := []string{".", "#", "$"}
	sepScope := []string{"::", ".", "#"}

	fallback := LanguageSpec{
		// The fallback is what 190-odd grammars get, so its values are chosen to
		// be the SAFE answer rather than the common one.
		//
		// PackageIsDirectory false: claiming a neighbour is in scope when it is
		// not produces High-confidence edges out of pure adjacency, which is the
		// 4.7pp mistake scrapy measured. Claiming it is not when it is costs
		// recall and reports Medium. The asymmetry decides it.
		//
		// ReceiverDenotesType false: without a varTypeQuery the receiver is a
		// variable name, and there is no language for which guessing otherwise
		// is safe.
		Name:                "",
		PackageIsDirectory:  false,
		ImportPathNamesFile: true,
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles:           TestFiles{Dirs: []string{"test", "tests", "testdata", "spec", "__tests__"}},
	}

	byName := map[string]LanguageSpec{}
	add := func(s LanguageSpec) { byName[s.Name] = s }

	// --- Directory-is-package languages -------------------------------------

	add(LanguageSpec{
		Name:    "go",
		Interop: []string{"go"},
		// A directory is a package and every file in it sees the others.
		PackageIsDirectory: true,
		// An import path names the DIRECTORY; go.mod paths never carry a file
		// stem. Enabling file matching here cost prometheus 0.4pp.
		ImportPathNamesFile: false,
		// No varTypeQuery: a Go receiver arrives as the identifier — `s`, `c`,
		// `r` — never as its type.
		//
		// A3 built one and measured it. It worked as a parser — `svc *Service`,
		// `var q *Queue` and `c := &Client{}` all reached the resolver as types
		// — and it still cost `high` precision, so the query was removed and
		// this stays false. The numbers are in the A3 note in resolver.go.
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		// No CallStatic: a Go method value is still reached through a receiver,
		// and `pkg.Func()` is CallQualified, not a call on a type.
		CallShapes: CallBare | CallMethod | CallQualified,
		TestFiles:  TestFiles{Suffixes: []string{"_test.go"}, Dirs: []string{"testdata"}},
	})

	add(LanguageSpec{
		Name:                "java",
		Interop:             []string{"java", "kotlin", "scala"},
		PackageIsDirectory:  true,
		ImportPathNamesFile: false,
		// varTypeQueries rewrites `b.build()` to `Builder.build()` before the
		// resolver ever sees it, which is what makes a receiver a type here.
		ReceiverDenotesType: true,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepJVM,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes:  []string{"test.java", "tests.java", "testcase.java"},
			Prefixes:  []string{"test"},
			Dirs:      []string{"test"},
			BaseNames: nil,
		},
	})

	// Groovy. A JVM language with Java's module system: a package is a directory
	// (across source roots, `src/main/groovy` and `src/test/groovy`), an import
	// names a class by its package path and the class's own name and never a file
	// stem, and a class sees its package's with no import. Classes map to files
	// the Java way, though a Groovy file may hold several, so the class is found by
	// the directory its package names and not by a file name.
	//
	// Unlike Java's, a Groovy receiver is not rewritten to a type by a query
	// (`def x = ...` states none), so ReceiverDenotesType is false, as for Scala.
	// Spock specifications are `*Spec.groovy` and JUnit tests `*Test.groovy`, both
	// under src/test/groovy.
	add(LanguageSpec{
		Name: "groovy",
		// Groovy calls Java and the other JVM languages through languageFamilies
		// (canBind), which is what the binding path reads; Interop is consulted
		// only by Python's static rules. It is not widened to the JVM set because
		// that would have to be declared back by Java, Kotlin and Scala, whose
		// behaviour this change does not touch.
		Interop:             []string{"groovy"},
		PackageIsDirectory:  true,
		ImportPathNamesFile: false,
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepJVM,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{"test.groovy", "tests.groovy", "spec.groovy", "testcase.groovy"},
			Dirs:     []string{"test"},
		},
	})

	// Apex. Java's syntax with a different module system: there are no imports and
	// no packages. A class is global to the org by its name, `Foo` in
	// classes/Foo.cls is visible from every file of the org whatever directory it
	// is in, and Salesforce refuses two classes of one name, so a type name that
	// the repository defines once IS the type (calltraits.go gives that rule the
	// tier it earns, UniqueTypeName). Proximity therefore says nothing: a
	// neighbour is no more in scope than any other class, so it is no evidence.
	//
	// Not a directory package (PackageIsDirectory false) and no file stem in an
	// import (there is no import). A receiver is a type where the parser has
	// stated one, as Java's is. Tests are classes annotated @isTest, which a file
	// convention cannot see; by convention they are named `FooTest` or `FooTests`.
	add(LanguageSpec{
		Name:                "apex",
		Interop:             []string{"apex"},
		PackageIsDirectory:  false,
		ImportPathNamesFile: false,
		ReceiverDenotesType: true,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: false,
		MemberSeparators:    sepJVM,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{"test.cls", "tests.cls", "_test.cls", "_tests.cls"},
			Dirs:     []string{"test", "tests"},
		},
	})

	// Solidity. A file is a module and sees nothing of its directory: a contract,
	// library or interface reaches another file's by an import, whose string names
	// the file (the parser rewrites it to the file's repository path, so an import
	// names one file and not whichever file has that ending). Proximity is
	// therefore no evidence, and a type next door is not in scope until imported
	// (TypesNeedImport), as in Dart.
	//
	// A library's functions and a contract's static-like members are reached as
	// `Lib.f()`, so a qualified call is on a type; MemberSeparators is the dot. A
	// receiver is a type where the parser stated one (`Vault v; v.f()`), and the
	// one inherited through `contract B is A` is found through the extends edges.
	// Foundry tests are `*.t.sol` under test/, and scripts `*.s.sol` under script/;
	// Hardhat's are JavaScript or TypeScript and are not in this language.
	solidity := LanguageSpec{
		Name:                "solidity",
		Interop:             []string{"solidity"},
		PackageIsDirectory:  false,
		ImportPathNamesFile: true,
		ReceiverDenotesType: true,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: false,
		TypesNeedImport:     true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{".t.sol", ".s.sol"},
			Dirs:     []string{"test", "tests"},
		},
	}
	add(solidity)

	add(LanguageSpec{
		Name:                "kotlin",
		Interop:             []string{"java", "kotlin", "scala"},
		PackageIsDirectory:  true,
		ImportPathNamesFile: false,
		ReceiverDenotesType: true,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepJVM,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{"test.kt", "tests.kt", "spec.kt"},
			Dirs:     []string{"test"},
		},
	})

	// --- File-is-module languages -------------------------------------------

	add(LanguageSpec{
		Name:    "typescript",
		Interop: []string{"typescript", "tsx", "javascript", "vue", "svelte", "astro"},
		// Each file is its own module; a neighbour is invisible without an
		// import. Imports must outrank proximity.
		PackageIsDirectory: false,
		// `from './sibling'` names sibling.ts. Matching file stems took
		// TypeScript from 87.0% to 87.5%.
		ImportPathNamesFile: true,
		// varTypeQueries covers `const b: Builder` and typed parameters.
		ReceiverDenotesType: true,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{".test.ts", ".spec.ts", ".test.tsx", ".spec.tsx"},
			Dirs:     []string{"__tests__", "test", "tests"},
		},
	})

	add(LanguageSpec{
		Name:    "tsx",
		Interop: []string{"typescript", "tsx", "javascript", "vue", "svelte", "astro"},
		// tsx is the typescript grammar plus JSX, and a .tsx file imports
		// ordinary .ts and .js modules constantly. Omitting Interop while tsx
		// sits in knownLanguages made bindableAcross fall through to a set of
		// {tsx} alone, so every cross-file reference out of a .tsx file was
		// refused — silently, as a missing edge rather than an error.
		PackageIsDirectory: false,
		// tsx is the typescript grammar plus JSX. It differs from `typescript`
		// in exactly one field, ReceiverDenotesType, and ONLY because
		// internal/parse ships no varTypeQuery for "tsx" — so a tsx receiver
		// really does arrive as a variable name. Recorded as a value rather
		// than left as an absent switch case, which is what it used to be.
		ImportPathNamesFile: true,
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{".test.tsx", ".spec.tsx"},
			Dirs:     []string{"__tests__", "test", "tests"},
		},
	})

	add(LanguageSpec{
		Name:    "vue",
		Interop: []string{"typescript", "tsx", "javascript", "vue", "svelte", "astro"},
		// A .vue file's <script> block is ordinary TypeScript or JavaScript and
		// imports ordinary modules. vue was in knownLanguages with no spec at
		// all, so it took the fallback — empty Interop — and bound only to other
		// .vue files, which is the narrowest possible answer and the wrong one.
		//
		// Mirrors the typescript spec because the script block IS typescript;
		// the only frontend-specific difference is that a component's members
		// are reached through the component object, which the receiver rules
		// already handle as a plain variable.
		PackageIsDirectory:  false,
		ImportPathNamesFile: true,
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{".test.vue", ".spec.vue"},
			Dirs:     []string{"__tests__", "test", "tests"},
		},
	})

	// svelte and astro are single-file components: their script blocks are
	// JavaScript or TypeScript and import ordinary modules, so they share the
	// script languages' interop set exactly as vue does.
	for _, sfc := range []string{"svelte", "astro"} {
		add(LanguageSpec{
			Name:                sfc,
			Interop:             []string{"typescript", "tsx", "javascript", "vue", "svelte", "astro"},
			PackageIsDirectory:  false,
			ImportPathNamesFile: true,
			ReceiverDenotesType: false,
			QualifiedMembers:    false,
			ClassSelfNames:      nil,
			SuperName:           "",
			PackageInitStem:     "",
			ProximityIsEvidence: true,
			MemberSeparators:    sepDefault,
			SelfNames:           commonSelfNames,
			MinTypeNameLength:   3,
			MaxCandidates:       MaxCandidates,
			CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		})
	}

	// css is deliberately alone in its interop set. A stylesheet's @import
	// names another stylesheet; it never names a function in another language.
	// The edges that connect CSS to markup are string-keyed class references,
	// which this package does not produce and which could not be defended above
	// `low` if it did.
	add(LanguageSpec{
		Name:                "css",
		Interop:             []string{"css"},
		PackageIsDirectory:  false,
		ImportPathNamesFile: true,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		// CSS has no methods or qualified calls, but `url(...)` and `var(...)`
		// are unqualified functional notation, so CallBare alone is the
		// honest shape. TestSpecsAreInternallyConsistent requires every spec
		// to declare at least one; leaving this zero is not "css has no
		// calls", it is silence that reads as an oversight.
		CallShapes: CallBare,
	})

	add(LanguageSpec{
		Name:                "javascript",
		Interop:             []string{"typescript", "tsx", "javascript", "vue", "svelte", "astro"},
		PackageIsDirectory:  false,
		ImportPathNamesFile: true,
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{".test.js", ".spec.js", ".test.mjs", ".spec.mjs"},
			Dirs:     []string{"__tests__", "test", "tests"},
		},
	})

	add(LanguageSpec{
		Name:    "python",
		Interop: []string{"python"},
		// A directory with __init__.py IS a package, but importing it still
		// requires an import statement — `from . import sibling` — so a
		// neighbour is NOT in scope unqualified the way a Go one is. Measured:
		// consulting proximity before imports cost scrapy 4.7pp of `high`
		// precision, 144 wrong edges from `get_crawler` alone.
		PackageIsDirectory: false,
		// `from .helpers import X` names helpers.py.
		ImportPathNamesFile: true,
		// Dynamically typed, so false, and it stays false with the scope
		// analysis in place: internal/parse replaces a receiver with a class
		// only where the source states one, and every other receiver is still
		// the variable. This field gates the two paths that treat ANY receiver
		// as a type — the rescue from the candidate cap and the ancestry walk
		// over an unbound name — and neither is sound for a variable.
		ReceiverDenotesType: false,
		// internal/parse/pyscope.go qualifies every nested definition the way
		// Python's own __qualname__ does, so `Account.save` and `Audit.save`
		// are different names and ownership can be read rather than inferred.
		QualifiedMembers: true,
		ClassSelfNames:   []string{"self", "cls"},
		SuperName:        "super",
		// shop/models/__init__.py is the module `shop.models`.
		PackageInitStem: "__init__",
		// Measured at 1 right in 213 across three repositories; see the field.
		ProximityIsEvidence: false,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes:  []string{"_test.py"},
			Prefixes:  []string{"test_"},
			BaseNames: []string{"conftest.py"},
			Dirs:      []string{"test", "tests"},
		},
	})

	// --- Scope-resolution-operator languages --------------------------------
	//
	// These carry non-default separators, their own test conventions, and
	// nothing else. That is not a claim that resolution is tuned for them; it
	// is the honest state, written out per language so that a measurement can
	// contradict one line rather than a loop.
	//
	// Ruby, Rust and PHP are closed to themselves, for the reason the frontend
	// languages were (see knownLanguages): none of them shares a module system
	// with Go, Python or TypeScript, so a binding across that line is a wrong
	// edge, and a wrong edge is worse than a missing one. Before this, a Ruby
	// caller with a nil Interop bound to Go definitions because ruby was
	// outside knownLanguages and bindableAcross kept the candidate. C# below is
	// closed the same way. C and C++ are left unclassified (nil): they share
	// headers with each other, and whether they should declare {"c", "cpp"} is
	// a widening question nobody has measured, so it is left open here rather
	// than answered in passing.

	scopeSpec := func(name string, interop []string, tests TestFiles) LanguageSpec {
		return LanguageSpec{
			Name:                name,
			Interop:             interop,
			PackageIsDirectory:  false,
			ImportPathNamesFile: true,
			ReceiverDenotesType: false,
			QualifiedMembers:    false,
			ClassSelfNames:      nil,
			SuperName:           "",
			PackageInitStem:     "",
			ProximityIsEvidence: true,
			MemberSeparators:    sepScope,
			SelfNames:           commonSelfNames,
			MinTypeNameLength:   3,
			MaxCandidates:       MaxCandidates,
			CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
			TestFiles:           tests,
		}
	}
	// RSpec and Minitest: `_spec.rb` under spec/, `_test.rb` under test/.
	ruby := scopeSpec("ruby", []string{"ruby"}, TestFiles{
		Suffixes: []string{"_spec.rb", "_test.rb"},
		Dirs:     []string{"spec", "test"},
	})
	// Measured on sinatra: a free function of the call's name in the same
	// directory was the target 0 times in 12. A Ruby file reaches another
	// through `require`, and a neighbour it did not require is not in scope.
	ruby.ProximityIsEvidence = false
	add(ruby)
	// Cargo: integration tests are any file under tests/, benchmarks under
	// benches/. Unit tests live INSIDE the module under #[cfg(test)], which a
	// file-level convention cannot see and this spec does not claim to.
	rust := scopeSpec("rust", []string{"rust"}, TestFiles{
		Dirs: []string{"tests", "benches"},
	})
	// `flags/doc/mod.rs` is the module `doc`: the directory names it and the
	// stem names nothing, exactly as `__init__.py` does for Python. Without
	// this `super::render()` from flags/doc/man.rs looked for a file called
	// `doc` and found none.
	rust.PackageInitStem = "mod"
	add(rust)
	// PHPUnit: `*Test.php`, conventionally under tests/.
	add(scopeSpec("php", []string{"php"}, TestFiles{
		Suffixes: []string{"test.php"},
		Dirs:     []string{"tests", "test"},
	}))
	// Bash. A script reaches another's functions only by `source`, so what a
	// script calls is a function it defines or one in a file it sources, and a
	// neighbour it does not source is no more in scope than any other file:
	// ProximityIsEvidence is off, as for Lua and Ruby. An import names a file
	// (the parser drops its extension), and a function has no qualifier, so a
	// call is always bare and is told from a command by name alone
	// (corefunctions.go).
	bash := scopeSpec("bash", []string{"bash"}, TestFiles{
		Suffixes: []string{".bats", "_test.sh", "-test.sh", ".test.sh"},
		Dirs:     []string{"test", "tests"},
	})
	bash.ProximityIsEvidence = false
	bash.MemberSeparators = sepDefault
	add(bash)
	// C and C++ have no test convention worth claiming; they keep the directory
	// rule. Interop nil: unclassified, pending the header-sharing decision above.
	// Swift. A module is the unit: `Sources/<Target>/**` is one package whose
	// files see each other with no import, and `import Core` names the target
	// whose directory is Core. So a directory tree is the package
	// (ModuleDirs), proximity is evidence at the strength Go's and Java's is,
	// and an import names a directory, never a file.
	swift := scopeSpec("swift", []string{"swift", "objc"}, TestFiles{
		Suffixes: []string{"tests.swift", "test.swift"},
		Dirs:     []string{"tests"},
	})
	swift.PackageIsDirectory = true
	swift.ImportPathNamesFile = false
	swift.ModuleDirs = []string{"Sources", "Tests"}
	swift.MemberSeparators = sepDefault
	add(swift)
	// Lua. A file is a module (`require("a.b")` is a/b.lua or a/b/init.lua) and
	// loads nothing it does not require, so a neighbour in the directory is no
	// more in scope than any other file: ProximityIsEvidence is off, as for
	// Ruby, which has the same rule.
	lua := scopeSpec("lua", []string{"lua"}, TestFiles{
		Suffixes: []string{"_spec.lua", "_test.lua"},
		Dirs:     []string{"spec", "test", "tests"},
	})
	lua.ProximityIsEvidence = false
	lua.PackageInitStem = "init"
	lua.MemberSeparators = sepDefault
	add(lua)
	// Elixir. A call reaches a module by name, not a file by path, and every rule
	// of this table that is about files (neighbours, import paths) is moot: the
	// resolution is elixir.go's. The spec says what is true regardless: a
	// directory is no package and proximity is no evidence.
	elixir := scopeSpec("elixir", []string{"elixir"}, TestFiles{
		Suffixes: []string{"_test.exs"},
		Dirs:     []string{"test"},
	})
	elixir.ProximityIsEvidence = false
	elixir.MemberSeparators = sepDefault
	add(elixir)
	// PowerShell. A script reaches another's functions by dot-sourcing it and a
	// module's by importing it, and a class by `using module`: a neighbour is in
	// scope only through one of them (ProximityIsEvidence off, TypesNeedImport on,
	// as for Dart). An import names a file or a module by name, with its extension
	// dropped by the parser.
	powershell := scopeSpec("powershell", []string{"powershell"}, TestFiles{
		Suffixes: []string{".tests.ps1"},
		Dirs:     []string{"tests", "test"},
	})
	powershell.ProximityIsEvidence = false
	powershell.TypesNeedImport = true
	powershell.MemberSeparators = sepDefault
	powershell.FoldNames = true
	add(powershell)
	// R. A file is a script that loads nothing it does not `source()`, except
	// inside a package's `R/`, whose files are one namespace (NamespaceDirs). An
	// import names a file (the parser drops its extension), and `pkg::f()` is
	// qualified by a package, not by a file of the repository.
	r := scopeSpec("r", []string{"r"}, TestFiles{
		Prefixes: []string{"test-", "test_"},
		Dirs:     []string{"testthat", "tests"},
	})
	r.PackageIsDirectory = true
	r.NamespaceDirs = []string{"R"}
	r.ProximityIsEvidence = false
	r.MemberSeparators = sepDefault
	add(r)
	// Erlang. A module is a file (the compiler insists that `-module(m)` lives in
	// m.erl), reached by name with no import, and a bare call reaches the file's
	// own functions or a `-import`: no neighbour is in scope, and the resolution
	// is modules.go's. Common Test suites are `*_SUITE.erl` and EUnit's are
	// `*_tests.erl`, both under test/.
	erlang := scopeSpec("erlang", []string{"erlang"}, TestFiles{
		Suffixes: []string{"_suite.erl", "_tests.erl", "_test.erl"},
		Dirs:     []string{"test"},
	})
	erlang.ProximityIsEvidence = false
	erlang.MemberSeparators = sepDefault
	add(erlang)
	// Clojure. A namespace is a path and a call reaches one by the name `:require`
	// gave it; a neighbour is in scope only by being required (modules.go).
	clojure := scopeSpec("clojure", []string{"clojure"}, TestFiles{
		Suffixes: []string{"_test.clj", "_test.cljc", "_test.cljs"},
		Dirs:     []string{"test"},
	})
	clojure.ProximityIsEvidence = false
	clojure.MemberSeparators = sepDefault
	add(clojure)
	// Julia. A module is a block with a name, and a file is part of the module
	// that `include`s it: no neighbour is in scope and the resolution is
	// modules.go's. Tests run from test/runtests.jl, which includes the others.
	julia := scopeSpec("julia", []string{"julia"}, TestFiles{
		Suffixes:  []string{"_test.jl", "_tests.jl"},
		BaseNames: []string{"runtests.jl"},
		Dirs:      []string{"test"},
	})
	julia.ProximityIsEvidence = false
	julia.MemberSeparators = sepDefault
	add(julia)
	// Haskell. A module is a file reached by the name `module A.B where` gives it
	// (A/B.hs, under a source directory the code does not state), and a bare name
	// reaches the module's own functions or what an `import` brings in: no
	// neighbour is in scope, so proximity is no evidence, and the resolution is
	// modules.go's. Hspec specs are `*Spec.hs`, Tasty and HUnit tests `*Test.hs` or
	// `*Tests.hs`, under test/ (or tests/, spec/).
	haskell := scopeSpec("haskell", []string{"haskell"}, TestFiles{
		Suffixes:  []string{"spec.hs", "test.hs", "tests.hs"},
		BaseNames: []string{"Spec.hs"},
		Dirs:      []string{"test", "tests", "spec"},
	})
	haskell.ProximityIsEvidence = false
	haskell.MemberSeparators = sepDefault
	add(haskell)
	// Zig. A file is a struct and sees nothing of its directory: what it uses it
	// imports with `@import("x.zig")`, by a path relative to itself (rewritten by
	// the parser to the file's repository path, so an import names one file and not
	// whichever file has that ending). Proximity is therefore no evidence, as for
	// Dart, and a type next door is not in scope until imported. `@import("std")`
	// names no file of the repository. Tests are `test "..." {}` blocks in ordinary
	// files, which a file convention cannot see; a file named for its tests
	// (`foo_test.zig`) and the test/ directory are what is claimed.
	zig := scopeSpec("zig", []string{"zig"}, TestFiles{
		Suffixes: []string{"_test.zig", "_tests.zig"},
		Dirs:     []string{"test", "tests"},
	})
	zig.ProximityIsEvidence = false
	zig.TypesNeedImport = true
	zig.MemberSeparators = sepDefault
	add(zig)
	// Dart. A file is a library and sees nothing of its directory: what it
	// uses it imports, by a path relative to itself or `package:` (rewritten by
	// the parser to the file's repository path, so an import names one file and
	// not whichever file has that ending). Proximity is therefore no evidence,
	// as in Python and Ruby, and a type next door is not in scope until
	// imported.
	dart := scopeSpec("dart", []string{"dart"}, TestFiles{
		Suffixes: []string{"_test.dart"},
		Dirs:     []string{"test", "tests", "integration_test"},
	})
	dart.ProximityIsEvidence = false
	dart.TypesNeedImport = true
	dart.ReExports = true
	dart.MemberSeparators = sepDefault
	add(dart)
	// Objective-C. A quoted #import names a header, and the header's methods are
	// defined in the .m of the same stem, so an import names a file stem (the
	// parser drops the extension) and covers both. Messages and C calls share one
	// linker with C and C++ (calltraits.go), and with Swift on Apple platforms.
	objc := scopeSpec("objc", []string{"objc", "swift"}, TestFiles{
		Suffixes: []string{"tests.m", "test.m", "tests.mm", "test.mm"},
		Dirs:     []string{"tests", "test"},
	})
	objc.MemberSeparators = sepDefault
	// A type in a neighbouring file is in scope only through an #import, as in C:
	// without one the compiler does not know it.
	objc.TypesNeedImport = true
	add(objc)
	// A class of a neighbouring file is in scope only through an #include, as in
	// Objective-C and CUDA, so two directories' `Stack` are told apart by the header
	// the file includes. ProximityIsEvidence stays on, unlike CUDA: measured on C and
	// C++ trees, the same-directory tier carries edges that are right far more often
	// than not (a function declared in a header whose name is not its source's), and
	// the `static` neighbour it binds wrongly is reported at low (calltraits.go).
	cpp := scopeSpec("cpp", nil, TestFiles{Dirs: []string{"test", "tests"}})
	cpp.TypesNeedImport = true
	add(cpp)
	add(scopeSpec("c", nil, TestFiles{Dirs: []string{"test", "tests"}}))
	// CUDA is C++: the same linker, the same #include of headers (`.cuh`, `.h`), and
	// a kernel launch `kern<<<g, b>>>(x)` is a call of `kern`. The spec is the
	// C++ one (the C family's Interop is the call traits' Family), with a type in a
	// neighbouring file in scope only through an #include, as in Objective-C.
	cuda := scopeSpec("cuda", nil, TestFiles{Dirs: []string{"test", "tests"}})
	cuda.TypesNeedImport = true
	// A `.cu` file is a translation unit of its own: what a neighbour defines is in
	// scope only through the header that declares it (an include of `vec.cuh` covers
	// `vec.cu`, which is how the pair is told), and a `static` function of the
	// neighbour is not linkable at all. Measured on the fixture, the C++ spec's
	// same-directory tier bound such a call.
	cuda.ProximityIsEvidence = false
	add(cuda)
	// GLSL and HLSL. A shader reaches another file's functions by #include and by
	// nothing else: it is compiled as one translation unit, per entry point, from
	// the file the compiler is handed. So a neighbour is no more in scope than any
	// other file (ProximityIsEvidence off, as for Lua), and a struct next door is
	// unknown until it is included. The language has no classes, no namespaces and
	// no qualified call: a call is bare, or a method of a built-in resource type
	// (`tex.Sample(s, uv)`), which no file of the repository defines.
	glsl := scopeSpec("glsl", nil, TestFiles{Dirs: []string{"test", "tests"}})
	glsl.ProximityIsEvidence = false
	glsl.TypesNeedImport = true
	glsl.MemberSeparators = sepDefault
	add(glsl)
	hlsl := scopeSpec("hlsl", nil, TestFiles{Dirs: []string{"test", "tests"}})
	hlsl.ProximityIsEvidence = false
	hlsl.TypesNeedImport = true
	hlsl.MemberSeparators = sepDefault
	add(hlsl)
	// CMake. A command (a function() or macro() that was run) is global once its
	// definition has run, wherever it was written, and what runs is what a file
	// include()s, what add_subdirectory() enters, and what the directories above it
	// ran before them: no neighbour is in scope on adjacency alone, and the
	// resolution is cmake.go's. Tests are `*_test.cmake` and a `test/` tree.
	cmake := scopeSpec("cmake", nil, TestFiles{
		Suffixes: []string{"_test.cmake", "test.cmake"},
		Dirs:     []string{"test", "tests"},
	})
	cmake.ProximityIsEvidence = false
	cmake.FoldNames = true
	cmake.MemberSeparators = sepDefault
	add(cmake)
	// MATLAB. A function file `foo.m` defines `foo`, and every function on the path
	// can call it by that name with no import; the repository is taken as one path
	// (matlab.go). A file is not a package and a directory is not either, so neither
	// proximity nor an import path means anything here: only a `+pkg` folder, which
	// qualifies, and an `@Class` folder, which makes its files methods. Tests are
	// `*Test.m` classes and a `tests/` folder.
	matlab := scopeSpec("matlab", []string{"matlab"}, TestFiles{
		Suffixes: []string{"test.m", "tests.m"},
		Prefixes: []string{"test_", "test"},
		Dirs:     []string{"test", "tests"},
	})
	matlab.ProximityIsEvidence = false
	matlab.MemberSeparators = sepDefault
	add(matlab)
	// Fortran. A module is a declaration and not a file, reached by `use` of its name
	// (modules.go), so no neighbour is in scope and proximity is no evidence. pFUnit
	// and test-drive tests are `test_*.f90` and `*_test.f90`, under test/ or tests/.
	fortran := scopeSpec("fortran", []string{"fortran"}, TestFiles{
		Suffixes: []string{"_test.f90", "_test.f", "_tests.f90", ".pf"},
		Prefixes: []string{"test_"},
		Dirs:     []string{"test", "tests"},
	})
	fortran.ProximityIsEvidence = false
	fortran.FoldNames = true
	fortran.MemberSeparators = sepDefault
	add(fortran)
	// HCL. A Terraform module is a directory: every `.tf` file in it shares one scope, and
	// a reference reaches a block by its address in any of them (hcl.go), so the directory
	// is the package. A `.tftest.hcl` file and the test/ and tests/ directories are Terraform's
	// own tests.
	hcl := scopeSpec("hcl", []string{"hcl"}, TestFiles{
		Suffixes: []string{".tftest.hcl"},
		Dirs:     []string{"test", "tests"},
	})
	hcl.PackageIsDirectory = true
	hcl.ProximityIsEvidence = false
	hcl.MemberSeparators = sepDefault
	add(hcl)
	// GDScript. A script is a class, reached by the class_name it declares, by the path
	// a preload or an extends writes, and by nothing else: a neighbour is no more in
	// scope than any other script (ProximityIsEvidence off), and what a name reaches is
	// gdscript.go's. GUT tests are `test_*.gd` files under test/ or tests/.
	gdscript := scopeSpec("gdscript", []string{"gdscript"}, TestFiles{
		Prefixes: []string{"test_"},
		Suffixes: []string{"_test.gd"},
		Dirs:     []string{"test", "tests"},
	})
	gdscript.ProximityIsEvidence = false
	gdscript.MemberSeparators = sepDefault
	add(gdscript)
	// Crystal. A program is the files its entry point requires, compiled as one: types
	// and modules are open and global by name, so a neighbour is no more in scope than
	// any file the program requires (ProximityIsEvidence off, as for Ruby). Specs are
	// `spec/**/*_spec.cr`.
	crystal := scopeSpec("crystal", []string{"crystal"}, TestFiles{
		Suffixes: []string{"_spec.cr"},
		Dirs:     []string{"spec"},
	})
	crystal.ProximityIsEvidence = false
	add(crystal)
	// Nim. A module is a file, reached by `import a/b` and nothing else, and only the
	// names it exports (`proc f*()`) are visible to its importers. A neighbour is no
	// more in scope than any other file (ProximityIsEvidence off), and what a name
	// reaches is filemodules.go's. Testament's tests are `t*.nim` under `tests/`, and
	// unittest's `test_*.nim` and `*_test.nim`.
	nim := scopeSpec("nim", []string{"nim"}, TestFiles{
		Suffixes: []string{"_test.nim", "_tests.nim"},
		Prefixes: []string{"test_"},
		Dirs:     []string{"tests", "test"},
	})
	nim.ProximityIsEvidence = false
	nim.MemberSeparators = sepDefault
	add(nim)
	// D. A module is a file, reached by `import a.b;`, and what a name reaches is
	// what the file declares, what its class declares or what an import brings in
	// (filemodules.go): a neighbour is no more in scope than any other file. Tests
	// are `unittest {}` blocks in ordinary files, which a file convention cannot see;
	// the `test/` and `tests/` directories and `*_test.d` files are what is claimed.
	dlang := scopeSpec("d", []string{"d"}, TestFiles{
		Suffixes: []string{"_test.d", "_tests.d", "_unittest.d"},
		Dirs:     []string{"tests", "test", "unittest"},
	})
	dlang.ProximityIsEvidence = false
	dlang.MemberSeparators = sepDefault
	add(dlang)
	// ReScript. A file Foo.res is the module Foo, global to the project: no import is
	// needed to write `Foo.f()`, and a neighbour is no more in scope than any other
	// file (ProximityIsEvidence off). What a bare name reaches is the file's own
	// definitions or what an `open` brings in, which is modules.go's. Tests are
	// `*_test.res` and `*Test.res` files and the `__tests__/` directory.
	rescript := scopeSpec("rescript", []string{"rescript"}, TestFiles{
		Suffixes: []string{"_test.res", "test.res", "_spec.res", "_test.resi"},
		Dirs:     []string{"__tests__", "test", "tests"},
	})
	rescript.ProximityIsEvidence = false
	rescript.MemberSeparators = sepDefault
	add(rescript)

	// --- C# ------------------------------------------------------------------
	//
	// In the resolved tier (it has an import query) and, until now, on the
	// fallback spec — which, having no Interop, let a .cs file bind to Go and
	// Python definitions. A namespace is declared, not derived from the
	// directory, and a `using` names a namespace, never a file, so this is the
	// first row where PackageIsDirectory and ImportPathNamesFile are BOTH false
	// — the case the ImportPathNamesFile doc says the two fields were kept
	// apart for. `.` separates every member. xUnit, NUnit and MSTest all end a
	// test class file in `Tests.cs` or `Test.cs`. A test PROJECT directory ends
	// in `.Tests`, and Matches catches it through the `tests` Dir because
	// pathSegments splits directory names on `.` as well as `/` — an accident
	// of the import-path splitter, not a declared rule, and one
	// capability.IsTestPath does not share.
	add(LanguageSpec{
		Name:                "c_sharp",
		Interop:             []string{"c_sharp"},
		PackageIsDirectory:  false,
		ImportPathNamesFile: false,
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepDefault,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{"tests.cs", "test.cs"},
			Dirs:     []string{"tests", "test"},
		},
	})

	add(LanguageSpec{
		// Scala is JVM-shaped in its separators and was NOT in the
		// directory-is-package switch. Scala packages are declared, not derived
		// from the directory, so false is defensible — but it is now a stated
		// value that a measurement can contradict, rather than an omission.
		Name:                "scala",
		Interop:             []string{"java", "kotlin", "scala"},
		PackageIsDirectory:  false,
		ImportPathNamesFile: true,
		ReceiverDenotesType: false,
		QualifiedMembers:    false,
		ClassSelfNames:      nil,
		SuperName:           "",
		PackageInitStem:     "",
		ProximityIsEvidence: true,
		MemberSeparators:    sepJVM,
		SelfNames:           commonSelfNames,
		MinTypeNameLength:   3,
		MaxCandidates:       MaxCandidates,
		CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
		TestFiles: TestFiles{
			Suffixes: []string{"spec.scala", "test.scala"},
			Dirs:     []string{"test"},
		},
	})

	generic := make(map[string]struct{}, len(genericNames()))
	for _, n := range genericNames() {
		generic[n] = struct{}{}
	}

	// Every spec takes its call traits from the one table that states them.
	for name, spec := range byName {
		spec.Calls = callTraitsFor(name)
		byName[name] = spec
	}

	return &Specs{byName: byName, fallback: fallback, generic: generic}
}

// For returns the spec for a language, or the fallback carrying that language's
// name.
//
// Never nil, never an error: a language RepoCortex has no spec for still parses,
// and refusing to resolve it would turn a recall gap into an outage.
func (s *Specs) For(language string) LanguageSpec {
	if spec, ok := s.byName[language]; ok {
		return spec
	}
	spec := s.fallback
	spec.Name = language
	spec.Calls = callTraitsFor(language)
	return spec
}

// Names lists the languages carrying an explicit spec, sorted.
//
// Sorted, not in map order: everything this package exposes has to be stable
// across runs, and a caller that prints or hashes this must not see a fresh
// permutation each time.
func (s *Specs) Names() []string {
	out := make([]string, 0, len(s.byName))
	for name := range s.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// interopSet is the set of languages spec may bind into, itself always included.
// An empty Interop means "only itself" for a language in knownLanguages; for
// any other language bindableAcross treats the empty set as unclassified and
// keeps the candidate, so the fallback spec refuses nothing.
func (s LanguageSpec) interopSet() map[string]bool {
	out := map[string]bool{s.Name: true}
	for _, l := range s.Interop {
		out[l] = true
	}
	return out
}

// languageOfPath names a file's language from its extension.
//
// Derived here rather than carried on SymbolIndexEntry because that would mean
// a schema column and a wider query for a fact the path already states. It
// returns "" for anything unrecognised, and every caller treats "" as UNKNOWN
// and keeps the candidate — the A19 lesson: a filter that removes candidates on
// incomplete information removes the ambiguity that was protecting the caller.
func languageOfPath(p string) string {
	i := strings.LastIndexByte(p, '.')
	if i < 0 {
		return ""
	}
	switch strings.ToLower(p[i+1:]) {
	case "go":
		return "go"
	case "py", "pyi":
		return "python"
	case "ts", "mts", "cts":
		return "typescript"
	case "tsx":
		return "tsx"
	case "js", "mjs", "cjs", "jsx":
		return "javascript"
	case "vue":
		return "vue"
	case "svelte":
		return "svelte"
	case "astro":
		return "astro"
	case "css", "scss", "less":
		return "css"
	case "java":
		return "java"
	case "kt", "kts":
		return "kotlin"
	case "scala", "sc":
		return "scala"
	case "rb":
		return "ruby"
	case "rs":
		return "rust"
	case "php":
		return "php"
	case "c", "h":
		return "c"
	case "cc", "cpp", "cxx", "hpp", "hh", "hxx":
		return "cpp"
	case "swift":
		return "swift"
	case "lua":
		return "lua"
	case "sh", "bash", "bats":
		return "bash"
	case "r":
		return "r"
	case "ex", "exs":
		return "elixir"
	case "ps1", "psm1", "psd1":
		return "powershell"
	case "dart":
		return "dart"
	case "groovy":
		return "groovy"
	case "cls", "trigger", "apex":
		return "apex"
	case "sol":
		return "solidity"
	case "cs":
		// The GRAMMAR name, as internal/parse spells it and as the c_sharp spec
		// is keyed. This used to say "csharp", which no spec is named, so a
		// c_sharp caller with a declared Interop would have refused every .cs
		// destination as a foreign language.
		return "c_sharp"
	}
	return ""
}

// bindableAcross reports whether a definition in dstPath is reachable from a
// caller governed by spec.
//
// FAILS OPEN. An unrecognised extension on either side keeps the candidate: the
// point is to drop bindings between languages that demonstrably share no module
// system, not to drop everything this function cannot classify.
func (s LanguageSpec) bindableAcross(dstPath string) bool {
	dst := languageOfPath(dstPath)
	if dst == "" || s.Name == "" {
		return true
	}
	if dst == s.Name {
		return true
	}
	set := s.interopSet()
	// The caller's own language is unclassified here (fallback spec): keep.
	if len(set) <= 1 && len(s.Interop) == 0 && s.Name != dst {
		// Only refuse when we positively know both sides AND they are both
		// languages we have specs for. Otherwise this is a guess.
		if _, known := knownLanguages[dst]; !known {
			return true
		}
		if _, known := knownLanguages[s.Name]; !known {
			return true
		}
	}
	return set[dst]
}

// knownLanguages are the languages this package has an opinion about. A pair
// where either side is absent is never refused.
var knownLanguages = map[string]bool{
	"go": true, "python": true, "typescript": true, "tsx": true,
	"javascript": true, "vue": true, "java": true, "kotlin": true, "scala": true,
	// Frontend languages. Absent from this map, bindableAcross returned true
	// for every destination, so a .svelte or .css file could bind to a Go
	// symbol. A wrong edge is worse than a missing one, and these three have a
	// module system that has nothing to do with Go's.
	"svelte": true, "astro": true, "css": true,
	// C# declares Interop {"c_sharp"}, so it has an opinion and belongs here.
	"c_sharp": true,
}
