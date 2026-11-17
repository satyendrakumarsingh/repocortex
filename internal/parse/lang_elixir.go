package parse

import (
	"regexp"
	"strconv"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Elixir: modules, protocols and their implementations, functions and macros,
// types, callbacks and module attributes.
//
// Everything in Elixir is a `call` whose target is an identifier, so each
// pattern names the macro: `defmodule`, `def`, `defp`, `defmacro`,
// `defmacrop`, `defguard`, `defguardp`, `defdelegate`, `defprotocol`,
// `defimpl`.
//
// Kinds:
//   - defmodule is a `package`; defprotocol an `interface`; defimpl a `class`
//     named by the protocol it implements (the implementation module has no
//     name of its own that a node carries).
//   - def, defp, defmacro, defmacrop, defguard, defguardp and defdelegate are
//     `function`; inside a defprotocol they are `method`.
//   - @type, @typep and @opaque are `type_alias`; @callback and @macrocallback
//     are `method`; every other module attribute that is given a value, except
//     the ones the compiler reads (@doc, @spec, @impl, @behaviour ...), is a
//     `constant`.
//
// The query matches one clause head at a time, so a function written as
// several clauses arrives as several symbols; registerClauseMerge folds the
// consecutive clauses of one name and arity back into one (see
// clausemerge.go). `def f(a)` and `def f(a, b)` stay two functions, as does a
// later `def f` that follows another function. A clause with a guard
// (`def f(n) when is_integer(n)`) or without parentheses (`def f, do: 1`) is
// found like any other.
//
// Not recorded: defstruct and defexception (they declare no name; the struct
// is the module), @spec, imports, and the generated functions of `test`
// blocks.
// elixirForms are the macros and special forms that open a construct and are
// written like a call. Quoted for a query predicate.
const elixirForms = `"def" "defp" "defmacro" "defmacrop" "defguard" "defguardp" "defdelegate" ` +
	`"defmodule" "defprotocol" "defimpl" "defstruct" "defexception" "defoverridable" "defcallback" ` +
	`"alias" "import" "require" "use" "quote" "unquote" "unquote_splicing" "if" "unless" "case" "cond" ` +
	`"with" "for" "try" "receive" "fn" "else" "rescue" "catch" "after" "do" "end" ` +
	`"doc" "moduledoc" "typedoc" "spec" "type" "typep" "opaque" "callback" "macrocallback" ` +
	`"impl" "behaviour" "derive" "enforce_keys" "deprecated" "since" "dialyzer" "compile"`

func init() {
	const defs = `"def" "defp" "defmacro" "defmacrop" "defguard" "defguardp" "defdelegate"`
	// The ways a clause head can be written, as the first argument of def.
	forms := []string{
		`(call (identifier) @name)`,                     // def f(a), do: ...
		`(identifier) @name`,                            // def f, do: ...
		`(binary_operator . (call (identifier) @name))`, // def f(a) when guard
		`(binary_operator . (identifier) @name)`,        // def f when guard
	}

	var q strings.Builder
	// Calls: `f(x)` (and `f x`), `Mod.f(x)` and `__MODULE__.f(x)`. A macro of the
	// language that only opens a construct (`def`, `alias`, `if`) is not a call
	// of anything the repository defines, and neither is the head of a clause
	// (`total(c)` in `def total(c)`), which shape.go finds from the text before it.
	q.WriteString(`
(call (identifier) @name) @reference.call
  (#not-any-of? @name ` + elixirForms + `)
(call (dot left: (alias) right: (identifier) @name)) @reference.call
(call (dot left: (identifier) @_m right: (identifier) @name) (#eq? @_m "__MODULE__")) @reference.call

(call (identifier) @_head (arguments . (alias) @name) (#eq? @_head "defmodule")) @definition.module
(call (identifier) @_head (arguments . (alias) @name) (#eq? @_head "defprotocol")) @definition.interface
(call (identifier) @_head (arguments . (alias) @name) (#eq? @_head "defimpl")) @definition.class
`)
	for _, f := range forms {
		q.WriteString("(call (identifier) @_d (arguments . " + f + ") (#any-of? @_d " + defs + ")) @definition.function\n")
		q.WriteString("(call (identifier) @_p (do_block (call (identifier) @_d (arguments . " + f + ") (#any-of? @_d " + defs + ")) @definition.method) (#eq? @_p \"defprotocol\"))\n")
	}
	q.WriteString(`
; @type t :: ..., @typep t(a) :: ..., @opaque t :: ..., also with a when clause
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (call (identifier) @name))) (#any-of? @_a "type" "typep" "opaque"))) @definition.type_alias
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (identifier) @name)) (#any-of? @_a "type" "typep" "opaque"))) @definition.type_alias
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (binary_operator . (call (identifier) @name)))) (#any-of? @_a "type" "typep" "opaque"))) @definition.type_alias
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (binary_operator . (identifier) @name))) (#any-of? @_a "type" "typep" "opaque"))) @definition.type_alias

; @callback f(a) :: ..., @macrocallback f(a) :: ..., also with no parentheses (f :: ...)
; and with a when clause after the return type (f(a) :: r when a: term)
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (call (identifier) @name))) (#any-of? @_a "callback" "macrocallback"))) @definition.method
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (identifier) @name)) (#any-of? @_a "callback" "macrocallback"))) @definition.method
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (binary_operator . (call (identifier) @name)))) (#any-of? @_a "callback" "macrocallback"))) @definition.method
(unary_operator (call (identifier) @_a (arguments . (binary_operator . (binary_operator . (identifier) @name))) (#any-of? @_a "callback" "macrocallback"))) @definition.method

; @name value
(do_block (unary_operator (call (identifier) @name (arguments (_)))) @definition.constant
  (#match? @definition.constant "^@")
  (#not-any-of? @name "doc" "moduledoc" "typedoc" "spec" "type" "typep" "opaque" "callback" "macrocallback" "impl" "behaviour" "derive" "enforce_keys" "deprecated" "since" "dialyzer" "compile" "on_definition" "before_compile" "after_compile" "on_load" "external_resource" "file" "vsn" "optional_callbacks" "nifs"))
`)
	registerDefinitions("elixir", q.String(), true)
	registerClauseMerge("elixir", elixirClauseArity)
	definitionRefiners["elixir"] = elixirOwners
	registerImportFix("elixir", elixirImport)
}

// elixirModuleCall is the module a `defmodule`, `defprotocol` or `defimpl` call
// declares, as the name its functions are qualified by: `Shop.Cart`, and for an
// implementation `Shop.Printable.Cart`, which is what the compiler calls it.
// ok is false for any other call.
func elixirModuleCall(call *ts.Node, lang *ts.Language, src []byte) (name string, ok bool) {
	if call == nil || call.Type(lang) != "call" || call.ChildCount() == 0 {
		return "", false
	}
	head := call.Child(0)
	if head == nil || head.Type(lang) != "identifier" {
		return "", false
	}
	switch head.Text(src) {
	case "defmodule", "defprotocol", "defimpl":
	default:
		return "", false
	}
	var first *ts.Node
	for i := 1; i < call.ChildCount(); i++ {
		if c := call.Child(i); c != nil && c.Type(lang) == "arguments" {
			for j := 0; j < c.ChildCount(); j++ {
				if a := c.Child(j); a != nil && a.Type(lang) == "alias" {
					first = a
					break
				}
			}
			if first != nil {
				if head.Text(src) == "defimpl" {
					if m := elixirImplFor.FindStringSubmatch(c.Text(src)); m != nil {
						return first.Text(src) + "." + m[1], true
					}
				}
			}
			break
		}
	}
	if first == nil {
		return "", false
	}
	return first.Text(src), true
}

var elixirImplFor = regexp.MustCompile(`for:\s*([A-Z][A-Za-z0-9_.]*)`)

// elixirOwners says which module every definition is in. A function's name does
// not state it (`def total(c)` in `defmodule Shop.Cart`), and a call from
// another file names the module and not the file, so the module is the
// function's owner and its qualified name becomes `path:Shop.Cart.total`. A
// module nested in another is qualified by it (`path:Shop.Cart` for `defmodule
// Cart` inside `defmodule Shop`), as the compiler names it, and keeps the name
// it is written with.
func elixirOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		// The module a tag declares has itself as its first enclosing call.
		declares := ""
		if kind := kindFromTag(tag.Kind); kind == model.KindPackage || kind == model.KindInterface || kind == model.KindClass {
			if p := n; p != nil {
				for depth := 0; p != nil && depth < 3; depth++ {
					if name, ok := elixirModuleCall(p, lang, src); ok {
						declares = name
						break
					}
					p = p.Parent()
				}
			}
		}
		var chain []string
		skipped := declares == ""
		for p := n; p != nil; p = p.Parent() {
			name, ok := elixirModuleCall(p, lang, src)
			if !ok {
				continue
			}
			if !skipped && name == declares {
				skipped = true
				continue
			}
			chain = append([]string{name}, chain...)
		}
		if len(chain) == 0 {
			continue
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		// A module nested in another is a member of it as far as its qualified
		// name goes (`path:Shop.Cart.Line`); its name stays as written. A
		// protocol or an implementation nested in a module keeps the plain
		// qualified name its callers know it by.
		if declares != "" && kindFromTag(tag.Kind) != model.KindPackage {
			continue
		}
		fixes[tag.NameRange.StartByte] = definitionFix{owner: strings.Join(chain, ".")}
	}
	return fixes
}

