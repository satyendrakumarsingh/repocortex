package parse

import (
	"path"
	"regexp"
	"strings"
)

// R has no declaration syntax: a function is a value assigned to a name, and
// an S4 or reference class is a call to setClass / setRefClass. The grammar's
// `binary_operator` has the operator as an anonymous node, so it is matched by
// field and literal.
//
// Only file-level assignments are definitions; a function assigned inside
// another function's body is a local. Non-function assignments (`x <- 10`)
// are not extracted: in R they are data or script state, not API.
//
// A call is `f(x)`, `pkg::f(x)` (or `pkg:::f(x)`) and `obj$f(x)`, the last on
// whatever the left side is: shape.go reads `$` as the member operator for R.
// A function passed as a value (`sapply(xs, f)`) is not a call.
func init() {
	registerDefinitions("r", `
(call function: (identifier) @name) @reference.call
(call function: (namespace_operator lhs: (_) rhs: (identifier) @name)) @reference.call
(call function: (extract_operator rhs: (identifier) @name)) @reference.call

; name <- function(...)  /  name = function(...)  /  name <<- function(...)
(program (binary_operator lhs: (identifier) @name operator: ["<-" "=" "<<-"] rhs: (function_definition)) @definition.function)
(program (binary_operator lhs: (string (string_content) @name) operator: ["<-" "=" "<<-"] rhs: (function_definition)) @definition.function)
; function(...) ... -> name
(program (binary_operator lhs: (function_definition) operator: "->" rhs: (identifier) @name) @definition.function)

; the same inside a top-level if / else block
(program (if_statement consequence: (braced_expression (binary_operator lhs: (identifier) @name operator: ["<-" "=" "<<-"] rhs: (function_definition)) @definition.function)))

; setClass("Name", ...), setRefClass("Name", ...), R6Class("Name", ...), with or without "X <-"
(program (call function: (identifier) @_c arguments: (arguments . (argument value: (string (string_content) @name)))) @definition.class
  (#any-of? @_c "setClass" "setRefClass" "R6Class"))
(program (binary_operator operator: ["<-" "=" "<<-"] rhs: (call function: (identifier) @_c arguments: (arguments . (argument value: (string (string_content) @name))))) @definition.class
  (#any-of? @_c "setClass" "setRefClass" "R6Class"))

; setGeneric("name", ...) is a generic function; setMethod("name", signature, ...) a method
(program (call function: (identifier) @_c arguments: (arguments . (argument value: (string (string_content) @name)))) @definition.function
  (#eq? @_c "setGeneric"))
(program (call function: (identifier) @_c arguments: (arguments . (argument value: (string (string_content) @name)))) @definition.method
  (#eq? @_c "setMethod"))
; methods of a reference class: setRefClass("X", methods = list(name = function(...) ...)),
; and R6: R6Class("X", public = list(name = function(...) ...)), also private and active
(program (binary_operator operator: ["<-" "=" "<<-"] rhs: (call function: (identifier) @_c arguments: (arguments (argument name: (identifier) @_k value: (call function: (identifier) @_l arguments: (arguments (argument name: (identifier) @name value: (function_definition)) @definition.method))))))
  (#any-of? @_c "setRefClass" "R6Class") (#any-of? @_k "methods" "public" "private" "active") (#eq? @_l "list"))
(program (call function: (identifier) @_c arguments: (arguments (argument name: (identifier) @_k value: (call function: (identifier) @_l arguments: (arguments (argument name: (identifier) @name value: (function_definition)) @definition.method)))))
  (#any-of? @_c "setRefClass" "R6Class") (#any-of? @_k "methods" "public" "private" "active") (#eq? @_l "list"))
`, true)
}

func init() {
	registerImportFix("r", rImport)
}

// rBasePackages ship with R and are attached to every session or are loaded by
// name from its own library. `library(utils)` names no file of the repository,
// and a repository's own utils.R is not what it loads.
var rBasePackages = map[string]bool{
	"base": true, "stats": true, "utils": true, "methods": true, "graphics": true,
	"grDevices": true, "datasets": true, "tools": true, "parallel": true,
	"compiler": true, "grid": true, "splines": true, "stats4": true, "tcltk": true,
}

var (
	rPackageName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.]*$`)
	rPathCall    = regexp.MustCompile(`^(?:file\.path|here(?:::here)?)\s*\((.*)\)$`)
	rStringArg   = regexp.MustCompile(`^\s*(?:"([^"]*)"|'([^']*)')\s*(?:,|$)`)
)

// rImport turns the first argument of source() and of library() into what the
// resolver compares with a file.
//
//	source("R/util.R")            the file, as written and relative to the script
//	source(file.path("R", "u.R")) likewise; so is here("R", "u.R")
//	sys.source("util.R", env)     likewise
//	library(pkg), library("pkg"), require(pkg), requireNamespace("pkg")
//	                              the package's name: a package of the
//	                              repository is told from CRAN's by whether a
//	                              file of that name exists (see rModule)
//
// A path built from anything but string literals (`paste0(dir, "/u.R")`), an
// absolute or home path, and a package that ships with R name no file of this
// repository and are dropped. The extension is dropped because the resolver
// compares stems, and both readings of a relative path are recorded for the
// reason given at bashImport.
func rImport(filePath string, imp Import) []Import {
	command := imp.Alias
	imp.Alias = ""
	text := strings.TrimSpace(imp.Path)
	switch command {
	case "library", "require", "requireNamespace", "loadNamespace", "attachNamespace":
		name := unquote(text)
		if !rPackageName.MatchString(name) || rBasePackages[name] {
			return nil
		}
		imp.Path = name
		return []Import{imp}
	}

	// source() and sys.source().
	var p string
	if m := rPathCall.FindStringSubmatch(text); m != nil {
		var parts []string
		for rest := strings.TrimSpace(m[1]); rest != ""; {
			a := rStringArg.FindStringSubmatch(rest)
			if a == nil {
				return nil
			}
			parts = append(parts, a[1]+a[2])
			rest = strings.TrimSpace(rest[len(a[0]):])
		}
		p = path.Join(parts...)
	} else if ext := path.Ext(text); (ext == ".R" || ext == ".r") && !strings.ContainsAny(text, "()") {
		// A string literal: extractImports has already removed its quotes, so a
		// variable (`source(script)`) is told from it by the extension a script
		// file has.
		p = text
	} else {
		return nil
	}
	if p == "" || strings.ContainsAny(p, "$~\\") || strings.HasPrefix(p, "/") || strings.Contains(p, "://") {
		return nil
	}
	if ext := path.Ext(p); ext == ".R" || ext == ".r" {
		p = strings.TrimSuffix(p, ext)
	}
	var out []Import
	add := func(cleaned string) {
		if cleaned == "" || cleaned == "." || strings.HasPrefix(cleaned, "..") {
			return
		}
		next := imp
		next.Path = cleaned
		out = append(out, next)
	}
	add(path.Join(path.Dir(filePath), p))
	add(path.Clean(p))
	return out
}
