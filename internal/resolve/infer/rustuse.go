package infer

import (
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Rust: a bare call to a name a `use` brought in
//
// `use crate::core::utils::{resolved_command, tool_exists};` and then
// `resolved_command("git")`: the import names the function and the module that
// defines it, and the module is a file whose place under the crate's src
// directory follows from the path. Before the parser wrote a row for each leaf
// of a list (parse/lang_rust_uses.go) the call had no evidence and was left to
// the weakest rules, which bound a neighbour of the same name at medium
// ("same directory, but the language requires an import") or nothing.
//
// A module `a::b` is the file `<src>/a/b.rs` or `<src>/a/b/mod.rs`, where
// <src> is the crate's source root: the directory called src nearest the top of
// the caller's path. `crate::` starts at the root, `self::` at the module the
// caller's file is, `super::` at its parent, and a path that starts with
// anything else is read as a child of the caller's module (the 2018 edition) or
// as rooted at the crate (the 2015 one). A path that cannot be placed — a
// `tests/` file, another crate of the workspace — binds nothing here.
//
// Three kinds of row bring a name in:
//
//	use m::f;            the leaf `f` of module m
//	use m::f as g;       `g` is f of module m (a member row, renamed)
//	use m::*;            every public name of module m
//
// The first two name the item, and are bound at high when the module's file
// defines it. A glob names the module only, and binds when exactly one of the
// globbed modules defines the name.

// rustSrc splits a Rust file's path into the crate's source root and the
// module the file is, as segments below that root.
func rustSrc(filePath string) (root string, module []string, ok bool) {
	segs := strings.Split(filePath, "/")
	if len(segs) < 2 {
		return "", nil, false
	}
	dirs, file := segs[:len(segs)-1], segs[len(segs)-1]
	src := -1
	for i, d := range dirs {
		if d == "src" {
			src = i
			break
		}
	}
	if src < 0 || !strings.HasSuffix(file, ".rs") {
		return "", nil, false
	}
	stem := strings.TrimSuffix(file, ".rs")
	module = append(module, dirs[src+1:]...)
	switch {
	case stem == "mod":
	case (stem == "lib" || stem == "main") && len(module) == 0:
	default:
		module = append(module, stem)
	}
	return strings.Join(dirs[:src+1], "/"), module, true
}

// rustModuleFiles lists the files the module `segs`, counted from the source
// root, can be: its own file and its mod.rs.
func rustModuleFiles(root string, segs []string) []string {
	if len(segs) == 0 {
		return []string{root + "/lib.rs", root + "/main.rs"}
	}
	dir := path.Join(append([]string{root}, segs...)...)
	return []string{dir + ".rs", dir + "/mod.rs"}
}

// rustPlace turns the module part of a path into the places it can be, each a
// list of segments from the source root. `crate::` starts at the root; any
// other head is a child of the caller's module and, for a `use`, also read from
// the root, as the 2015 edition did. `self::` and `super::` are not placed: an
// inline `mod tests { use super::*; }` makes the caller's module something the
// file's path does not say. Nil when the path cannot be placed.
func rustPlace(caller []string, mod []string, fromRoot bool) [][]string {
	if len(mod) == 0 {
		return nil
	}
	switch mod[0] {
	case "crate":
		return [][]string{append([]string{}, mod[1:]...)}
	case "self", "super", "Self":
		return nil
	}
	out := [][]string{append(append([]string{}, caller...), mod...)}
	if fromRoot && len(caller) > 0 {
		out = append(out, append([]string{}, mod...))
	}
	return out
}

// rustFilesOf is the set of files any of the places can be.
func rustFilesOf(root string, places [][]string) map[string]bool {
	files := map[string]bool{}
	for _, p := range places {
		for _, f := range rustModuleFiles(root, p) {
			files[f] = true
		}
	}
	return files
}

// rustImportedBare binds a bare call to the definition a `use` of its name
// points at.
func (r *Resolver) rustImportedBare(c call, reach []registry.SymbolIndexEntry) (decision, bool) {
	if c.spec.Name != "rust" || len(c.imports.imports) == 0 {
		return decision{}, false
	}
	root, caller, ok := rustSrc(c.fr.Path)
	name := c.ref.Name

	// The entries that are the module of a member row (a rename), not a leaf of
	// their own.
	memberEntry := map[int]bool{}
	for _, idx := range c.imports.members {
		for _, i := range idx {
			memberEntry[i] = true
		}
	}

	// find collects the definitions of `want` in the modules a path can name.
	find := func(mod []string, want string, among []registry.SymbolIndexEntry) (found []registry.SymbolIndexEntry, placed bool) {
		if !ok {
			return nil, false
		}
		places := rustPlace(caller, mod, true)
		if len(places) == 0 {
			return nil, false
		}
		files := rustFilesOf(root, places)
		for f := range files {
			if r.layout.HasRustFile(f) {
				placed = true
			}
		}
		for _, s := range among {
			if s.Name == want && isModuleLevel(s) && files[s.FilePath] {
				found = append(found, s)
			}
		}
		return found, placed
	}
	bind := func(group []registry.SymbolIndexEntry, rule string) (decision, bool) {
		if len(group) == 0 {
			return decision{}, false
		}
		if first, ok := callableFirst(c.ref, group); ok {
			return bound(candidate{first, model.ConfidenceHigh, rule}), true
		}
		return ambiguousCall, true
	}

	// A rename: `use a::f as g;` and the call `g()`.
	if ren, isRenamed := c.imports.renamed[name]; isRenamed {
		mod := strings.Split(c.imports.imports[ren.entry].raw, "::")
		if c.spec.hasExternalRoot(mod) {
			return externalCall, true
		}
		found, _ := find(mod, ren.exported, c.symbols[ren.exported])
		return bind(found, ruleImportedByName)
	}

	var globs [][]string
	explicit := false
	for i, e := range c.imports.imports {
		if memberEntry[i] || e.alias != "" || !strings.Contains(e.raw, "::") {
			continue
		}
		segs := strings.Split(e.raw, "::")
		last, mod := segs[len(segs)-1], segs[:len(segs)-1]
		if last == "*" {
			globs = append(globs, mod)
			continue
		}
		if last != name {
			continue
		}
		explicit = true
		if c.spec.hasExternalRoot(mod) {
			return externalCall, true
		}
		found, placed := find(mod, name, reach)
		if d, ok := bind(found, ruleImportedByName); ok {
			return d, true
		}
		if placed {
			// The module is in this crate and does not define the name: it
			// re-exports it, and what a neighbour of the same name is says
			// nothing about what the call reaches.
			return unresolvedCall, true
		}
	}
	if explicit || len(globs) == 0 {
		return decision{}, false
	}

	// A glob brings in every name of the module, and binds when exactly one of
	// the globbed modules defines the name.
	var group []registry.SymbolIndexEntry
	for _, mod := range globs {
		if c.spec.hasExternalRoot(mod) {
			continue
		}
		found, _ := find(mod, name, reach)
		group = append(group, found...)
	}
	if len(group) == 0 {
		return decision{}, false
	}
	return bind(group, ruleImportedByName+" (glob)")
}

// rustScopedCall binds `a::b::f()` to the free function `f` of the module
// `a::b`, where the path says which file that is: from the crate root
// (`crate::a::b`), from the caller's module (`core::utils::f()` beside
// `mod core;`), or through a `use` that names the module or one of its
// ancestors (`use crate::core::utils;` then `utils::f()`).
//
// Before this a path that began with `core`, `std` or `alloc` was external
// whatever the crate declares, and a module reached by an import was matched by
// the file's stem alone. Nothing is claimed when the module's file is not one
// the repository has, or does not define the name: the other rules go on.
func (r *Resolver) rustScopedCall(c call, segs []string) (decision, bool) {
	if c.spec.Name != "rust" || len(segs) == 0 {
		return decision{}, false
	}
	root, caller, ok := rustSrc(c.fr.Path)
	if !ok {
		return decision{}, false
	}
	try := func(mod []string) (decision, bool) {
		places := rustPlace(caller, mod, false)
		if len(places) == 0 {
			return decision{}, false
		}
		files := rustFilesOf(root, places)
		var found []registry.SymbolIndexEntry
		for _, s := range c.cands {
			if isModuleLevel(s) && files[s.FilePath] {
				found = append(found, s)
			}
		}
		if len(found) == 0 {
			return decision{}, false
		}
		if first, ok := callableFirst(c.ref, found); ok {
			return bound(candidate{first, model.ConfidenceHigh, ruleQualifiedFile}), true
		}
		return ambiguousCall, true
	}
	if d, ok := try(segs); ok {
		return d, true
	}
	// The head names a module a `use` brought in.
	memberEntry := map[int]bool{}
	for _, idx := range c.imports.members {
		for _, j := range idx {
			memberEntry[j] = true
		}
	}
	for i, e := range c.imports.imports {
		if memberEntry[i] || e.alias != "" || !strings.Contains(e.raw, "::") {
			continue
		}
		parts := strings.Split(e.raw, "::")
		if parts[len(parts)-1] != segs[0] {
			continue
		}
		return try(append(append([]string{}, parts...), segs[1:]...))
	}
	return decision{}, false
}
