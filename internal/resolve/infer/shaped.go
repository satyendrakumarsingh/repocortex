package infer

import (
	"fmt"
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Binding a call by what it was made on
//
// static.go binds a Python call by the class its receiver statically holds,
// and refuses a candidate the language's scoping makes unreachable. This file
// is those two ideas for every language whose parser records the shape of a
// call (model.ReceiverBare and its siblings) and qualifies definitions by what
// encloses them — which, since shape.go, is every language with references.
//
// One rule per shape. Each rule names the evidence it rests on in the reason
// it attaches to an edge, so its precision can be measured on its own, and
// each carries the tier that measurement earned (docs/accuracy.md):
//
//	bare      f()         a definition the name can reach lexically: the
//	                      enclosing scope, the file, the package, an import
//	                      that names it. In a language with an implicit `this`,
//	                      a member of the enclosing type. Never a member of
//	                      some other type.
//	self      self.f()    a member of the enclosing type, then of its ancestors
//	super     super.f()   a member of an ancestor, never the enclosing method
//	type      x.f()       a member of the type the source states for x
//	name      x.f()       x is an import: the package it names, or nothing
//	                      here at all if that package is elsewhere. x is a
//	                      type: its member. Otherwise a value of unknown type.
//	scope     a::f()      the module or type the path names
//	path      a.b.f()     the same, through a longer qualifier
//	value     x.f()       a value whose type nothing states
//	expr      g().f()     an expression with no name
//
// What no rule does is the thing the old tiers did for all of them: bind a
// qualified call to a free function, or to the function it is written in,
// because the name matched.
//
// # A member call on a receiver of unknown type
//
// `value`, `expr`, and every `name` or `path` that turned out to be neither an
// import nor a type end in the same place: a method was called on something
// whose type the source does not state. That is dynamic dispatch, and the only
// evidence left is the name. If exactly one type in the repository declares a
// member of that name, the call is bound to it at a tier below `high`; if
// several do it is ambiguous; and a name too common for uniqueness to mean
// anything is left alone. See memberByName.

// verdict is what a rule concluded about one call.
type verdict uint8

const (
	// verdictUnresolved: nothing the call could denote is in the index.
	verdictUnresolved verdict = iota
	// verdictBound: one definition, with the confidence its evidence earns.
	verdictBound
	// verdictAmbiguous: several definitions and nothing to choose by.
	verdictAmbiguous
	// verdictExternal: the qualifier names something outside the repository.
	verdictExternal
	// verdictDeferred: parked for the ancestry pass, which settles it.
	verdictDeferred
)

type decision struct {
	verdict verdict
	best    candidate
}

func bound(c candidate) decision { return decision{verdict: verdictBound, best: c} }

var (
	unresolvedCall = decision{verdict: verdictUnresolved}
	ambiguousCall  = decision{verdict: verdictAmbiguous}
	externalCall   = decision{verdict: verdictExternal}
	deferredCall   = decision{verdict: verdictDeferred}
)

// The reasons attached to edges. Each is one rule, and the text up to the
// first parenthesis is stable so that precision can be grouped by rule.
const (
	ruleEnclosingScope    = "defined in the enclosing scope"
	ruleSameFile          = "defined in the same file"
	ruleSameFileSeveral   = "several definitions in the same file"
	ruleSameFileOtherType = "the only method of this name in the file; which object receives the call is decided at run time"
	ruleRequiredOtherType = "a method of this name in a required file; which object receives the call is decided at run time"
	ruleEnclosingType     = "declared on the enclosing type"
	ruleNestedType        = "a type nested in the enclosing type"
	ruleSamePackage       = "same package directory"
	ruleImportedByName    = "imported by name from the module that defines it"
	ruleReExported        = "imported by name from a package that re-exports it"
	ruleStaticImport      = "statically imported from the class that declares it"
	ruleQualifiedModule   = "qualified by an imported package that defines it"
	ruleQualifiedPackage  = "qualified by an imported package that re-exports it"
	ruleQualifiedFile     = "qualified by the module whose file defines it"
	ruleOwnExports        = "qualified by exports: defined at the top level of the same file"
	ruleImportedObject    = "declared on the object the receiver imports"
	ruleStatedType        = "declared on the receiver's stated type"
	ruleNamedType         = "declared on the type the receiver names"
	ruleConstructs        = "constructs the type the receiver names"
	ruleExtension         = "the only function of this name; called as a method, so an extension function of the receiver's type"
	ruleNamedAfterType    = "receiver is named after the type that declares it"
	ruleOnlyMember        = "the only member of this name in the repository; the receiver's type is not known"
	ruleOnlyType          = "the only type that declares a member of this name; the receiver's type is not known"
	ruleOnlyFree          = "the only free function of this name in the repository; its namespace is not a file"
)

// call is one reference being resolved, with what every rule needs about it.
type call struct {
	spec    LanguageSpec
	fr      registry.FileRefs
	imports importSet
	ref     registry.Reference
	cands   []registry.SymbolIndexEntry
	symbols map[string][]registry.SymbolIndexEntry

	// scope is the qualified name of the definition the call is written in,
	// class the qualified name of the type that definition belongs to, and
	// within the id of that definition. Each is zero at file level.
	scope  string
	class  string
	within int64
}

// isConstructorDelegation reports whether the call is `this(...)` written in a
// constructor, which runs another constructor of the same class. Java and C#
// record it as a call named after the class on the receiver `this`; Scala
// writes `def this(...) = this(...)` and records it as a call named `this`.
//
// It never targets the constructor it is written in: `def this() = this(0)` is
// not a call of itself, and a language does not allow a constructor to delegate
// to itself. Its candidates are the class's other constructors, and the primary
// constructor, which Scala does not declare as a symbol of its own.
func (c call) isConstructorDelegation() bool {
	switch c.spec.Name {
	case "scala":
		return c.ref.Name == "this" && c.ref.ReceiverKind == model.ReceiverBare
	case "java", "c_sharp", "kotlin":
		return c.ref.ReceiverKind == model.ReceiverSelf && c.ref.Receiver == "this" &&
			c.class != "" && c.ref.Name == lastQualSegment(c.class)
	}
	return false
}

// otherConstructors is what a delegation can reach: the constructors the
// enclosing class declares in this file, less the one it is written in, that
// take the number of arguments it passes. A
// constructor of another class that has the same name (Scala's are all called
// `this`) is not a candidate.
func (c call) otherConstructors() []registry.SymbolIndexEntry {
	owner := lastQualSegment(c.class)
	var kept []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if s.ID == c.within || s.FileID != c.fr.FileID {
			continue
		}
		if o, ok := memberOwner(s); ok && o == owner {
			kept = append(kept, s)
		}
	}
	// The one whose parameter count is the argument count; with none, the
	// delegation is to a constructor the index does not hold (Scala's primary).
	return narrowByArity(c.ref, kept)
}

// resolveCall binds one call by its shape.
func (r *Resolver) resolveCall(c call, stats *Stats) decision {
	c.scope = enclosingScope(c.fr, c.ref.Line)
	c.class = enclosingClass(c.scope)
	c.within, _ = enclosing(c.fr.Symbols, c.ref.Line)
	if c.class == "" && c.spec.Calls.ClassBodyCode && c.scope != "" && declaresMembers(c.fr, c.scope) {
		c.class = c.scope
	}

	if c.isConstructorDelegation() {
		c.cands = c.otherConstructors()
		if len(c.cands) == 0 {
			// Delegation to the primary constructor, or to one the index does
			// not hold.
			return unresolvedCall
		}
	}

	if isMacroInvocation(c.ref.Kind) {
		return r.callMacro(c)
	}
	c.cands = withoutMacros(c.cands)
	if isConstruction(c.ref.Kind) {
		return r.callNew(c, stats)
	}
	if !c.spec.Calls.CallConstructs && c.spec.Calls.NewConstructs {
		// canBind let types through for the sake of `new`. A call without it
		// still cannot denote one.
		kept := c.cands[:0:0]
		for _, s := range c.cands {
			if !isConstructibleKind(s.Kind) {
				kept = append(kept, s)
			}
		}
		c.cands = kept
	}

	if c.spec.Name == "elixir" {
		// A call reaches a module by its name, which no rule below can say. Only
		// Elixir's calls come here; see elixir.go.
		return r.callElixir(c, stats)
	}

	if modularLanguages[c.spec.Name] {
		// Erlang, Clojure and Julia reach a function through the module it is in,
		// which no rule below can say. Only their calls come here; see modules.go.
		return r.callModular(c, stats)
	}

	switch c.ref.ReceiverKind {
	case model.ReceiverBare:
		return r.callBare(c, stats)
	case model.ReceiverSelf:
		return r.callSelf(c, stats)
	case model.ReceiverSuper:
		return r.callSuper(c, stats)
	case model.ReceiverType:
		return r.callTyped(c, stats)
	case model.ReceiverName:
		return r.callNamed(c, stats)
	case model.ReceiverScope:
		return r.callScoped(c, stats)
	case model.ReceiverPath:
		return r.callPath(c, stats)
	case model.ReceiverValue:
		// `const util = require('./util')` is a declaration to the parser's walk
		// and an import to the file: the walk can say the name was declared and
		// not that it was declared by a require. A name the file's imports bind
		// is read as that import.
		if c.spec.Calls.Imports == ImportsNameModules {
			if _, _, ok := r.scriptQualifier(c.fr, c.imports, c.ref.Receiver); ok {
				return r.callNamed(c, stats)
			}
		}
		return r.memberByName(c, model.ConfidenceHigh, stats)
	default:
		// An expression with no name, or a shape nothing recorded.
		return r.memberByName(c, model.ConfidenceLow, stats)
	}
}

