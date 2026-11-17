package infer

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Resolution where qualified names state ownership
//
// Everything in resolver.go weighs a reference against candidates it knows
// almost nothing about: a name, a file, and a qualified name that is usually
// `path:name`. This file is what resolution can do when the parser says more —
// when a qualified name states the class a definition belongs to and whether it
// is local to a function, when an empty receiver means the source wrote none,
// and when an import row names the member it brought in. That began with
// Python (internal/parse/pyscope.go), and the rules in this file are still
// reached only where LanguageSpec.QualifiedMembers holds.
//
// Their ideas are no longer Python's alone: internal/parse/shape.go
// writes the same qualified names and the same receiver shapes for every
// language, and shaped.go binds by them using the readers defined here
// (memberOwner, isModuleLevel, visibleFrom, innermost, firstDeclaration).
//
// Three things change, and each replaces a guess with something the source
// states.
//
// THE RECEIVER'S CLASS. `self.save()`, `cls.build()`, `super().save()`, and
// `entry.post()` where the parser replaced `entry` with the class the function
// declares for it. For those the question "which `save`" does not depend on
// which file happens to contain one: it is the one that class declares, and
// failing that the nearest one an ancestor declares. See staticReceiverOf.
//
// WHAT A NAME CAN REACH. A bare call cannot name a method; a definition local
// to a function exists only inside it. See reachable.
//
// WHERE A NAME WAS IMPORTED FROM. `from shop.models import Account` says where
// this file's `Account` comes from, which is stronger than "some module this
// file imports contains an Account". See resolveMember.
//
// And one thing is taken away. A method called on an object whose class nothing
// states — `target.save()`, `self.repo.save()` — is dynamic dispatch, and
// binding it to the only `save` in the file, or the only one in an imported
// module, is a guess about a name. Those bindings were published at `high`.
// Measured against scip-python on four repositories they are right between a
// quarter and four fifths of the time depending on the tier and the
// repository, so they are still made and are reported one level down. See
// pickQualified, and docs/accuracy.md for the table.

// indirectCallSuffix marks a call whose callee is a local value. See
// parse.IndirectCallSuffix.
const indirectCallSuffix = ".indirect"

// localsSegment is what internal/parse writes between a function and a
// definition nested in it, following Python's __qualname__.
const localsSegment = "<locals>"

// staticReceiver is the class a call was made on, identified exactly.
type staticReceiver struct {
	// class is the class's own name, which is what the hierarchy is keyed by.
	class string

	// fileID, filePath and qual identify the class's definition: the file it is
	// in and its qualified name there. Ownership is decided against these, not
	// against the name — a repository with an `Article` model in every test
	// package has fifty classes answering to `Article`.
	fileID   int64
	filePath string
	qual     string

	// how says where the class came from, for the reason attached to an edge.
	how string

	// super is true for a call on the superclass, where the class itself is
	// skipped and the search starts from its bases.
	super bool
}

// staticReceiverOf reports the class a reference's receiver statically holds.
//
// False is the answer to everything not understood, and false changes nothing:
// the reference is resolved by the tiers that resolved it before.
func (r *Resolver) staticReceiverOf(
	spec LanguageSpec,
	fr registry.FileRefs,
	imports importSet,
	ref registry.Reference,
	scope string,
	symbols map[string][]registry.SymbolIndexEntry,
) (staticReceiver, bool) {
	if !spec.QualifiedMembers || ref.Receiver == "" || ref.Receiver == opaqueReceiver {
		return staticReceiver{}, false
	}
	isSuper := spec.SuperName != "" && ref.Receiver == spec.SuperName
	if isSuper || spec.isClassSelfName(ref.Receiver) {
		qual := enclosingClass(scope)
		if qual == "" {
			// `self` in a function that belongs to no class is a parameter
			// like any other.
			return staticReceiver{}, false
		}
		return staticReceiver{
			class: lastQualSegment(qual), fileID: fr.FileID, filePath: fr.Path,
			qual: qual, how: "the enclosing class", super: isSuper,
		}, true
	}

	// A named receiver is a class only if it is spelled as one and can be
	// pinned to ONE class definition. The lookup is by exact name, which is
	// what keeps a variable from being read as a type: `settings` is not
	// `Settings`.
	if !spelledAsClass(ref.Receiver) {
		return staticReceiver{}, false
	}
	var classes []registry.SymbolIndexEntry
	for _, c := range symbols[ref.Receiver] {
		if c.Kind == string(model.KindClass) && spec.bindableAcross(c.FilePath) {
			classes = append(classes, c)
		}
	}
	if len(classes) == 0 {
		return staticReceiver{}, false
	}
	class, ok := identifyClass(spec, fr, imports, ref.Receiver, scope, classes)
	if !ok {
		return staticReceiver{}, false
	}
	qual, _ := strings.CutPrefix(class.FQN, class.FilePath+":")
	return staticReceiver{
		class: ref.Receiver, fileID: class.FileID, filePath: class.FilePath,
		qual: qual, how: "the receiver's class",
	}, true
}

