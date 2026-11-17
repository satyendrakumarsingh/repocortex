package parse

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// F#: modules, namespaces, types, exceptions, module-level lets and members.
//
// The grammar does not separate a function from a value in the tree the way
// the source does:
//
//   - `let f (x: int) = ...` is a function_declaration_left;
//   - `let f x = ...` is a value_declaration_left whose identifier_pattern holds
//     the name AND the argument patterns, so a pattern with more than one child
//     is a function and one with a single child is a value;
//   - `let f = fun x -> ...` is a single-child pattern with a fun_expression.
//
// A function ranks below a variable when a declaration is tagged both ways, so
// the variable pattern excludes lambda bodies by text rather than relying on
// ranking. A return type written after the parameters (`member _.F(x) : T =`)
// wraps them in a typed_pattern, and type parameters (`member _.F<'T>() =`)
// put a type_arguments node between the name and them; both are methods. For members the ranking is used the other way round: the broad
// pattern is a field (property) and the narrower one a method, and the method,
// ranking higher, wins.
//
// Lets are definitions only directly inside a file, namespace or module;
// a let in a function body or in a class (a private field) is not.
//
// Calls. F# applies by juxtaposition, so a call is an application whose function
// is a name (`slug items`, `Money.zero ()`, `Shop.Money.add a b`), and the
// function on the right of `|>` (`xs |> List.map f`: the grammar reads it as the
// function of the application that follows). A name passed as an argument is not
// tagged: without scopes it cannot be told from a parameter of the same name.
// The grammar writes a lone name two ways, with and without a long_identifier
// around it, and both are read.
//
// A module and a namespace are declarations, not files, so each definition that
// is not a member of a type is qualified by the namespaces and modules around it
// (`Shop.Cart.total`, see fsharpOwners). The imports are `open` and
// `module X = A.B`, read from the statement's text (fsharpImport).
func init() {
	scopes := []string{
		"(file %s)",
		"(namespace %s)",
		"(named_module %s)",
		"(module_defn %s)",
	}
	const pat = "(identifier_pattern . (long_identifier_or_op [(identifier) @name (long_identifier . (identifier) @name .)]) %s)"
	// A type with type parameters (`type Box<'T> =`) has its name wrapped in a
	// long_identifier; one without has a bare identifier.
	const tn = "[(identifier) @name (long_identifier . (identifier) @name)]"
	decls := []string{
		// functions
		"(declaration_expression (function_or_value_defn (function_declaration_left . [(identifier) (op_identifier) (active_pattern)] @name) ) @definition.function)",
		"(declaration_expression (function_or_value_defn . (value_declaration_left " + strings.Replace(pat, "%s", ". (_)", 1) + ")) @definition.function)",
		"(declaration_expression (function_or_value_defn . (value_declaration_left " + strings.Replace(pat, "%s", ".", 1) + ") . (fun_expression)) @definition.function)",
		"(declaration_expression (function_or_value_defn . (value_declaration_left " + strings.Replace(pat, "%s", ".", 1) + ") . (function_expression)) @definition.function)",
		// an access modifier moves the name: `let internal f (x: int) =` is a function_declaration_left
		// with the modifier first, `let inline private f x =` likewise
		"(declaration_expression (function_or_value_defn (function_declaration_left (access_modifier) . [(identifier) (op_identifier) (active_pattern)] @name)) @definition.function)",
		// the second and later functions of an `and` group are children of the first one's
		// definition, as further pairs of a pattern and a body
		"(declaration_expression (function_or_value_defn (_) (_) (value_declaration_left " + strings.Replace(pat, "%s", ". (_)", 1) + ") @definition.function))",
		// values
		"(declaration_expression (function_or_value_defn . (value_declaration_left " + strings.Replace(pat, "%s", ".", 1) + ") . (_) @_body) @definition.variable (#not-match? @_body \"^(fun|function)\\\\b\"))",
		// types
		"(type_definition [(union_type_defn (type_name " + tn + ")) (record_type_defn (type_name " + tn + ")) (anon_type_defn (type_name " + tn + ")) (delegate_type_defn (type_name " + tn + ")) (enum_type_defn (type_name " + tn + "))] @definition.class)",
		"(type_definition (interface_type_defn (type_name " + tn + ")) @definition.interface)",
		"(type_definition (type_abbrev_defn (type_name [(identifier) @name (long_identifier . (identifier) @name)])) @definition.type_alias)",
		// `type Id = int` has no bar and no fields: the grammar reads it as a union with one case
		"(type_definition (union_type_defn (type_name " + tn + ") (union_type_cases . (union_type_case (identifier) .) .)) @definition.type_alias)",
		// modules
		"(module_defn . (identifier) @name) @definition.module",
		"(module_defn (attributes) . (identifier) @name) @definition.module",
		// `module internal Inner =`, `module private P =`, with or without attributes
		"(module_defn (access_modifier) . (identifier) @name) @definition.module",
		"(module_defn (attributes) (access_modifier) . (identifier) @name) @definition.module",
		"(exception_definition . [(identifier) @name (long_identifier . (identifier) @name .)]) @definition.class",
		"(exception_definition (access_modifier) . [(identifier) @name (long_identifier . (identifier) @name .)]) @definition.class",
	}
	// The grammar sometimes reads a top-level let as a value_declaration
	// rather than a declaration_expression (after a module it lost track of).
	for _, d := range decls {
		if strings.HasPrefix(d, "(declaration_expression") {
			decls = append(decls, strings.Replace(d, "(declaration_expression", "(value_declaration", 1))
		}
	}
	var b strings.Builder
	for _, scope := range scopes {
		for _, d := range decls {
			b.WriteString(strings.Replace(scope, "%s", d, 1))
			b.WriteByte('\n')
		}
	}
	// A module or namespace header names itself.
	b.WriteString("(named_module . (long_identifier) @name) @definition.module\n")
	b.WriteString("(namespace . (long_identifier) @name) @definition.module\n")
	// When the content of a namespace or module is indented, the grammar drops
	// the header node and leaves its dotted name as the first thing in the
	// file. Nothing else puts a bare long_identifier there.
	b.WriteString("(file . (long_identifier) @name) @definition.module\n")
	// Members
	b.WriteString(`
(member_defn (method_or_prop_defn . (property_or_ident (identifier) @name .)) @definition.field)
(member_defn (method_or_prop_defn . (property_or_ident (identifier) @name .) . [(paren_pattern) (argument_patterns) (identifier_pattern) (typed_pattern) (const (unit))]) @definition.method)
(member_defn (method_or_prop_defn . (property_or_ident (identifier) @name .) . (type_arguments) . [(paren_pattern) (argument_patterns) (identifier_pattern) (typed_pattern) (const (unit))]) @definition.method)
(member_defn . (property_or_ident (identifier) @name .)) @definition.field
(member_defn (member_signature . (identifier) @name) @definition.field)
(member_defn (member_signature . (identifier) @name (curried_spec (arguments_spec))) @definition.method)
`)
	b.WriteString(fsharpCalls)
	registerDefinitions("fsharp", b.String(), true)
	definitionRefiners["fsharp"] = refineFSharpDefinitions
	registerImportFix("fsharp", fsharpImport)
}