// elixirClauseArity counts the arguments of the clause head a name sits in:
// the named children of the `arguments` of `f(a, b)`. A head with no
// parentheses (`def f, do: 1`) has none.
func elixirClauseArity(name *ts.Node, lang *ts.Language) string {
	head := name.Parent()
	if head == nil || head.Type(lang) != "call" {
		return "0"
	}
	return namedChildCount(head, "arguments", lang)
}

// namedChildCount is how many named, non-comment children the first child of
// the given type has, as a string; "" when the node has no such child.
func namedChildCount(n *ts.Node, childType string, lang *ts.Language) string {
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || c.Type(lang) != childType {
			continue
		}
		total := 0
		for j := 0; j < c.ChildCount(); j++ {
			if a := c.Child(j); a != nil && a.IsNamed() && !a.IsExtra() {
				total++
			}
		}
		return strconv.Itoa(total)
	}
	return ""
}

// The import rows of an Elixir file are directives, and a module is named by
// what it is called and not by where it is, so the rows carry the module name
// in Path and the directive in Alias:
//
//	alias Shop.Money                 Path Shop.Money, Alias alias:Money
//	alias Shop.Billing.Invoice, as: Inv        ... Alias alias:Inv
//	alias Shop.{Tax, Order}          one row each: Shop.Tax and Shop.Order
//	import Shop.Util                 Path Shop.Util, Alias import
//	import Shop.Fmt, only: [fmt: 1]  Path Shop.Fmt, Alias import:fmt
//	use Shop.Worker, retries: 3      Path Shop.Worker, Alias use
//	require Shop.Mod, as: M          like alias: it names M
//
// A plain `require` names no function and no alias and is not a row. A module
// named by an Erlang atom (`:crypto`) or relative to the module it is written
// in (`__MODULE__.Sub`) is not one either.
//
// The prefixes are read by the resolver (internal/resolve/infer/elixir.go).
const (
	ElixirAliasPrefix  = "alias:"
	ElixirImportAlias  = "import"
	ElixirImportPrefix = "import:"
	ElixirUseAlias     = "use"
)