// spelledAsClass reports whether a receiver is written the way a class is: it
// begins with a capital letter.
//
// internal/parse replaces a variable with a class only for names of this shape
// (see className there), and the same rule is applied to a receiver the source
// wrote itself, so that what is looked up does not depend on which of the two
// it was. A class named in lower case is resolved as the unknown receiver it
// looks like.
func spelledAsClass(receiver string) bool {
	return receiver != "" && receiver[0] >= 'A' && receiver[0] <= 'Z'
}

// identifyClass decides which class definition a name denotes in one file.
//
// Three pieces of evidence, in the order the language itself resolves a name:
// a class the file defines and the call can see; a class the file imported by
// that name; and, failing both, a name only one class in the repository has.
// Anything else — several classes of the name and nothing to choose by — is
// not an identification, and the receiver is treated as the unknown it is.
func identifyClass(
	spec LanguageSpec,
	fr registry.FileRefs,
	imports importSet,
	name, scope string,
	classes []registry.SymbolIndexEntry,
) (registry.SymbolIndexEntry, bool) {
	var local []registry.SymbolIndexEntry
	for _, c := range classes {
		if c.FileID == fr.FileID && visibleFrom(scope, c) {
			local = append(local, c)
		}
	}
	if len(local) > 0 {
		return firstDeclaration(innermost(local))
	}
	if len(imports.members[name]) > 0 {
		// The file imported this name. If that import does not lead to a class
		// here, the name is somebody else's — a third-party class that happens
		// to share it — and the repository's own class of that name is not what
		// the file means.
		group, _ := resolveMember(spec, imports, name, classes)
		return firstDeclaration(group)
	}
	return firstDeclaration(classes)
}

// firstDeclaration returns the earliest declaration when every candidate is
// one definition, and false otherwise.
//
// One definition may be declared several times: an @overload set and its
// implementation, a property and its setter. Which definition is not in doubt
// then — only which declaration stands for it, and the FIRST is chosen because
// that is where a compiler index attaches the whole set. A heuristic edge that
// named a different declaration would be right about the method and still
// disagree with the exact edge that later replaces it.
func firstDeclaration(cs []registry.SymbolIndexEntry) (registry.SymbolIndexEntry, bool) {
	if !sameDefinition(cs) {
		return registry.SymbolIndexEntry{}, false
	}
	first := cs[0]
	for _, c := range cs[1:] {
		if c.ID < first.ID {
			first = c
		}
	}
	return first, true
}

func (s LanguageSpec) isClassSelfName(r string) bool {
	for _, name := range s.ClassSelfNames {
		if r == name {
			return true
		}
	}
	return false
}

// enclosingScope returns the qualified name of the innermost definition a line
// sits in, or "" at module level — or when that definition's qualified name is
// not one this package can read, a compiler's symbol string for instance.
func enclosingScope(fr registry.FileRefs, line int) string {
	var fqn string
	for _, s := range fr.Symbols {
		if s.StartLine > line {
			break
		}
		end := s.EndLine
		if end == 0 {
			end = s.StartLine
		}
		if line <= end {
			fqn = s.FQN
		}
	}
	qual, _ := strings.CutPrefix(fqn, fr.Path+":")
	return qual
}

// enclosingClass returns the qualified name of the class a scope's code
// belongs to, or "".
//
// `Account.close` is a method of Account. `Account.close.<locals>.retry` is a
// function nested in that method, and a `self` inside it is still the method's
// — so nested functions are peeled off first. What is left is a member of a
// class exactly when it is qualified by something other than a function's
// `<locals>`, which is how the parser spells the difference.
func enclosingClass(scope string) string {
	if scope == "" {
		return ""
	}
	segs := strings.Split(scope, ".")
	for len(segs) >= 3 && segs[len(segs)-2] == localsSegment {
		segs = segs[:len(segs)-2]
	}
	if len(segs) < 2 || segs[len(segs)-2] == localsSegment {
		return ""
	}
	return strings.Join(segs[:len(segs)-1], ".")
}

func lastQualSegment(qual string) string {
	if i := strings.LastIndexByte(qual, '.'); i >= 0 {
		return qual[i+1:]
	}
	return qual
}

