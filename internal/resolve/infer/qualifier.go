package infer

import (
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// # What a qualifier names
//
// `errors.New(...)`, `util.joinValues(...)`, `Collections.sort(...)`: each is a
// call qualified by a name the file's imports introduced, and each language
// spells the introduction differently. This file turns an import row into the
// one thing binding needs from it — a module: a predicate over candidate files
// that says "this file is in the package the import names", and a verdict on
// whether that package is in the repository at all.
//
// The verdict is three-valued on purpose. A package the repository's layout
// shows is absent makes the call EXTERNAL. A package that is present makes it
// an ordinary lookup, which may still find nothing. And where the layout
// cannot say, the answer is "unknown", which records nothing as external: a
// claim that a call leaves the repository is a claim about the program and is
// made only on evidence.

// presence is where a module sits relative to the repository. The zero value
// is "unknown": nothing shows the module is here or elsewhere.
type presence uint8

const (
	// presenceInRepo: the module is in the repository.
	presenceInRepo presence = iota + 1
	// presenceExternal: the module is the standard library or a third-party
	// package. A call qualified by it binds to nothing here.
	presenceExternal
)

// module is what an import, or a written qualifier, was traced to.
type module struct {
	where presence

	// contains reports whether a candidate's file is the module itself.
	// beneath reports whether it lies under the module as a package, for the
	// case where the package re-exports what its files define. Either may be
	// nil.
	contains func(c registry.SymbolIndexEntry) bool
	beneath  func(c registry.SymbolIndexEntry) bool

	// what describes the import in the reason attached to an edge.
	what string
}

// goImportName is the name a Go import binds when it writes no alias: the
// package's name, which is by convention the last path segment — past a major
// version suffix, and without the `go-` and `.go` decorations a repository
// name carries and a package name cannot.
func goImportName(importPath string) string {
	segs := strings.Split(importPath, "/")
	last := segs[len(segs)-1]
	if len(segs) > 1 && isMajorVersion(last) {
		last = segs[len(segs)-2]
	}
	// gopkg.in/yaml.v3 is package yaml.
	if i := strings.Index(last, ".v"); i > 0 && isMajorVersion(last[i+1:]) {
		last = last[:i]
	}
	last = strings.TrimPrefix(last, "go-")
	last = strings.TrimSuffix(last, "-go")
	last = strings.TrimSuffix(last, ".go")
	return strings.ReplaceAll(last, "-", "_")
}

// goStandardRoots are the first segments of the Go standard library's import
// paths. A module path may not begin with one of them without a dot, so an
// import that does is the standard library whatever directories the
// repository happens to have.
var goStandardRoots = map[string]bool{
	"archive": true, "bufio": true, "builtin": true, "bytes": true, "cmp": true, "compress": true,
	"container": true, "context": true, "crypto": true, "database": true, "debug": true,
	"embed": true, "encoding": true, "errors": true, "expvar": true, "flag": true, "fmt": true,
	"go": true, "hash": true, "html": true, "image": true, "index": true, "io": true, "iter": true,
	"log": true, "maps": true, "math": true, "mime": true, "net": true, "os": true, "path": true,
	"plugin": true, "reflect": true, "regexp": true, "runtime": true, "slices": true, "sort": true,
	"strconv": true, "strings": true, "structs": true, "sync": true, "syscall": true,
	"testing": true, "text": true, "time": true, "unicode": true, "unique": true, "unsafe": true,
	"weak": true, "C": true,
}

func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// goModule traces a Go qualifier to the import that binds it.
//
// A Go import path is a module path followed by a directory under the
// module's root, so a package is in the repository exactly when some indexed
// directory is a module root followed by a tail of the import path. A
// module's own root package is the one case with no tail to match, and is
// recognised by elimination: not the standard library, not a declared
// dependency, and not a directory.
func (r *Resolver) goModule(imports importSet, name string) (module, bool) {
	var entry importEntry
	found := false
	for _, e := range imports.imports {
		if e.alias == name || (e.alias == "" && goImportName(e.raw) == name) {
			entry, found = e, true
			break
		}
	}
	if !found {
		return module{}, false
	}
	segs := dirSegments(entry.raw)
	m := module{what: "the imported package " + entry.raw}

	// The longest tail of the import path that is a directory under some
	// module root.
	tail := -1
	for n := len(segs); n >= 1; n-- {
		if r.layout.HasDirUnderModule(segs[len(segs)-n:]) {
			tail = n
			break
		}
	}
	first := strings.SplitN(entry.raw, "/", 2)[0]
	switch {
	case r.layout.DeclaresDependency(entry.raw):
		m.where = presenceExternal
	case goStandardRoots[first], !strings.Contains(first, ".") && (tail < 0 || tail == len(segs)):
		// The standard library: a root the toolchain ships, or a dotless path
		// no directory here could be the tail of. A repository whose own
		// module is named without a dot is the exception the tail test keeps:
		// its directory is the import path minus that name, never all of it.
		m.where = presenceExternal
	case tail > 0:
		m.where = presenceInRepo
		want := segs[len(segs)-tail:]
		m.contains = func(c registry.SymbolIndexEntry) bool {
			dir := dirSegments(path.Dir(c.FilePath))
			if len(dir) < len(want) {
				return false
			}
			for i := range want {
				if dir[len(dir)-len(want)+i] != want[i] {
					return false
				}
			}
			return r.layout.IsModuleRoot(dir[:len(dir)-len(want)])
		}
	case r.layout.Known():
		// A module's own root package.
		m.where = presenceInRepo
		m.contains = func(c registry.SymbolIndexEntry) bool {
			return r.layout.IsModuleRoot(dirSegments(path.Dir(c.FilePath)))
		}
	default:
		// No layout to ask. Fall back to the tail the candidates themselves
		// show, and claim nothing about where the package is.
		m.contains = func(c registry.SymbolIndexEntry) bool {
			dir := dirSegments(path.Dir(c.FilePath))
			return len(dir) > 0 && commonSuffixLen(segs, dir) == len(dir)
		}
	}
	return m, true
}

// scriptExtensions are the extensions an ECMAScript import may write or omit.
var scriptExtensions = []string{".d.ts", ".tsx", ".ts", ".mts", ".cts", ".jsx", ".js", ".mjs", ".cjs", ".vue", ".svelte", ".astro"}

// trimScriptExtension removes a script extension from a path, if it has one.
func trimScriptExtension(p string) string {
	for _, ext := range scriptExtensions {
		if strings.HasSuffix(p, ext) {
			return p[:len(p)-len(ext)]
		}
	}
	return p
}

// pathAliasPrefixes are the specifier prefixes that conventionally map to the
// repository's own source root through a bundler or tsconfig `paths` entry.
var pathAliasPrefixes = []string{"@/", "~/", "#/", "src/"}

// scriptModule traces an ECMAScript import specifier to a module.
//
// A relative specifier names one file, and is resolved against the importing
// file exactly as the runtime resolves it: `../core/util.js` from
// `v4/classic/parse.ts` is `v4/core/util`, whatever other `util` the
// repository holds. That exactness is the point — the comparison this
// replaces matched a receiver's name against any file's base name, and bound
// 121 calls in zod's v4 tree to its v3 `util.ts`.
//
// A bare specifier names a package. It is external unless the repository has a
// directory of that name, in which case it may be a workspace package and is
// matched by the rest of its path.
func (r *Resolver) scriptModule(fromPath, specifier string) module {
	m := module{what: "the imported module " + specifier}
	lowerNoExt := func(p string) string { return strings.ToLower(trimScriptExtension(p)) }

	if strings.HasPrefix(specifier, ".") {
		target := strings.ToLower(trimScriptExtension(path.Clean(path.Join(path.Dir(fromPath), specifier))))
		m.where = presenceInRepo
		m.contains = func(c registry.SymbolIndexEntry) bool {
			p := lowerNoExt(c.FilePath)
			return p == target || p == target+"/index"
		}
		// `./core/index.js` re-exports what the files beside it define.
		dir := strings.TrimSuffix(target, "/index")
		m.beneath = func(c registry.SymbolIndexEntry) bool {
			return strings.HasPrefix(lowerNoExt(c.FilePath), dir+"/")
		}
		return m
	}

	rest := ""
	for _, prefix := range pathAliasPrefixes {
		if strings.HasPrefix(specifier, prefix) {
			rest = strings.TrimPrefix(specifier, prefix)
			if prefix == "src/" {
				rest = specifier
			}
			break
		}
	}
	if rest == "" {
		// A package: `zod`, `zod/v4/core`, `@scope/pkg/sub`.
		parts := strings.Split(specifier, "/")
		nameParts := 1
		if strings.HasPrefix(specifier, "@") && len(parts) > 1 {
			nameParts = 2
		}
		pkg := parts[nameParts-1]
		if strings.HasPrefix(specifier, "node:") || !r.layout.Known() || !r.layout.HasDirNamed(pkg) {
			if r.layout.Known() || strings.HasPrefix(specifier, "node:") {
				m.where = presenceExternal
			}
			return m
		}
		// Possibly a workspace package. Its sub-path, if it wrote one, is the
		// only part that can be matched against a file.
		rest = strings.Join(parts[nameParts:], "/")
		if rest == "" {
			return m
		}
	}
	suffix := "/" + strings.ToLower(trimScriptExtension(rest))
	m.contains = func(c registry.SymbolIndexEntry) bool {
		p := "/" + lowerNoExt(c.FilePath)
		return strings.HasSuffix(p, suffix) || strings.HasSuffix(p, suffix+"/index")
	}
	return m
}

// scriptQualifier traces an ECMAScript receiver to the import that binds it:
// a namespace import, or a default or named import.
//
// member is true when the name was imported FROM the module rather than bound
// to it, in which case the receiver is something the module defines — a class,
// or a namespace it re-exports — and not the module.
func (r *Resolver) scriptQualifier(fr registry.FileRefs, imports importSet, name string) (m module, member, ok bool) {
	for _, e := range imports.imports {
		if e.alias == name {
			return r.moduleOf(imports.spec, fr.Path, e.raw), false, true
		}
	}
	if idx := imports.members[name]; len(idx) > 0 {
		return r.moduleOf(imports.spec, fr.Path, imports.imports[idx[0]].raw), true, true
	}
	return module{}, false, false
}

// moduleOf traces an import's path to a module in the language's own terms.
// ECMAScript's rules (extensions, `./`, `index`, bare packages) are the
// default; a language whose specifier means something else says so here
// rather than being read as if it were a JavaScript one.
func (r *Resolver) moduleOf(spec LanguageSpec, fromPath, specifier string) module {
	switch spec.Name {
	case "lua":
		return r.luaModule(spec, specifier)
	case "dart":
		return r.dartModule(specifier)
	}
	return r.scriptModule(fromPath, specifier)
}

// luaModule traces a `require` name to a module.
//
// `require("a.b")` names a.b by its dots, a/b.lua or a/b/init.lua beneath a
// source root the code does not state (the working directory, a `lua/`
// directory, the package path), so the module is the file whose path ENDS with
// every segment of the name: a longer prefix is the unknown root. Anything
// less than the whole name matching is some other module with a similar
// ending, which is what the trailing-segment import tier (which asks only for
// one segment) cannot tell apart.
//
// A name no file's stem or directory carries (`cjson`, `lpeg`, `socket.http`)
// is a rock or the standard library, and a call qualified by it leaves the
// repository: asking the layout is the only way to say so, since the module
// itself has no file to point at. Where the layout is not known nothing is
// claimed.
func (r *Resolver) luaModule(spec LanguageSpec, specifier string) module {
	m := module{what: "the required module " + specifier}
	segs := pathSegments(specifier)
	if len(segs) == 0 {
		return m
	}
	last := segs[len(segs)-1]
	if r.layout.Known() && !r.layout.HasStem(last) && !r.layout.HasDirNamed(last) {
		m.where = presenceExternal
		return m
	}
	m.where = presenceInRepo
	m.contains = func(c registry.SymbolIndexEntry) bool {
		return commonSuffixLen(segs, fileSegments(spec, c.FilePath)) == len(segs)
	}
	return m
}

// classImport traces a class name to the Java-style import that brought it
// in: `import com.google.gson.stream.JsonReader` binds `JsonReader` to the
// class in a file `…/com/google/gson/stream/JsonReader.java`.
//
// The package is in the repository exactly when some indexed directory ends
// with its path. That is asked of the layout rather than of the candidates,
// because a class the repository does not define has no candidate to ask.
func (r *Resolver) classImport(imports importSet, name string) (module, bool) {
	for _, e := range imports.imports {
		if e.alias != "" || len(e.segments) < 2 || lastRawSegment(e.raw) != name {
			continue
		}
		pkg := e.segments[:len(e.segments)-1]
		m := module{what: "the import " + e.raw}
		switch {
		case r.layout.HasDirEnding(pkg):
			m.where = presenceInRepo
		case r.layout.Known():
			m.where = presenceExternal
		}
		want := e.segments
		m.contains = func(c registry.SymbolIndexEntry) bool {
			segs := append(pathSegments(path.Dir(c.FilePath)), strings.ToLower(fileBaseName(c.FilePath)))
			return commonSuffixLen(want, segs) == len(want)
		}
		return m, true
	}
	return module{}, false
}

// staticImport traces a bare name to a Java static import:
// `import static a.c.Util.help` binds `help` to the member of class Util in
// `…/a/c/Util.java`. It returns the class's name and the module its file is.
func (r *Resolver) staticImport(imports importSet, name string) (owner string, m module, ok bool) {
	for _, e := range imports.imports {
		if e.alias != "" || len(e.segments) < 3 || lastRawSegment(e.raw) != name {
			continue
		}
		raw := strings.Split(e.raw, ".")
		owner = raw[len(raw)-2]
		if !capitalised(owner) {
			continue
		}
		class := e.segments[:len(e.segments)-1]
		m = module{what: "the static import " + e.raw}
		switch {
		case r.layout.HasDirEnding(class[:len(class)-1]):
			m.where = presenceInRepo
		case r.layout.Known():
			m.where = presenceExternal
		}
		m.contains = func(c registry.SymbolIndexEntry) bool {
			segs := append(pathSegments(path.Dir(c.FilePath)), strings.ToLower(fileBaseName(c.FilePath)))
			return commonSuffixLen(class, segs) == len(class)
		}
		return owner, m, true
	}
	return "", module{}, false
}

// lastRawSegment is the final dotted segment of an import path as written,
// case preserved: the class a Java import names.
func lastRawSegment(raw string) string {
	if i := strings.LastIndexAny(raw, "./"); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

// hasExternalRoot reports whether a qualified path begins with a segment that
// always lies outside a repository in this language: `std::fs`, `System.Math`.
func (s LanguageSpec) hasExternalRoot(segments []string) bool {
	if len(segments) == 0 {
		return false
	}
	for _, root := range s.Calls.ExternalRoots {
		if segments[0] == root {
			return true
		}
	}
	return false
}

// dartModule traces a Dart import to the one file it names.
//
// The parser has already turned the URI into the file's repository path
// without its extension (`lib/src/pricing`; see dartImport), so the module is
// the file whose path IS that, or ends with it where a monorepo keeps the
// package under a directory (`packages/shop/lib/src/pricing`). Asking for the
// whole path is the point: the import tier asks for one trailing segment, and
// two `util.dart` under different directories are two modules.
//
// A path no file's stem carries is somebody else's package (`lib/material`
// from `package:flutter/material.dart`) and a call qualified by it is external.
func (r *Resolver) dartModule(rooted string) module {
	m := module{what: "the imported library " + rooted}
	want := strings.ToLower(rooted)
	last := want[strings.LastIndexByte(want, '/')+1:]
	if r.layout.Known() && !r.layout.HasStem(last) {
		m.where = presenceExternal
		return m
	}
	m.where = presenceInRepo
	m.contains = func(c registry.SymbolIndexEntry) bool {
		p := strings.ToLower(strings.TrimSuffix(c.FilePath, path.Ext(c.FilePath)))
		return p == want || strings.HasSuffix(p, "/"+want)
	}
	return m
}
