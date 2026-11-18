package parse

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ReScript: lets (functions and values), types, modules, module types,
// externals and exceptions, at the top of the file and in module bodies.
//
// A let is a function when its value is a `function` node, or when it is a
// signature (`let f: a => b`, in a module type or an interface file) whose
// type is a function type. Every other let is a variable. The two sets are
// written as disjoint patterns on purpose: when a declaration is tagged both
// ways the kind ranked more specific wins, and `function` ranks lowest, so a
// catch-all variable pattern would turn every function into a variable.
//
// Local lets inside a function body are not definitions, so every pattern is
// anchored to the source file or to the block that is a module's body.
//
// A file `Foo.res` (or its interface `Foo.resi`) IS the module `Foo`, global to the
// project with no import, and a `module M = { ... }` inside it is a module of its
// own, so each definition is qualified by the module blocks around it (see
// rescriptOwners): the resolver reads the file's module from the path and the
// nested ones from the name. The imports are the statements that bring a module's
// names in bare or rename it: `open Foo`, `include Foo` and `module X = Foo.Bar`
// (rescriptImport).
//
// Calls: `f(x)`, `Foo.f(x)` and `Foo.Bar.f(x)`, and the right of a pipe, which is
// a call with the left as its first argument: `x->f`, `x->Foo.f` and `x->Foo.f(y)`.
// A call node begins at its module path, so the text before the name is what
// shape.go reads the module from; the right side of a pipe is tagged by itself, so
// that the pipe's left side is not read as a module.
func init() {
	scopes := []string{
		"(source_file %s)",
		"(module_binding (block %s))",
	}
	// Node types a non-function let value can have. A value of any other
	// type is missed rather than guessed at.
	values := "[(number) (string) (template_string) (list) (array) (record) (tuple) (unit) (call_expression) (value_identifier) (member_expression) (binary_expression) (variant) (true) (false) (if_expression) (switch_expression) (pipe_expression) (parenthesized_expression) (ternary_expression) (unary_expression) (block) (polyvar) (character)]"
	plainTypes := "[(type_identifier) (generic_type) (tuple_type) (unit_type) (module_identifier_path) (polyvar_type)]"

	decls := []string{
		// let
		"(let_declaration (let_binding . (value_identifier) @name (function)) @definition.function)",
		"(let_declaration (let_binding . (value_identifier) @name (type_annotation (function_type))) @definition.function)",
		"(let_declaration (let_binding . (value_identifier) @name " + values + ") @definition.variable)",
		"(let_declaration (let_binding . (value_identifier) @name (type_annotation " + plainTypes + ") .) @definition.variable)",
		// type
		"(type_declaration (type_binding . (type_identifier) @name [(variant_type) (record_type)]) @definition.class)",
		"(type_declaration (type_binding . (type_identifier) @name .) @definition.class)",
		"(type_declaration (type_binding . (type_identifier) @name (type_parameters) .) @definition.class)",
		"(type_declaration (type_binding . (type_identifier) @name [(type_identifier) (generic_type) (tuple_type) (function_type) (unit_type) (polyvar_type)]) @definition.type_alias)",
		"(type_declaration (type_binding . (type_identifier) @name (type_parameters) [(type_identifier) (generic_type) (tuple_type) (function_type) (unit_type) (polyvar_type)]) @definition.type_alias)",
		// module, module type, functor, alias
		"(module_declaration (module_binding . (module_identifier) @name) @definition.module)",
		// external and exception
		"(external_declaration . (value_identifier) @name) @definition.function",
		"(exception_declaration . (variant_identifier) @name) @definition.class",
	}
	var b strings.Builder
	for _, scope := range scopes {
		for _, d := range decls {
			b.WriteString(strings.Replace(scope, "%s", d, 1))
			b.WriteByte('\n')
		}
	}
	b.WriteString(rescriptCalls)
	registerDefinitions("rescript", b.String(), true)
	definitionRefiners["rescript"] = rescriptOwners
	registerImportFix("rescript", rescriptImport)
}

const rescriptCalls = `
(call_expression function: (value_identifier) @name) @reference.call
(call_expression function: (member_expression property: (property_identifier) @name)) @reference.call
(call_expression function: (value_identifier_path (value_identifier) @name .)) @reference.call
(pipe_expression (value_identifier) @name @reference.call .)
(pipe_expression (value_identifier_path (value_identifier) @name .) @reference.call .)
(pipe_expression (member_expression property: (property_identifier) @name) @reference.call .)
`

// rescriptOwners says which modules every definition is in: the names of the
// `module M = { ... }` blocks around it, outermost first, joined with dots. The
// file's own module is not in the list: it is the file's name.
func rescriptOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		isModule := kindFromTag(tag.Kind) == model.KindPackage
		var chain []string
		for p := n; p != nil; p = p.Parent() {
			if p.Type(lang) != "module_binding" {
				continue
			}
			var name *ts.Node
			for i := 0; i < p.ChildCount() && name == nil; i++ {
				if c := p.Child(i); c != nil && c.Type(lang) == "module_identifier" {
					name = c
				}
			}
			if name == nil || isModule && name.StartByte() == tag.NameRange.StartByte {
				continue
			}
			chain = append([]string{name.Text(src)}, chain...)
		}
		if len(chain) == 0 {
			continue
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		fixes[tag.NameRange.StartByte] = definitionFix{owner: strings.Join(chain, ".")}
	}
	return fixes
}

// rescriptImport reads a statement that brings a module into the file.
//
//	open Foo.Bar   include Foo.Bar    Foo.Bar, import     the module's names are in scope bare
//	module X = Foo.Bar                Foo.Bar, alias:X    `X.f` names Foo.Bar's f
//
// A local open (`Foo.(f(x))`, an `open` inside a function body) is not read: its
// names are in scope in one expression and not in the file.
func rescriptImport(_ string, imp Import) []Import {
	text := strings.TrimSpace(imp.Path)
	if m := rescriptOpenStmt.FindStringSubmatch(text); m != nil {
		imp.Path, imp.Alias = m[1], ModuleImportAll
		return []Import{imp}
	}
	if m := rescriptAliasStmt.FindStringSubmatch(text); m != nil {
		imp.Path, imp.Alias = m[2], ModuleAliasPrefix+m[1]
		return []Import{imp}
	}
	return nil
}

var (
	rescriptOpenStmt  = regexp.MustCompile(`^(?:open|include)!?\s+([A-Z][A-Za-z0-9_']*(?:\.[A-Z][A-Za-z0-9_']*)*)\s*;?$`)
	rescriptAliasStmt = regexp.MustCompile(`^module\s+([A-Z][A-Za-z0-9_']*)\s*=\s*([A-Z][A-Za-z0-9_']*(?:\.[A-Z][A-Za-z0-9_']*)*)\s*;?$`)
)