// qualOf returns a candidate's qualified name within its file — what follows
// `path:` — and false when the qualified name is not one this package can
// read: a compiler's symbol string, say, which says nothing here.
//
// This and the three readers below run once per CANDIDATE per reference, and a
// common name has a thousand candidates, so none of them allocates.
func qualOf(c registry.SymbolIndexEntry) (string, bool) {
	n := len(c.FilePath)
	if len(c.FQN) <= n+1 || c.FQN[n] != ':' || c.FQN[:n] != c.FilePath {
		return "", false
	}
	return c.FQN[n+1:], true
}

// memberOwner reports the class that declares a candidate, read from its
// qualified name.
//
//	path.py:save                    -> "", false   a module-level definition
//	path.py:Account.save            -> "Account", true
//	path.py:build.<locals>.Item     -> "", false   local to a function
//	path.py:build.<locals>.Item.run -> "Item", true
//
// ok is false for anything that is not a class member, INCLUDING a definition
// local to a function, which belongs to no class and is not reachable through
// a module either.
func memberOwner(c registry.SymbolIndexEntry) (owner string, ok bool) {
	qual, isDefault := qualOf(c)
	if !isDefault || !strings.HasSuffix(qual, c.Name) {
		return "", false
	}
	head := qual[:len(qual)-len(c.Name)]
	if len(head) < 2 || head[len(head)-1] != '.' {
		return "", false
	}
	owner = lastQualSegment(head[:len(head)-1])
	if owner == localsSegment {
		return "", false
	}
	return owner, true
}

// isModuleLevel reports whether a candidate is defined at the top of its file,
// which is the only kind of definition an import or a module-qualified call can
// reach.
func isModuleLevel(c registry.SymbolIndexEntry) bool {
	qual, ok := qualOf(c)
	return ok && qual == c.Name
}

// localsInfix is the locals segment as it sits between two names.
const localsInfix = "." + localsSegment + "."

// localScope reports the function a candidate is local to: `build` for
// `path.py:build.<locals>.Item`, and false for anything not defined directly
// inside a function.
func localScope(c registry.SymbolIndexEntry) (string, bool) {
	qual, isDefault := qualOf(c)
	if !isDefault || !strings.HasSuffix(qual, c.Name) {
		return "", false
	}
	head := qual[:len(qual)-len(c.Name)]
	if !strings.HasSuffix(head, localsInfix) {
		return "", false
	}
	return head[:len(head)-len(localsInfix)], true
}

// visibleFrom reports whether a definition in the referencing file can be seen
// from a scope in it: always, unless it is local to a function the scope is
// not inside.
func visibleFrom(scope string, c registry.SymbolIndexEntry) bool {
	fn, isLocal := localScope(c)
	if !isLocal {
		return true
	}
	return scope == fn ||
		(len(scope) > len(fn) && scope[len(fn)] == '.' && strings.HasPrefix(scope, fn))
}

// reachable reports whether a call could denote a candidate at all, under the
// language's scoping rules.
//
// # Why this is an exclusion and not a tier
//
// Two guards that removed candidates were reverted — A18 and A19, recorded at
// the top of resolver.go — and both were guesses about what a call LIKELY
// meant. These are statements about what it CAN mean, of the same kind as the
// cross-language exclusion beside which this runs:
//
//   - a bare call cannot name a class member. Python has no implicit `self`:
//     `save()` inside a method is a module-level or local `save`, never the
//     class's own;
//   - a definition local to a function exists only inside that function. A
//     `helper` nested in `build_a` is not what a `helper()` in `build_b` calls,
//     and it is not what any call in another file calls;
//   - a call made on an object cannot reach a function's local definition,
//     which is no attribute of anything.
//
// A18 made the first of these claims for Go and was wrong about its premise:
// an EMPTY receiver did not mean the source wrote none, because the receiver
// query cannot express `s.repo.Save()`. It becomes sound, as A18's own note
// says, once the parser distinguishes the two — which is what
// parse.OpaqueReceiver does, and why this applies only where
// LanguageSpec.QualifiedMembers holds. A19's lesson still stands and is not
// argued away: removing candidates changes which references resolve. That is
// why this was measured rather than reasoned about; the numbers are in
// docs/accuracy.md.
//
// Only calls are filtered, which reachableFrom decides once per reference. A
// supertype reference — `class B(Outer.Inner)` — arrives with no receiver and
// legitimately names a nested class.
func reachable(fr registry.FileRefs, ref registry.Reference, scope string, c registry.SymbolIndexEntry) bool {
	if _, isLocal := localScope(c); isLocal {
		return ref.Receiver == "" && c.FileID == fr.FileID && visibleFrom(scope, c)
	}
	if ref.Receiver == "" {
		_, isMember := memberOwner(c)
		return !isMember
	}
	return true
}

