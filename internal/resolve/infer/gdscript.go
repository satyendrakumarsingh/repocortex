package infer

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # GDScript: a script is a class, and a name reaches it by class_name or by path
//
// A `.gd` file is a class. Its functions, variables and constants are its members,
// qualified by the parser with the script's name (`Player.take_damage`, parse.gdscriptOwners):
// the `class_name` the file declares, or without one the file's own name. An inner
// `class` is a member type, and its members are qualified by it as well.
//
// A script is reached by three names, none of which is an import statement:
//
//	class_name Foo         `Foo.f()` and `Foo.new()` from any script of the project
//	preload("res://x.gd")  `const X = preload(...)`: `X.f()` and `X.new()` in the file that
//	                       writes it, whatever the script calls itself
//	extends "res://x.gd"   `extends Foo`: the members of the parent are this script's own, so
//	                       a bare `f()` and `self.f()` may be its
//
// A `res://` path is the project's own root, the directory that holds project.godot,
// and that file is not one the index reads, so the path is matched by its tail (the
// parser keeps the path beneath the root): see fileIsModule.
//
// What a call reaches, in the order the language asks:
//
//	f(x)        a member of the script or inner class the call is written in; a member of the
//	            script it extends, and of the one that extends in turn; the language's own
//	            (`print`, `preload`), which no script replaces. What the engine class it
//	            extends (`extends Node`) provides (`add_child`) is not in the repository: a
//	            name nothing here declares is unresolved
//	self.f()    the same, but never the language's own
//	super.f()   a member of the parent, never the script's own
//	T.f(x)      the member f of the script or inner class T names; T.new() constructs T
//	x.f(y)      the same for a value whose type the source states (`var x: T`, `var x := T.new()`)
//
// A type the engine provides (`Vector2.ZERO.length()`, `Input.is_action_pressed(...)`) is
// external. A call on a value whose type the source does not state is bound to the
// one member of that name in the repository, at low.
func (r *Resolver) callGDScript(c call, stats *Stats) decision {
	switch c.ref.ReceiverKind {
	case model.ReceiverBare:
		return r.gdBare(c, false)
	case model.ReceiverSelf:
		return r.gdBare(c, true)
	case model.ReceiverSuper:
		return r.gdInherited(c, c.fr.Path, nil)
	case model.ReceiverExpr:
		return unresolvedCall
	}
	return r.gdQualified(c, stats)
}

// gdMembersOfClass are the callables of one file that are members of the class with
// this qualified name.
func gdMembersOfClass(c call, fileID int64, qual string) []registry.SymbolIndexEntry {
	var out []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if s.FileID == fileID && isCallableKind(s.Kind) && ownerQual(s) == qual {
			out = append(out, s)
		}
	}
	return out
}

// gdScriptLevel reports whether a definition is a member of its script, as against
// one of an inner class of it: the script's name is the one segment of its owner.
func gdScriptLevel(s registry.SymbolIndexEntry) bool {
	owner := ownerQual(s)
	return owner != "" && !strings.Contains(owner, ".")
}

func (r *Resolver) gdBare(c call, selfOnly bool) decision {
	if !selfOnly && c.spec.isCoreFunction(c.ref.Name) {
		return externalCall
	}
	if c.class != "" {
		if own := gdMembersOfClass(c, c.fr.FileID, c.class); len(own) > 0 {
			return pickModular(c, own, ruleEnclosingType, model.ConfidenceHigh)
		}
	}
	// A script's own top level is its class: a call written in the body of an inner
	// class does not see the script's members, but the script's own code does see the
	// parent's.
	if c.class == "" || !strings.Contains(c.class, ".") {
		return r.gdInherited(c, c.fr.Path, c.factsOf().moduleDirectives().includes)
	}
	return unresolvedCall
}

// gdParents are the files a script extends: those its `extends` rows name, and for
// each the ones it extends in turn. A class_name is resolved to the file that declares
// it. Bounded by maxExportDepth, with a visited set.
func (r *Resolver) gdParents(c call, filePath string, rows []moduleInclude) []string {
	var out []string
	seen := map[string]bool{filePath: true}
	var walk func(parents []string, depth int)
	walk = func(parents []string, depth int) {
		for _, p := range parents {
			file := p
			if name, ok := strings.CutPrefix(p, "class:"); ok {
				// Two scripts that declare one class_name name no parent: the project refuses it.
				file = ""
				if c.typesNamed != nil {
					for _, s := range c.typesNamed(name) {
						if s.Kind != string(model.KindClass) || languageOfCandidate(s) != "gdscript" {
							continue
						}
						if _, member := memberOwner(s); member {
							continue
						}
						if file != "" && file != s.FilePath {
							file = ""
							break
						}
						file = s.FilePath
					}
				}
				if file == "" {
					continue
				}
			}
			if seen[file] {
				continue
			}
			seen[file] = true
			out = append(out, file)
			if depth >= maxExportDepth {
				continue
			}
			if g := r.reachGraphs["gdscript"]; g != nil {
				key := strings.TrimSuffix(file, ".gd")
				if d, ok := g.lookup(key); ok {
					walk(d.Parts, depth+1)
				}
			}
		}
	}
	var first []string
	for _, row := range rows {
		first = append(first, row.path)
	}
	walk(first, 0)
	return out
}

