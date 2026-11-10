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
// the fact. Nothing reads it to exclude a candidate, and nothing may, until
// internal/parse can distinguish "no receiver in the source" from "a receiver
// the query could not express".
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
	return r != "" && !s.isSelfName(r)
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
// DESCRIPTIVE; see the TestFiles doc for why it is not consulted when binding.
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

	add(LanguageSpec{
		Name:                "kotlin",
		Interop:             []string{"java", "kotlin", "scala"},
		PackageIsDirectory:  true,
		ImportPathNamesFile: false,
		ReceiverDenotesType: true,
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
		// Dynamically typed, and internal/parse ships no varTypeQuery: a Python
		// receiver is the variable, never the class.
		ReceiverDenotesType: false,
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
			MemberSeparators:    sepScope,
			SelfNames:           commonSelfNames,
			MinTypeNameLength:   3,
			MaxCandidates:       MaxCandidates,
			CallShapes:          CallBare | CallMethod | CallQualified | CallStatic,
			TestFiles:           tests,
		}
	}
	// RSpec and Minitest: `_spec.rb` under spec/, `_test.rb` under test/.
	add(scopeSpec("ruby", []string{"ruby"}, TestFiles{
		Suffixes: []string{"_spec.rb", "_test.rb"},
		Dirs:     []string{"spec", "test"},
	}))
	// Cargo: integration tests are any file under tests/, benchmarks under
	// benches/. Unit tests live INSIDE the module under #[cfg(test)], which a
	// file-level convention cannot see and this spec does not claim to.
	add(scopeSpec("rust", []string{"rust"}, TestFiles{
		Dirs: []string{"tests", "benches"},
	}))
	// PHPUnit: `*Test.php`, conventionally under tests/.
	add(scopeSpec("php", []string{"php"}, TestFiles{
		Suffixes: []string{"test.php"},
		Dirs:     []string{"tests", "test"},
	}))
	// C and C++ have no test convention worth claiming; they keep the directory
	// rule. Interop nil: unclassified, pending the header-sharing decision above.
	add(scopeSpec("cpp", nil, TestFiles{Dirs: []string{"test", "tests"}}))
	add(scopeSpec("c", nil, TestFiles{Dirs: []string{"test", "tests"}}))

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
