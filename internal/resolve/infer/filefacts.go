package infer

import (
	"path"

	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fileFacts are what the module resolvers read of a file's own rows (its
// directives, the package or module blocks around a line, the files its
// CMakeLists runs). They are built once per file, lazily, in ResolveFile and
// shared by every call of the file through call.facts. Before, each call
// rebuilt them: moduleDirectivesOf walked and split every import row, and
// perlPackageAt / juliaModuleAt walked every symbol, so a file of n calls cost
// n times its size. On a synthetic Perl repository moduleDirectivesOf alone
// was 8% of the resolve CPU.
//
// A fileFacts belongs to one file and one goroutine, like the ResolveFile call
// that makes it, and holds no locks.
type fileFacts struct {
	fr registry.FileRefs

	modDirs    *moduleDirectiveSet
	elixirDirs *elixirDirectiveSet
	cmake      *cmakeRunSet

	// packages are the file's package or module symbols, in file order: the
	// only symbols perlPackageAt and juliaModuleAt can answer with.
	packages     []registry.EnclosingSymbol
	packagesDone bool

	// modules are the modules the file's imports name; see allModules.
	modules     []string
	modulesDone bool

	perlHome   map[int]string
	juliaHome  map[int]string
	elixirHome map[int]string
}

func newFileFacts(fr registry.FileRefs) *fileFacts { return &fileFacts{fr: fr} }

// factsOf is the call's file facts, built on the spot for a call made outside
// ResolveFile (the renamed-import path), which has no shared set.
func (c call) factsOf() *fileFacts {
	if c.facts != nil {
		return c.facts
	}
	return newFileFacts(c.fr)
}

func (f *fileFacts) moduleDirectives() moduleDirectiveSet {
	if f.modDirs == nil {
		set := moduleDirectivesOf(f.fr.Imports)
		f.modDirs = &set
	}
	return *f.modDirs
}

// allModules are the modules the file's imports name, whatever they do with them
// (bring every name in, a list of them, a qualifier, or nothing but the qualified
// names): the modules a qualified call may be made on. A re-export and an include
// are not imports of the file.
func (f *fileFacts) allModules() []string {
	if !f.modulesDone {
		f.modulesDone = true
		seen := map[string]struct{}{}
		for _, imp := range f.fr.Imports {
			if imp.Alias == parse.ModuleExport || imp.Alias == parse.ModuleInclude {
				continue
			}
			if _, dup := seen[imp.Path]; !dup {
				seen[imp.Path] = struct{}{}
				f.modules = append(f.modules, imp.Path)
			}
		}
	}
	return f.modules
}

func (f *fileFacts) elixirDirectives() elixirDirectiveSet {
	if f.elixirDirs == nil {
		set := elixirDirectives(f.fr.Imports)
		f.elixirDirs = &set
	}
	return *f.elixirDirs
}

func (f *fileFacts) packageSymbols() []registry.EnclosingSymbol {
	if !f.packagesDone {
		f.packagesDone = true
		for _, s := range f.fr.Symbols {
			if s.Kind == string(model.KindPackage) {
				f.packages = append(f.packages, s)
			}
		}
	}
	return f.packages
}

// perlPackageAt is perlPackageAt (modules.go) answered once per line.
func (f *fileFacts) perlPackageAt(line int) string {
	if home, ok := f.perlHome[line]; ok {
		return home
	}
	home := perlPackageIn(f.fr, f.packageSymbols(), line)
	if f.perlHome == nil {
		f.perlHome = map[int]string{}
	}
	f.perlHome[line] = home
	return home
}

// juliaModuleAt is juliaModuleAt (modules.go) answered once per line.
func (f *fileFacts) juliaModuleAt(line int) string {
	if home, ok := f.juliaHome[line]; ok {
		return home
	}
	home := juliaModuleIn(f.fr, f.packageSymbols(), line)
	if f.juliaHome == nil {
		f.juliaHome = map[int]string{}
	}
	f.juliaHome[line] = home
	return home
}

// elixirEnclosingModule is elixirEnclosingModule (elixir.go) answered once per
// line.
func (f *fileFacts) elixirEnclosingModule(line int) string {
	if home, ok := f.elixirHome[line]; ok {
		return home
	}
	home := elixirEnclosingModule(f.fr, line)
	if f.elixirHome == nil {
		f.elixirHome = map[int]string{}
	}
	f.elixirHome[line] = home
	return home
}

// cmakeRunSet answers whether a file's own commands run a candidate file
// (include, add_subdirectory, include(module)) without looking at each row for
// each candidate, as the loop it replaced did.
type cmakeRunSet struct {
	exact   map[string]struct{} // include(path), add_subdirectory(path): the whole path
	ending  map[string]struct{} // paths a file may end with, after a "/"
	modules map[string]struct{} // include(Module): a .cmake file of that name
}

func (f *fileFacts) cmakeRunSet() *cmakeRunSet {
	if f.cmake != nil {
		return f.cmake
	}
	set := &cmakeRunSet{
		exact: map[string]struct{}{}, ending: map[string]struct{}{}, modules: map[string]struct{}{},
	}
	for _, imp := range f.fr.Imports {
		switch imp.Alias {
		case parse.ModuleInclude:
			set.exact[imp.Path] = struct{}{}
		case parse.CMakeIncludeEnd:
			set.exact[imp.Path] = struct{}{}
			set.ending[imp.Path] = struct{}{}
		case parse.CMakeModule:
			set.modules[imp.Path] = struct{}{}
		}
	}
	f.cmake = set
	return set
}

// runs reports whether the file at filePath is one of the files run.
func (s *cmakeRunSet) runs(filePath string) bool {
	if _, ok := s.exact[filePath]; ok {
		return true
	}
	if len(s.ending) > 0 {
		for i := 0; i < len(filePath); i++ {
			if filePath[i] != '/' {
				continue
			}
			if _, ok := s.ending[filePath[i+1:]]; ok {
				return true
			}
		}
	}
	if len(s.modules) > 0 && path.Ext(filePath) == ".cmake" {
		if _, ok := s.modules[fileBaseName(filePath)]; ok {
			return true
		}
	}
	return false
}