// --- types and their members -------------------------------------------------

// typeRef is a type definition, identified.
type typeRef struct {
	name     string
	fileID   int64
	filePath string
	qual     string

	// strong is true when the identification rests on something the source
	// states — the file defines it, the package does, an import names it — and
	// false when all that is known is that one type in the repository has the
	// name.
	strong bool
}

// enclosingType is the type the call is written in.
func (c call) enclosingType() (typeRef, bool) {
	if c.class == "" {
		return typeRef{}, false
	}
	return typeRef{
		name: lastQualSegment(c.class), fileID: c.fr.FileID, filePath: c.fr.Path,
		qual: c.class, strong: true,
	}, true
}

// typeNamed identifies the type a name denotes from where the call is.
//
// The order is the order a language resolves a name in: something the file
// defines and the call can see; something the file's package defines;
// something an import names; and, failing all three, a name only one type in
// the repository has. known reports whether ANY type of that name exists,
// identified or not — a caller needs it to tell "defined nowhere here" from
// "defined several times, and nothing chooses".
func (r *Resolver) typeNamed(c call, name string) (t typeRef, found, known bool) {
	var types []registry.SymbolIndexEntry
	for _, s := range c.symbols[name] {
		if isTypeKind(s.Kind) && c.spec.canBind(s, model.EdgeReferences) {
			types = append(types, s)
		}
	}
	if len(types) == 0 {
		return typeRef{}, false, false
	}
	pick := func(group []registry.SymbolIndexEntry, strong bool) (typeRef, bool) {
		if len(group) == 0 {
			return typeRef{}, false
		}
		first := group[0]
		for _, s := range group[1:] {
			if s.ID < first.ID {
				first = s
			}
		}
		qual, _ := strings.CutPrefix(first.FQN, first.FilePath+":")
		return typeRef{name: name, fileID: first.FileID, filePath: first.FilePath, qual: qual, strong: strong}, true
	}

	var local, sameDir []registry.SymbolIndexEntry
	dir := c.spec.packageOf(c.fr.Path)
	for _, s := range types {
		switch {
		case s.FileID == c.fr.FileID && visibleFrom(c.scope, s):
			local = append(local, s)
		case c.spec.packageOf(s.FilePath) == dir && (!c.spec.TypesNeedImport || importsFile(c.imports, s.FilePath)):
			sameDir = append(sameDir, s)
		}
	}
	if t, ok := pick(innermost(local), true); ok {
		return t, true, true
	}
	// A class the file imports by name is the one it means, before any class
	// that merely shares its package: a single-type import shadows the
	// package's own types. moshi: a test in package kotlin.reflect imports
	// nothing of that name, but `Moshi` (the class) matched the directory
	// `moshi` and made a deprecated shim two levels up the "imported" one.
	if c.spec.Calls.Imports == ImportsNameClasses {
		if t, ok := pick(r.importedByName(c, name, types), true); ok {
			return t, true, true
		}
	}
	if t, ok := pick(sameDir, true); ok {
		return t, true, true
	}
	if c.spec.Calls.WildcardImports {
		// `import a.b.*` brings the types of package a.b into scope, after the
		// file's own package and a single-type import of the name.
		if t, ok := pick(r.importedByPackage(c, types), true); ok {
			return t, true, true
		}
	}
	if c.spec.PackageIsDirectory && c.spec.Calls.Imports == ImportsNameClasses {
		// A JVM package spans source roots: `src/main/java/com/pay` and
		// `src/test/java/com/pay` are one package, and its classes see each
		// other with no import. The package is the tail the two directories
		// share past their source roots.
		var samePackage []registry.SymbolIndexEntry
		for _, s := range types {
			if sharesPackageTail(dir, path.Dir(s.FilePath)) {
				samePackage = append(samePackage, s)
			}
		}
		if sameDirectory(c.spec, samePackage) {
			if t, ok := pick(samePackage, true); ok {
				return t, true, true
			}
		}
	}

	switch c.spec.Calls.Imports {
	case ImportsNameModules:
		if m, _, ok := r.scriptQualifier(c.fr, c.imports, name); ok {
			var imported []registry.SymbolIndexEntry
			for _, s := range types {
				if (m.contains != nil && m.contains(s)) || (m.beneath != nil && m.beneath(s)) {
					imported = append(imported, s)
				}
			}
			if sameDirectory(c.spec, imported) {
				if t, ok := pick(imported, true); ok {
					return t, true, true
				}
			}
		}
	}

	// A name the platform defines (`Response`, `Request`, `Headers`, `URL`,
	// `Map`, `Date`, `Error`) that the file neither declares, nor sits beside,
	// nor imports is the platform's, whatever one type of the name some other
	// directory of the repository declares. hono: `response.clone()` on a
	// parameter typed `Response` in a test file was bound, at `high`, to the
	// `clone` of a type reached through the client's own `Response` interface.
	if c.spec.Calls.Imports == ImportsNameModules && c.spec.isBuiltinType(name) {
		return typeRef{}, false, false
	}

	// One type of the name in the whole repository — or one directory's worth,
	// which is one type declared in parts (a header and its source, a partial
	// class).
	if len(c.spec.ModuleDirs) > 0 || c.spec.TypesNeedImport {
		// A module imported whole brings its types into scope with no name to
		// say which file they are in (`import Core` and `Money.zero()`), and a
		// type of the same name in a module that was not imported is not in
		// scope at all. Asked after the file's own module, before the
		// repository-wide fallback below, which would call two modules' Money
		// ambiguous and leave the call unbound.
		var imported []registry.SymbolIndexEntry
		best := 0
		for _, s := range types {
			if d, ok := c.imports.covers(s.FilePath); ok && d > best {
				best = d
			}
		}
		for _, s := range types {
			if d, ok := c.imports.covers(s.FilePath); ok && d == best && best > 0 {
				imported = append(imported, s)
			}
		}
		if sameDirectory(c.spec, imported) {
			if t, ok := pick(imported, true); ok {
				return t, true, true
			}
		}
	}
	if sameDirectory(c.spec, types) {
		t, _ := pick(types, false)
		return t, true, true
	}
	return typeRef{}, false, true
}

// sourceRootMarkers are the directory names a JVM build puts between a
// module and its packages: `src/main/java/…`, `src/test/kotlin/…`.
var sourceRootMarkers = map[string]bool{"java": true, "kotlin": true, "scala": true, "groovy": true}

// sharesPackageTail reports whether two directories are the same package
// under different source roots: past a source-root marker, the rest of both
// paths is identical.
func sharesPackageTail(a, b string) bool {
	tail := func(dir string) (string, bool) {
		segs := strings.Split(dir, "/")
		for i := len(segs) - 1; i >= 0; i-- {
			if sourceRootMarkers[segs[i]] && i < len(segs)-1 {
				return strings.Join(segs[i+1:], "/"), true
			}
		}
		return "", false
	}
	ta, okA := tail(a)
	tb, okB := tail(b)
	return okA && okB && ta == tb
}

// importedBare binds a bare name the file imported by name from a package:
// Kotlin's and Scala's `Foo()`. It comes from that import and from nowhere else,
// ahead of a class the same file or package holds under that name and of the one
// definition of the name in the repository; an explicit import outranks the
// package's own declarations in both languages. moshi: `KotlinJsonAdapterFactory()`
// in a test of the package that declares it, and in the deprecated shim that
// imports the real class, was bound at `high` to the shim.
//
// A declaration nearer than the file's top level is not outranked by an import:
// a local, and a type nested in the enclosing one (nestedType).
func (r *Resolver) importedBare(c call, reach []registry.SymbolIndexEntry) (decision, bool) {
	if c.spec.Calls.Imports != ImportsNameClasses || !c.spec.Calls.CallConstructs {
		return decision{}, false
	}
	imported := false
	for _, e := range c.imports.imports {
		if importsFromPackage(e, c.ref.Name) {
			imported = true
			break
		}
	}
	if !imported {
		return decision{}, false
	}
	for _, s := range c.cands {
		if _, isLocal := localScope(s); isLocal && s.FileID == c.fr.FileID && visibleFrom(c.scope, s) {
			return decision{}, false
		}
	}
	for qual := c.class; qual != ""; qual = outerQual(qual) {
		if _, nested := c.nestedType(qual); nested {
			return decision{}, false
		}
	}
	for _, e := range c.imports.imports {
		if importsFromPackage(e, c.ref.Name) && r.layout.Known() && !r.layout.HasDirEnding(e.segments[:len(e.segments)-1]) {
			return externalCall, true
		}
	}
	if group := r.importedByName(c, c.ref.Name, reach); len(group) > 0 {
		if first, ok := callableFirst(c.ref, group); ok {
			return bound(candidate{first, model.ConfidenceHigh, ruleImportedByName}), true
		}
		return ambiguousCall, true
	}
	return decision{}, false
}