// reachableFrom keeps the candidates a call can reach. It returns its input
// untouched, without allocating, in the common case where all of them can.
func reachableFrom(
	fr registry.FileRefs,
	ref registry.Reference,
	scope string,
	cands []registry.SymbolIndexEntry,
) []registry.SymbolIndexEntry {
	if edgeKindFor(ref.Kind) != model.EdgeCalls {
		return cands
	}
	for i, c := range cands {
		if reachable(fr, ref, scope, c) {
			continue
		}
		kept := append(cands[:0:0], cands[:i]...)
		for _, rest := range cands[i+1:] {
			if reachable(fr, ref, scope, rest) {
				kept = append(kept, rest)
			}
		}
		return kept
	}
	return cands
}

// innermost returns the candidates nested most deeply.
//
// Called with same-file candidates already known to be visible from the call,
// so depth of nesting is nearness: a definition local to the calling function
// shadows a module-level one of the same name. Never empty for a non-empty
// input; the caller decides whether what comes back is one definition.
func innermost(cs []registry.SymbolIndexEntry) []registry.SymbolIndexEntry {
	var near []registry.SymbolIndexEntry
	depth := -1
	for _, c := range cs {
		d := strings.Count(c.FQN, localsInfix)
		switch {
		case d > depth:
			near, depth = append(near[:0:0], c), d
		case d == depth:
			near = append(near, c)
		}
	}
	return near
}

// sameDefinition reports whether every candidate is a declaration of one
// qualified name in one file. False for none.
func sameDefinition(cs []registry.SymbolIndexEntry) bool {
	if len(cs) == 0 {
		return false
	}
	for _, c := range cs[1:] {
		if c.FileID != cs[0].FileID || c.FQN != cs[0].FQN {
			return false
		}
	}
	return true
}

// pickOwned returns the candidate the receiver's class declares, or false when
// that class does not declare the method — which is when its ancestors are
// asked.
func pickOwned(sr staticReceiver, cands []registry.SymbolIndexEntry) (candidate, bool) {
	want := sr.filePath + ":" + sr.qual + "."
	var owned []registry.SymbolIndexEntry
	for _, c := range cands {
		if c.FileID == sr.fileID && c.FQN == want+c.Name {
			owned = append(owned, c)
		}
	}
	first, ok := firstDeclaration(owned)
	if !ok {
		return candidate{}, false
	}
	reason := fmt.Sprintf("declared on %s, %s", sr.class, sr.how)
	if len(owned) > 1 {
		reason += fmt.Sprintf(" (first of %d declarations)", len(owned))
	}
	return candidate{first, model.ConfidenceHigh, reason}, true
}

