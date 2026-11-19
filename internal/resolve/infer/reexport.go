package infer

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// # What a library reaches beyond what it imports
//
// A Dart library is a file and the parts that name it. `export 'src/x.dart';`
// in a barrel (`lib/shop.dart`) means that importing the barrel brings x.dart's
// names into scope, though the importer never wrote x.dart; and `part
// 'x.g.dart';` joins a generated file into the library that names it, so a
// function defined there is called by its owner as though it were written in
// the same file.
//
// The parser records both as import rows whose alias says what they are
// (parse.ModuleExport, parse.ModuleInclude). An export is NOT an import of its
// own file: the file that writes it does not see the names it re-exports. A
// part is, because the library and its parts are one scope.
//
// What a file reaches is then its imports plus, for each, the parts of that
// library and everything it exports, followed to maxExportDepth with a visited
// set (barrels export barrels, and a barrel may export the library that
// imports it). Show and hide combinators are not read: a name a combinator
// hides would be reachable here, and it is reached at the same confidence the
// import tier gives any name of an imported file, no higher.

// maxExportDepth bounds how many barrels deep a name is followed. Real
// packages stack one or two; the bound is for cycles and for chains that are
// wrong.
const maxExportDepth = 4

// Directives is the optional port a Candidates source offers for the export and
// part rows of a language's files. A source without it reaches nothing beyond
// a file's own imports, as before.
type Directives interface {
	// Directives returns, for each file of the language, keyed by its path
	// without extension, the paths it exports and the parts it names.
	Directives(ctx context.Context, language string) (map[string]registry.FileDirectives, error)
}

// reexportGraph is a run's export and part directives, indexed for lookup by
// the path an import writes.
type reexportGraph struct {
	files map[string]registry.FileDirectives
	// keys is every file with a directive, sorted.
	keys []string
	// bySuffix maps each "/"-bounded tail of every key to the one key it
	// ends, or to ambiguous when two keys share it. It is how an import path
	// that is a suffix of a file's (a package living under a directory) is
	// found without scanning every key: that scan made resolving a Dart
	// repository quadratic, most of all for `package:` imports of external
	// packages, which never match and so always scanned them all.
	bySuffix map[string]string
}

// ambiguous marks a suffix two files end with. It cannot be a file key: keys
// are paths without extension and this holds a NUL.
const ambiguous = "\x00ambiguous"

func newReexportGraph(files map[string]registry.FileDirectives) *reexportGraph {
	g := &reexportGraph{files: files, keys: make([]string, 0, len(files)), bySuffix: make(map[string]string, len(files)*3)}
	for k := range files {
		g.keys = append(g.keys, k)
	}
	sort.Strings(g.keys)
	for _, k := range g.keys {
		for i := 0; i < len(k); i++ {
			if k[i] != '/' {
				continue
			}
			tail := k[i+1:]
			if _, dup := g.bySuffix[tail]; dup {
				g.bySuffix[tail] = ambiguous
			} else {
				g.bySuffix[tail] = k
			}
		}
	}
	return g
}

// lookup finds the directives of the file an import path names: the file whose
// path is that, or the one that ends with it where a package lives under a
// directory. Two such files are no answer.
func (g *reexportGraph) lookup(importPath string) (registry.FileDirectives, bool) {
	k, ok := g.lookupKey(importPath)
	if !ok {
		return registry.FileDirectives{}, false
	}
	return g.files[k], true
}

// lookupKey is lookup's file: the key of the file an import path names.
func (g *reexportGraph) lookupKey(importPath string) (string, bool) {
	if _, ok := g.files[importPath]; ok {
		return importPath, true
	}
	found, ok := g.bySuffix[importPath]
	if !ok || found == ambiguous {
		return "", false
	}
	return found, true
}