// importedByName returns, of the given definitions, the top-level ones in the
// package a single-type import of the name brings it from: `import a.b.Foo`
// names the Foo declared in a directory that ends with a/b, whatever the file
// is called and whichever source root the directory is under. Nothing when the
// file has no such import.
func (r *Resolver) importedByName(c call, name string, among []registry.SymbolIndexEntry) []registry.SymbolIndexEntry {
	for _, e := range c.imports.imports {
		if !importsFromPackage(e, name) {
			continue
		}
		pkg := e.segments[:len(e.segments)-1]
		var out []registry.SymbolIndexEntry
		for _, s := range among {
			if isModuleLevel(s) && commonSuffixLen(pkg, pathSegments(path.Dir(s.FilePath))) == len(pkg) {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// importedByPackage returns, of the given types, the top-level ones in a package
// the file imports whole: `import a.b.*` is the row `a.b`, a package, and names
// the types declared in a directory that ends with a/b. A row whose last segment
// is a class (`import a.b.Outer.*`, a static member) or a path no directory ends
// with names none.
func (r *Resolver) importedByPackage(c call, types []registry.SymbolIndexEntry) []registry.SymbolIndexEntry {
	var out []registry.SymbolIndexEntry
	for _, e := range c.imports.imports {
		if e.alias != "" || len(e.segments) == 0 || capitalised(lastRawSegment(e.raw)) {
			continue
		}
		for _, s := range types {
			if isModuleLevel(s) && commonSuffixLen(e.segments, pathSegments(path.Dir(s.FilePath))) == len(e.segments) {
				out = append(out, s)
			}
		}
	}
	return out
}

// importsFromPackage reports whether an import is `import pkg.Name`: it names
// Name, and what precedes it is a package, not a class or object whose member
// Name is (`import test.TestUtil.prep` takes a member, not a class of a
// package `testutil`).
func importsFromPackage(e importEntry, name string) bool {
	if e.alias != "" || len(e.segments) < 2 || lastRawSegment(e.raw) != name {
		return false
	}
	parts := strings.Split(e.raw, ".")
	return len(parts) >= 2 && !capitalised(parts[len(parts)-2])
}

// importsFile reports whether one of the file's imports covers the file.
func importsFile(imports importSet, filePath string) bool {
	_, ok := imports.covers(filePath)
	return ok
}

// sameDirectory reports whether every entry is defined in one directory.
// False for none.
func sameDirectory(spec LanguageSpec, cs []registry.SymbolIndexEntry) bool {
	if len(cs) == 0 {
		return false
	}
	dir := spec.packageOf(cs[0].FilePath)
	for _, c := range cs[1:] {
		if spec.packageOf(c.FilePath) != dir {
			return false
		}
	}
	return true
}

// membersOf returns the candidates a type declares, nearest first: in the
// type's own file under its own qualified name; then, where the language lets
// a type's members be declared elsewhere, in its directory; then anywhere. The
// rank says which.
func membersOf(spec LanguageSpec, t typeRef, cands []registry.SymbolIndexEntry) (group []registry.SymbolIndexEntry, rank int) {
	want := t.filePath + ":" + t.qual + "."
	var same, dirOnly, anywhere []registry.SymbolIndexEntry
	dir := path.Dir(t.filePath)
	for _, c := range cands {
		if !isCallableKind(c.Kind) {
			continue
		}
		if c.FileID == t.fileID && c.FQN == want+c.Name {
			same = append(same, c)
			continue
		}
		if !spec.Calls.OpenTypes {
			continue
		}
		if ownerQual(c) == t.qual {
			if path.Dir(c.FilePath) == dir {
				dirOnly = append(dirOnly, c)
			} else {
				anywhere = append(anywhere, c)
			}
		}
	}
	switch {
	case len(same) > 0:
		return same, 0
	case len(dirOnly) > 0:
		return dirOnly, 1
	default:
		return anywhere, 2
	}
}

// ownerQual is the qualified name of the type that declares a candidate:
// `Rack.Protection.Base` for `path:Rack.Protection.Base.call`. "" for anything
// that is not a member.
//
// The whole of it is compared, not its last segment. Sinatra::Base and
// Rack::Protection::Base are both `Base`, and matching on that alone bound a
// `new(nil)` written in one to the constructor of the other.
func ownerQual(c registry.SymbolIndexEntry) string {
	if _, isMember := memberOwner(c); !isMember {
		return ""
	}
	qual, _ := qualOf(c)
	return qual[:len(qual)-len(c.Name)-1]
}

// oneDeclaration picks the declaration that stands for a group which is one
// definition. ok is false when the group is not one type's members at all.
// Otherwise the choice says which declaration is named and whether the call
// identifies it: where the language has overloads the argument count narrows
// the group (overload.go), and where it does not the earliest declaration is
// THE definition (an overload signature and its implementation) unless the
// count selects another.
func oneDeclaration(spec LanguageSpec, ref registry.Reference, group []registry.SymbolIndexEntry) (overloadChoice, bool) {
	if len(group) == 0 {
		return overloadChoice{}, false
	}
	// Declarations of one name on one type in one directory are overloads, or
	// a declaration and its definition. Which TYPE is not in doubt.
	for _, c := range group[1:] {
		if c.Name != group[0].Name || path.Dir(c.FilePath) != path.Dir(group[0].FilePath) {
			return overloadChoice{}, false
		}
		// The whole qualified owner, not its last segment: Rack::Request::Helpers
		// and Rack::Response::Helpers are two types, and a name they both
		// declare is not an overload.
		if ownerQual(c) != ownerQual(group[0]) {
			return overloadChoice{}, false
		}
	}
	if len(group) == 1 {
		return overloadChoice{first: group[0], chosen: true, declared: 1, allowed: 1}, true
	}
	if spec.Calls.Overloads {
		return chooseOverload(ref, group), true
	}
	if c, picked := preferByArity(ref, group); picked {
		return overloadChoice{first: c, chosen: true, declared: len(group), allowed: 1}, true
	}
	return overloadChoice{first: earliest(group), declared: len(group), allowed: len(group)}, true
}

// bindMember binds the call to the member of a type, if the type declares one.
func (c call) bindMember(t typeRef, rule string) (decision, bool) {
	group, rank := membersOf(c.spec, t, c.cands)
	if len(group) == 0 {
		return decision{}, false
	}
	o, ok := oneDeclaration(c.spec, c.ref, group)
	if !ok {
		return ambiguousCall, true
	}
	first := o.first
	confidence := model.ConfidenceHigh
	reason := rule + " (" + t.name + ")"
	switch {
	case o.overloaded():
		// The type is right. Which of its methods of this name is a choice the
		// argument count did not make, and the first declaration is the one
		// called less than half the time: 49 right of 92 across Java, C# and
		// C++.
		confidence = model.ConfidenceLow
		reason = overloadedReason(rule, t.name, o)
	case !t.strong:
		confidence = c.spec.Calls.Measured.uniqueTypeName()
		reason = rule + ", the only type of that name (" + t.name + ")"
	case rank == 2:
		// Declared on a type of this name somewhere else. One such type is
		// evidence; it is not a statement — and in a language whose types are
		// reopened anywhere and whose class and instance methods share a
		// name, it measured as none (sinatra: 0 right of 2).
		confidence = model.ConfidenceMedium
		if c.spec.Calls.DynamicSelf {
			confidence = model.ConfidenceLow
		}
		reason = rule + ", declared in another directory (" + t.name + ")"
	}
	return bound(candidate{first, confidence, reason}), true
}

// inherit parks a call for the ancestry pass: the type is known and does not
// declare the member, so its ancestors are asked. fallback is what the call
// binds to if they do not answer either.
func (r *Resolver) inherit(c call, t typeRef, super bool, fallback decision, stats *Stats) decision {
	sr := staticReceiver{
		class: t.name, fileID: t.fileID, filePath: t.filePath, qual: t.qual,
		how: "an ancestor", super: super,
	}
	// Nothing to inherit unless some type declares a member of this name. A
	// call whose name the repository does not define at all is settled now.
	declared := false
	for _, s := range c.cands {
		if _, isMember := memberOwner(s); isMember && isCallableKind(s.Kind) {
			declared = true
			break
		}
	}
	if !declared || fallback.verdict == verdictExternal {
		return fallback
	}
	has := fallback.verdict == verdictBound && fallback.best.confidence.AtLeast(r.opts.MinConfidence)
	if r.deferStatic(c.spec, c.fr, c.ref, sr, fallback.best, has, stats) {
		if fallback.verdict == verdictUnresolved {
			// If the ancestors do not answer, the call is bound to nothing —
			// which is unresolved, not a choice nobody could make.
			r.deferred[len(r.deferred)-1].orUnresolved = true
		}
		return deferredCall
	}
	return fallback
}

// --- the rules ---------------------------------------------------------------

// callBare binds `f()`.
func (r *Resolver) callBare(c call, stats *Stats) decision {
	if c.spec.isCoreSyntax(c.ref.Name) {
		// `format!(...)`: the parser records a macro invocation as a call with
		// no receiver, and a macro is not a function of the same name.
		return externalCall
	}
	// What a bare name can reach at all: a definition that is not a member of
	// anything, and — in a file the call is in — one local to a function the
	// call is inside.
	var reach, local []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if _, isLocal := localScope(s); isLocal {
			if s.FileID == c.fr.FileID && visibleFrom(c.scope, s) {
				local = append(local, s)
			}
			continue
		}
		if _, isMember := memberOwner(s); isMember {
			continue
		}
		reach = append(reach, s)
		if s.FileID == c.fr.FileID && !c.spec.isObjectMember(s) {
			local = append(local, s)
		}
	}

	if d, ok := r.importedBare(c, reach); ok {
		return d
	}

	// The enclosing scope, innermost first, then the file.
	if near := innermost(local); len(near) > 0 {
		if first, ok := firstDeclaration(near); ok {
			rule := ruleSameFile
			if _, isLocal := localScope(first); isLocal {
				rule = ruleEnclosingScope
			}
			switch {
			case len(near) > 1 && c.spec.Calls.Overloads:
				// Several declarations of one name where the language has
				// overloads are several functions, or a prototype and its
				// definition, and only the argument count can tell which. A
				// count that leaves one is the callee. Anything else is a
				// choice, and is recorded as one: where no declaration's count
				// is known - C++ today - a prototype cannot be told from an
				// overload either, and the first declaration was the callee
				// 3 times in 30 on google/benchmark (FormatKV).
				o := chooseOverload(c.ref, near)
				if o.chosen {
					return bound(candidate{o.first, model.ConfidenceHigh, rule})
				}
				return bound(candidate{o.first, model.ConfidenceLow, overloadedReason(rule, "", o)})
			case c.spec.Calls.DynamicSelf && c.within == first.ID:
				// A helper that shares its name with the method it wraps:
				// `def respond_to; app { respond_to(...) } end`. Inside the
				// block the name is sent to another object, far more often
				// than the helper recurses.
				return bound(candidate{first, model.ConfidenceLow, rule + ", the function the call is written in"})
			}
			return bound(candidate{first, model.ConfidenceHigh, rule})
		}
		// Several definitions of one name in one file: conditional
		// compilation, or overloads. The file is right; which is a preference.
		sortCandidates(near)
		if s, ok := preferByArity(c.ref, near); ok {
			return bound(candidate{s, model.ConfidenceMedium, ruleSameFileSeveral + " (argument count selects one)"})
		}
		return bound(candidate{near[0], model.ConfidenceMedium, ruleSameFileSeveral})
	}

	// A member of the enclosing type, where a bare name may be one, and of
	// each type that one is nested in.
	if c.spec.Calls.ImplicitSelf && c.class != "" {
		for qual := c.class; qual != ""; qual = outerQual(qual) {
			// `KeySet()` inside `LinkedHashTreeMap` constructs the type nested in
			// it, which is nearer than any class of that name elsewhere.
			if d, ok := c.nestedType(qual); ok {
				return d
			}
			t := typeRef{name: lastQualSegment(qual), fileID: c.fr.FileID, filePath: c.fr.Path, qual: qual, strong: true}
			if d, ok := c.bindMember(t, ruleEnclosingType); ok {
				return d
			}
		}
		// A member of the enclosing type shadows an object of the same name;
		// failing one, the name may be an object called through its apply.
		if d, ok := r.callBareObject(c); ok {
			return d
		}
		// Not declared on it: inherited, statically imported, or a free
		// function. Its ancestors are asked first, and what the name alone
		// answers is what stands if they say nothing.
		t, _ := c.enclosingType()
		return r.inherit(c, t, false, r.bareBeyondType(c, reach, stats), stats)
	}
	if d, ok := r.callBareObject(c); ok {
		return d
	}
	if c.spec.Calls.DynamicSelf {
		return r.bareBeyondType(c, reach, stats)
	}
	return r.bareByName(c, reach, stats)
}

// nestedType binds a constructing call to a type declared inside the type with
// this qualified name, in the call's own file. Only where a bare call
// constructs (CallTraits.CallConstructs): Kotlin's `Node()`, Scala's `Node()`.
// Measured on moshi, where the nested LinkedHashTreeMap.Node was bound to a
// Java test class of the same name and KeySet() and EntrySet() to nothing.
func (c call) nestedType(qual string) (decision, bool) {
	if !c.spec.Calls.CallConstructs {
		return decision{}, false
	}
	var nested []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if isConstructibleKind(s.Kind) && s.FileID == c.fr.FileID && ownerQual(s) == qual {
			nested = append(nested, s)
		}
	}
	if first, ok := firstDeclaration(nested); ok {
		return bound(candidate{first, model.ConfidenceHigh, ruleNestedType + " (" + lastQualSegment(qual) + ")"}), true
	}
	if len(nested) > 1 {
		return ambiguousCall, true
	}
	return decision{}, false
}

// bareBeyondType is what a bare name answers where a bare name may be a
// member and the enclosing type does not declare it: a free function first,
// then — where the receiver of a bare call is decided at run time — a method
// of some other type that the file's own evidence points at, then the one
// member of that name in the repository.
func (r *Resolver) bareBeyondType(c call, reach []registry.SymbolIndexEntry, stats *Stats) decision {
	rest := r.bareByName(c, reach, stats)
	if rest.verdict == verdictBound || rest.verdict == verdictExternal {
		return rest
	}
	if c.spec.isCoreFunction(c.ref.Name) {
		// `require(x)`, `include Mod`: the language's own, and the method of
		// that name some other type declares is not what the call reaches.
		return rest
	}
	if c.spec.Calls.DynamicSelf {
		if d := r.bareOnOtherType(c, stats); d.verdict == verdictBound {
			return d
		}
	}
	if viaName := r.memberByName(c, model.ConfidenceLow, stats); viaName.verdict == verdictBound {
		return viaName
	}
	return rest
}

// bareOnOtherType weighs a bare name against methods of types the call is not
// written in, by the evidence the file itself gives: one method of the name in
// this file, or one in a file it requires.
func (r *Resolver) bareOnOtherType(c call, stats *Stats) decision {
	var methods, here []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if s.ID == c.within || !isCallableKind(s.Kind) {
			continue
		}
		methods = append(methods, s)
		if s.FileID == c.fr.FileID {
			here = append(here, s)
		}
	}
	if len(methods) == 0 {
		return unresolvedCall
	}
	// Both rules below read as guesses and measured as better than that on
	// sinatra: 24 right of 25 each, which is the `high` floor with room. The
	// file's own definitions and its `require` lines are evidence about what a
	// block will be evaluated against, and in Ruby they are most of the
	// evidence there is.
	if len(here) == 1 {
		return bound(candidate{here[0], model.ConfidenceHigh, ruleSameFileOtherType})
	}
	if len(methods) > c.spec.MaxCandidates {
		return ambiguousCall
	}
	if best, ok := r.pickImported(c.imports, c.ref, methods); ok {
		if best.confidence != model.ConfidenceHigh {
			// Several required files each define one: a preference among
			// them, and labelled as the least.
			best.confidence = model.ConfidenceLow
		}
		best.reason = ruleRequiredOtherType
		return bound(best)
	}
	return ambiguousCall
}

// outerQual is the qualified name of what a nested type is nested in, or "".
func outerQual(qual string) string {
	i := strings.LastIndexByte(qual, '.')
	if i < 0 {
		return ""
	}
	outer := qual[:i]
	if strings.HasSuffix(outer, "."+localsSegment) || outer == localsSegment {
		return ""
	}
	return outer
}

// bareByName binds a bare name to something outside its own file: its
// package, an import that names it, a module the file imports, a neighbour.
func (r *Resolver) bareByName(c call, reach []registry.SymbolIndexEntry, stats *Stats) decision {
	// A name the file imported by name comes from that import and from
	// nowhere else, whatever the repository also defines.
	switch c.spec.Calls.Imports {
	case ImportsNameModules:
		// A module imported whole and then called (`const debug =
		// require('debug')`, `module.exports = function`) is called by the name
		// it was imported under, so it reads like a member imported by name.
		if m, _, ok := r.scriptQualifier(c.fr, c.imports, c.ref.Name); ok {
			if m.where == presenceExternal {
				return externalCall
			}
			if d, ok := bindInModule(c, m, reach, ruleImportedByName, ruleReExported); ok {
				return d
			}
		}
	case ImportsNameClasses:
		if owner, m, ok := r.staticImport(c.imports, c.ref.Name); ok {
			if m.where == presenceExternal {
				return externalCall
			}
			var imported []registry.SymbolIndexEntry
			for _, s := range c.cands {
				if o, isMember := memberOwner(s); isMember && o == owner && m.contains(s) && isCallableKind(s.Kind) {
					imported = append(imported, s)
				}
			}
			if o, ok := oneDeclaration(c.spec, c.ref, imported); ok {
				if o.overloaded() {
					return bound(candidate{o.first, model.ConfidenceLow, overloadedReason(ruleStaticImport, owner, o)})
				}
				return bound(candidate{o.first, model.ConfidenceHigh, ruleStaticImport + " (" + owner + ")"})
			}
		}
	}

	// Past the import, what a bare name denotes is a name in scope. A method
	// or accessor written in an object literal is a property of that object,
	// not a name: `vary(this.res, field)` inside `vary (field) {}` is the
	// `vary` package, and `new URL(...)` is not the `get URL ()` of another
	// file. See isObjectMember.
	scoped := reach[:0:0]
	for _, s := range reach {
		if !c.spec.isObjectMember(s) {
			scoped = append(scoped, s)
		}
	}
	reach = scoped
	if len(reach) == 0 {
		if c.spec.isBuiltinType(c.ref.Name) {
			// `new URL(...)` with nothing in the repository by that name is
			// the language's own.
			return externalCall
		}
		return unresolvedCall
	}
	if c.spec.isCoreFunction(c.ref.Name) {
		// What is left goes by the name: the package it shares, an import that
		// defines something of that name, a neighbour, the only one there is.
		// The name is the language's own (corefunctions.go), and a definition
		// of it that is not in this file or named by an import says nothing
		// about what the call reaches. Measured on moshi, where a private
		// `require` of one class was bound to every stdlib `require(...)`.
		return unresolvedCall
	}
	if c.spec.PackageIsDirectory {
		dir := c.spec.packageOf(c.fr.Path)
		var pkg []registry.SymbolIndexEntry
		for _, s := range reach {
			// The same directory, or — on the JVM — the same package under
			// another source root (`src/test/kotlin/a/b` beside `src/main/java/a/b`).
			d := c.spec.packageOf(s.FilePath)
			// A test the framework puts a package in scope for (testSees) is the
			// one case of a neighbour that is not a directory's: R only.
			if d == dir || c.spec.testSees(c.fr.Path, s.FilePath) || (c.spec.Calls.Imports == ImportsNameClasses && sharesPackageTail(dir, d) && !r.callsTestHelper(c, s)) {
				pkg = append(pkg, s)
			}
		}
		if len(pkg) == 1 {
			return bound(candidate{pkg[0], model.ConfidenceHigh, ruleSamePackage})
		}
		if len(pkg) > 1 {
			// One name declared more than once in a package: build variants.
			sortCandidates(pkg)
			if s, ok := preferByArity(c.ref, pkg); ok {
				return bound(candidate{s, model.ConfidenceMedium, ruleSamePackage + " (several, argument count selects one)"})
			}
			return bound(candidate{pkg[0], model.ConfidenceMedium, ruleSamePackage + " (several)"})
		}
	}

	if c.spec.Calls.NoFreeFunctions {
		// Every function here belongs to a type. A candidate that seems to
		// belong to none was not attributed, and is not something an import
		// or a neighbouring file could bring into scope.
		return ambiguousCall
	}
	if len(reach) > c.spec.MaxCandidates {
		stats.CapExceeded++
		stats.CapDropped++
		return ambiguousCall
	}
	if best, ok := r.pickTypeByClassImport(c.imports, c.ref, reach); ok {
		return bound(best)
	}
	if best, ok := r.pickImported(c.imports, c.ref, reach); ok {
		return bound(best)
	}
	if !c.spec.PackageIsDirectory {
		if best, ok := r.pickSameDir(c.spec, c.fr, reach); ok && !r.callsTestHelper(c, best.entry) {
			best.confidence = weaker(best.confidence, c.spec.Calls.Measured.neighbour())
			return bound(best)
		}
	}
	if len(reach) == 1 && reach[0].Exported && !r.specs.IsGenericName(c.ref.Name) && !r.callsTestHelper(c, reach[0]) {
		return bound(candidate{reach[0], model.ConfidenceLow, "unique name in repository, no import evidence"})
	}
	return ambiguousCall
}

// bindInModule binds the call to what a module defines under the call's name:
// in the module's own file, or — where the module is a package's index —
// in exactly one file beneath it.
func bindInModule(c call, m module, among []registry.SymbolIndexEntry, ruleDefines, ruleReExports string) (decision, bool) {
	var defined, beneath []registry.SymbolIndexEntry
	for _, s := range among {
		if !isModuleLevel(s) {
			continue
		}
		switch {
		case m.contains != nil && m.contains(s):
			defined = append(defined, s)
		case m.beneath != nil && m.beneath(s):
			beneath = append(beneath, s)
		}
	}
	if len(defined) > 0 {
		if first, ok := callableFirst(c.ref, defined); ok {
			return bound(candidate{first, model.ConfidenceHigh, ruleDefines}), true
		}
		return ambiguousCall, true
	}
	if len(beneath) > 0 {
		if first, ok := callableFirst(c.ref, beneath); ok {
			return bound(candidate{first, model.ConfidenceHigh, ruleReExports}), true
		}
		return ambiguousCall, true
	}
	return decision{}, false
}

// callableFirst picks one definition from module-level candidates of one name
// in one module: a function before a type of the same name (TypeScript
// declares both under one name, and a call invokes the function), and among
// several declarations of one function the one the argument count selects.
func callableFirst(ref registry.Reference, group []registry.SymbolIndexEntry) (registry.SymbolIndexEntry, bool) {
	var callables []registry.SymbolIndexEntry
	for _, s := range group {
		if isCallableKind(s.Kind) {
			callables = append(callables, s)
		}
	}
	if len(callables) > 0 {
		group = callables
	}
	if first, ok := firstDeclaration(group); ok {
		if len(group) > 1 {
			if s, picked := preferByArity(ref, group); picked {
				return s, true
			}
		}
		return first, true
	}
	return registry.SymbolIndexEntry{}, false
}

// callSelf binds `self.f()` and `this.f()`.
func (r *Resolver) callSelf(c call, stats *Stats) decision {
	t, ok := c.enclosingType()
	if !ok {
		// An ES5 constructor is a type whose methods are assigned to its
		// prototype: `this.describe()` written in `function Shape` reaches
		// `Shape.prototype.describe`, which is indexed as a member of Shape.
		if c.spec.Calls.Imports == ImportsNameModules && c.scope != "" && !strings.Contains(c.scope, ".") {
			ctor := typeRef{name: c.scope, fileID: c.fr.FileID, filePath: c.fr.Path, qual: c.scope, strong: true}
			if d, found := c.bindMember(ctor, ruleEnclosingType); found {
				return d
			}
		}
		// `this` in a function that belongs to no type the index knows: an
		// object literal's method, a callback. A value of unknown type.
		return r.memberByName(c, model.ConfidenceMedium, stats)
	}
	if d, ok := c.bindMember(t, ruleEnclosingType); ok {
		return d
	}
	// Not declared on the type: inherited, mixed in, or generated. Its
	// ancestors are asked; failing them the name alone is a weak answer.
	return r.inherit(c, t, false, r.memberByName(c, model.ConfidenceLow, stats), stats)
}

// callSuper binds `super.f()`. Only an ancestor's member is an answer, and
// with no fallback: the one definition of the name in the file is, more often
// than not, the very method the call is written in.
func (r *Resolver) callSuper(c call, stats *Stats) decision {
	t, ok := c.enclosingType()
	if !ok {
		return unresolvedCall
	}
	return r.inherit(c, t, true, unresolvedCall, stats)
}

// callTyped binds `x.f()` where the source states x's type.
func (r *Resolver) callTyped(c call, stats *Stats) decision {
	segs := receiverSegments(c.ref.Receiver)
	if len(segs) == 0 {
		return r.memberByName(c, model.ConfidenceLow, stats)
	}
	name := segs[len(segs)-1]
	if len(segs) > 1 {
		if c.spec.hasExternalRoot(segs) {
			return externalCall
		}
		// A type qualified by a package: `http.ResponseWriter`. Where the
		// parser followed a field chain as far as a qualified type and no
		// further, the fields it could not type follow the type's name.
		if c.spec.Calls.Imports == ImportsNameDirectories {
			m, ok := r.goModule(c.imports, segs[0])
			switch {
			case ok && m.where == presenceExternal:
				return externalCall
			case ok && len(segs) == 2:
				return r.memberOfTypeIn(c, m, name, stats)
			default:
				return r.memberByName(c, model.ConfidenceMedium, stats)
			}
		}
		if len(segs) > 2 && !capitalised(name) {
			// A field chain behind a type; the field's type is not known.
			return r.memberByName(c, model.ConfidenceMedium, stats)
		}
	}

	t, found, known := r.typeNamed(c, name)
	if found && !qualifierFits(c.spec, segs, t) {
		// `fs::DirEntry` is not this repository's DirEntry.
		return unresolvedCall
	}
	if !found {
		if r.importedFromOutside(c, name) || c.spec.isBuiltinType(name) {
			return externalCall
		}
		if known {
			// Several types of the name and nothing to choose by.
			return ambiguousCall
		}
		// The type is not in the index. Whatever declares a member of this
		// name here, it is not what this call reaches.
		return unresolvedCall
	}
	if d, ok := c.bindMember(t, ruleStatedType); ok {
		return d
	}
	return r.inherit(c, t, false, unresolvedCall, stats)
}

// memberOfTypeIn binds the call to a member of the type `name` declared in a
// module.
func (r *Resolver) memberOfTypeIn(c call, m module, name string, stats *Stats) decision {
	if m.contains == nil {
		return unresolvedCall
	}
	for _, s := range c.symbols[name] {
		if !isTypeKind(s.Kind) || !m.contains(s) {
			continue
		}
		qual, _ := strings.CutPrefix(s.FQN, s.FilePath+":")
		t := typeRef{name: name, fileID: s.FileID, filePath: s.FilePath, qual: qual, strong: true}
		if d, ok := c.bindMember(t, ruleStatedType); ok {
			return d
		}
		return r.inherit(c, t, false, unresolvedCall, stats)
	}
	return unresolvedCall
}

// importedFromOutside reports whether the file's imports show a name comes
// from a package that is not in the repository.
func (r *Resolver) importedFromOutside(c call, name string) bool {
	switch c.spec.Calls.Imports {
	case ImportsNameClasses:
		m, ok := r.classImport(c.imports, name)
		return ok && m.where == presenceExternal
	case ImportsNameModules:
		m, _, ok := r.scriptQualifier(c.fr, c.imports, name)
		return ok && m.where == presenceExternal
	}
	return false
}

// callNamed binds `x.f()` where x is an identifier the enclosing code does not
// declare: an import, a type, or something at file level or beyond.
func (r *Resolver) callNamed(c call, stats *Stats) decision {
	name := c.ref.Receiver

	// An import that renames a class: `Fmt.format(x)` after `import a.b.Formatter
	// as Fmt` is a call on Formatter, which the plain row of the same import names.
	if c.spec.Calls.ImportAliases {
		for _, e := range c.imports.imports {
			if e.alias == name {
				name = lastRawSegment(e.raw)
				break
			}
		}
	}

	// CommonJS: `exports.f()` is a call of what this module exports.
	if c.spec.Calls.Imports == ImportsNameModules && name == "exports" {
		return ownExports(c)
	}

	// An import says what the name is before anything else is asked.
	switch c.spec.Calls.Imports {
	case ImportsNameDirectories:
		if m, ok := r.goModule(c.imports, name); ok {
			return qualifiedBy(c, m)
		}
	case ImportsNameModules:
		if m, member, ok := r.scriptQualifier(c.fr, c.imports, name); ok {
			if m.where == presenceExternal {
				return externalCall
			}
			if !member {
				return qualifiedBy(c, m)
			}
			// Imported FROM the module: a class it defines, or a namespace it
			// re-exports. A class is found below, as a type. A namespace is
			// the file of that name beneath the module.
			if _, isType, _ := r.typeNamed(c, name); !isType {
				if d, ok := ownedByImported(c, m, name); ok {
					return d
				}
				return namespaceBeneath(c, m, name)
			}
		}
	}
	return r.callOnTypeOrValue(c, name, stats)
}

// ownExports binds `exports.f()` and `module.exports.f()`: the module is the
// file the call is written in, and f is a function it defines at its top
// level. A name the file does not define (`exports.x = require(...)`, an
// assignment from elsewhere) is not a call of anything the index holds.
func ownExports(c call) decision {
	var here []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if s.FileID == c.fr.FileID && isModuleLevel(s) {
			here = append(here, s)
		}
	}
	if first, ok := callableFirst(c.ref, here); ok {
		return bound(candidate{first, model.ConfidenceHigh, ruleOwnExports})
	}
	return unresolvedCall
}

