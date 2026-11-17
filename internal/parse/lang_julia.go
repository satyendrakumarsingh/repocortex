package parse

import (
	"path"
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Julia. What the index holds, see fixtures/langs/julia/expected.tsv.
//
// The inferred query takes only `function f(args)` with a bare name. It drops
// a qualified name (`function Base.get(...)`), a constructor
// (`Dict{K,V}(...)`), a `where` clause, a return type, the short form
// `f(x) = ...`, and every type that is not a plain struct, so it is replaced.
//
//   - module and baremodule are packages;
//   - struct, mutable struct, abstract type and primitive type are classes,
//     named without their type parameters or supertype; `@enum Color a b`
//     is a class named Color (its members are not extracted);
//   - function f(...), function Base.get(...), function Base.:+(...),
//     function Dict{K,V}(...), each with or without `where` and a return type,
//     and `function f end`, are functions, at any depth;
//   - a macro is a function named without the @;
//   - the short form `f(x) = ...` (same name shapes, `+(a, b) = ...`
//     included, also behind a macro such as @inline) is a function when it
//     stands at the top level of a file, a module or a struct, or in the
//     branches of an `if` (or `@static if`) there. Inside a function body it is a local closure and is not extracted. An inner
//     constructor in a struct is such a method;
//   - const NAME = ... (also `const global`) is a constant; const NAME{T} = ... and const NAME =
//     Union{...} (a right-hand side that is a parametrised type) are
//     type_alias;
//   - an operator method on a qualified name is named by the operator, in all
//     the spellings: `Base.:+(a, b)`, `Base.:(==)(a, b)`;
//   - when the grammar gives up on a file, the short forms and the headers
//     that are direct children of the ERROR at its root are taken (see the
//     recovery patterns at the end of juliaDefinitions);
//   - plain assignments, global declarations and `(x::T)(args) = ...`
//     functors are not definitions.
//
// Calls: `f(x)`, `M.f(x)`, `A.B.f(x)` and a constructor `Dict{K,V}(...)`. The call node
// begins at the receiver, so the text before the name is how shape.go reads what the
// call was made on. The left side of a method definition is a call to the grammar
// (`f(x) = ...`, `f(x::T) where T = ...`) and is not one: a signature is excluded by
// the pattern and the short form by shape.go (juliaDefinitionHead).
//
// Every definition is qualified by the modules around it (`Shop.total` for `total` in
// `module Shop`, see juliaOwners), because a call reaches a function through the
// module it is in. A file with no module of its own has names with no owner: it is
// part of whichever module `include`s it.
func init() {
	registerDefinitions("julia", juliaCalls+juliaDefinitions(), true)
	definitionRefiners["julia"] = juliaOwners
	registerImportFix("julia", juliaImport)
}

const juliaCalls = `
((call_expression . (identifier) @name) @reference.call
  (#not-has-ancestor? @reference.call signature))
((call_expression . (field_expression (_) . (identifier) @name .)) @reference.call
  (#not-has-ancestor? @reference.call signature))
((call_expression . (parametrized_type_expression . (identifier) @name)) @reference.call
  (#not-has-ancestor? @reference.call signature))
`

// juliaCallName matches the call-shaped left side of a method definition and
// captures its name.
const juliaCallName = `(call_expression . [
    (identifier) @name
    (operator) @name
    (field_expression (_) . (identifier) @name .)
    (field_expression (quote_expression (operator) @name))
    (field_expression (quote_expression (parenthesized_expression (operator) @name)))
    (parametrized_type_expression . (identifier) @name)
  ])`

func juliaDefinitions() string {
	// A method's left side is the call, optionally with a return type or a
	// where clause around it, in either order.
	shapes := []string{
		juliaCallName,
		"(typed_expression " + juliaCallName + ")",
		"(where_expression " + juliaCallName + ")",
		"(where_expression (typed_expression " + juliaCallName + "))",
		"(typed_expression (where_expression " + juliaCallName + "))",
	}

	var b strings.Builder
	b.WriteString(`
(module_definition . (identifier) @name) @definition.module

(struct_definition (type_head . (identifier) @name)) @definition.class
(struct_definition (type_head . (parametrized_type_expression . (identifier) @name))) @definition.class
(struct_definition (type_head . (binary_expression . (identifier) @name))) @definition.class
(struct_definition (type_head . (binary_expression . (parametrized_type_expression . (identifier) @name)))) @definition.class
(abstract_definition (type_head . (identifier) @name)) @definition.class
(abstract_definition (type_head . (parametrized_type_expression . (identifier) @name))) @definition.class
(abstract_definition (type_head . (binary_expression . (identifier) @name))) @definition.class
(abstract_definition (type_head . (binary_expression . (parametrized_type_expression . (identifier) @name)))) @definition.class
(primitive_definition (type_head . (identifier) @name)) @definition.class
(primitive_definition (type_head . (parametrized_type_expression . (identifier) @name))) @definition.class
(primitive_definition (type_head . (binary_expression . (identifier) @name))) @definition.class

(macrocall_expression
  (macro_identifier (identifier) @kw)
  (macro_argument_list . [(identifier) @name (typed_expression . (identifier) @name)])
  (#eq? @kw "enum")) @definition.class

(const_statement (assignment . (identifier) @name)) @definition.constant
(const_statement (global_statement (assignment . (identifier) @name))) @definition.constant
(const_statement (assignment . (typed_expression . (identifier) @name))) @definition.constant
(const_statement (assignment . (open_tuple (identifier) @name))) @definition.constant
(const_statement (assignment . (parametrized_type_expression . (identifier) @name))) @definition.type_alias
(const_statement (assignment . (identifier) @name . (operator) . (parametrized_type_expression))) @definition.type_alias

(function_definition (signature . (identifier) @name)) @definition.function
(function_definition (signature . (field_expression (_) . (identifier) @name .))) @definition.function
(macro_definition (signature . (call_expression . (identifier) @name))) @definition.function
(macro_definition (signature . (identifier) @name)) @definition.function
`)

	for _, s := range shapes {
		b.WriteString("(function_definition (signature " + s + ")) @definition.function\n")
	}

	// Short form: at the top of a file, a module or a struct, behind a macro
	// such as @inline, and inside the branches of a top-level `if` (also
	// behind @static), which is how version-dependent methods are written.
	inner := []string{
		"%s",
		"(if_statement (block %s))",
		"(if_statement (elseif_clause (block %s)))",
		"(if_statement (else_clause (block %s)))",
	}
	outer := []string{
		"(source_file %s)",
		"(module_definition (block %s))",
		"(struct_definition (block %s))",
		"(source_file (macrocall_expression (macro_argument_list %s)))",
		"(module_definition (block (macrocall_expression (macro_argument_list %s))))",
	}
	for _, s := range shapes {
		lhs := "(assignment . " + s + " . (operator)) @definition.function"
		for _, in := range inner {
			mid := strings.Replace(in, "%s", lhs, 1)
			for _, out := range outer {
				if in != "%s" && strings.HasPrefix(out, "(struct_definition") {
					continue
				}
				b.WriteString(strings.Replace(out, "%s", mid, 1) + "\n")
			}
		}
	}

	// RECOVERY. When the grammar cannot read one construct (a transpose after a
	// closing bracket, `)'`, is the usual one) it gives up on the whole file:
	// the root is an ERROR whose children are the file's items, the ones it could
	// read as nodes of their own. The short-form methods among them are not under
	// a source_file, so the patterns above never reach them. They are taken here,
	// but only from an ERROR that is the root of the tree or a direct child of the
	// source_file: an ERROR deeper down is as likely the remains of a function
	// body, where a short form is a local closure.
	for _, s := range shapes {
		lhs := "(assignment . " + s + " . (operator)) @definition.function"
		b.WriteString("(ERROR " + lhs + ") @_top\n  (#not-has-ancestor? @_top ERROR source_file)\n")
		b.WriteString("(source_file (ERROR " + lhs + ") @_top)\n  (#not-has-ancestor? @_top ERROR)\n")
		// A header `function name(...)` whose body the grammar could not read stays
		// a keyword and a signature, with no function_definition around them.
		b.WriteString("(ERROR \"function\" . (signature " + s + ") @definition.function) @_top\n  (#not-has-ancestor? @_top ERROR source_file)\n")
		b.WriteString("(source_file (ERROR \"function\" . (signature " + s + ") @definition.function) @_top)\n  (#not-has-ancestor? @_top ERROR)\n")
		// ... or the keyword is an ERROR of its own and the signature follows it.
		b.WriteString("(ERROR (ERROR \"function\") . (signature " + s + ") @definition.function) @_top\n  (#not-has-ancestor? @_top ERROR source_file)\n")
		b.WriteString("(source_file (ERROR (ERROR \"function\") . (signature " + s + ") @definition.function) @_top)\n  (#not-has-ancestor? @_top ERROR)\n")
		// ... which can itself be an ERROR inside the top one.
		b.WriteString("(ERROR (ERROR \"function\" . (signature " + s + ") @definition.function)) @_top\n  (#not-has-ancestor? @_top ERROR source_file)\n")
		b.WriteString("(source_file (ERROR (ERROR \"function\" . (signature " + s + ") @definition.function)) @_top)\n  (#not-has-ancestor? @_top ERROR)\n")
	}
	return b.String()
}

// juliaDefinitionHead reports whether the text that follows a call is the rest
// of a method definition: `f(x) = ...`, `f(x)::Int = ...`, `f(x) where T = ...`.
// The grammar reads the left side as a call, and it declares what it names.
func juliaDefinitionHead(after []byte) bool {
	n := len(after)
	if n > 200 {
		n = 200
	}
	return juliaHeadTail.Match(after[:n])
}

var juliaHeadTail = regexp.MustCompile(`^[ \t]*(::[^=\n]*)?([ \t]+where[ \t][^=\n]*)?[ \t]*=([^=>]|$)`)

// juliaOwners says which modules every definition is in: the names of the
// `module` blocks around it, outermost first, joined with dots. A module is
// qualified by the ones around it and not by itself.
func juliaOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		isModule := kindFromTag(tag.Kind) == model.KindPackage
		var chain []string
		for p := n; p != nil; p = p.Parent() {
			if p.Type(lang) != "module_definition" {
				continue
			}
			var name *ts.Node
			for i := 0; i < p.ChildCount(); i++ {
				if c := p.Child(i); c != nil && c.Type(lang) == "identifier" {
					name = c
					break
				}
			}
			if name == nil {
				continue
			}
			if isModule && name.StartByte() == tag.NameRange.StartByte {
				// The module a tag declares is not its own owner.
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

// The import rows of a Julia file name a module by what it is called, or a file
// by where it is (`include`); see lang_modules.go for the encoding.
//
//	using Money                  Money, import         (its exported names)
//	using .Money, Base.Fmt       Money, Base.Fmt       (a leading dot says
//	                             "a module of this one" and is dropped)
//	using Money: zero, add       Money, import:zero,add
//	import Money                 Money, no directive: `Money.f()` only
//	import Money: add            Money, import:add
//	import Money as M            Money, alias:M
//	include("util/format.jl")    util/format.jl, include   (rooted at the repository)
//
// A name imported under another name (`import Money: add as plus`) is dropped:
// the row does not carry which name it stood for.
var (
	juliaStatement = regexp.MustCompile(`(?s)^(using|import)\s+(.*)$`)
	juliaInclude   = regexp.MustCompile(`^include\(\s*(?:joinpath\(\s*(?:@__DIR__\s*,\s*)?)?((?:"[^"]*"\s*,?\s*)+)\)?`)
	juliaQuoted    = regexp.MustCompile(`"([^"]*)"`)
	juliaModuleRef = regexp.MustCompile(`^\.*([A-Za-z_][A-Za-z0-9_!]*(?:\.[A-Za-z_][A-Za-z0-9_!]*)*)`)
)

func juliaImport(filePath string, imp Import) []Import {
	text := strings.TrimSpace(imp.Path)
	if m := juliaInclude.FindStringSubmatch(text); m != nil {
		var segs []string
		for _, q := range juliaQuoted.FindAllStringSubmatch(m[1], -1) {
			segs = append(segs, q[1])
		}
		if len(segs) == 0 {
			return nil
		}
		imp.Path = path.Join(path.Dir(filePath), path.Join(segs...))
		imp.Alias = ModuleInclude
		return []Import{imp}
	}
	m := juliaStatement.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	verb, rest := m[1], strings.TrimSpace(m[2])
	row := func(module, alias string) Import {
		next := imp
		next.Path, next.Alias = module, alias
		return next
	}
	if module, names, ok := strings.Cut(rest, ":"); ok && !strings.HasPrefix(names, ":") {
		ref := juliaModuleRef.FindStringSubmatch(strings.TrimSpace(module))
		if ref == nil {
			return nil
		}
		var listed []string
		for _, n := range strings.FieldsFunc(names, func(r rune) bool { return r == ',' || r == '(' || r == ')' || r == '\n' }) {
			n = strings.TrimSpace(n)
			if n == "" || strings.Contains(n, " as ") {
				continue
			}
			listed = append(listed, strings.TrimPrefix(n, "@"))
		}
		if len(listed) == 0 {
			return []Import{row(ref[1], "")}
		}
		return []Import{row(ref[1], ModuleImportPrefix+strings.Join(listed, ","))}
	}
	var out []Import
	for _, item := range strings.Split(rest, ",") {
		item = strings.TrimSpace(item)
		module, alias, aliased := strings.Cut(item, " as ")
		ref := juliaModuleRef.FindStringSubmatch(strings.TrimSpace(module))
		if ref == nil {
			continue
		}
		switch {
		case aliased:
			out = append(out, row(ref[1], ModuleAliasPrefix+strings.TrimSpace(alias)))
		case verb == "using":
			out = append(out, row(ref[1], ModuleImportAll))
		default:
			out = append(out, row(ref[1], ""))
		}
	}
	return out
}