// pickQualified is pick for a language whose qualified names state ownership.
//
// It is given only candidates the call can reach, and a receiver that is NOT a
// statically known class — ResolveFile settles those first. So what arrives is
// one of two things, and they are resolved by different evidence.
//
// A BARE NAME, which the language resolves lexically: a definition the call can
// see in its own file, innermost first; then the import that brought the name
// in. Past those two the file has said nothing about the name, and the generic
// import tier's answer — some imported module defines one — is reported one
// level down. Measured against scip-python it was right 28 times in 48.
//
// A CALL ON SOMETHING, where the something is either a module — an alias, a
// package imported whole or by name, a file whose stem the receiver spells — or
// an object of unknown class. A module qualifies the call exactly, and those
// bindings are High. An unknown object does not, and what is left for it is the
// name alone: the only `save` in the file, the only one in an imported module.
// That is a guess about dynamic dispatch, and it is reported one level below
// what the same tier claims for a bare name. See the file comment for the
// measurement.
func (r *Resolver) pickQualified(
	spec LanguageSpec,
	fr registry.FileRefs,
	imports importSet,
	ref registry.Reference,
	cands []registry.SymbolIndexEntry,
) (candidate, bool) {
	if ref.Receiver == "" {
		// Visibility is checked here as well as in reachable, because reachable
		// filters calls only: a supertype reference arrives unfiltered, and a
		// class local to some other function is not what `class C(Base)` names.
		scope := enclosingScope(fr, ref.Line)
		var local []registry.SymbolIndexEntry
		for _, c := range cands {
			if c.FileID == fr.FileID && visibleFrom(scope, c) {
				local = append(local, c)
			}
		}
		if first, ok := firstDeclaration(innermost(local)); ok {
			reason := "defined in the same file"
			if len(local) > 1 {
				reason = "defined in the enclosing scope, nearest of several in the file"
			}
			return candidate{first, model.ConfidenceHigh, reason}, true
		}
		if group, reason := resolveMember(spec, imports, ref.Name, cands); len(group) > 0 {
			if first, ok := firstDeclaration(group); ok {
				return candidate{first, model.ConfidenceHigh, reason}, true
			}
		}
		if isPythonTest(fr.Path) {
			if nearest, ok := resolveConftest(fr.Path, cands); ok {
				return candidate{nearest, model.ConfidenceHigh, "defined in ancestor conftest.py"}, true
			}
		}
		// The file neither defines the name nor imported it by name. What is
		// left is a module the file imports that happens to define one — which
		// is how a wildcard import supplies a name, and also how a builtin or a
		// name bound some other way gets mistaken for one. It is the name
		// alone again, and reported as that.
		c, ok := r.pickByName(spec, fr, imports, ref, cands)
		if !ok {
			return candidate{}, false
		}
		return nameOnly(c, "the file does not import this name"), true
	}

	// The receiver names a module — unless the parser saw it declared as a
	// local value, in which case it is a parameter or a variable that merely
	// shares a module's name. `auth.login()` on a fixture called `auth` is not
	// a call into auth.py, and reading it as one bound a test helper to a view
	// at `high`.
	if ref.ReceiverKind != model.ReceiverValue {
		if c, ok := pickModuleMember(spec, imports, ref, cands); ok {
			return c, true
		}
		if c, ok := r.pickAliased(imports, ref, cands); ok {
			return c, true
		}
		var byReceiver []registry.SymbolIndexEntry
		for _, c := range cands {
			if receiverMatches(spec, ref.Receiver, c) {
				byReceiver = append(byReceiver, c)
			}
		}
		if first, ok := firstDeclaration(byReceiver); ok {
			return candidate{first, model.ConfidenceHigh, "receiver type matches"}, true
		}
	}

	// The receiver's class is unknown. Whatever the name-only tiers answer is
	// an answer about the NAME — and two things the name alone could reach are
	// ruled out first, because the call's shape rules them out.
	//
	// The function the call is written in. `current_app.url_for(...)` inside
	// `url_for` is a wrapper delegating to another object, and binding it to
	// the wrapper was the largest group of wrong edges this tier produced on
	// flask: 10 wrong of 12 sampled under "defined in the same file".
	//
	// A module-level function, where the receiver is a value or an expression.
	// A method called on an object is an attribute of that object; a function
	// defined at the top of some file is not, whatever it is called.
	within, _ := enclosing(fr.Symbols, ref.Line)
	onObject := ref.ReceiverKind == model.ReceiverValue || ref.ReceiverKind == model.ReceiverExpr
	kept := cands[:0:0]
	for _, c := range cands {
		if c.ID == within || (onObject && isModuleLevel(c)) {
			continue
		}
		kept = append(kept, c)
	}
	if spec.isCoreMember(ref.Name) {
		// A call on an object of unknown class, by a name the standard library
		// and its test framework declare on their own types (`stdin.close()`,
		// `thread.start()`, `sys.stdout.flush()`): the repository's one method of
		// the name says nothing about it. See corenames.go.
		return candidate{}, false
	}
	c, ok := r.pickByName(spec, fr, imports, ref, kept)
	if !ok {
		return candidate{}, false
	}
	sameFile := c.reason == "defined in the same file"
	c = nameOnly(c, "the receiver's class is not known")
	if sameFile {
		// The weakest of the name-only answers, and labelled as that. One
		// definition of the name in the file is strong evidence for a bare
		// call and almost none for a call on an object of unknown class:
		// measured on flask it was right 2 times in 12 before the two
		// exclusions above, and 1 in 7 of what they left.
		c.confidence = model.ConfidenceLow
	}
	return c, true
}

// pickByName is the tiers that weigh a name with no evidence about what it was
// called on: a single definition in the same file, an imported module, and a
// name unique in the repository.
//
// The same-directory tier is absent on purpose; see
// LanguageSpec.ProximityIsEvidence.
func (r *Resolver) pickByName(
	spec LanguageSpec,
	fr registry.FileRefs,
	imports importSet,
	ref registry.Reference,
	cands []registry.SymbolIndexEntry,
) (candidate, bool) {
	var sameFile []registry.SymbolIndexEntry
	for _, c := range cands {
		if c.FileID == fr.FileID {
			sameFile = append(sameFile, c)
		}
	}
	if len(sameFile) == 1 {
		return candidate{sameFile[0], model.ConfidenceHigh, "defined in the same file"}, true
	}
	if c, ok := r.pickTypeByClassImport(imports, ref, cands); ok {
		return c, true
	}
	if c, ok := r.pickImported(imports, ref, cands); ok {
		return c, true
	}
	if c, ok := r.pickSameDir(spec, fr, cands); ok {
		return c, true
	}
	if len(cands) == 1 && cands[0].Exported && !r.specs.IsGenericName(ref.Name) {
		return candidate{cands[0], model.ConfidenceLow, "unique name in repository, no import evidence"}, true
	}
	if proj := r.subprojectOf(fr.Path); proj != "" && !r.specs.IsGenericName(ref.Name) {
		var sameProj []registry.SymbolIndexEntry
		for _, c := range cands {
			if r.subprojectOf(c.FilePath) == proj && c.Exported {
				sameProj = append(sameProj, c)
			}
		}
		// Not when a shared library defines the name too. A service that
		// calls a name both it and libs/common (or packages/shared) define
		// most likely means the library's, and only lost the import that
		// would have said so; choosing the local one on folder alone would
		// bind it to the wrong definition.
		if len(sameProj) == 1 && !r.sharedLibraryAlsoDefines(cands, proj) {
			return candidate{sameProj[0], model.ConfidenceLow, "unique name in service subproject"}, true
		}
	}
	return candidate{}, false
}

