package parse

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// OCaml. What the index holds, see fixtures/langs/ocaml/expected.tsv.
//
// The inferred query names every `let` binding a function wherever it is, so a
// `let x = ... in` inside a function body, and every binding in a local
// `let module`, was indexed as a definition (about one symbol in six was one of
// those). It also knows no operator definitions, no `external`, no `val`
// specification in an .mli, and gives every value the kind function. It is
// replaced.
//
// A definition is an item of the file or of a module or signature written in
// the file, however deep, with no expression above it (see ocamlNotLocal):
//
//   - let and let rec (and every `and` binding) is a function when it has
//     parameters or its body is `fun` or `function`, and a variable otherwise.
//     A parenthesised operator (`let ( +++ ) a b`) is named by the operator.
//     `let () = ...`, `let _ = ...` and tuple patterns declare no name;
//   - val in an .mli or a signature is a function when its type is a function
//     type and a variable otherwise;
//   - external is a function;
//   - type is a class when it is a variant, a record, abstract or extensible,
//     and a type_alias when it is an alias of another type expression;
//   - exception is a class;
//   - module is a package, module type an interface;
//   - class is a class, class type an interface, a method of either is a
//     method and an instance variable (`val`) is a field.
//
// Not definitions: a let inside an expression (`let x = ... in`), a module
// declared by `let module`, the constructors of a variant, the fields of a
// record, and open/include.
//
// Calls. An application whose function is a path is a call: `f x`, `A.f x`,
// `A.B.f x`; the tag spans the application, so the text before the name is what
// shape.go reads the module from. The right of `|>` is a call too (`xs |> f`).
// A name passed as an argument (`List.map pad xs`) is not tagged: without scopes
// it cannot be told from a parameter of the same name.
//
// A module is the file (a.ml is `A`) and a `module M = struct ... end` inside it
// is a module of its own, so each definition is qualified by the struct modules
// around it (`Cart.Inner`, see ocamlOwners). The imports are `open`, `include`
// and `module X = A.B`, read from the statement's text (ocamlImport).
func init() {
	registerDefinitions("ocaml", ocamlCalls+ocamlDefinitions(), true)
	definitionRefiners["ocaml"] = ocamlOwners
	registerImportFix("ocaml", ocamlImport)
}

const ocamlCalls = `
(application_expression function: (value_path . (value_name) @name .)) @reference.call
(application_expression function: (value_path (module_path) (value_name) @name .)) @reference.call
(infix_expression operator: (rel_operator) @_op right: (value_path (value_name) @name .) @reference.call
  (#eq? @_op "|>"))
`

