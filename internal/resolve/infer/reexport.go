package infer

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
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
// imports it).
//
// # Show and hide
//
// `import 'a.dart' show f;` brings in f alone and `hide g` everything but g,
// and `export` takes the same combinators. They are carried on the rows
// (parse.ModuleShowPrefix, parse.ModuleHidingPrefix) and kept along the chain a
// name travels: a name reaches a file through a barrel only if the import and
// every export on the way let it pass. The same file reached by two routes is
// reached if either lets the name through, so a name hidden by one import is
// still bound through another that exposes it.
//
// A combinator names top-level declarations. A candidate that is one, defined
// in a file only combinator-limited routes reach, and that no route lets
// through, is dropped before any tier runs (hideCandidates); members are
// reached through their type and are left alone.

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
	if d, ok := g.files[importPath]; ok {
		return d, true
	}
	found, ok := g.bySuffix[importPath]
	if !ok || found == ambiguous {
		return registry.FileDirectives{}, false
	}
	return g.files[found], true
}

// reach returns the paths a file reaches by way of the files it imports: their
// parts, and what they export, transitively. In the order first reached, each
// once, and never a path the file imports itself. Combinators are not read.
func (g *reexportGraph) reach(imported []string) []string {
	roots := make([]reachRoot, len(imported))
	have := make(map[string]struct{}, len(imported))
	for i, p := range imported {
		roots[i] = reachRoot{path: p}
		have[p] = struct{}{}
	}
	var out []string
	for _, r := range g.reachFiltered(roots) {
		if _, dup := have[r.path]; dup {
			continue
		}
		have[r.path] = struct{}{}
		out = append(out, r.path)
	}
	return out
}

// reachRoot is a file an import names, with the combinators the import wrote.
type reachRoot struct {
	path   string
	filter nameFilter
}

// reached is a file a root reaches and the combinators every step of the way
// there carried. A file reached by two routes appears once per route.
type reached struct {
	path  string
	chain filterChain
}

// reachFiltered is reach with the chain of combinators kept. In the order first
// reached, each (path, chain) once. A file the importer names itself is listed
// too, when a barrel exports it: the two routes may let different names pass.
func (g *reexportGraph) reachFiltered(roots []reachRoot) []reached {
	var out []reached
	seen := make(map[string]struct{})
	add := func(p string, c filterChain) {
		k := p + "|" + c.key()
		if _, dup := seen[k]; dup {
			return
		}
		seen[k] = struct{}{}
		out = append(out, reached{path: p, chain: c})
	}
	visited := make(map[string]struct{})
	var walk func(p string, c filterChain, depth int)
	walk = func(p string, c filterChain, depth int) {
		k := p + "|" + c.key()
		if _, seen := visited[k]; seen {
			return
		}
		visited[k] = struct{}{}
		d, ok := g.lookup(p)
		if !ok {
			return
		}
		for _, part := range d.Parts {
			add(part, c)
		}
		if depth >= maxExportDepth {
			return
		}
		for i, exp := range d.Exports {
			next := c
			if i < len(d.ExportFilters) {
				next = c.with(parseFilter(d.ExportFilters[i]))
			}
			add(exp, next)
			walk(exp, next, depth+1)
		}
	}
	for _, r := range roots {
		walk(r.path, filterChain{}.with(r.filter), 0)
	}
	return out
}

// nameFilter is what one `show` and `hide` pair lets through.
type nameFilter struct {
	raw     string // the directives it was read from; its identity
	limited bool   // a `show` list was written
	show    map[string]struct{}
	hide    map[string]struct{}
}

// parseFilter reads the directives the parser wrote (`show:f,g;hiding:h`). A
// string with none gives the filter that lets everything through.
func parseFilter(directives string) nameFilter {
	var f nameFilter
	for _, part := range strings.Split(directives, parse.ModuleDirectiveSeparator) {
		switch {
		case strings.HasPrefix(part, parse.ModuleShowPrefix):
			f.limited = true
			f.show = nameSet(strings.TrimPrefix(part, parse.ModuleShowPrefix), f.show)
		case strings.HasPrefix(part, parse.ModuleHidingPrefix):
			f.hide = nameSet(strings.TrimPrefix(part, parse.ModuleHidingPrefix), f.hide)
		default:
			continue
		}
		if f.raw != "" {
			f.raw += parse.ModuleDirectiveSeparator
		}
		f.raw += part
	}
	return f
}

func nameSet(list string, into map[string]struct{}) map[string]struct{} {
	if into == nil {
		into = map[string]struct{}{}
	}
	for _, n := range strings.Split(list, ",") {
		into[n] = struct{}{}
	}
	return into
}

// open reports a filter that lets every name through.
func (f nameFilter) open() bool { return !f.limited && len(f.hide) == 0 }

func (f nameFilter) allows(name string) bool {
	if f.limited {
		if _, ok := f.show[name]; !ok {
			return false
		}
	}
	_, hidden := f.hide[name]
	return !hidden
}

// filterChain is the combinators a name passes on its way to a file: the
// import's, then each export's. Open filters are not kept.
type filterChain []nameFilter

// with returns the chain extended by f, never sharing the receiver's storage.
func (c filterChain) with(f nameFilter) filterChain {
	if f.open() {
		return c
	}
	out := make(filterChain, len(c), len(c)+1)
	copy(out, c)
	return append(out, f)
}