// sharedLibraryFolders name the folders a monorepo keeps its shared code in.
var sharedLibraryFolders = map[string]bool{"libs": true, "lib": true, "common": true, "shared": true, "packages": true}

// sharedLibraryAlsoDefines reports whether a candidate outside the caller's
// subproject sits in a shared-library folder.
func (r *Resolver) sharedLibraryAlsoDefines(cands []registry.SymbolIndexEntry, proj string) bool {
	for _, c := range cands {
		if !c.Exported || r.subprojectOf(c.FilePath) == proj {
			continue
		}
		for _, seg := range strings.Split(path.Dir(filepath.ToSlash(c.FilePath)), "/") {
			if sharedLibraryFolders[seg] {
				return true
			}
		}
	}
	return false
}

// subprojectOf returns the top-level service or package prefix for a file path in a monorepo,
// or "" if the path does not belong to a recognized multi-project structure.
func subprojectOf(p string) string {
	clean := filepath.ToSlash(p)
	parts := strings.Split(clean, "/")
	if len(parts) >= 2 {
		switch parts[0] {
		case "services", "packages", "apps", "frontend", "modules", "components", "libs", "plugins", "microservices", "mobile", "projects", "clients", "tools":
			return parts[0] + "/" + parts[1]
		case "backend":
			if len(parts) >= 3 {
				return parts[0] + "/" + parts[1] + "/" + parts[2]
			}
			return parts[0] + "/" + parts[1]
		}
	}
	return ""
}

// isPythonTest reports whether a file path points to a Python test file.
func isPythonTest(p string) bool {
	base := filepath.Base(p)
	return strings.HasSuffix(p, ".py") && (strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || strings.Contains(filepath.ToSlash(p), "/tests/"))
}

// resolveConftest finds the definition of a fixture or helper declared in the nearest ancestor
// conftest.py for a Python test file.
func resolveConftest(callerPath string, cands []registry.SymbolIndexEntry) (registry.SymbolIndexEntry, bool) {
	callerDir := filepath.ToSlash(filepath.Dir(callerPath))
	var nearest registry.SymbolIndexEntry
	nearestLen := -1
	for _, c := range cands {
		cPath := filepath.ToSlash(c.FilePath)
		if filepath.Base(cPath) == "conftest.py" && isModuleLevel(c) {
			cDir := filepath.Dir(cPath)
			if cDir == callerDir || strings.HasPrefix(callerDir, cDir+"/") {
				if len(cDir) > nearestLen {
					nearest = c
					nearestLen = len(cDir)
				}
			}
		}
	}
	return nearest, nearestLen >= 0
}

// nameOnly reports a binding made on a name alone, one confidence level below
// what its tier claims for a bare name, and says why the name is all there was.
func nameOnly(c candidate, why string) candidate {
	switch c.confidence {
	case model.ConfidenceHigh:
		c.confidence = model.ConfidenceMedium
	case model.ConfidenceMedium:
		c.confidence = model.ConfidenceLow
	default:
		// Already the weakest claim there is, and its own reason — a name
		// unique in the repository, with no import evidence — says as much.
		return c
	}
	c.reason += "; " + why + ", so this rests on the name"
	return c
}

