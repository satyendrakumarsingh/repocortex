package infer

import (
	"path"
	"strconv"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Erlang, Clojure and Julia: a call reaches a function of a MODULE
//
// Like Elixir's (elixir.go), and for the same reason, none of the rules in this
// package that are about files fit these languages. `money:zero()` reaches the
// function `zero` of the module that is called `money`; `(d/total x)` reaches
// the `total` of the namespace `d` stands for; `Money.zero()` the `zero` of the
// module `Money`. What the module is called is the question, and what file holds
// it is only how the name is found:
//
//	Erlang    the module is the file's name (the compiler insists: `-module(m)`
//	          lives in m.erl), so a module is a file stem. `-import(m, [f/1])`
//	          brings f/1 in under its own name, and arity is part of a function's
//	          identity: f/1 and f/2 are different functions.
//	Clojure   a namespace is a path: `a.b-c` is a/b_c.clj, .cljc or .cljs under
//	          some source root the code does not state. `:as` binds a qualifier,
//	          `:refer [f]` a name, `:refer :all` and `:use` every public name.
//	Julia     a module is a `module M` block, qualified into every definition's
//	          name by the parser (`Shop.total`; see parse.juliaOwners), because a
//	          file need not be named for the module it holds. `using M` brings
//	          M's exports in, `import M: f` a name, `import M as N` a qualifier,
//	          and `include("x.jl")` makes the definitions of the file named part
//	          of the including module.
//
// resolveCall hands this file every call made in one of these three languages
// and no other, so nothing in it changes what any other language is asked.
//
// What a call reaches, in the order the language asks:
//
//	f(x)      a definition of the module the call is written in (for Erlang and
//	          Clojure, the file; for Julia, the module block around the call, or
//	          the file's own top level when it has none); a function an `include`d
//	          file defines; one an import names or brings in whole; the
//	          language's own (corefunctions.go), which no definition of the
//	          repository replaces except one written in the same file
//	m.f(x)    the function f of the module m names: written in full, or an alias
//	          of a directive, or Erlang's `?MODULE`
//
// A qualified call whose module no file of the repository names (`lists:map`,
// `str/join`, `LinearAlgebra.norm`) leaves the repository. Nothing is bound on a
// name alone: a call that none of this accounts for is unresolved, never a guess.
// modularLanguages are the languages callModular resolves.
var modularLanguages = map[string]bool{"erlang": true, "clojure": true, "julia": true}

func (r *Resolver) callModular(c call, _ *Stats) decision {
	dirs := moduleDirectivesOf(c.fr.Imports)
	home := moduleHome(c)

	switch c.ref.ReceiverKind {
	case model.ReceiverBare:
		return r.callModularBare(c, dirs, home)
	case model.ReceiverName, model.ReceiverPath, model.ReceiverScope, model.ReceiverSelf:
		return r.callModularQualified(c, dirs, home)
	}
	// A call on a value, an expression, a variable module (`Mod:f()`): the
	// module is chosen at run time and nothing here names it.
	return unresolvedCall
}

func (r *Resolver) callModularBare(c call, dirs moduleDirectiveSet, home string) decision {
	lang := c.spec.Name

	var local []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if s.FileID != c.fr.FileID || !moduleLevel(lang, s) {
			continue
		}
		// Julia: a module block is a namespace of its own, and a call in one does
		// not reach what the module around it or the file's top level defines.
		if lang == "julia" && juliaOwner(s) != home {
			continue
		}
		local = append(local, s)
	}
	if len(local) > 0 {
		return pickModular(c, local, "defined in the same module", model.ConfidenceHigh)
	}

	if lang == "julia" {
		// What an `include` names is part of the module the statement is written
		// in, and the definitions of a file with no module block of its own are
		// that module's.
		var included []registry.SymbolIndexEntry
		for _, inc := range dirs.includes {
			if juliaModuleAt(c.fr, inc.line) != home {
				continue
			}
			for _, s := range c.cands {
				if s.FilePath == inc.path && juliaOwner(s) == "" && moduleLevel(lang, s) {
					included = append(included, s)
				}
			}
		}
		if len(included) > 0 {
			return pickModular(c, included, "defined in a file this one includes", model.ConfidenceHigh)
		}
	}

	var imported []registry.SymbolIndexEntry
	for _, d := range dirs.imports {
		if !d.brings(c.ref) {
			continue
		}
		for _, s := range c.cands {
			if moduleDefines(lang, s, d.module) && moduleLevel(lang, s) {
				imported = append(imported, s)
			}
		}
	}
	if len(imported) > 0 {
		return pickModular(c, imported, "imported from the module that defines it", model.ConfidenceHigh)
	}

	if c.spec.isCoreFunction(c.ref.Name) {
		return externalCall
	}
	return unresolvedCall
}

