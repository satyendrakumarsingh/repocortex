package infer

import (
	"context"
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Nim and D: a module is a file
//
// Both name a module by a path (`import shop/cart` in Nim, `import shop.cart;` in
// D, stored by the parser with slashes) and bring its public names into the file
// that imports it. Neither has the packages, namespaces or classpaths the shaped
// rules read, so resolveCall hands this file every call of these two languages.
//
//	Nim   a name is public when it carries the export marker (`proc f*()`): the
//	      parser flags every other as private (parse/private.go) and a private
//	      name of another file is never a candidate (fileLocalPrivate).
//	      `import a/b` brings every public name, `from a import f` the listed ones,
//	      `import a except f` all but those, `include a` makes a's definitions
//	      this file's own, and `export a` in a file brings a's names to whoever
//	      imports that file. `import a as x` names the module x as well as a.
//	D     a name is public unless it is marked `private`, which is the module's
//	      own. `import a.b` brings every public name, `import a.b : f` the listed
//	      ones, `import x = a.b` the qualifier x only, `static import a.b` the
//	      qualified names only, and `public import a.b` re-exports a.b. A class
//	      member is reached through its type, and a bare name inside a method may
//	      be a member of the class around it.
//
// A module's name is matched against a file by the path's tail: the repository
// does not state its source root (`-I src`, nimble's srcDir). Nim searches the
// importing file's own directory first, so a module written by a path that is
// that directory's own beats a longer match; where several files remain the call
// is ambiguous and binds nothing.
//
// What a call reaches, in the order the language asks:
//
//	f(x)        a definition of the file itself (a member of the class around the
//	            call in D) or of a file it `include`s; a public name of an imported
//	            module; the language's own (corefunctions.go), which no module of the
//	            repository replaces except by being in the same file
//	m.f(x)      the public f of the module m names: an alias, the module's name
//	            written in full (D), or the last segment of its path (Nim)
//	T.f(x)      D: the member f of the type T the receiver names
//	x.f(y)      uniform function-call syntax: the one visible function f whose first
//	            parameter is x, which is bound at medium (the parameter's type is not
//	            read, so the overloads of a name cannot be told apart) and at low where
//	            the name is also one of the language's own (`x.add(1)` on a sequence)
//
// A qualified call whose module no file of the repository is (`strutils.split`,
// `std.stdio.writeln`) leaves the repository. Nothing is bound on a name alone: a
// call that none of this accounts for is unresolved, never a guess. Nim ignores
// case after the first letter and underscores in a name; this does not, so a name
// written in another style than it was declared in does not bind.
var fileModuleLanguages = map[string]bool{"nim": true, "d": true}

// fileModuleLang is what differs between the two languages.
type fileModuleLang struct {
	// dotted is the separator a receiver writes between a module's segments: D's
	// `a.b.f()`. Nim's qualifier is one identifier.
	dotted bool
	// sameDirFirst: the language looks for a module beside the importing file first.
	sameDirFirst bool
	// members: a type's members are reached through the type, and a bare name inside
	// a type may be a member of it.
	members bool
}

var fileModuleRules = map[string]fileModuleLang{
	"nim": {sameDirFirst: true},
	"d":   {dotted: true, members: true},
}

// loadReach reads the export directives of a language's files, once, so that a
// module's re-exports can be followed (`export x`, `public import x`).
func (r *Resolver) loadReach(ctx context.Context, lang string) {
	if _, done := r.reachGraphs[lang]; done {
		return
	}
	if r.reachGraphs == nil {
		r.reachGraphs = map[string]*reexportGraph{}
	}
	r.reachGraphs[lang] = nil
	if src, ok := r.candidates.(Directives); ok {
		if files, err := src.Directives(ctx, lang); err == nil && len(files) > 0 {
			r.reachGraphs[lang] = newReexportGraph(files)
		}
	}
}

// moduleWithReach is a module and every module its file re-exports, followed to
// maxExportDepth.
func (r *Resolver) moduleWithReach(lang, module string) []string {
	g := r.reachGraphs[lang]
	if g == nil {
		return []string{module}
	}
	key := lang + "\x00" + module
	if got, ok := r.reachMemo[key]; ok {
		return got
	}
	if r.reachMemo == nil {
		r.reachMemo = map[string][]string{}
	}
	got := append([]string{module}, g.reach([]string{module})...)
	r.reachMemo[key] = got
	return got
}

// fileIsModule reports whether a file is the module a path names: the path is the
// file's own, without extension, or the tail of it. A D package module is its
// directory's `package.d`.
func fileIsModule(filePath, module string) bool {
	p := strings.TrimSuffix(filePath, path.Ext(filePath))
	if p == module || strings.HasSuffix(p, "/"+module) {
		return true
	}
	if base := path.Base(p); base == "package" {
		dir := path.Dir(p)
		return dir == module || strings.HasSuffix(dir, "/"+module)
	}
	return false
}

func fileInModules(filePath string, modules []string) bool {
	for _, m := range modules {
		if fileIsModule(filePath, m) {
			return true
		}
	}
	return false
}

// callFileModule resolves one call of a Nim or D file.
func (r *Resolver) callFileModule(c call, stats *Stats) decision {
	if isConstruction(c.ref.Kind) {
		return r.fmConstruct(c)
	}
	switch c.ref.ReceiverKind {
	case model.ReceiverBare:
		return r.fmBare(c)
	case model.ReceiverSelf:
		if rules := fileModuleRules[c.spec.Name]; rules.members && c.class != "" {
			return r.fmMember(c, c.class, ruleEnclosingType, model.ConfidenceHigh)
		}
	case model.ReceiverSuper:
		return unresolvedCall
	}
	return r.fmQualified(c, stats)
}

// fmVisible splits the candidates a bare name reaches into those the file defines
// (or includes) and those an import brings in.
func (r *Resolver) fmVisible(c call) (local, imported []registry.SymbolIndexEntry) {
	lang := c.spec.Name
	facts := c.factsOf()
	dirs := facts.moduleDirectives()
	for _, s := range c.cands {
		if !moduleLevel(lang, s) {
			continue
		}
		if s.FileID == c.fr.FileID {
			local = append(local, s)
			continue
		}
		included := false
		for _, inc := range dirs.includes {
			if fileIsModule(s.FilePath, inc.path) {
				included = true
				break
			}
		}
		if included {
			local = append(local, s)
			continue
		}
		for _, d := range dirs.imports {
			if d.brings(c.ref) && fileInModules(s.FilePath, r.moduleWithReach(lang, d.module)) {
				imported = append(imported, s)
				break
			}
		}
	}
	return local, imported
}

// fmNarrow keeps, of definitions in several files, those in the file the language
// would have found first for the module written: the one whose path is the
// module's own (a path relative to the repository root, which the parser writes
// for `./x` and `../x`), and for Nim the one beside the importing file.
func fmNarrow(c call, in []registry.SymbolIndexEntry) []registry.SymbolIndexEntry {
	files := map[int64]struct{}{}
	for _, s := range in {
		files[s.FileID] = struct{}{}
	}
	if len(files) < 2 {
		return in
	}
	dirs := c.factsOf().moduleDirectives()
	rules := fileModuleRules[c.spec.Name]
	var keep []registry.SymbolIndexEntry
	for _, s := range in {
		p := strings.TrimSuffix(s.FilePath, path.Ext(s.FilePath))
		for _, d := range dirs.imports {
			if p == d.module || (rules.sameDirFirst && p == path.Join(path.Dir(c.fr.Path), d.module)) {
				keep = append(keep, s)
				break
			}
		}
	}
	if len(keep) > 0 {
		return keep
	}
	return in
}

func (r *Resolver) fmBare(c call) decision {
	lang := c.spec.Name
	rules := fileModuleRules[lang]
	core := c.spec.isCoreFunction(c.ref.Name)

	// A member of the class the call is written in, or of one it is nested in.
	if rules.members && c.class != "" {
		for qual := c.class; qual != ""; qual = outerQual(qual) {
			var members []registry.SymbolIndexEntry
			for _, s := range c.cands {
				if s.FileID == c.fr.FileID && ownerQual(s) == qual && isCallableKind(s.Kind) {
					members = append(members, s)
				}
			}
			if len(members) > 0 {
				return pickModular(c, members, "declared on the enclosing type", model.ConfidenceHigh)
			}
		}
	}

	local, imported := r.fmVisible(c)
	if len(local) > 0 {
		conf := model.ConfidenceHigh
		if core {
			// `add(x, 1)` where the file also declares an `add`: the overload is chosen by
			// the argument's type, which is not read.
			conf = model.ConfidenceMedium
		}
		return pickModular(c, local, "defined in the same module", conf)
	}
	if len(imported) > 0 {
		conf := model.ConfidenceHigh
		if core {
			conf = model.ConfidenceLow
		}
		return pickModular(c, fmNarrow(c, imported), "imported from the module that defines it", conf)
	}
	if core {
		return externalCall
	}
	return unresolvedCall
}

// fmConstruct binds `new T(...)` to the type T the file defines or imports.
func (r *Resolver) fmConstruct(c call) decision {
	local, imported := r.fmVisible(c)
	var types []registry.SymbolIndexEntry
	for _, s := range append(local, fmNarrow(c, imported)...) {
		if isConstructibleKind(s.Kind) {
			types = append(types, s)
		}
	}
	if len(types) == 0 {
		return unresolvedCall
	}
	files := map[int64]struct{}{}
	for _, s := range types {
		files[s.FileID] = struct{}{}
	}
	if len(files) > 1 {
		return ambiguousCall
	}
	sortCandidates(types)
	return bound(candidate{types[0], model.ConfidenceHigh, ruleConstructs + " (" + c.ref.Name + ")"})
}

// fmMember binds a call to the member of a type the file or one it imports declares.
func (r *Resolver) fmMember(c call, qual, reason string, conf model.Confidence) decision {
	var members []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if isCallableKind(s.Kind) && ownerQual(s) == qual {
			members = append(members, s)
		}
	}
	if len(members) == 0 {
		return unresolvedCall
	}
	return pickModular(c, members, reason, conf)
}

