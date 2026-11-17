package infer

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Layout is what resolution knows about the repository as a whole: which
// directories hold indexed files, which file names exist, and which packages
// its manifests declare as dependencies.
//
// It exists to answer one question the candidate set cannot: when a call is
// qualified by an import and no candidate lives in that import, is that because
// the package is OUTSIDE the repository, or because it is inside and the name
// simply was not extracted? The first is recorded as external. The second is
// an ordinary unresolved reference, and stamping it external would be a claim
// about the program that nothing supports.
//
// Built once per run from two bounded reads, and never consulted through SQL:
// resolution stays a pure function of what it is handed. A nil *Layout answers
// "unknown" to everything, which is the state every caller was in before it
// existed — nothing is recorded external, and nothing else changes.
type Layout struct {
	// dirs holds each directory with an indexed file, as its lowercased
	// segments joined by '/'. The root directory is "".
	dirs map[string]struct{}

	// reversed holds the same directories with their segments reversed, each
	// terminated by '/', sorted. A directory ENDING with given segments is then
	// a prefix search, which is what an import path asks: `a.b` names whatever
	// directory ends `a/b`, under any source root.
	reversed []string

	// dirNames is every directory's own name, and stems every file's name
	// without its extension.
	dirNames map[string]struct{}
	stems    map[string]struct{}

	// initDirs holds the directories that contain a package marker file — a
	// Python `__init__.py`. An absolute import cannot name a module from
	// INSIDE such a directory: `import json` is the top-level `json`, never
	// `flask/json`, because `flask` is itself a package.
	initDirs map[string]struct{}

	// moduleRoots holds the directories that contain a Go module file. A Go
	// import path is a module path followed by a directory RELATIVE TO THE
	// MODULE'S ROOT, which is the repository's root only when the repository
	// is one module.
	moduleRoots map[string]struct{}

	// projectRoots holds the directories that contain a subproject manifest
	// (e.g. package.json, go.mod, pyproject.toml, Cargo.toml, pom.xml, composer.json).
	// Used in polyglot repositories and parent folders to partition candidate
	// searches so unrelated subprojects do not contaminate each other.
	projectRoots map[string]struct{}

	// deps are the dependencies the repository's manifests declare.
	deps map[string]struct{}
}

// packageMarkers are the file names that make a directory a package whose
// name is the directory's.
var packageMarkers = map[string]bool{"__init__.py": true, "__init__.pyi": true}

// projectMarkers are manifest files that mark the root of a project or subproject
// in monorepos and polyglot parent folders.
var projectMarkers = map[string]bool{
	"package.json":          true,
	"tsconfig.json":         true,
	"deno.json":             true,
	"deno.jsonc":            true,
	"go.mod":                true,
	"go.work":               true,
	"pyproject.toml":        true,
	"requirements.txt":      true,
	"setup.py":              true,
	"setup.cfg":             true,
	"Pipfile":               true,
	"pom.xml":               true,
	"build.gradle":          true,
	"build.gradle.kts":      true,
	"Cargo.toml":            true,
	"composer.json":         true,
	"Gemfile":               true,
	"Package.swift":         true,
	"pubspec.yaml":          true,
	"mix.exs":               true,
	"rebar.config":          true,
	"compile_commands.json": true,
	"CMakeLists.txt":        true,
	"meson.build":           true,
}

// secondaryMarkers configure a folder without declaring a project: a
// tsconfig.json for tests, a requirements.txt for building the docs.
var secondaryMarkers = map[string]bool{"tsconfig.json": true, "requirements.txt": true}

// conventionalSubfolders are the folders inside a project that such a marker
// configures rather than founds.
var conventionalSubfolders = map[string]bool{
	"src": true, "test": true, "tests": true, "__tests__": true, "spec": true,
	"doc": true, "docs": true, "example": true, "examples": true, "scripts": true,
}

// secondaryMarkerInSubfolder reports whether a marker only configures a
// conventional folder of an enclosing project. Counting it as a project root
// split one project in two, and a test lost the code it exercises as a
// candidate.
func secondaryMarkerInSubfolder(base, dir string) bool {
	return secondaryMarkers[base] && conventionalSubfolders[path.Base(dir)]
}

