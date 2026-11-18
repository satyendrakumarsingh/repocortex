package parse

import (
	"path"
	"regexp"
	"strings"
)

// CMake. function() and macro() are the definitions, and the inferred query reads
// both as functions; a command is invoked by name whatever defined it, so a macro
// and a function are one kind here. Command names are case-insensitive in CMake
// and are indexed as written.
//
// Every command is a call (queries.go), the builtins (`set`, `add_library`,
// `target_link_libraries`) among them: the resolver lists those and keeps them off
// the repository's names (infer/cmake.go).
//
// The import rows say which other files this one runs, because a function or macro
// defined there is a command from the point it runs on, in every scope:
//
//	include(cmake/helpers.cmake)         the file, beside this one and from the root
//	include("${CMAKE_CURRENT_LIST_DIR}/x.cmake")   beside this one
//	include("${CMAKE_SOURCE_DIR}/cmake/x.cmake")   from the root
//	include(FooModule)                   FooModule.cmake wherever the repository
//	                                     keeps it (CMAKE_MODULE_PATH is not read)
//	find_package(Foo)                    FindFoo.cmake, the same way
//	add_subdirectory(src)                src/CMakeLists.txt
//
// Rows are directives, as Julia's include is (lang_modules.go): ModuleInclude for a
// file with its exact repository path, CMakeModule for a module named by its stem.
// A variable the code does not define (`${FOO_DIR}/x.cmake`) is dropped from the
// path, and what follows it names the file by its end (CMakeIncludeEnd).
func init() {
	registerImportFix("cmake", cmakeImport)
}

// The directives of a CMake import row (lang_modules.go has the others, and
// ModuleInclude is a file at its exact repository path).
const (
	// CMakeModule: Path names a module (`Foo` for Foo.cmake) and not a file.
	CMakeModule = "module"
	// CMakeIncludeEnd: Path names a file by its end, because the directory it was
	// written under is a variable the code does not define.
	CMakeIncludeEnd = "include-end"
)

var (
	cmakeRootVars = map[string]bool{
		"CMAKE_SOURCE_DIR": true, "PROJECT_SOURCE_DIR": true,
	}
	cmakeHereVars = map[string]bool{
		"CMAKE_CURRENT_LIST_DIR": true, "CMAKE_CURRENT_SOURCE_DIR": true,
		"CMAKE_CURRENT_FUNCTION_LIST_DIR": true,
	}
	cmakeVarPrefix = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

func cmakeImport(filePath string, imp Import) []Import {
	command := strings.ToLower(imp.Alias)
	arg := strings.TrimSpace(imp.Path)
	imp.Alias = ""

	row := func(p, directive string) Import {
		next := imp
		next.Path, next.Alias = p, directive
		return next
	}
	dir := path.Dir(filePath)
	if dir == "." {
		dir = ""
	}

	if command == "find_package" {
		// The module-mode search: FindFoo.cmake on the module path. A package found
		// in config mode (FooConfig.cmake, an installed one) is not a file of the
		// repository.
		if !regexp.MustCompile(`^[A-Za-z0-9_+-]+$`).MatchString(arg) {
			return nil
		}
		return []Import{row("Find"+arg, CMakeModule)}
	}

	// The leading variable says what the rest is relative to.
	base := dir
	known := true
	plain := !strings.HasPrefix(arg, "${")
	for {
		m := cmakeVarPrefix.FindStringSubmatch(arg)
		if m == nil {
			break
		}
		arg = strings.TrimPrefix(arg[len(m[0]):], "/")
		switch {
		case cmakeRootVars[m[1]]:
			base = ""
		case cmakeHereVars[m[1]]:
			base = dir
		default:
			known = false
		}
	}
	if arg == "" || strings.Contains(arg, "${") || strings.Contains(arg, "$<") {
		return nil
	}
	if strings.ContainsAny(arg, "\"'") {
		return nil
	}

	if command == "add_subdirectory" {
		if !known {
			// `${FOO_DIR}/ext` names a directory by its end.
			return []Import{row(path.Clean(arg)+"/CMakeLists.txt", CMakeIncludeEnd)}
		}
		p := path.Join(base, arg)
		if p == "." || strings.HasPrefix(p, "..") || path.IsAbs(arg) {
			return nil
		}
		return []Import{row(p+"/CMakeLists.txt", ModuleInclude)}
	}

	// include(): a file, or a module named by its stem.
	if !strings.Contains(arg, "/") && !strings.HasSuffix(arg, ".cmake") && known && base == dir {
		return []Import{row(arg, CMakeModule)}
	}
	if !strings.HasSuffix(arg, ".cmake") && !strings.HasSuffix(arg, ".txt") {
		// `include(cmake/helpers)`: CMake looks for the file as written first.
		arg += ".cmake"
	}
	if !known {
		return []Import{row(path.Clean(arg), CMakeIncludeEnd)}
	}
	p := path.Join(base, arg)
	if p == "." || strings.HasPrefix(p, "..") || path.IsAbs(arg) {
		return nil
	}
	out := []Import{row(p, ModuleInclude)}
	if plain && base != "" && !strings.HasPrefix(arg, "./") && !strings.HasPrefix(arg, "../") {
		// A plain relative path is read from the directory of the CMakeLists being
		// processed, which an included file does not always share: from the root too.
		out = append(out, row(path.Clean(arg), ModuleInclude))
	}
	return out
}