func (c filterChain) allows(name string) bool {
	for _, f := range c {
		if !f.allows(name) {
			return false
		}
	}
	return true
}

func (c filterChain) key() string {
	if len(c) == 0 {
		return ""
	}
	parts := make([]string, len(c))
	for i, f := range c {
		parts[i] = f.raw
	}
	return strings.Join(parts, "|")
}

// dartRoute is one way a file's names are reached: the import path as the
// resolver compares it, and the combinators on the way.
type dartRoute struct {
	segments []string
	chain    filterChain
}

// reexports returns a Dart file's import rows as the import tier should read
// them, with the paths its imports reach added, and the routes (see dartRoute)
// that say which names each path lets through. An `export` row is dropped (see
// above) and a `part` row becomes a plain import. A row's second column is its
// prefix alone afterwards: the combinators are in the routes.
func (r *Resolver) reexports(ctx context.Context, spec LanguageSpec, rows []registry.Import) ([]registry.Import, []dartRoute) {
	var roots []reachRoot
	var routes []dartRoute
	kept := make([]registry.Import, 0, len(rows))
	route := func(p string, c filterChain) {
		if segs := importSegments(spec, strings.Trim(p, "./")); len(segs) > 0 {
			routes = append(routes, dartRoute{segments: segs, chain: c})
		}
	}
	for _, row := range rows {
		prefix, kind, filter := splitDartAlias(row.Alias)
		switch kind {
		case exportDirective:
			continue
		case includeDirective:
			prefix = ""
		}
		row.Alias = prefix
		kept = append(kept, row)
		route(row.Path, filterChain{}.with(filter))
		if prefix == "" {
			roots = append(roots, reachRoot{path: row.Path, filter: filter})
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
		return kept, routes
	}
	have := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		have[root.path] = struct{}{}
	}
	for _, got := range r.reexportGraph.reachFiltered(roots) {
		route(got.path, got.chain)
		if _, dup := have[got.path]; dup {
			continue
		}
		have[got.path] = struct{}{}
		kept = append(kept, registry.Import{Path: got.path})
	}
	return kept, routes
}

// splitDartAlias reads the second column of a Dart import row: an optional
// prefix, an optional `export` or `include`, and the combinators.
func splitDartAlias(col string) (prefix, kind string, filter nameFilter) {
	if col == "" {
		return "", "", nameFilter{}
	}
	var rest []string
	for _, part := range strings.Split(col, parse.ModuleDirectiveSeparator) {
		switch {
		case part == exportDirective || part == includeDirective:
			kind = part
		case strings.HasPrefix(part, parse.ModuleShowPrefix), strings.HasPrefix(part, parse.ModuleHidingPrefix):
			rest = append(rest, part)
		default:
			prefix = part
		}
	}
	return prefix, kind, parseFilter(strings.Join(rest, parse.ModuleDirectiveSeparator))
}

// hideCandidates drops, from what each name of the file could be, the top-level
// definitions the file's imports do not let through. A candidate is dropped
// only when the imports that name its file (the deepest match, as the import
// tier ranks them) all carry combinators that exclude the name: a file no
// import names, the file itself, and members of types are left as they were.
func (r *Resolver) hideCandidates(spec LanguageSpec, fr registry.FileRefs, routes []dartRoute, symbols map[string][]registry.SymbolIndexEntry) {
	limited := false
	for _, rt := range routes {
		if len(rt.chain) > 0 {
			limited = true
			break
		}
	}
	if !limited {
		return
	}
	done := make(map[string]struct{}, len(fr.Refs))
	segsOf := map[string][]string{}
	for _, ref := range fr.Refs {
		if _, ok := done[ref.Name]; ok {
			continue
		}
		done[ref.Name] = struct{}{}
		cands := symbols[ref.Name]
		var kept []registry.SymbolIndexEntry
		for i, c := range cands {
			if hiddenBy(spec, fr.Path, routes, segsOf, c) {
				if kept == nil {
					kept = append(make([]registry.SymbolIndexEntry, 0, len(cands)), cands[:i]...)
				}
				continue
			}
			if kept != nil {
				kept = append(kept, c)
			}
		}
		if kept != nil {
			symbols[ref.Name] = kept
		}
	}
}

// topLevelKind reports whether a symbol of this kind is something a library
// declares at its top level, which is what a combinator can name. A method or a
// field is reached through its type.
func topLevelKind(kind string) bool {
	switch model.EntityKind(kind) {
	case model.KindFunction, model.KindClass, model.KindInterface, model.KindConstant, model.KindVariable, model.KindTypeAlias:
		return true
	}
	return false
}

func hiddenBy(spec LanguageSpec, callerPath string, routes []dartRoute, segsOf map[string][]string, c registry.SymbolIndexEntry) bool {
	if c.FilePath == callerPath || strings.Contains(c.Name, ".") || !topLevelKind(c.Kind) {
		return false
	}
	segs, ok := segsOf[c.FilePath]
	if !ok {
		segs = fileSegments(spec, c.FilePath)
		segsOf[c.FilePath] = segs
	}
	best := 0
	allowed := false
	for _, rt := range routes {
		d := commonSuffixLen(rt.segments, segs)
		switch {
		case d == 0 || d < best:
		case d > best:
			best, allowed = d, rt.chain.allows(c.Name)
		default:
			allowed = allowed || rt.chain.allows(c.Name)
		}
	}
	return best > 0 && !allowed
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