// NewLayout builds a Layout from every indexed file's path and every declared
// dependency's name.
func NewLayout(filePaths, dependencies []string) *Layout {
	l := &Layout{
		dirs:         make(map[string]struct{}, len(filePaths)/4+1),
		dirNames:     make(map[string]struct{}, len(filePaths)/4+1),
		stems:        make(map[string]struct{}, len(filePaths)/2+1),
		initDirs:     make(map[string]struct{}, 16),
		moduleRoots:  make(map[string]struct{}, 4),
		projectRoots: make(map[string]struct{}, 8),
		deps:         make(map[string]struct{}, len(dependencies)),
	}
	// Secondary markers in conventional subfolders, decided once every
	// primary root is known: they count only when no project encloses them.
	var deferred []string
	for _, p := range filePaths {
		dir := path.Dir(p)
		segs := dirSegments(dir)
		key := strings.Join(segs, "/")
		base := path.Base(p)
		if packageMarkers[base] {
			l.initDirs[key] = struct{}{}
		}
		if base == "go.mod" {
			l.moduleRoots[key] = struct{}{}
		}
		if projectMarkers[base] || strings.HasSuffix(base, ".csproj") || strings.HasSuffix(base, ".sln") {
			if key != "" {
				if secondaryMarkerInSubfolder(base, key) {
					deferred = append(deferred, key)
				} else {
					l.projectRoots[key] = struct{}{}
				}
			}
		}
		l.stems[strings.ToLower(strings.TrimSuffix(base, path.Ext(base)))] = struct{}{}
		if _, seen := l.dirs[key]; seen {
			continue
		}
		// Every ancestor of a directory holding a file is a directory too.
		for n := len(segs); n >= 0; n-- {
			k := strings.Join(segs[:n], "/")
			if _, seen := l.dirs[k]; seen {
				break
			}
			l.dirs[k] = struct{}{}
			if n > 0 {
				l.dirNames[segs[n-1]] = struct{}{}
			}
		}
	}
	l.reversed = make([]string, 0, len(l.dirs))
	for k := range l.dirs {
		if k == "" {
			continue
		}
		l.reversed = append(l.reversed, reverseKey(strings.Split(k, "/")))
	}
	sort.Strings(l.reversed)
	for _, d := range dependencies {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			l.deps[d] = struct{}{}
		}
	}
	// A secondary marker in a conventional folder is a root only when no
	// project encloses it: a repository whose one marker is src/tsconfig.json
	// still has a project there.
	for _, key := range deferred {
		if l.ProjectOf(key+"/x") == "" {
			l.projectRoots[key] = struct{}{}
		}
	}
	return l
}

