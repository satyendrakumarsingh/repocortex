package parse

import (
	"path"
	"regexp"
	"strings"
)

// Nim. What the index holds, see fixtures/langs/nim/expected.tsv.
//
// The inferred query takes a routine only when its name is a bare identifier.
// A name followed by the export marker (`proc add*`) is an exported_symbol
// node and a backquoted operator (`proc `+`*`) is an accent_quoted node, so
// the public API of every library was missing; iterators, templates, macros,
// converters, ref and distinct types, aliases and let/var were never read. The
// inferred query is replaced.
//
//   - proc, func, iterator, template, macro and converter are functions, at
//     any depth (a proc inside a proc is a named routine); method is a method.
//     Forward declarations (a proc header with no body) count as well;
//   - in a type section, object, enum, tuple, distinct, `ref object` and
//     variant types are classes, a concept is an interface, and everything
//     else (`T = seq[int]`, `proc (x: int)`, `ptr int`, `ref Foo`) is a
//     type_alias;
//   - const NAME is a constant, let and var NAME are variables. These and type
//     sections are read at the top level of the file and in the branches of a
//     top-level `when`; inside a routine they are locals and are skipped;
//   - fields, enum members and parameters are not definitions.
//
// An exported name's `*` is not part of the name; a backquoted operator is
// named by its symbol. A name written without the marker is the module's own and
// is flagged private (private.go): the resolver does not bind a call in another
// file to it.
//
// A module is a file. `import a/b, c as d`, `from a import f, g`, `include x` and
// `export a` are read from the statement's text (nimImport): the paths are the
// module's name as written, without extension, and a path relative to the
// importing file (`./x`, `../lib/x`) is rewritten to the repository path of the
// file it names, as Zig's and Dart's are.
//
// Calls: `f(x)`, `x.f(y)` (method-call syntax: the first argument written first,
// so the text before the name is the receiver) and `f x` (command syntax), which
// the grammar reads as the same `call` node, and `T(a: 1)`, which constructs.
func init() {
	registerDefinitions("nim", nimDefinitions()+nimCalls, true)
	registerImportFix("nim", nimImport)
}

const nimCalls = `
(call . (identifier) @name) @reference.call
(call . (dot_expression (identifier) @name .)) @reference.call
(call . (bracket_expression . (identifier) @name)) @reference.call
`

// nimImport turns an import, from-import, include or export statement into rows.
//
//	import a/b                   a/b, import                every exported name of a/b is in scope,
//	                                                        and `b.f` names a/b's f
//	import a/b as x              a/b, alias:x;import        `x.f` as well
//	import a/[b, c]              a/b and a/c
//	import a except f, g         a, hiding:f,g
//	from a import f, g           a, import:f,g              the listed names only
//	include x                    x, include                 x's definitions join this file
//	export x                     x, export                  whoever imports this file reaches x's names
//
// A path relative to the file (`./x`, `../lib/x`) is rewritten to the repository
// path of the file it names, without extension. A module of the standard library
// (`std/...`) or a package (`pkg/...`) is no file of the repository: dropped.
func nimImport(filePath string, imp Import) []Import {
	text := nimStatement(imp.Path)
	var rows []Import
	add := func(module, alias string) {
		if module = nimModulePath(filePath, module); module != "" {
			rows = append(rows, Import{Path: module, Alias: alias, Range: imp.Range})
		}
	}
	switch {
	case strings.HasPrefix(text, "from "):
		m := nimFromStmt.FindStringSubmatch(text)
		if m == nil {
			return nil
		}
		var names []string
		for _, n := range nimSplitTop(m[2]) {
			// A renamed name (`f as g`) is called by another name here.
			if f := strings.Fields(n); len(f) == 1 && nimIdent.MatchString(f[0]) && f[0] != "nil" {
				names = append(names, f[0])
			}
		}
		if len(names) > 0 {
			add(m[1], ModuleImportPrefix+strings.Join(names, ","))
		}
	case strings.HasPrefix(text, "include "):
		for _, item := range nimSplitTop(strings.TrimPrefix(text, "include ")) {
			add(item, ModuleInclude)
		}
	case strings.HasPrefix(text, "export "):
		for _, item := range nimSplitTop(strings.TrimPrefix(text, "export ")) {
			if !strings.Contains(item, " ") {
				add(item, ModuleExport)
			}
		}
	case strings.HasPrefix(text, "import "):
		body := strings.TrimPrefix(text, "import ")
		if i := strings.Index(body, " except "); i >= 0 {
			hidden := strings.Join(strings.Fields(strings.ReplaceAll(body[i+len(" except "):], ",", " ")), ",")
			add(strings.TrimSpace(body[:i]), ModuleHidingPrefix+hidden)
			break
		}
		for _, item := range nimSplitTop(body) {
			mod, alias := item, ""
			if i := strings.Index(item, " as "); i >= 0 {
				mod, alias = strings.TrimSpace(item[:i]), strings.TrimSpace(item[i+len(" as "):])
			}
			for _, one := range nimExpandGroup(mod) {
				if alias != "" {
					add(one, moduleDirectives(ModuleAliasPrefix+alias, ModuleImportAll))
				} else {
					add(one, ModuleImportAll)
				}
			}
		}
	}
	return rows
}