// ownedByImported binds `Card.Header()` and `<Card.Header />` where Card was
// imported by name and the module assigns Header onto it (`Card.Header =
// function`): a function or object that carries its own members, which is not
// a type and is not a namespace file either.
func ownedByImported(c call, m module, owner string) (decision, bool) {
	var owned []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if o, ok := memberOwner(s); !ok || o != owner || !isCallableKind(s.Kind) {
			continue
		}
		if (m.contains != nil && m.contains(s)) || (m.beneath != nil && m.beneath(s)) {
			owned = append(owned, s)
		}
	}
	if o, ok := oneDeclaration(c.spec, c.ref, owned); ok && o.chosen {
		return bound(candidate{o.first, model.ConfidenceHigh, ruleImportedObject + " (" + owner + ")"}), true
	}
	return decision{}, false
}

// qualifiedBy binds a call qualified by a module to what that module defines.
func qualifiedBy(c call, m module) decision {
	if m.where == presenceExternal {
		return externalCall
	}
	if d, ok := bindInModule(c, m, c.cands, ruleQualifiedModule, ruleQualifiedPackage); ok {
		return d
	}
	// The package is here and defines nothing callable by this name: a
	// function-valued variable, or a definition the extractor missed.
	return unresolvedCall
}

// namespaceBeneath binds a call qualified by a namespace that a package index
// re-exports: `import { util } from "../core/index.js"` and
// `util.joinValues()`, where core/index.ts has `export * as util from
// "./util.js"`. The namespace is the file of that name beneath the package.
func namespaceBeneath(c call, m module, namespace string) decision {
	if m.beneath == nil && m.contains == nil {
		return unresolvedCall
	}
	var inFile []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if !isModuleLevel(s) || !strings.EqualFold(fileBaseName(s.FilePath), namespace) {
			continue
		}
		if (m.beneath != nil && m.beneath(s)) || (m.contains != nil && m.contains(s)) {
			inFile = append(inFile, s)
		}
	}
	if first, ok := callableFirst(c.ref, inFile); ok {
		return bound(candidate{first, model.ConfidenceHigh, ruleQualifiedPackage})
	}
	if len(inFile) > 0 {
		return ambiguousCall
	}
	return unresolvedCall
}