// dirSegments splits a directory path into lowercased segments on '/' only.
// pathSegments also splits on '.', which is right for comparing an import path
// with a file path and wrong for naming a directory: `v1.2/x` is two
// directories, not three.
func dirSegments(dir string) []string {
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}
	parts := strings.Split(strings.ToLower(dir), "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

func reverseKey(segs []string) string {
	var b strings.Builder
	for i := len(segs) - 1; i >= 0; i-- {
		b.WriteString(segs[i])
		b.WriteByte('/')
	}
	return b.String()
}

// Known reports whether there is a layout to ask. Every other method answers
// false on a nil Layout, and a caller that needs to tell "no" from "unknown"
// asks this first.
func (l *Layout) Known() bool { return l != nil }

// HasDir reports whether a directory, given as lowercased segments from the
// repository root, holds indexed files.
func (l *Layout) HasDir(segs []string) bool {
	if l == nil {
		return false
	}
	_, ok := l.dirs[strings.Join(segs, "/")]
	return ok
}

// HasDirEnding reports whether some indexed directory's path ends with the
// given segments.
func (l *Layout) HasDirEnding(segs []string) bool {
	if l == nil || len(segs) == 0 {
		return false
	}
	want := reverseKey(segs)
	i := sort.SearchStrings(l.reversed, want)
	return i < len(l.reversed) && strings.HasPrefix(l.reversed[i], want)
}

// HasTopLevelDir reports whether some indexed directory ends with the given
// segments AND does not sit inside a package directory — which is what an
// absolute import of that dotted path could name.
func (l *Layout) HasTopLevelDir(segs []string) bool {
	if l == nil || len(segs) == 0 {
		return false
	}
	want := reverseKey(segs)
	for i := sort.SearchStrings(l.reversed, want); i < len(l.reversed) && strings.HasPrefix(l.reversed[i], want); i++ {
		// The rest of the reversed key is the directory above, innermost first.
		above := strings.Split(strings.TrimSuffix(l.reversed[i][len(want):], "/"), "/")
		for a, b := 0, len(above)-1; a < b; a, b = a+1, b-1 {
			above[a], above[b] = above[b], above[a]
		}
		if len(above) == 1 && above[0] == "" {
			above = nil
		}
		if _, isPackage := l.initDirs[strings.Join(above, "/")]; !isPackage {
			return true
		}
	}
	return false
}

// HasDirNamed reports whether any indexed directory has this name.
func (l *Layout) HasDirNamed(name string) bool {
	if l == nil {
		return false
	}
	_, ok := l.dirNames[strings.ToLower(name)]
	return ok
}

// HasStem reports whether any indexed file has this name, extension aside.
func (l *Layout) HasStem(stem string) bool {
	if l == nil {
		return false
	}
	_, ok := l.stems[strings.ToLower(stem)]
	return ok
}

// IsPackageDir reports whether a directory, as lowercased segments from the
// root, carries a package marker file.
func (l *Layout) IsPackageDir(segs []string) bool {
	if l == nil {
		return false
	}
	_, ok := l.initDirs[strings.Join(segs, "/")]
	return ok
}

// IsModuleRoot reports whether a directory, as lowercased segments from the
// repository root, is the root of a Go module: it holds a go.mod, or it is the
// repository root of a tree that holds none (an index of part of a module).
func (l *Layout) IsModuleRoot(segs []string) bool {
	if l == nil {
		return false
	}
	if _, ok := l.moduleRoots[strings.Join(segs, "/")]; ok {
		return true
	}
	return len(segs) == 0 && len(l.moduleRoots) == 0
}

// HasDirUnderModule reports whether some Go module root has a directory at
// the given path beneath it. With no go.mod in the index the repository root
// stands for the one module.
func (l *Layout) HasDirUnderModule(tail []string) bool {
	if l == nil || len(tail) == 0 {
		return false
	}
	rel := strings.Join(tail, "/")
	if len(l.moduleRoots) == 0 {
		_, ok := l.dirs[rel]
		return ok
	}
	for root := range l.moduleRoots {
		key := rel
		if root != "" {
			key = root + "/" + rel
		}
		if _, ok := l.dirs[key]; ok {
			return true
		}
	}
	return false
}

// DeclaresDependency reports whether an import path is, or lies beneath, a
// dependency a manifest declares: `github.com/stretchr/testify/require` under
// a declared `github.com/stretchr/testify`.
func (l *Layout) DeclaresDependency(importPath string) bool {
	if l == nil || len(l.deps) == 0 {
		return false
	}
	p := strings.ToLower(importPath)
	for {
		if _, ok := l.deps[p]; ok {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i <= 0 {
			return false
		}
		p = p[:i]
	}
}

// ProjectOf returns the innermost subproject root enclosing p, or "" if p is
// not under any recognized subproject directory.
func (l *Layout) ProjectOf(p string) string {
	if l == nil || len(l.projectRoots) == 0 {
		return ""
	}
	clean := filepath.ToSlash(p)
	dir := path.Dir(clean)
	segs := dirSegments(dir)
	for n := len(segs); n > 0; n-- {
		k := strings.Join(segs[:n], "/")
		if _, ok := l.projectRoots[k]; ok {
			return k
		}
	}
	return ""
}