func (r *Resolver) callModularQualified(c call, dirs moduleDirectiveSet, home string) decision {
	lang := c.spec.Name
	target := moduleQualifier(c, dirs, home)
	if target == "" {
		return unresolvedCall
	}
	var inModule []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if moduleDefines(lang, s, target) && moduleLevel(lang, s) {
			inModule = append(inModule, s)
		}
	}
	if len(inModule) > 0 {
		return pickModular(c, inModule, "qualified by the module that defines it ("+target+")", model.ConfidenceHigh)
	}
	if c.spec.hasExternalRoot(receiverSegments(target)) {
		return externalCall
	}
	if r.layout.Known() && !r.moduleExists(lang, target) {
		// No file of the repository is that module: a dependency's.
		return externalCall
	}
	return unresolvedCall
}

// moduleExists reports whether some indexed file could be the named module. A
// Julia module is a block and not a file, so nothing can be said of it.
func (r *Resolver) moduleExists(lang, module string) bool {
	switch lang {
	case "erlang":
		return r.layout.HasStem(module)
	case "clojure":
		segs := strings.Split(module, ".")
		return r.layout.HasStem(strings.ReplaceAll(segs[len(segs)-1], "-", "_"))
	}
	return true
}

// pickModular chooses among the functions of one module that share a name. An
// argument count that a declaration contradicts rules it out (Erlang's f/1 is
// not f/2); one answer is what the call reaches; several that the count does
// not tell apart are overloads, clauses or methods of one name, and the module
// is right and the choice is a guess, which is recorded as one.
func pickModular(c call, among []registry.SymbolIndexEntry, reason string, confidence model.Confidence) decision {
	var fits []registry.SymbolIndexEntry
	for _, s := range among {
		if c.spec.Name == "clojure" && !clojureLoads(c.fr.Path, s.FilePath) {
			continue
		}
		if c.ref.ArgCount.Known() && s.ParamCount.Known() && c.ref.ArgCount.Count() != s.ParamCount.Count() {
			continue
		}
		fits = append(fits, s)
	}
	if len(fits) == 0 {
		return unresolvedCall
	}
	// A function before a method of the same name: Clojure's `defmulti` is the
	// callable and its `defmethod`s are what it dispatches to.
	var callables []registry.SymbolIndexEntry
	for _, s := range fits {
		if s.Kind == string(model.KindFunction) {
			callables = append(callables, s)
		}
	}
	if len(callables) > 0 {
		fits = callables
	}
	files := map[int64]struct{}{}
	for _, s := range fits {
		files[s.FileID] = struct{}{}
	}
	if len(files) > 1 {
		return ambiguousCall
	}
	if len(fits) == 1 {
		return bound(candidate{fits[0], confidence, reason})
	}
	sortCandidates(fits)
	if s, ok := preferByArity(c.ref, fits); ok {
		return bound(candidate{s, confidence, reason + "; argument count selects the arity"})
	}
	return bound(candidate{fits[0], model.ConfidenceLow, reason + " (several definitions of the name)"})
}

// moduleLevel reports whether a candidate is a definition a module's callers
// reach: a function (or a type, which Julia constructs by calling) that is no
// member of a type and is not local to another function. In Julia the module is
// in the qualified name (`Shop.total`), so "module level" is relative to that.
func moduleLevel(lang string, c registry.SymbolIndexEntry) bool {
	if !isCallableKind(c.Kind) && c.Kind != string(model.KindClass) {
		return false
	}
	if languageOfCandidate(c) != lang {
		return false
	}
	if lang == "julia" {
		qual, ok := qualOf(c)
		return ok && !strings.Contains(qual, localsSegment)
	}
	return isModuleLevel(c)
}

// juliaOwner is the module a definition is in: its qualified name less the file
// and its own name. "" for a definition at a file's top level.
func juliaOwner(c registry.SymbolIndexEntry) string {
	qual, ok := qualOf(c)
	if !ok || !strings.HasSuffix(qual, c.Name) {
		return ""
	}
	head := qual[:len(qual)-len(c.Name)]
	return strings.TrimSuffix(head, ".")
}

// moduleDefines reports whether a candidate is defined in the named module.
func moduleDefines(lang string, s registry.SymbolIndexEntry, module string) bool {
	switch lang {
	case "erlang":
		return fileBaseName(s.FilePath) == module
	case "clojure":
		return clojureNamespaceFile(s.FilePath, module)
	case "julia":
		owner := juliaOwner(s)
		// `Money` names `Shop.Money` as well as a top-level `Money`: a module
		// written in a file is reached by the name its callers write, which is
		// relative to where they are.
		return owner != "" && (owner == module || strings.HasSuffix(owner, "."+module))
	}
	return false
}

