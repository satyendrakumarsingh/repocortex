package parse

import (
	"path"
	"regexp"
	"strings"
)

// Bash (and POSIX sh): functions, aliases, and the variables a script sets at
// its top level.
//
// A function is recorded in all four spellings (`name() {}`, `function name
// {}`, `function name() {}` and a subshell body `name() ( ... )`), wherever it
// is written, nested ones and those inside `if` included.
//
// A variable is recorded when it is assigned at the top level of the file:
// `NAME=value`, `export NAME=value`, `declare [-flags] NAME[=value]`,
// `typeset` and `readonly NAME=value`. `readonly` and `declare -r` make a
// `constant`; everything else is a `variable`. Locals (`local`, and anything
// assigned inside a function, loop or conditional) are not recorded, and a
// bare `export NAME` re-exports a name that is declared elsewhere and is not
// recorded. A name assigned twice at the top level is recorded at each
// assignment. An append (`NAME+=value`, `NAME+=(...)`) changes a variable
// that is declared elsewhere and is not an assignment of its own, so it is not
// recorded.
//
// An `alias name=value` anywhere in the file is a `function`: it defines a
// command. The grammar gives the name together with its `=` (`ll=`), which
// bashName removes.
//
// A call is a command whose name is a plain word: `deploy --now`, `FOO=1 deploy`,
// the head of a pipeline or of an `if`. `$cmd` and `"$cmd"` name nothing and are
// not calls, and neither is the function an argument names, except where the
// command runs it (`run fetch x` in bats, `time fn`, `trap cleanup EXIT`).
// Which words are the shell's own (`echo`, `cd`, `git`) and not a function of
// the repository is the resolver's to say (corefunctions.go).
func init() {
	registerDefinitions("bash", `
(command name: (command_name (word) @name) @reference.call
  (#not-any-of? @name "source" "." "load"))
; A function handed to a command that runs it: bats's "run fetch x", "time fn",
; "trap cleanup EXIT". The tag is the word alone, so the call is bare.
(command (command_name (word) @_r) . (word) @name @reference.call
  (#any-of? @_r "run" "time" "trap")
  (#not-match? @name "^-"))

(function_definition name: (word) @name) @definition.function

(program (variable_assignment name: (variable_name) @name) @definition.variable
  (#not-match? @definition.variable "^[^=]*[+]="))
(program (variable_assignments (variable_assignment name: (variable_name) @name) @definition.variable
  (#not-match? @definition.variable "^[^=]*[+]=")))

(program (declaration_command (variable_assignment name: (variable_name) @name)) @definition.constant
  (#match? @definition.constant "^(readonly\\b|(declare|typeset)\\s+(-\\S+\\s+)*-[A-Za-z]*r)"))
(program (declaration_command (variable_assignment name: (variable_name) @name)) @definition.variable
  (#not-match? @definition.variable "^(readonly\\b|local\\b|(declare|typeset)\\s+(-\\S+\\s+)*-[A-Za-z]*r)"))
(program (declaration_command (variable_name) @name) @definition.variable
  (#match? @definition.variable "^(declare|typeset)\\b"))

(command (command_name (word) @_c) . (concatenation . (word) @name)) @definition.function
  (#eq? @_c "alias")
(command (command_name (word) @_c) . (word) @name) @definition.function
  (#eq? @_c "alias")
  (#match? @name "^[^=-]+=")
`, true)
	registerNameFix("bash", bashName)
}

// bashName cuts an alias name at its `=`. Other captured names (function and
// variable names) cannot contain one.
func bashName(name string) string {
	if i := strings.IndexByte(name, '='); i > 0 {
		return name[:i]
	}
	return name
}

func init() {
	registerImportFix("bash", bashImport)
}

// bashSourceDir matches the spellings of "the directory this script is in" that
// open a sourced path. Anything else that starts with a variable is somebody's
// environment and is skipped.
//
//	"$(dirname "$0")/x.sh"            "$(dirname "${BASH_SOURCE[0]}")/x.sh"
//	"$(cd "$(dirname "$0")" && pwd)/x.sh"
//	"${BASH_SOURCE%/*}/x.sh"          "${0%/*}/x.sh"
//	"$SCRIPT_DIR/x.sh"                "${SCRIPT_DIR}/x.sh"
//	"$BATS_TEST_DIRNAME/x.bash"       (bats: the test file's directory)
var bashSourceDir = regexp.MustCompile(`^"?(?:` +
	`\$\(\s*dirname\s+"?(?:\$0|\$\{?BASH_SOURCE(?:\[[0-9@*]\])?\}?)"?\s*\)` +
	`|\$\(\s*cd\s+"?\$\(\s*dirname\s+"?(?:\$0|\$\{?BASH_SOURCE(?:\[[0-9@*]\])?\}?)"?\s*\)"?\s*&&\s*pwd\s*\)` +
	`|\$\{(?:BASH_SOURCE(?:\[[0-9@*]\])?|0)%/\*\}` +
	`|\$\{?(?i:script_?dir|bats_test_dirname)\}?` +
	`)"?(?:/|$)`)

// bashImport turns the word after `source` or `.` into the repository path of
// the file it names, without the extension, which is what the resolver compares
// with a file's.
//
//	./lib/x.sh, lib/x.sh, ../x.sh   relative to the script AND, as written, to
//	                                the working directory (a script is usually
//	                                run from the repository root)
//	"$SCRIPT_DIR/x.sh" and the other spellings of the script's own directory
//	                                relative to the script
//	"$HOME/x.sh", /etc/profile, ~/.bashrc, "$LIB/x.sh"
//	                                no file of this repository: dropped
//
// Both readings of a relative path are recorded, as for an Objective-C include:
// the resolver prefers the longer match, so the one that names a file that
// exists is the one that counts and the other names nothing.
func bashImport(filePath string, imp Import) []Import {
	command := imp.Alias
	imp.Alias = ""
	if command == "load" && !strings.HasSuffix(filePath, ".bats") {
		return nil
	}
	p := strings.TrimSpace(imp.Path)
	// bats resolves a `load` against the test file's directory alone.
	fromScript := command == "load"
	if loc := bashSourceDir.FindStringIndex(p); loc != nil {
		p = strings.TrimPrefix(p[loc[1]:], "/")
		fromScript = true
	}
	p = unquote(p)
	if p == "" || strings.ContainsAny(p, "$`*?~ \t(){}") || strings.HasPrefix(p, "/") {
		return nil
	}
	ext := path.Ext(p)
	if ext == ".sh" || ext == ".bash" || ext == ".zsh" || ext == ".ksh" {
		p = strings.TrimSuffix(p, ext)
	}
	dir := path.Dir(filePath)
	var out []Import
	add := func(cleaned string) {
		if cleaned == "" || cleaned == "." || strings.HasPrefix(cleaned, "..") {
			return
		}
		next := imp
		next.Path = cleaned
		out = append(out, next)
	}
	add(path.Join(dir, p))
	if !fromScript {
		add(path.Clean(p))
	}
	return out
}