// callOnTypeOrValue binds a call made on a name that is a type — its member —
// or, failing that, a value of unknown type.
func (r *Resolver) callOnTypeOrValue(c call, name string, stats *Stats) decision {
	if d, ok := r.callInPackage(c, name); ok {
		return d
	}
	if d, ok := r.callInObject(c, []string{name}); ok {
		return d
	}
	t, found, known := r.typeNamed(c, name)
	if found {
		if d, ok := c.bindMember(t, ruleNamedType); ok {
			return d
		}
		// `Type.new`, `Type::new()` on a type that writes no such member:
		// the language constructs it.
		if c.ref.Name == "new" {
			if d, ok := r.constructs(c, t); ok {
				return d
			}
		}
		return r.inherit(c, t, false, unresolvedCall, stats)
	}
	if known {
		return ambiguousCall
	}
	if r.importedFromOutside(c, name) || c.spec.isBuiltinType(name) {
		return externalCall
	}
	if c.spec.Calls.TypesAreCapitalised && capitalised(name) && len(c.symbols[name]) == 0 {
		// Spelled as a type, and the repository defines nothing of that name.
		// It is somebody else's type, and a member of that name declared here
		// is not what the call reaches.
		return unresolvedCall
	}
	return r.memberByName(c, model.ConfidenceHigh, stats)
}