// clojureNamespaceFile reports whether a path is the file of a namespace: the
// namespace's segments, dashes written as underscores, are the path's last
// segments, in a file Clojure loads.
func clojureNamespaceFile(filePath, ns string) bool {
	ext := path.Ext(filePath)
	switch ext {
	case ".clj", ".cljc", ".cljs", ".bb":
	default:
		return false
	}
	have := strings.Split(strings.TrimSuffix(filePath, ext), "/")
	want := strings.Split(strings.ReplaceAll(ns, "-", "_"), ".")
	if len(have) < len(want) {
		return false
	}
	tail := have[len(have)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			return false
		}
	}
	return true
}

// clojureLoads reports whether a file of one dialect loads a namespace written
// in another's: a .clj file loads .clj and .cljc, a .cljs file .cljs and .cljc,
// and a .cljc file either. A namespace kept in both (`store.clj` and
// `store.cljs`) is one namespace per platform, not an ambiguity.
func clojureLoads(callerPath, filePath string) bool {
	caller, file := path.Ext(callerPath), path.Ext(filePath)
	if caller == ".cljc" || file == ".cljc" || caller == file {
		return true
	}
	return caller != ".cljs" && file != ".cljs"
}

// moduleHome is the module the call is written in, where the language names
// one the call can be told by: Erlang's module is the file's, and Julia's is the
// innermost `module` block around the call ("" at a file's top level).
func moduleHome(c call) string {
	switch c.spec.Name {
	case "erlang":
		return fileBaseName(c.fr.Path)
	case "julia":
		return juliaModuleAt(c.fr, c.ref.Line)
	}
	return ""
}

// juliaModuleAt is the innermost `module` block around a line of a file, by its
// qualified name ("" at the file's top level).
func juliaModuleAt(fr registry.FileRefs, line int) string {
	home := ""
	for _, s := range fr.Symbols {
		if s.StartLine > line {
			break
		}
		end := s.EndLine
		if end == 0 {
			end = s.StartLine
		}
		if line <= end && s.Kind == string(model.KindPackage) {
			home, _ = strings.CutPrefix(s.FQN, fr.Path+":")
		}
	}
	return home
}

// moduleQualifier is the module a call's qualifier names, in full: an alias
// expanded, `?MODULE` replaced by the module the call is written in. "" when the
// qualifier names none.
func moduleQualifier(c call, dirs moduleDirectiveSet, home string) string {
	written := c.ref.Receiver
	if written == "?MODULE" {
		return home
	}
	head, rest, _ := strings.Cut(written, ".")
	if full, ok := dirs.aliases[head]; ok {
		if rest == "" {
			return full
		}
		return full + "." + rest
	}
	return written
}

// moduleDirectiveSet is a file's directives, read from its import rows.
type moduleDirectiveSet struct {
	aliases  map[string]string
	imports  []moduleImported
	includes []moduleInclude
}

// moduleInclude is a file a Julia file includes, and the line the statement is on.
type moduleInclude struct {
	path string
	line int
}

// moduleImported is a module whose functions are in scope under their own names:
// all of them, or only those listed.
type moduleImported struct {
	module string
	all    bool
	names  map[string]bool // "f", and "f/2" where the language writes the arity
}

// arityMark keys a name the list wrote with an arity (Erlang's `f/2`), so that
// the list names that arity and no other.
const arityMark = "#arity:"

// brings reports whether the import puts the called function in scope.
func (m moduleImported) brings(ref registry.Reference) bool {
	if m.all {
		return true
	}
	if m.names[arityMark+ref.Name] {
		return !ref.ArgCount.Known() || m.names[ref.Name+"/"+strconv.Itoa(ref.ArgCount.Count())]
	}
	return m.names[ref.Name]
}

func moduleDirectivesOf(imports []registry.Import) moduleDirectiveSet {
	var set moduleDirectiveSet
	for _, imp := range imports {
		for _, part := range strings.Split(imp.Alias, parse.ModuleDirectiveSeparator) {
			switch {
			case part == parse.ModuleInclude:
				set.includes = append(set.includes, moduleInclude{imp.Path, imp.Line})
			case strings.HasPrefix(part, parse.ModuleAliasPrefix):
				if set.aliases == nil {
					set.aliases = map[string]string{}
				}
				local := strings.TrimPrefix(part, parse.ModuleAliasPrefix)
				if _, dup := set.aliases[local]; !dup {
					set.aliases[local] = imp.Path
				}
			case part == parse.ModuleImportAll:
				set.imports = append(set.imports, moduleImported{module: imp.Path, all: true})
			case strings.HasPrefix(part, parse.ModuleImportPrefix):
				names := map[string]bool{}
				for _, n := range strings.Split(strings.TrimPrefix(part, parse.ModuleImportPrefix), ",") {
					names[n] = true
					if name, _, ok := strings.Cut(n, "/"); ok {
						names[arityMark+name] = true
					}
				}
				set.imports = append(set.imports, moduleImported{module: imp.Path, names: names})
			}
		}
	}
	return set
}