var (
	nimFromStmt = regexp.MustCompile(`^from\s+(\S+)\s+import\s+(.*)$`)
	nimIdent    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// nimStatement is a statement's text with comments dropped and its white space
// folded to single spaces.
func nimStatement(text string) string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = l[:i]
		}
		lines = append(lines, l)
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// nimSplitTop splits a list at the commas that are not inside brackets.
func nimSplitTop(list string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(list[start:i]))
				start = i + 1
			}
		}
	}
	if last := strings.TrimSpace(list[start:]); last != "" {
		out = append(out, last)
	}
	return out
}

// nimExpandGroup expands `a/[b, c]` to a/b and a/c; any other item is itself.
func nimExpandGroup(item string) []string {
	i := strings.Index(item, "[")
	if i < 0 || !strings.HasSuffix(item, "]") {
		return []string{item}
	}
	var out []string
	for _, member := range nimSplitTop(item[i+1 : len(item)-1]) {
		out = append(out, item[:i]+member)
	}
	return out
}

// nimModulePath is the name a file's import stores for a module: the path as
// written less its extension, or for a relative one the repository path of the
// file it names. "" when it names no file of the repository.
func nimModulePath(filePath, module string) string {
	module = strings.Trim(strings.TrimSpace(module), `"`)
	module = strings.TrimSuffix(module, ".nim")
	if module == "" || strings.ContainsAny(module, " \t[]()") {
		return ""
	}
	if strings.HasPrefix(module, "./") || strings.HasPrefix(module, "../") {
		module = path.Join(path.Dir(filePath), module)
		if module == "" || module == "." || strings.HasPrefix(module, "..") {
			return ""
		}
		return module
	}
	if first, _, _ := strings.Cut(module, "/"); first == "std" || first == "pkg" {
		return ""
	}
	return path.Clean(module)
}

// nimName matches the name position of a routine, type or variable.
const nimName = `[
    (identifier) @name
    (exported_symbol . (identifier) @name)
    (accent_quoted . (identifier) @name .)
    (exported_symbol (accent_quoted . (identifier) @name .))
  ]`

func nimDefinitions() string {
	var b strings.Builder

	for _, r := range []struct{ node, kind string }{
		{"proc_declaration", "function"},
		{"func_declaration", "function"},
		{"method_declaration", "method"},
		{"iterator_declaration", "function"},
		{"template_declaration", "function"},
		{"macro_declaration", "function"},
		{"converter_declaration", "function"},
	} {
		b.WriteString("(" + r.node + " . " + nimName + ") @definition." + r.kind + "\n")
	}

	// Type, constant, let and var: the top level and the branches of `when`.
	where := []string{
		"(source_file %s)",
		"(source_file (when (statement_list %s)))",
		"(source_file (when (elif_branch (statement_list %s))))",
		"(source_file (when (else_branch (statement_list %s))))",
	}
	typeName := "(type_symbol_declaration . " + nimName + ")"
	types := []struct{ rhs, kind string }{
		{"(object_declaration)", "class"},
		{"(enum_declaration)", "class"},
		{"(distinct_type)", "class"},
		{"(ref_type (object_declaration))", "class"},
		{"(type_expression (tuple_type))", "class"},
		{"(concept_declaration)", "interface"},
		{"(pointer_type)", "type_alias"},
		{"(ref_type (type_expression))", "type_alias"},
		{"(type_expression [(identifier) (bracket_expression) (dot_expression) (proc_type) (tuple_construction) (infix_expression) (call) (prefix_expression)])", "type_alias"},
	}
	for _, w := range where {
		for _, t := range types {
			decl := "(type_section (type_declaration " + typeName + " " + t.rhs + ") @definition." + t.kind + ")"
			b.WriteString(strings.Replace(w, "%s", decl, 1) + "\n")
		}
		for _, v := range []struct{ section, kind string }{
			{"const_section", "constant"},
			{"let_section", "variable"},
			{"var_section", "variable"},
		} {
			decl := "(" + v.section + " (variable_declaration (symbol_declaration_list (symbol_declaration " + nimName + "))) @definition." + v.kind + ")"
			b.WriteString(strings.Replace(w, "%s", decl, 1) + "\n")
		}
	}
	return b.String()
}