// declaresMembers reports whether the definition with this qualified name has
// members of its own in the file: it is a type, and not a function with local
// functions.
func declaresMembers(fr registry.FileRefs, scope string) bool {
	prefix := fr.Path + ":" + scope + "."
	for _, s := range fr.Symbols {
		if rest, ok := strings.CutPrefix(s.FQN, prefix); ok && !strings.HasPrefix(rest, localsSegment+".") {
			return true
		}
	}
	return false
}

// constructedKind marks a reference the parser tagged on the type of a `new`
// expression: `new Widget(3)` in Java, PHP and Scala. Its name is the type's,
// never a method's, and it is bound to a class or to nothing.
//
// This cannot be said through CallTraits alone. A constructor is indexed as a
// method named after its class, so a plain reference named `Widget` is
// indistinguishable from a call to that method, and a language where a bare
// call does not construct (Java, PHP) has to refuse the class as a candidate
// for every OTHER reference of that name. The kind is the one thing that tells
// the two apart.
const constructedKind = ".new"

func isConstruction(refKind string) bool { return strings.HasSuffix(refKind, constructedKind) }

// callNew binds a `new T(...)` to the type T, as an instantiation.
//
// The qualifier, where the source wrote one, has to fit the type found: a
// package for the directory the type is in (`new com.shop.Widget()`), or the
// outer type of a nested one (`new Outer.Inner()`). A qualifier the parser
// could not read (PHP's namespace separator) leaves the type unidentified
// rather than guessed: a name that only one type in the repository bears is
// not evidence about a name written with a namespace.
func (r *Resolver) callNew(c call, stats *Stats) decision {
	name := c.ref.Name
	segs := receiverSegments(c.ref.Receiver)
	if c.ref.ReceiverKind == model.ReceiverExpr {
		return unresolvedCall
	}
	if len(segs) > 0 && c.spec.hasExternalRoot(segs) {
		return externalCall
	}
	t, found, known := r.typeNamed(c, name)
	if found {
		if len(segs) > 0 && !newQualifierFits(segs, t) {
			return unresolvedCall
		}
		if d, ok := r.constructs(c, t); ok {
			if !t.strong {
				// Identified only as the one type of that name: the tier
				// the language measured for that (docs/accuracy.md).
				d.best.confidence = c.spec.Calls.Measured.uniqueTypeName()
			}
			return d
		}
		// An interface, an enum, an alias: named by a `new`, never built by it.
		return unresolvedCall
	}
	if known {
		return ambiguousCall
	}
	if r.importedFromOutside(c, name) || c.spec.isBuiltinType(name) {
		return externalCall
	}
	return unresolvedCall
}