var (
	elixirDirective = regexp.MustCompile(`^(alias|import|use|require)\s*\(?\s*([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)(?:\.\{([^}]*)\})?`)
	elixirAs        = regexp.MustCompile(`\bas:\s*([A-Z][A-Za-z0-9_]*)`)
	elixirOnly      = regexp.MustCompile(`\bonly:\s*\[([^\]]*)\]`)
	elixirOnlyName  = regexp.MustCompile(`([a-z_][A-Za-z0-9_?!]*)\s*:`)
)

func elixirImport(_ string, imp Import) []Import {
	text := imp.Path
	m := elixirDirective.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	directive, module, group := m[1], m[2], m[3]
	rest := text[len(m[0]):]
	lastSegment := func(name string) string { return name[strings.LastIndexByte(name, '.')+1:] }
	row := func(path, alias string) Import {
		next := imp
		next.Path, next.Alias = path, alias
		return next
	}
	switch directive {
	case "use":
		return []Import{row(module, ElixirUseAlias)}
	case "import":
		if o := elixirOnly.FindStringSubmatch(rest); o != nil {
			var names []string
			for _, n := range elixirOnlyName.FindAllStringSubmatch(o[1], -1) {
				names = append(names, n[1])
			}
			if len(names) > 0 {
				return []Import{row(module, ElixirImportPrefix+strings.Join(names, ","))}
			}
		}
		return []Import{row(module, ElixirImportAlias)}
	}
	if group != "" {
		var out []Import
		for _, part := range strings.Split(group, ",") {
			part = strings.TrimSpace(part)
			if part == "" || part[0] < 'A' || part[0] > 'Z' {
				continue
			}
			full := module + "." + part
			out = append(out, row(full, ElixirAliasPrefix+lastSegment(full)))
		}
		return out
	}
	if a := elixirAs.FindStringSubmatch(rest); a != nil {
		return []Import{row(module, ElixirAliasPrefix+a[1])}
	}
	if directive == "require" {
		return nil
	}
	return []Import{row(module, ElixirAliasPrefix+lastSegment(module))}
}