// fmModulesNamed is the modules a qualifier names in this file: an alias, the
// module's whole name (D), or its last segment (Nim).
func (r *Resolver) fmModulesNamed(c call, recv string) []string {
	rules := fileModuleRules[c.spec.Name]
	facts := c.factsOf()
	dirs := facts.moduleDirectives()
	var out []string
	if m, ok := dirs.aliases[recv]; ok {
		out = append(out, m)
	}
	want := recv
	if rules.dotted {
		want = strings.ReplaceAll(recv, ".", "/")
	}
	for _, m := range facts.allModules() {
		switch {
		case rules.dotted && m == want:
			out = append(out, m)
		case !rules.dotted && path.Base(m) == want:
			out = append(out, m)
		}
	}
	return out
}

func (r *Resolver) fmQualified(c call, stats *Stats) decision {
	lang := c.spec.Name
	recv := c.ref.Receiver
	segs := receiverSegments(recv)
	if len(segs) == 0 {
		return unresolvedCall
	}

	// The qualifier names a module of this file's imports.
	if c.ref.ReceiverKind != model.ReceiverValue && c.ref.ReceiverKind != model.ReceiverExpr {
		if mods := r.fmModulesNamed(c, recv); len(mods) > 0 {
			var reach []string
			for _, m := range mods {
				reach = append(reach, r.moduleWithReach(lang, m)...)
			}
			var defs []registry.SymbolIndexEntry
			for _, s := range c.cands {
				if moduleLevel(lang, s) && fileInModules(s.FilePath, reach) {
					defs = append(defs, s)
				}
			}
			if len(defs) > 0 {
				return pickModular(c, fmNarrow(c, defs), "qualified by the module that defines it ("+recv+")", model.ConfidenceHigh)
			}
			for _, m := range mods {
				if r.moduleExists(lang, m) {
					// A module of the repository that does not define it.
					return unresolvedCall
				}
			}
			return externalCall
		}
	}
	if c.ref.ReceiverKind != model.ReceiverValue && c.ref.ReceiverKind != model.ReceiverExpr && c.spec.hasExternalRoot(segs) {
		return externalCall
	}

	rules := fileModuleRules[lang]
	// A type the receiver names (D): the member of that type.
	if rules.members && len(segs) == 1 && capitalised(segs[0]) && c.ref.ReceiverKind != model.ReceiverValue {
		var types []registry.SymbolIndexEntry
		for _, s := range c.symbols[segs[0]] {
			if isConstructibleKind(s.Kind) && moduleLevel(lang, s) && r.fmTypeVisible(c, s) {
				types = append(types, s)
			}
		}
		if len(types) > 0 {
			var members []registry.SymbolIndexEntry
			for _, t := range types {
				for _, s := range c.cands {
					if s.FileID == t.FileID && isCallableKind(s.Kind) && ownerQual(s) == segs[0] {
						members = append(members, s)
					}
				}
			}
			if len(members) > 0 {
				return pickModular(c, members, ruleNamedType, model.ConfidenceHigh)
			}
			return unresolvedCall
		}
	}
	// A type the source states for the value (D `Cart c; c.pub()`).
	if rules.members && c.ref.ReceiverKind == model.ReceiverType && len(segs) == 1 {
		var types []registry.SymbolIndexEntry
		for _, s := range c.symbols[segs[0]] {
			if isConstructibleKind(s.Kind) && moduleLevel(lang, s) && r.fmTypeVisible(c, s) {
				types = append(types, s)
			}
		}
		if len(types) == 1 {
			var members []registry.SymbolIndexEntry
			for _, s := range c.cands {
				if s.FileID == types[0].FileID && isCallableKind(s.Kind) && ownerQual(s) == segs[0] {
					members = append(members, s)
				}
			}
			if len(members) > 0 {
				return pickModular(c, members, ruleStatedType, model.ConfidenceHigh)
			}
		}
	}

	// Uniform function-call syntax.
	local, imported := r.fmVisible(c)
	among := append(local, fmNarrow(c, imported)...)
	var funcs []registry.SymbolIndexEntry
	for _, s := range among {
		// A function written as a wrapper of a method of the same name (`s.xs.add(x)`
		// in `proc add(s: Stack, x)`) calls the method it wraps, not itself.
		if isCallableKind(s.Kind) && s.ID != c.within {
			funcs = append(funcs, s)
		}
	}
	if len(funcs) > 0 {
		conf := model.ConfidenceMedium
		if c.spec.isCoreFunction(c.ref.Name) || c.ref.ReceiverKind == model.ReceiverExpr {
			conf = model.ConfidenceLow
		}
		return pickModular(c, funcs, "called as a method: the visible function of this name takes the receiver first", conf)
	}
	if rules.members {
		// A method of some type the receiver's type is not stated for.
		return r.memberByName(c, model.ConfidenceHigh, stats)
	}
	return unresolvedCall
}

// fmTypeVisible reports whether a type is in scope for the call: the file's own,
// or one a module the file imports defines.
func (r *Resolver) fmTypeVisible(c call, t registry.SymbolIndexEntry) bool {
	if t.FileID == c.fr.FileID {
		return true
	}
	dirs := c.factsOf().moduleDirectives()
	for _, d := range dirs.imports {
		if d.brings(registry.Reference{Name: t.Name}) && fileInModules(t.FilePath, r.moduleWithReach(c.spec.Name, d.module)) {
			return true
		}
	}
	return false
}