// reach returns the paths a file reaches by way of the files it imports: their
// parts, and what they export, transitively. In the order first reached, each
// once, and never a path the file imports itself.
func (g *reexportGraph) reach(imported []string) []string {
	have := make(map[string]struct{}, len(imported))
	for _, p := range imported {
		have[p] = struct{}{}
	}
	var out []string
	add := func(p string) bool {
		if _, dup := have[p]; dup {
			return false
		}
		have[p] = struct{}{}
		out = append(out, p)
		return true
	}
	visited := make(map[string]struct{})
	var walk func(p string, depth int)
	walk = func(p string, depth int) {
		if _, seen := visited[p]; seen {
			return
		}
		visited[p] = struct{}{}
		d, ok := g.lookup(p)
		if !ok {
			return
		}
		for _, part := range d.Parts {
			add(part)
		}
		if depth >= maxExportDepth {
			return
		}
		for _, exp := range d.Exports {
			add(exp)
			walk(exp, depth+1)
		}
	}
	for _, p := range imported {
		walk(p, 0)
	}
	return out
}

// reexports returns a Dart file's import rows as the import tier should read
// them, with the paths its imports reach added. An `export` row is dropped
// (see above) and a `part` row becomes a plain import.
func (r *Resolver) reexports(ctx context.Context, spec LanguageSpec, rows []registry.Import) []registry.Import {
	var imported []string
	kept := make([]registry.Import, 0, len(rows))
	for _, row := range rows {
		switch row.Alias {
		case exportDirective:
			continue
		case includeDirective:
			row.Alias = ""
		}
		kept = append(kept, row)
		if row.Alias == "" {
			imported = append(imported, row.Path)
		}
	}
	if !r.reexportLoaded {
		r.reexportLoaded = true
		if src, ok := r.candidates.(Directives); ok {
			if files, err := src.Directives(ctx, spec.Name); err == nil && len(files) > 0 {
				r.reexportGraph = newReexportGraph(files)
			}
		}
	}
	if r.reexportGraph == nil {
		return kept
	}
	for _, p := range r.reexportGraph.reach(imported) {
		kept = append(kept, registry.Import{Path: p})
	}
	return kept
}

// The two directive aliases the parser gives a Dart row (parse.ModuleExport,
// parse.ModuleInclude), restated here because this package does not import the
// parser. Keep them in step.
const (
	exportDirective  = "export"
	includeDirective = "include"
)

// importSegments splits an import path into the segments a file compares with.
// A Dart import names a file, whose stem may hold a dot (`model.g`, a generated
// part): it is one segment, as it is in fileSegments, and not two.
func importSegments(spec LanguageSpec, p string) []string {
	dir, base := path.Split(p)
	if !spec.ReExports || base == "" {
		return pathSegments(p)
	}
	return append(pathSegments(dir), strings.ToLower(base))
}

// privateElsewhere reports whether a call to a `_private` name cannot reach the
// candidate: in Dart a name that starts with an underscore is private to its
// LIBRARY, which is a file and the parts that name it, so a definition in
// another library is not what a call there denotes whatever the imports say.
// The library is read from the part directives; where none are indexed a
// library is its one file.
func (r *Resolver) privateElsewhere(spec LanguageSpec, name, callerPath string, c registry.SymbolIndexEntry) bool {
	if spec.Name != "dart" || !strings.HasPrefix(name, "_") || c.FilePath == callerPath {
		return false
	}
	if r.reexportGraph == nil {
		return true
	}
	return !r.reexportGraph.sameLibrary(callerPath, c.FilePath)
}

// sameLibrary reports whether two files are one library: one names the other
// as a part, or both are parts of a third.
func (g *reexportGraph) sameLibrary(a, b string) bool {
	ka := strings.TrimSuffix(a, path.Ext(a))
	kb := strings.TrimSuffix(b, path.Ext(b))
	names := func(parts []string, key string) bool {
		for _, p := range parts {
			if p == key || strings.HasSuffix(key, "/"+p) {
				return true
			}
		}
		return false
	}
	if d, ok := g.lookup(ka); ok && names(d.Parts, kb) {
		return true
	}
	if d, ok := g.lookup(kb); ok && names(d.Parts, ka) {
		return true
	}
	for _, key := range g.keys {
		if d := g.files[key]; names(d.Parts, ka) && names(d.Parts, kb) {
			return true
		}
	}
	return false
}