// resolveMember finds the definition a name imported BY NAME denotes, among
// candidates that already carry that name.
//
// Two shapes, tried in order, and the reason returned says which.
//
// THE MODULE DEFINES IT. `from scrapy.utils.test import get_crawler` and a
// `get_crawler` defined in scrapy/utils/test.py. The generic import tier would
// usually reach the same answer, but through weaker reasoning — "exactly one
// candidate lives in SOME module this file imports" — which breaks down as soon
// as two imported modules each define the name. Here only the import that named
// it is consulted, and its path must match the candidate's module in full.
//
// THE PACKAGE RE-EXPORTS IT. `from shop.models import Account` where Account is
// defined in shop/models/account.py and `shop/models/__init__.py` imports it
// for its callers. No candidate lives in the module the import names, and the
// import tier therefore had nothing to say: on scrapy this was the single
// largest pool of correct bindings left at `low`, because a library's public
// names are imported from its packages, not from the modules that define them.
// The rule is that the named package's own directory tree contains EXACTLY ONE
// module-level definition of that name. It does not follow the re-export chain
// — that would need every intermediate file's imports — and it does not need
// to: the file said the name comes from that package, and one definition of it
// exists there.
//
// Only module-level candidates are eligible in either shape. A name brought in
// by an import statement is a module's attribute; it is never a method of some
// class in that module, however well the names match.
//
// The group returned is every declaration of the one definition found, or nil.
func resolveMember(
	spec LanguageSpec,
	imports importSet,
	name string,
	cands []registry.SymbolIndexEntry,
) ([]registry.SymbolIndexEntry, string) {
	declaring := imports.members[name]
	if len(declaring) == 0 {
		return nil, ""
	}
	var defined, beneath []registry.SymbolIndexEntry
	for _, c := range cands {
		if !isModuleLevel(c) {
			continue
		}
		segs := fileSegments(spec, c.FilePath)
		for _, i := range declaring {
			isModule, isBeneath := imports.locate(imports.imports[i], nil, segs)
			switch {
			case isModule && !containsID(defined, c.ID):
				defined = append(defined, c)
			case isBeneath && !containsID(beneath, c.ID):
				beneath = append(beneath, c)
			}
		}
	}
	switch {
	case sameDefinition(defined):
		return defined, "imported by name from the module that defines it"
	case len(defined) == 0 && sameDefinition(beneath):
		return beneath, "imported by name from a package that re-exports it"
	}
	return nil, ""
}

// pickModuleMember resolves `mod.name(...)` where `mod` is a module the file
// imported: a submodule or package imported by name (`from django.db import
// models`), or a package imported whole (`import httpx`).
//
// It is the same declaration as importing the member by name, spelled at the
// call site instead of at the top of the file, and it is resolved by the same
// two shapes: the module defines the name, or exactly one module beneath the
// package does.
//
// The receiver must be spelled in lower case, exactly as the module is. A
// capitalised receiver imported by name is a class, and ResolveFile has already
// asked whether it is one.
func pickModuleMember(
	spec LanguageSpec,
	imports importSet,
	ref registry.Reference,
	cands []registry.SymbolIndexEntry,
) (candidate, bool) {
	if len(imports.imports) == 0 || ref.Receiver != strings.ToLower(ref.Receiver) {
		return candidate{}, false
	}
	type module struct {
		entry importEntry
		extra []string
	}
	var modules []module
	for _, i := range imports.members[ref.Receiver] {
		modules = append(modules, module{imports.imports[i], []string{ref.Receiver}})
	}
	if len(modules) == 0 {
		// `import httpx`: the receiver is the whole of an absolute import path.
		// A longer path proves nothing about which submodule the member is in,
		// and a relative one binds no name for its module at all.
		for _, e := range imports.imports {
			if e.alias == "" && e.relative == 0 && len(e.segments) == 1 && e.segments[0] == ref.Receiver {
				modules = append(modules, module{entry: e})
				break
			}
		}
	}
	if len(modules) == 0 {
		return candidate{}, false
	}

	var defined, beneath []registry.SymbolIndexEntry
	for _, c := range cands {
		if !isModuleLevel(c) {
			continue
		}
		segs := fileSegments(spec, c.FilePath)
		for _, m := range modules {
			isModule, isBeneath := imports.locate(m.entry, m.extra, segs)
			switch {
			case isModule && !containsID(defined, c.ID):
				defined = append(defined, c)
			case isBeneath && !containsID(beneath, c.ID):
				beneath = append(beneath, c)
			}
		}
	}
	if first, ok := firstDeclaration(defined); ok {
		return candidate{first, model.ConfidenceHigh,
			"qualified by an imported module that defines it"}, true
	}
	if len(defined) == 0 {
		if first, ok := firstDeclaration(beneath); ok {
			return candidate{first, model.ConfidenceHigh,
				"qualified by an imported package that re-exports it"}, true
		}
	}
	return candidate{}, false
}