const fsharpCalls = `
(application_expression . (long_identifier_or_op . (identifier) @name .)) @reference.call
(application_expression . (long_identifier_or_op . (long_identifier (identifier) @name .) .)) @reference.call
(application_expression . (long_identifier_or_op (long_identifier) (identifier) @name .)) @reference.call
; A name of three segments or more is a dot_expression: the module path is its base and
; the function its field.
(application_expression . (dot_expression field: (long_identifier_or_op . (identifier) @name .)) @reference.call)
(application_expression . (dot_expression field: (long_identifier_or_op (long_identifier (identifier) @name .))) @reference.call)
(application_expression . (infix_expression (dot_expression field: (long_identifier_or_op (long_identifier (identifier) @name .))) @reference.call .))
; The grammar reads ` + "`f a + g b`" + ` as ` + "`(f a + g) b`" + `: the function of the application is an infix
; expression whose last operand is the name that is called.
(application_expression . (infix_expression (long_identifier_or_op . (identifier) @name .) @reference.call .))
(application_expression . (infix_expression (long_identifier_or_op . (long_identifier (identifier) @name .) .) @reference.call .))
(application_expression . (infix_expression (long_identifier_or_op (long_identifier) (identifier) @name .) @reference.call .))
; The function on the right of |> when nothing is applied after it.
((infix_expression (infix_op) @_op . (long_identifier_or_op . (identifier) @name .) @reference.call .) @_i
  (#eq? @_op "|>") (#not-has-parent? @_i application_expression))
((infix_expression (infix_op) @_op . (long_identifier_or_op . (long_identifier (identifier) @name .) .) @reference.call .) @_i
  (#eq? @_op "|>") (#not-has-parent? @_i application_expression))
((infix_expression (infix_op) @_op . (long_identifier_or_op (long_identifier) (identifier) @name .) @reference.call .) @_i
  (#eq? @_op "|>") (#not-has-parent? @_i application_expression))
`