// newQualifierFits reports whether the qualifier written before a constructed
// type is consistent with the type that was found by its last name.
func newQualifierFits(segs []string, t typeRef) bool {
	last := segs[len(segs)-1]
	// A nested type: `Outer.Inner`. The type's own qualified name says what
	// it is nested in.
	if capitalised(last) {
		return strings.HasSuffix(t.qual, last+"."+t.name)
	}
	// A package: the directories the type's file is in end with it.
	var dir []string
	for _, p := range strings.Split(path.Dir(t.filePath), "/") {
		if p != "" && p != "." {
			dir = append(dir, p)
		}
	}
	if len(dir) < len(segs) {
		return false
	}
	tail := dir[len(dir)-len(segs):]
	for i, seg := range segs {
		if !strings.EqualFold(tail[i], seg) {
			return false
		}
	}
	return true
}

// constructs binds `Type.new` to the type itself, as an instantiation.
func (r *Resolver) constructs(c call, t typeRef) (decision, bool) {
	for _, s := range c.symbols[t.name] {
		if s.FileID == t.fileID && isConstructibleKind(s.Kind) && s.FQN == t.filePath+":"+t.qual {
			confidence := model.ConfidenceHigh
			if !t.strong {
				confidence = model.ConfidenceMedium
			}
			return bound(candidate{s, confidence, ruleConstructs + " (" + t.name + ")"}), true
		}
	}
	return decision{}, false
}

// callScoped binds `a::f()` and `a::b::f()`: the path names a type or a
// module, never a value.
func (r *Resolver) callScoped(c call, stats *Stats) decision {
	segs := receiverSegments(c.ref.Receiver)
	if len(segs) == 0 {
		return r.memberByName(c, model.ConfidenceLow, stats)
	}
	if c.spec.hasExternalRoot(segs) {
		return externalCall
	}
	last := segs[len(segs)-1]

	// Rust's path keywords: this module, its parent, the crate root.
	switch last {
	case "self", "super":
		// An inline `mod tests { use super::*; }` and its parent share a file.
		var here []registry.SymbolIndexEntry
		for _, s := range c.cands {
			if s.FileID == c.fr.FileID && isModuleLevel(s) {
				here = append(here, s)
			}
		}
		if first, ok := callableFirst(c.ref, here); ok {
			return bound(candidate{first, model.ConfidenceHigh, ruleQualifiedFile})
		}
		if last == "self" {
			return unresolvedCall
		}
		return r.moduleByStem(c, path.Base(path.Dir(c.fr.Path)), stats)
	case "crate":
		return r.freeByName(c)
	}

	// A type: `Status::OK()`, `SearcherBuilder::new()`, `Rack::Mutex.new`.
	if t, found, known := r.typeNamed(c, last); found || known {
		if !found {
			return ambiguousCall
		}
		if !qualifierFits(c.spec, segs, t) {
			// `Rack::Response` is not Sinatra::Response, and `textwrap::Options`
			// is not this workspace's Options.
			return unresolvedCall
		}
		if d, ok := c.bindMember(t, ruleNamedType); ok {
			return d
		}
		if c.ref.Name == "new" {
			if d, ok := r.constructs(c, t); ok {
				return d
			}
		}
		return r.inherit(c, t, false, unresolvedCall, stats)
	}
	if c.spec.isBuiltinType(last) {
		return externalCall
	}
	if c.spec.Calls.TypesAreCapitalised && capitalised(last) {
		// A type the repository does not define, or a variant of one.
		return unresolvedCall
	}
	if c.spec.Calls.ScopeNamesModule {
		return r.moduleByStem(c, last, stats)
	}
	return unresolvedCall
}

// qualifierFits reports whether the path a type was written with is
// consistent with the type that was identified by its last segment alone.
//
// Where qualified names carry their namespaces (Ruby's `Sinatra.Response`),
// the written qualifier must be the tail of the type's own. Where a module is
// a file (Rust), the written qualifier must name the type's file or one of the
// directories above it: `ignore::DirEntry` fits crates/ignore/src/walk.rs and
// `fs::DirEntry` does not. Anywhere else, and for an unqualified name, it
// fits — a qualifier that cannot be checked is not evidence against.
func qualifierFits(spec LanguageSpec, segs []string, t typeRef) bool {
	if len(segs) < 2 {
		return true
	}
	qualifier := segs[len(segs)-2]
	switch qualifier {
	case "crate", "self", "super", "Self":
		return true
	}
	typeSegs := strings.Split(t.qual, ".")
	if len(typeSegs) >= 2 {
		// A namespaced qualified name: compare as many trailing segments as
		// both have.
		for i := 1; i <= len(segs) && i <= len(typeSegs); i++ {
			if segs[len(segs)-i] != typeSegs[len(typeSegs)-i] {
				return false
			}
		}
		return true
	}
	if !spec.Calls.ScopeNamesModule || spec.Calls.ImplicitSelf {
		return true
	}
	want := strings.ReplaceAll(strings.ToLower(qualifier), "-", "_")
	for _, seg := range strings.Split(strings.ToLower(trimScriptExtension(strings.TrimSuffix(t.filePath, path.Ext(t.filePath)))), "/") {
		if strings.ReplaceAll(seg, "-", "_") == want || strings.HasSuffix(want, "_"+strings.ReplaceAll(seg, "-", "_")) {
			return true
		}
	}
	return false
}

// moduleByStem binds a call qualified by a module to a free function in the
// file that module is: `messages::errored()` and messages.rs.
func (r *Resolver) moduleByStem(c call, stem string, stats *Stats) decision {
	var inFile, free []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if !isModuleLevel(s) || !isCallableKind(s.Kind) {
			continue
		}
		free = append(free, s)
		if strings.EqualFold(moduleStem(c.spec, s.FilePath), stem) {
			inFile = append(inFile, s)
		}
	}
	if first, ok := firstDeclaration(inFile); ok {
		return bound(candidate{first, model.ConfidenceHigh, ruleQualifiedFile})
	}
	if len(inFile) > 1 {
		// Two files of that name each define one. Nearest wins only if one
		// shares the caller's directory.
		var near []registry.SymbolIndexEntry
		for _, s := range inFile {
			if strings.HasPrefix(s.FilePath, path.Dir(c.fr.Path)+"/") {
				near = append(near, s)
			}
		}
		if first, ok := firstDeclaration(near); ok {
			return bound(candidate{first, model.ConfidenceMedium, ruleQualifiedFile + " (nearest of several)"})
		}
		return ambiguousCall
	}
	// The module is not a file of that name: an inline module, a re-export, a
	// C++ namespace spread over files. One free function of the name in the
	// whole repository is weak evidence, and labelled as that.
	if r.layout.Known() && !r.layout.HasStem(stem) && !r.layout.HasDirNamed(stem) && !c.spec.Calls.ImplicitSelf {
		// No file and no directory of that name: a module of another crate.
		return unresolvedCall
	}
	if first, ok := firstDeclaration(free); ok && !r.specs.IsGenericName(c.ref.Name) && !r.callsTestHelper(c, first) {
		return bound(candidate{first, model.ConfidenceLow, ruleOnlyFree})
	}
	if len(free) > 1 {
		return ambiguousCall
	}
	return unresolvedCall
}

// freeByName binds `crate::f()`: a free function at the crate's root. The
// root is not identified, so this is the one free function of the name.
func (r *Resolver) freeByName(c call) decision {
	var free []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if isModuleLevel(s) && isCallableKind(s.Kind) {
			free = append(free, s)
		}
	}
	if first, ok := firstDeclaration(free); ok && !r.callsTestHelper(c, first) {
		return bound(candidate{first, model.ConfidenceMedium, ruleQualifiedFile + " (the crate's only one)"})
	}
	if len(free) > 1 {
		return ambiguousCall
	}
	return unresolvedCall
}