// ocamlOwners says which modules every definition is in: the names of the
// `module M = struct ... end` blocks and module types around it, outermost
// first, joined with dots. The file's own module is not in the list: it is the
// file's name, and the resolver reads it from there.
func ocamlOwners(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		isModule := kindFromTag(tag.Kind) == model.KindPackage
		var chain []string
		for p := n; p != nil; p = p.Parent() {
			var name *ts.Node
			switch p.Type(lang) {
			case "module_binding":
				for i := 0; i < p.ChildCount() && name == nil; i++ {
					if c := p.Child(i); c != nil && c.Type(lang) == "module_name" {
						name = c
					}
				}
			case "module_type_definition":
				for i := 0; i < p.ChildCount() && name == nil; i++ {
					if c := p.Child(i); c != nil && c.Type(lang) == "module_type_name" {
						name = c
					}
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

// ocamlImport reads a statement that brings a module into the file.
//
//	open A.B   include A.B      A.B, import      the module's names are in scope bare
//	module X = A.B              A.B, alias:X     `X.f` names A.B's f
//
// A local open (`let open A in`, `A.(f x)`) is not read: its names are in scope
// in one expression and not in the file, and reading it as an `open` would bind
// a name written elsewhere in the file to the module.
func ocamlImport(_ string, imp Import) []Import {
	text := strings.TrimSpace(imp.Path)
	if m := ocamlOpenStmt.FindStringSubmatch(text); m != nil {
		imp.Path, imp.Alias = m[1], ModuleImportAll
		return []Import{imp}
	}
	if m := ocamlAliasStmt.FindStringSubmatch(text); m != nil {
		imp.Path, imp.Alias = m[2], ModuleAliasPrefix+m[1]
		return []Import{imp}
	}
	return nil
}

var (
	ocamlOpenStmt  = regexp.MustCompile(`^(?:open|include)!?\s+([A-Z][A-Za-z0-9_']*(?:\.[A-Z][A-Za-z0-9_']*)*)\s*$`)
	ocamlAliasStmt = regexp.MustCompile(`^module\s+([A-Z][A-Za-z0-9_']*)\s*=\s*([A-Z][A-Za-z0-9_']*(?:\.[A-Z][A-Za-z0-9_']*)*)\s*$`)
)

// ocamlItems are the patterns for one declaration. Each is one pattern, not an
// alternation of them: when several siblings match (the bindings joined by
// `and`, the methods of one class) an alternation reports only the first.
// ocamlDefinitions adds the condition on what may be above it.
func ocamlItems() []string {
	opName := `[(value_name) @name (parenthesized_operator (_) @name)]`
	// A let binding with no parameter, whose body is not `fun` or `function`.
	// What may sit between the name and the body is given by between.
	variable := func(name, between string) string {
		return `(value_definition (let_binding pattern: ` + name + ` ` + between + ` body: (_) @_body
  (#not-match? @_body "^(fun|function)\\b")) @definition.variable)`
	}
	return []string{
		// let / let rec / and: a function when it has parameters
		`(value_definition (let_binding pattern: ` + opName + ` (parameter)) @definition.function)`,
		// or a function-valued body
		`(value_definition (let_binding pattern: ` + opName + ` body: [(fun_expression) (function_expression)]) @definition.function)`,
		// a variable otherwise: nothing between the name and the body but a
		// type annotation and comments
		variable(opName, `.`),
		variable(opName, `. (comment) .`),
		variable(opName, `. type: (_) .`),
		variable(opName, `. type: (_) . (comment) .`),

		// val in an interface or signature
		`(value_specification ` + opName + ` type: [(function_type) (polymorphic_type type: (function_type))]) @definition.function`,
		`(value_specification ` + opName + ` type: (_) @_type
  (#not-match? @_type "->")) @definition.variable`,
		`(value_specification ` + opName + ` type: [(constructed_type) (tuple_type)]) @definition.variable`,

		`(external ` + opName + `) @definition.function`,

		// types
		`(type_definition (type_binding name: (type_constructor) @name body: [(variant_declaration) (record_declaration)]) @definition.class)`,
		`(type_definition (type_binding name: (type_constructor) @name .) @definition.class)`,
		// abstract, with attributes: type t [@@deriving sexp_of]
		`(type_definition (type_binding name: (type_constructor) @name . (item_attribute)) @definition.class)`,
		// the "=" is there to leave out a destructive substitution, type t := u,
		// which removes a type from a signature and declares none
		`(type_definition (type_binding name: (type_constructor) @name "=" body: [
  (type_constructor_path) (constructed_type) (function_type) (tuple_type)
  (parenthesized_type) (aliased_type) (polymorphic_variant_type) (object_type)
  (hash_type) (package_type) (type_variable) (local_open_type) (any_type)]) @definition.type_alias)`,
		`(exception_definition (constructor_declaration . (constructor_name) @name)) @definition.class`,

		// modules
		`(module_definition (module_binding . (module_name) @name) @definition.module)`,
		`(module_type_definition . (module_type_name) @name) @definition.interface`,

		// classes and their members
		`(class_definition (class_binding . (class_name) @name) @definition.class)`,
		`(class_type_definition (class_type_binding . (class_type_name) @name) @definition.interface)`,
		`(class_definition (class_binding body: (object_expression (method_definition (method_name) @name) @definition.method)))`,
		`(class_definition (class_binding body: (object_expression (instance_variable_definition (instance_variable_name) @name) @definition.field)))`,
		`(class_definition (class_binding class_type: (class_body_type (method_specification (method_name) @name) @definition.method)))`,
		`(class_definition (class_binding class_type: (class_function_type codomain: (class_body_type (method_specification (method_name) @name) @definition.method))))`,
		`(class_type_definition (class_type_binding body: (class_body_type (method_specification (method_name) @name) @definition.method)))`,
	}
}

// An item is a definition of the file when nothing between it and the file is
// an expression. The old query spelled that out: the file, or the structure or
// signature of a module reached from it through at most three modules. That
// stopped at the module level and missed what OCaml libraries do all the time:
//
//	include struct ... end        include sig ... end
//	include module type of struct ... end        open struct ... end
//	module M : sig ... end with type t := u      module F (X : sig ... end) : sig ... end
//
// Each is another node between the module and its items, and spelling them out
// per level multiplies the query: the old one was 22 items by 40 nestings, 920
// lines and 272,159 bytes, and every new container would have multiplied the
// nestings again.
//
// The question is asked the other way round instead: an item is a definition
// unless one of its ancestors is something that holds expressions. Every way
// into a local item passes through a let binding (a `let module ... in` or a
// first-class module inside a function body), a top-level expression, a class
// body, an extension node, an ERROR (where nothing can be told), or the
// argument of a functor application. A module-level item has none of them as an
// ancestor, at any depth of modules, includes or signatures.
//
// There is no longer a limit of four modules. The parts of a functor
// application written inline are still not definitions (the application is on
// the list of ancestors that exclude), as before, and a destructive
// substitution (type t := u) is not one either.
const ocamlNotLocal = `let_binding expression_item let_module_expression class_binding module_application parenthesized_module_expression packed_module item_extension extension ERROR`

func ocamlDefinitions() string {
	var out []string
	for _, item := range ocamlItems() {
		kind := ocamlKindCapture.FindString(item)
		ancestors := ocamlNotLocal
		if strings.HasPrefix(item, "(class_definition") || strings.HasPrefix(item, "(class_type_definition") {
			// The members of a class sit inside its class_binding.
			ancestors = strings.Replace(ancestors, "class_binding ", "", 1)
		}
		out = append(out, item+"\n  (#not-has-ancestor? "+kind+" "+ancestors+")")
	}
	return strings.Join(out, "\n")
}

var ocamlKindCapture = regexp.MustCompile(`@definition\.[a-z_]+`)