// fsharpOwners says which namespaces and modules every definition is in: their
// names, outermost first, joined with dots. A member of a type keeps the type's
// name as its qualifier (shape.go) and is left alone, and a module is qualified
// by the ones around it and not by itself.
func fsharpOwners(root *ts.Node, lang *ts.Language, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		isModule := kindFromTag(tag.Kind) == model.KindPackage
		var chain []string
		member := false
		for p := n; p != nil; p = p.Parent() {
			var name *ts.Node
			switch p.Type(lang) {
			case "member_defn", "anon_type_defn", "interface_type_defn", "record_type_defn", "union_type_defn":
				// Inside a type; a type's own name is the definition's name, not a
				// scope, and only a member sits inside the type.
				if p.Type(lang) == "member_defn" || (!isTypeName(tag.Kind) && p.Type(lang) != "member_defn") {
					member = true
				}
			case "namespace":
				name = p.ChildByFieldName("name", lang)
			case "named_module":
				name = p.ChildByFieldName("name", lang)
				if name == nil {
					name = firstChildOfType(p, lang, "long_identifier")
				}
			case "module_defn":
				name = firstChildOfType(p, lang, "identifier")
			case "file":
				// An indented namespace or module loses its header node, and its
				// dotted name is the first thing in the file.
				if first := p.NamedChild(0); first != nil && first.Type(lang) == "long_identifier" && !hasAncestorOfType(n, lang, "named_module", "namespace") {
					name = first
				}
			}
			if name == nil || (isModule && name.StartByte() == tag.NameRange.StartByte) {
				continue
			}
			chain = append([]string{name.Text(src)}, chain...)
		}
		if member || len(chain) == 0 {
			continue
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		fixes[tag.NameRange.StartByte] = definitionFix{owner: strings.Join(chain, ".")}
	}
	return fixes
}

func isTypeName(kind string) bool {
	return strings.HasSuffix(kind, ".class") || strings.HasSuffix(kind, ".interface") || strings.HasSuffix(kind, ".type_alias")
}

func firstChildOfType(n *ts.Node, lang *ts.Language, typ string) *ts.Node {
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && c.Type(lang) == typ {
			return c
		}
	}
	return nil
}

func hasAncestorOfType(n *ts.Node, lang *ts.Language, types ...string) bool {
	for p := n; p != nil; p = p.Parent() {
		for _, t := range types {
			if p.Type(lang) == t {
				return true
			}
		}
	}
	return false
}

// fsharpImport reads a statement that brings a module in.
//
//	open A.B            A.B, import     the module's names are in scope bare
//	module X = A.B      A.B, alias:X    `X.f` names A.B's f
func fsharpImport(_ string, imp Import) []Import {
	text := strings.TrimSpace(imp.Path)
	if m := fsharpOpenStmt.FindStringSubmatch(text); m != nil {
		if m[1] == "type" {
			// `open type X.Y` brings a type's static members in; the grammar splits it
			// from the type, so the statement names no module here.
			return nil
		}
		imp.Path, imp.Alias = m[1], ModuleImportAll
		return []Import{imp}
	}
	if m := fsharpAliasStmt.FindStringSubmatch(text); m != nil {
		imp.Path, imp.Alias = m[2], ModuleAliasPrefix+m[1]
		return []Import{imp}
	}
	return nil
}