// callPath binds `a.b.f()`: a longer qualifier, whose head or whose tail may
// say what it is.
func (r *Resolver) callPath(c call, stats *Stats) decision {
	segs := receiverSegments(c.ref.Receiver)
	if len(segs) == 0 {
		return r.memberByName(c, model.ConfidenceLow, stats)
	}
	if c.spec.hasExternalRoot(segs) {
		return externalCall
	}
	head, last := segs[0], segs[len(segs)-1]

	// CommonJS: `module.exports.f()` is the same call as `exports.f()`.
	if c.spec.Calls.Imports == ImportsNameModules && len(segs) == 2 && head == "module" && last == "exports" {
		return ownExports(c)
	}

	// Anything reached through a package that is not here is not here.
	switch c.spec.Calls.Imports {
	case ImportsNameDirectories:
		if m, ok := r.goModule(c.imports, head); ok && m.where == presenceExternal {
			return externalCall
		}
	case ImportsNameModules:
		if m, _, ok := r.scriptQualifier(c.fr, c.imports, head); ok && m.where == presenceExternal {
			return externalCall
		}
	case ImportsNameClasses:
		if m, ok := r.classImport(c.imports, head); ok && m.where == presenceExternal {
			return externalCall
		}
	}

	if d, ok := r.callInObject(c, segs); ok {
		return d
	}

	// `a.b.Util.stat()`, `Outer.Inner.make()`: the tail names a type.
	if c.spec.Calls.TypesAreCapitalised && capitalised(last) {
		return r.callOnTypeOrValue(c, last, stats)
	}
	return r.memberByName(c, model.ConfidenceHigh, stats)
}

// memberByName binds a member call whose receiver's type nothing states.
//
// The candidates are members only — a call made on something cannot reach a
// free function — and never the function the call is written in: `x.f()`
// inside `f` on an `x` of unknown type is delegation far more often than it is
// recursion, and binding it to `f` was 193 self-loops on ripgrep, 13 wrong of
// 18 sampled.
//
// limit is the most this rule may claim for the shape it was reached from: an
// expression with no name is never more than `low`. Below that limit the tier
// is the one the language's measurement earned (CallTraits.Measured).
func (r *Resolver) memberByName(c call, limit model.Confidence, stats *Stats) decision {
	var members []registry.SymbolIndexEntry
	delegating := false
	for _, s := range c.cands {
		if !isCallableKind(s.Kind) {
			continue
		}
		if s.ID == c.within {
			delegating = true
			continue
		}
		// A method written in an object literal is a member of some object, and
		// the index does not say which: it still means the name is declared by
		// more than one thing, and `ctx.onerror(err)` is not the one class's
		// `onerror` when a context object has its own.
		if _, isMember := memberOwner(s); isMember || c.spec.isObjectMember(s) {
			members = append(members, s)
		}
	}
	if len(members) == 0 {
		if c.spec.Calls.ExtensionCalls {
			return r.extensionFunction(c)
		}
		return unresolvedCall
	}
	if delegating {
		// The call is written inside a member of the same name, which was set
		// aside. What is left is one candidate fewer than the repository has,
		// and being the only one LEFT is weaker than being the only one: the
		// wrapped value is usually of a type declared somewhere else.
		limit = model.ConfidenceLow
	}

	// A receiver named after a type is evidence about its type: `request` is
	// usually a Request. It chooses between types that declare the member.
	if hint := namingHint(c.ref); hint != "" {
		var named []registry.SymbolIndexEntry
		for _, s := range members {
			if owner, _ := memberOwner(s); strings.EqualFold(owner, hint) {
				named = append(named, s)
			}
		}
		if o, ok := oneDeclaration(c.spec, c.ref, named); ok && !r.callsTestHelper(c, o.first) {
			tier := weaker(c.spec.Calls.Measured.namedAfterType(), limit)
			if namedAfterTypeIsHint(c.spec.Name, c.ref.ReceiverKind) {
				// The receiver is a variable the source declared without saying
				// what it holds (`money = load(id)`), or a name no declaration
				// the parser reads binds (a loop variable, a method), so a name
				// that happens to match a type is the only evidence. In these
				// languages every type the source states is already read from
				// its declaration (shape.go), which makes the name a hint and
				// not a measurement: medium, not high.
				tier = weaker(tier, model.ConfidenceMedium)
			}
			reason := ruleNamedAfterType
			if o.overloaded() {
				tier, reason = weaker(tier, model.ConfidenceLow), overloadedReason(ruleNamedAfterType, hint, o)
			}
			best := candidate{o.first, tier, reason}
			if r.deferOverride(c, best, members) {
				return deferredCall
			}
			return bound(best)
		}
	}

	if o, ok := oneDeclaration(c.spec, c.ref, members); ok {
		first := o.first
		if r.specs.IsGenericName(c.ref.Name) || c.spec.isCoreMember(c.ref.Name) {
			// One `get` in a repository says nothing about what a `get(...)`
			// meant; the others are in libraries that were never indexed. The
			// same holds for a name the language's own library declares on
			// every string or collection. See corenames.go.
			return ambiguousCall
		}
		if r.callsTestHelper(c, first) {
			// The name is all there is to go on, and the one type that
			// declares it is in the test tree.
			return unresolvedCall
		}
		if o.overloaded() {
			// The type is the only one to declare the name, and it declares it
			// several times. "The only member" would be a false statement.
			owner, _ := memberOwner(first)
			return bound(candidate{first, weaker(weaker(c.spec.Calls.Measured.unknownReceiver(), limit), model.ConfidenceLow),
				overloadedReason(ruleOnlyType, owner, o)})
		}
		reason := ruleOnlyMember
		if o.declared > 1 {
			reason = ruleOnlyType + fmt.Sprintf(", declared %d times as one function; the first (%s) is its definition", o.declared, first.FQN)
		}
		return bound(candidate{first, weaker(c.spec.Calls.Measured.unknownReceiver(), limit), reason})
	}
	if len(members) > c.spec.MaxCandidates {
		stats.CapExceeded++
		stats.CapDropped++
	}
	return ambiguousCall
}

// extensionFunction binds `x.f()` to a function that is not a member of any
// type, in a language where such a function can be written to be called as if
// it were one (Kotlin's `fun String.shout()`). It runs only when no type in
// the repository declares a member of this name, so a method is never set
// aside for it.
//
// The index does not record that a function is an extension, nor what type it
// extends, so what is checked is that exactly one function of this name
// exists that a call written like a method could reach at all, and the tier
// is the one measured for the rule (CallTraits.Measured.Extension).
func (r *Resolver) extensionFunction(c call) decision {
	if c.ref.ReceiverKind == model.ReceiverBare {
		// Nothing was written before the name, so this is not a method-style
		// call; the bare-name rules have already had their say.
		return unresolvedCall
	}
	var free []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if !isCallableKind(s.Kind) || s.ID == c.within {
			continue
		}
		if _, isLocal := localScope(s); isLocal {
			continue
		}
		if _, isMember := memberOwner(s); isMember {
			continue
		}
		free = append(free, s)
	}
	if len(free) != 1 || r.specs.IsGenericName(c.ref.Name) || c.spec.isCoreMember(c.ref.Name) || r.callsTestHelper(c, free[0]) {
		return unresolvedCall
	}
	return bound(candidate{free[0], c.spec.Calls.Measured.extension(), ruleExtension})
}

// namingHint is the receiver as a type's name might be spelled: the last
// identifier of what the call was made on, without the sigils and prefixes a
// field carries. "" for a receiver with no name or one too short to mean
// anything.
func namingHint(ref registry.Reference) string {
	switch ref.ReceiverKind {
	case model.ReceiverName, model.ReceiverValue, model.ReceiverPath, model.ReceiverScope:
	default:
		return ""
	}
	hint := strings.TrimLeft(lastSegment(ref.Receiver), "_@$")
	hint = strings.TrimPrefix(hint, "m_")
	hint = strings.TrimRight(hint, "_")
	if len(hint) < 3 {
		return ""
	}
	return hint
}

// namedAfterTypeIsHint reports whether a receiver named after a type is only a
// hint in a language, for the kinds of receiver below.
//
// A variable the source declared and did not give a type (ReceiverValue) is a
// hint in Ruby, PHP and Rust. A name no declaration binds (ReceiverName: a
// loop variable, a closure parameter, a method used as a receiver) is a hint
// in Ruby and Rust; PHP keeps its measured tier for it. Gated by language
// because every other language's naming rule was measured as it is (see
// ruleTiers.NamedAfterType) and is left alone.
func namedAfterTypeIsHint(language string, kind string) bool {
	switch language {
	case "ruby", "rust":
		return kind == model.ReceiverValue || kind == model.ReceiverName
	case "php":
		return kind == model.ReceiverValue
	}
	return false
}

// weaker returns the lower of two confidences.
func weaker(a, b model.Confidence) model.Confidence {
	if a.AtLeast(b) {
		return b
	}
	return a
}