// gdInherited binds a call to a member of a script the one named extends.
func (r *Resolver) gdInherited(c call, filePath string, rows []moduleInclude) decision {
	if rows == nil {
		rows = c.factsOf().moduleDirectives().includes
	}
	var among []registry.SymbolIndexEntry
	for _, parent := range r.gdParents(c, filePath, rows) {
		for _, s := range c.cands {
			if isCallableKind(s.Kind) && gdScriptLevel(s) && (s.FilePath == parent || fileIsModule(s.FilePath, parent)) {
				among = append(among, s)
			}
		}
		if len(among) > 0 {
			break
		}
	}
	if len(among) == 0 {
		return unresolvedCall
	}
	return pickModular(c, among, "declared by the script this one extends", model.ConfidenceHigh)
}

// gdType is a type a receiver names: the members it has and the symbol that
// constructs it, where there is one.
type gdType struct {
	members []registry.SymbolIndexEntry
	// class is the type's own definition, which `T.new()` instantiates.
	class *registry.SymbolIndexEntry
	// file and qual identify the type, for the members it inherits.
	file string
}

// gdTypeNamed finds the script or inner class a name denotes from where the call is.
func (r *Resolver) gdTypeNamed(c call, name string) (t gdType, found, ambiguous bool) {
	dirs := c.factsOf().moduleDirectives()
	// A script this file preloads under the name.
	if p, ok := dirs.aliases[name]; ok {
		for _, s := range c.cands {
			if isCallableKind(s.Kind) && gdScriptLevel(s) && fileIsModule(s.FilePath, p) {
				t.members = append(t.members, s)
				t.file = s.FilePath
			}
		}
		if c.typesNamed != nil {
			// The constructor is the script's `_init`; of several files the path ends alike,
			// the one that sorts first by path is taken, as the rest of this file does.
			for _, s := range c.typesNamed("_init") {
				if isCallableKind(s.Kind) && languageOfCandidate(s) == "gdscript" && gdScriptLevel(s) && fileIsModule(s.FilePath, p) &&
					(t.class == nil || s.FilePath < t.class.FilePath) {
					init := s
					t.class = &init
				}
			}
		}
		if t.file == "" && t.class != nil {
			t.file = t.class.FilePath
		}
		return t, t.file != "" || t.class != nil, false
	}
	// A class_name, or an inner class of this file.
	var types []registry.SymbolIndexEntry
	look := c.symbols[name]
	if len(look) == 0 && c.typesNamed != nil {
		look = c.typesNamed(name)
	}
	for _, s := range look {
		if s.Kind == string(model.KindClass) && languageOfCandidate(s) == "gdscript" {
			if _, inner := memberOwner(s); inner && s.FileID != c.fr.FileID {
				// An inner class is named by the script that holds it.
				continue
			}
			types = append(types, s)
		}
	}
	switch len(types) {
	case 0:
		return gdType{}, false, false
	case 1:
		s := types[0]
		qual := name
		if owner := ownerQual(s); owner != "" {
			qual = owner + "." + name
		}
		t.class, t.file = &s, s.FilePath
		t.members = gdMembersOfClass(c, s.FileID, qual)
		return t, true, false
	}
	return gdType{}, false, true
}

func (r *Resolver) gdQualified(c call, stats *Stats) decision {
	segs := receiverSegments(c.ref.Receiver)
	if len(segs) == 0 {
		return unresolvedCall
	}
	name := segs[len(segs)-1]
	if len(segs) > 1 {
		// `Script.Inner.f()` and `a.b.f()`: a path no rule here reads.
		if capitalised(segs[0]) && capitalised(name) {
			name = segs[len(segs)-1]
		} else {
			return unresolvedCall
		}
	}
	if t, found, ambiguous := r.gdTypeNamed(c, name); found {
		if c.ref.Name == "new" {
			if t.class != nil {
				return bound(candidate{*t.class, model.ConfidenceHigh, ruleConstructs + " (" + name + ")"})
			}
			return unresolvedCall
		}
		if len(t.members) > 0 {
			return pickModular(c, t.members, ruleNamedType, model.ConfidenceHigh)
		}
		// Not declared by the script: its parents may.
		if t.file != "" {
			var rows []moduleInclude
			if g := r.reachGraphs["gdscript"]; g != nil {
				if d, ok := g.lookup(strings.TrimSuffix(t.file, ".gd")); ok {
					for _, p := range d.Parts {
						rows = append(rows, moduleInclude{path: p})
					}
				}
			}
			if len(rows) > 0 {
				return r.gdInherited(c, t.file, rows)
			}
		}
		return unresolvedCall
	} else if ambiguous {
		return ambiguousCall
	}
	if c.spec.isBuiltinType(name) || c.spec.hasExternalRoot(segs) {
		return externalCall
	}
	if capitalised(name) && c.ref.ReceiverKind != model.ReceiverValue {
		// A type the repository does not define: the engine's, or a constant.
		return unresolvedCall
	}
	return r.memberByName(c, model.ConfidenceHigh, stats)
}