// locate reports where a file sits relative to the module an import names,
// optionally extended by further segments: it IS that module, or it lies
// somewhere beneath that package.
//
// A relative import is anchored to the importing file's directory, so
// `from .models import Article` matches the `models.py` beside the file and no
// other. An absolute one is matched from the right, in full: `django.db.models`
// is the module whose path ENDS with those three segments, whatever source
// root sits in front of them.
func (s importSet) locate(e importEntry, extra, fileSegs []string) (isModule, isBeneath bool) {
	if e.relative > 0 {
		up := e.relative - 1
		if up > len(s.base) {
			return false, false
		}
		want := make([]string, 0, len(s.base)-up+len(e.segments)+len(extra))
		want = append(want, s.base[:len(s.base)-up]...)
		want = append(want, e.segments...)
		want = append(want, extra...)
		if len(fileSegs) < len(want) {
			return false, false
		}
		for i := range want {
			if fileSegs[i] != want[i] {
				return false, false
			}
		}
		return len(fileSegs) == len(want), len(fileSegs) > len(want)
	}
	want := e.segments
	if len(extra) > 0 {
		want = append(append(make([]string, 0, len(e.segments)+len(extra)), e.segments...), extra...)
	}
	if len(want) == 0 {
		return false, false
	}
	if commonSuffixLen(want, fileSegs) == len(want) {
		// An absolute import does not reach into a package: see insidePackage.
		if s.layout.Known() && s.spec.PackageInitStem != "" && len(fileSegs) > len(want) &&
			s.layout.IsPackageDir(fileSegs[:len(fileSegs)-len(want)]) {
			return false, false
		}
		return true, false
	}
	return false, liesBeneath(want, fileSegs)
}

// qualifiedByOutsideModule reports whether a call's receiver is the local name
// of an import that names nothing in the repository: an alias (`import json as
// _json`), a module imported whole (`import os`), or a name imported from one
// (`from os import path`).
//
// It answers true only where the layout shows the module is absent — no
// directory ends with its path and no file carries its name — and every
// import that could bind the name agrees. `from os import path` is read as
// binding `os` as well as `path`: the import row does not say which form was
// written, and a file that uses `os.` without importing it does not run. A relative import is in the repository by
// construction, and with no layout nothing is external.
func (r *Resolver) qualifiedByOutsideModule(imports importSet, ref registry.Reference) bool {
	if !r.layout.Known() || ref.Receiver == "" {
		return false
	}
	switch ref.ReceiverKind {
	case model.ReceiverName, model.ReceiverPath:
	default:
		return false
	}
	name := ref.Receiver
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	bound, outside := 0, 0
	consider := func(e importEntry) {
		bound++
		if e.relative == 0 && !r.pythonModulePresent(e.segments) {
			outside++
		}
	}
	for _, e := range imports.imports {
		switch {
		case e.alias != "" && e.alias == name:
			consider(e)
		case e.alias == "" && e.relative == 0 && len(e.segments) > 0 && e.raw != "" &&
			strings.SplitN(e.raw, ".", 2)[0] == name:
			consider(e)
		}
	}
	for _, i := range imports.members[name] {
		consider(imports.imports[i])
	}
	return bound > 0 && outside == bound
}

// pythonModulePresent reports whether a dotted module path could name
// something in the repository: a package directory that is not itself inside
// a package, or a file of that name.
func (r *Resolver) pythonModulePresent(segs []string) bool {
	if len(segs) == 0 {
		return true
	}
	if r.layout.HasTopLevelDir(segs) {
		return true
	}
	if !r.layout.HasStem(segs[len(segs)-1]) {
		return false
	}
	return len(segs) == 1 || r.layout.HasDirEnding(segs[:len(segs)-1])
}

// liesBeneath reports whether a file sits somewhere under the package an import
// path names: the path's segments occur as one run in the file's, ending before
// the file's own last segment.
//
// `shop.models` and `shop/models/account.py` — segments [shop models account] —
// is such a pair. `shop.models` and `shop/models.py` is not; that file IS the
// module, which is the trailing-match case the callers try first.
func liesBeneath(importSegs, fileSegs []string) bool {
	n := len(importSegs)
	if n == 0 || len(fileSegs) <= n {
		return false
	}
	for end := n; end < len(fileSegs); end++ {
		match := true
		for k := 0; k < n; k++ {
			if fileSegs[end-n+k] != importSegs[k] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// pickTypeByClassImport binds a type use written in a file that imports that
// very class by name (`import com.pay.repository.TransactionRepository;`):
// the import names the class, not merely a package that holds it, so the
// binding is as strong as the import.
func (r *Resolver) pickTypeByClassImport(imports importSet, ref registry.Reference, cands []registry.SymbolIndexEntry) (candidate, bool) {
	if !isTypeUse(ref.Kind) || imports.spec.Calls.Imports != ImportsNameClasses {
		return candidate{}, false
	}
	m, ok := r.classImport(imports, ref.Name)
	if !ok || m.contains == nil {
		return candidate{}, false
	}
	var named []registry.SymbolIndexEntry
	for _, c := range cands {
		if m.contains(c) {
			named = append(named, c)
		}
	}
	if len(named) != 1 {
		return candidate{}, false
	}
	return candidate{named[0], model.ConfidenceHigh, "the class an import names"}, true
}
