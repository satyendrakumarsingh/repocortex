package infer

import (
	"path"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # CMake: a command is global once the file that defines it has run
//
// `function(my_helper)` and `macro(my_macro)` define commands, and a command is
// called by its name, with nothing to qualify it: not by the directory it was
// written in, not by the file. CMake keeps the table of commands for the whole
// run, so a command defined in a file that ran earlier is callable from every
// later one (a variable set in a directory is not: that is scoped, and a command is
// not). What decides whether `my_helper(x)` reaches the definition is therefore
// whether the file that has it ran before the call, and what a repository can say
// of that is which files run which:
//
//	include(cmake/helpers.cmake)    runs that file, in this scope
//	include(FooModule)              runs FooModule.cmake from CMAKE_MODULE_PATH
//	find_package(Foo)               runs FindFoo.cmake from the same path
//	add_subdirectory(src)           runs src/CMakeLists.txt
//
// and, one more, that the directories above ran first: a CMakeLists.txt is entered
// from the one of its parent (or from a project that was told to), so the commands
// the parent had defined by then are in scope in the child. The parse says which
// files a file runs (parse.cmakeImport); these rules say what that reaches.
//
// What a call reaches, in the order the program asks:
//
//	cmd(...)    a command the file itself defines; one defined in a file this one
//	            runs; the program's own (corefunctions.go), which a definition of the
//	            repository does not replace; one defined in the CMakeLists.txt of a
//	            directory above, at `medium`: the order they ran in is not read; and
//	            the one command of that name in the repository, at `low`
//
// The last is for the usual layout, where the top-level file includes a directory of
// helpers and every subdirectory calls them: a file that was run by a file above this
// one is not something this one's own rows can name. Not read: the files that a file
// this one runs runs in turn, and CMAKE_MODULE_PATH, so a module is looked for by its
// name wherever the repository keeps it. Command names are not case-sensitive; a call is bound to a definition
// spelled as it is.
func (r *Resolver) callCMake(c call, _ *Stats) decision {
	if c.ref.ReceiverKind != model.ReceiverBare {
		return unresolvedCall
	}
	var here, ran, above []registry.SymbolIndexEntry
	runs := c.factsOf().cmakeRunSet()
	for _, s := range c.cands {
		if !isCallableKind(s.Kind) {
			continue
		}
		switch {
		case s.FileID == c.fr.FileID:
			here = append(here, s)
		case runs.runs(s.FilePath):
			ran = append(ran, s)
		case cmakeAbove(c.fr.Path, s.FilePath):
			above = append(above, s)
		}
	}
	if len(here) > 0 {
		return pickCMake(here, "defined in the same file", model.ConfidenceHigh)
	}
	if len(ran) > 0 {
		return pickCMake(ran, "defined in a file this one runs (include or add_subdirectory)", model.ConfidenceHigh)
	}
	if c.spec.isCoreFunction(c.ref.Name) {
		return externalCall
	}
	if len(above) > 0 {
		// The nearest directory above that defines it.
		deepest := 0
		for _, s := range above {
			if n := len(strings.Split(path.Dir(s.FilePath), "/")); n > deepest {
				deepest = n
			}
		}
		var nearest []registry.SymbolIndexEntry
		for _, s := range above {
			if len(strings.Split(path.Dir(s.FilePath), "/")) == deepest {
				nearest = append(nearest, s)
			}
		}
		return pickCMake(nearest, "defined in the CMakeLists.txt of a directory above, which runs first", model.ConfidenceMedium)
	}
	if r.specs.IsGenericName(c.ref.Name) {
		return unresolvedCall
	}
	var only []registry.SymbolIndexEntry
	for _, s := range c.cands {
		if isCallableKind(s.Kind) {
			only = append(only, s)
		}
	}
	if len(only) == 1 {
		return bound(candidate{only[0], model.ConfidenceLow, "the only command of that name in the repository, no include to say it ran first"})
	}
	return unresolvedCall
}

// cmakeAbove reports whether a file is the CMakeLists.txt of the directory a
// caller's file is in, or of one above it.
func cmakeAbove(callerPath, filePath string) bool {
	if path.Base(filePath) != "CMakeLists.txt" {
		return false
	}
	dir, caller := path.Dir(filePath), path.Dir(callerPath)
	return dir == "." || caller == dir || strings.HasPrefix(caller, dir+"/")
}

// pickCMake chooses among definitions of one command. Two files that define the
// same command are two definitions that ran in some order, and the later is the
// one in force: the order is not read, so it is not guessed.
func pickCMake(among []registry.SymbolIndexEntry, reason string, confidence model.Confidence) decision {
	files := map[int64]struct{}{}
	for _, s := range among {
		files[s.FileID] = struct{}{}
	}
	if len(files) > 1 {
		return ambiguousCall
	}
	if len(among) == 1 {
		return bound(candidate{among[0], confidence, reason})
	}
	sortCandidates(among)
	return bound(candidate{among[0], model.ConfidenceLow, reason + " (several definitions of the name)"})
}