var (
	fsharpOpenStmt  = regexp.MustCompile(`^open\s+(?:type\s+)?([A-Za-z_][A-Za-z0-9_']*(?:\.[A-Za-z_][A-Za-z0-9_']*)*)\s*$`)
	fsharpAliasStmt = regexp.MustCompile(`^module\s+(?:internal\s+|private\s+|public\s+)?([A-Za-z_][A-Za-z0-9_']*)\s*=\s*([A-Za-z_][A-Za-z0-9_']*(?:\.[A-Za-z_][A-Za-z0-9_']*)*)\s*$`)
)

// refineFSharpDefinitions repairs the name of a definition written with
// `inline` behind an access modifier, or a module written `module rec`. The
// grammar reads the keyword as an identifier, so `let private inline c x = ...`,
// `static member private inline S(x) = ...` and `module rec Parent.Name` come
// out named `inline` and `rec`; the name is the word after it.
func refineFSharpDefinitions(root *ts.Node, lang *ts.Language, _ string, src []byte, tags []ts.Tag) map[uint32]definitionFix {
	fixes := fsharpOwners(root, lang, src, tags)
	fixes = fsharpAndGroups(root, lang, tags, fixes)
	for _, t := range tags {
		if t.Name != "inline" && t.Name != "rec" || !strings.HasPrefix(t.Kind, "definition") {
			continue
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		fix := fixes[t.NameRange.StartByte]
		if name := fsharpWordAfter(src, int(t.NameRange.EndByte)); name != "" {
			fix.name = name
		} else {
			fix.drop = true
		}
		fixes[t.NameRange.StartByte] = fix
	}
	return fixes
}

// fsharpWordAfter is the first identifier after pos that is not a modifier.
func fsharpWordAfter(src []byte, pos int) string {
	for pos < len(src) {
		for pos < len(src) && (src[pos] == ' ' || src[pos] == '\t') {
			pos++
		}
		start := pos
		for pos < len(src) && (src[pos] == '_' || src[pos] == '.' || src[pos] == '\'' || src[pos] == '`' || src[pos] >= '0' && src[pos] <= '9' || src[pos] >= 'a' && src[pos] <= 'z' || src[pos] >= 'A' && src[pos] <= 'Z' || src[pos] >= 0x80) {
			pos++
		}
		if pos == start {
			return ""
		}
		switch w := string(src[start:pos]); w {
		case "private", "internal", "public", "mutable", "rec", "inline":
		default:
			return w
		}
	}
	return ""
}

// fsharpAndGroups bounds each function of an `and` group at its own body. The
// grammar puts `let rec f ... and g ... and h ...` in one function_or_value_defn,
// a run of patterns and bodies, so the first function's range would take in the
// others, and callers in the body of g would be attributed to f. The functions
// after the first are tagged on their own pattern (see init); each, and the
// first, ends where its body does.
func fsharpAndGroups(root *ts.Node, lang *ts.Language, tags []ts.Tag, fixes map[uint32]definitionFix) map[uint32]definitionFix {
	for _, t := range tags {
		if !strings.HasPrefix(t.Kind, "definition.function") {
			continue
		}
		var left, defn *ts.Node
		for n := root.NamedDescendantForByteRange(t.NameRange.StartByte, t.NameRange.EndByte); n != nil; n = n.Parent() {
			if n.Type(lang) == "value_declaration_left" && left == nil {
				left = n
			}
			if n.Type(lang) == "function_or_value_defn" {
				defn = n
				break
			}
		}
		if left == nil || defn == nil {
			continue
		}
		lefts := 0
		for i := 0; i < defn.NamedChildCount(); i++ {
			if c := defn.NamedChild(i); c != nil && c.Type(lang) == "value_declaration_left" {
				lefts++
			}
		}
		if lefts < 2 {
			continue
		}
		end := left
		for sib := left.NextSibling(); sib != nil; sib = sib.NextSibling() {
			if !sib.IsNamed() {
				continue
			}
			if sib.Type(lang) != "value_declaration_left" {
				end = sib
			}
			break
		}
		if fixes == nil {
			fixes = map[uint32]definitionFix{}
		}
		fix := fixes[t.NameRange.StartByte]
		fix.endLine, fix.endCol = int(end.EndPoint().Row)+1, int(end.EndPoint().Column)
		fixes[t.NameRange.StartByte] = fix
	}
	return fixes
}
