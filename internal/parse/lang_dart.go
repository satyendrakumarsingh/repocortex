package parse

import (
	"path"
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Dart: classes (abstract, sealed, base, final, interface and mixin classes),
// mixins, enums and their constants, extensions, extension types, typedefs,
// functions, methods, constructors, getters, setters, operators, fields and
// top-level variables.
//
// The inferred query is replaced. It records a constructor under every
// identifier of its signature, so `Shape.named()` gave a symbol `Shape` and a
// symbol `named`; it files a mixin and every ordinary method as a function;
// and it finds no setter, operator, typedef, extension, extension type,
// factory constructor, field or top-level variable.
//
// The Dart grammar does not wrap a declaration: at file level a function is a
// `function_signature` followed by a sibling `function_body`, and a variable
// is a type node followed by an `initialized_identifier_list`. A symbol's range
// is therefore the signature only, and for a file-level variable starts at the
// name, not at `const` or the type.
//
// Kinds: classes, enums, extensions and extension types are `class`; a mixin
// is `interface`; a typedef is `type_alias`; enum constants and `const` or
// `final` initialised variables (`static_final_declaration_list`) are
// `constant`; a `var` or typed variable is `variable` at file level and
// `field` in a type; a function at file level or nested in a function is
// `function`, in a type `method`; constructors, getters, setters and operators
// in a type are `method` (a file-level getter or setter is a `function`).
//
// A constructor is named as it is written: `Shape` for `Shape(...)`,
// `Shape.named` for `Shape.named(...)` and `Shape._` for `Shape._()`. Its two
// identifiers are not one node, so the query captures the whole signature as
// the name and the name fix below cuts it back to the words before the
// parameter list (dropping `const` and `factory`). An operator is
// named by its symbol (`==`, `+`, `[]`). An unnamed extension (`extension on
// int`) has no name and is not recorded; its members are.
func init() {
	registerDefinitions("dart", `
; Calls. The grammar has no call node: "a.b.c(x)" is the siblings identifier a, selector .b,
; selector .c, selector (x), so a call is a callee name immediately followed by an argument
; part, and its receiver is not inside any node a capture could span. The tag is the name
; alone, and shape.go reads the receiver back from the source text before it (dartReceiver).
((identifier) @name @reference.call . (selector (argument_part)))
((selector (unconditional_assignable_selector (identifier) @name @reference.call)) . (selector (argument_part)))
((selector (conditional_assignable_selector (identifier) @name @reference.call)) . (selector (argument_part)))
((unconditional_assignable_selector (identifier) @name @reference.call) . (selector (argument_part)))
(new_expression (type_identifier) @name @reference.call)
(const_object_expression (type_identifier) @name @reference.call)

(class_definition (identifier) @name) @definition.class
(class_definition (mixin_application_class . (identifier) @name)) @definition.class
(mixin_declaration (identifier) @name) @definition.interface
(enum_declaration (identifier) @name) @definition.class
(enum_constant (identifier) @name) @definition.constant
(extension_declaration (identifier) @name) @definition.class
(extension_type_declaration (identifier) @name) @definition.class

; typedefs: "typedef Name<T> = ...;" and the old "typedef R Name(args);"
(type_alias . (type_identifier) @name) @definition.type_alias
  (#match? @definition.type_alias "^typedef\\s+[A-Za-z0-9_$]+\\s*(<[^=]*>)?\\s*=")
(type_alias (type_identifier) @name . (formal_parameter_list)) @definition.type_alias
(type_alias (type_identifier) @name . (type_parameters) . (formal_parameter_list)) @definition.type_alias

; functions
(program (function_signature (identifier) @name) @definition.function)
(program (getter_signature (identifier) @name) @definition.function)
(program (setter_signature (identifier) @name) @definition.function)
(local_function_declaration (lambda_expression (function_signature (identifier) @name) @definition.function))

; members
(method_signature (function_signature (identifier) @name) @definition.method)
(declaration (function_signature (identifier) @name) @definition.method)
(method_signature (getter_signature (identifier) @name) @definition.method)
(declaration (getter_signature (identifier) @name) @definition.method)
(method_signature (setter_signature (identifier) @name) @definition.method)
(declaration (setter_signature (identifier) @name) @definition.method)
(operator_signature (binary_operator) @name) @definition.method
(operator_signature ["[]" "[]=" "~" "-"] @name) @definition.method

; constructors: the whole signature is captured as the name, see dartName
(constructor_signature (identifier) . (formal_parameter_list)) @definition.constructor @name
(constant_constructor_signature (identifier) . (formal_parameter_list)) @definition.constructor @name
(factory_constructor_signature (identifier) . (formal_parameter_list)) @definition.constructor @name
(redirecting_factory_constructor_signature (identifier) . (formal_parameter_list)) @definition.constructor @name

; variables and fields
(program (static_final_declaration_list (static_final_declaration . (identifier) @name)) @definition.constant)
(program (initialized_identifier_list (initialized_identifier . (identifier) @name)) @definition.variable)
(declaration (static_final_declaration_list (static_final_declaration . (identifier) @name))) @definition.constant
(declaration (initialized_identifier_list (initialized_identifier . (identifier) @name))) @definition.field
`, true)
	registerNameFix("dart", dartName)
	registerImportFix("dart", dartImport)
	definitionRefiners["dart"] = dartBodies
}

// dartBodies extends a function, method, constructor, getter or setter to the
// end of its body. The grammar writes `int f() { ... }` as a signature node and
// a SIBLING function_body, so a query can only capture the signature, and a
// symbol's range stopped at the closing parenthesis. A call written in the body
// was then outside every method: it was attributed to the class, and a bare
// call to a sibling method (`helper(x)` in a method of the same class) could not
// tell which class it was written in. The body is found as the next sibling of
// the signature or of the method_signature that wraps it.
func dartBodies(root *ts.Node, lang *ts.Language, _ string, _ []byte, tags []ts.Tag) map[uint32]definitionFix {
	var fixes map[uint32]definitionFix
	for _, tag := range tags {
		if !strings.HasPrefix(tag.Kind, "definition.") {
			continue
		}
		switch kindFromTag(tag.Kind) {
		case model.KindFunction, model.KindMethod:
		default:
			continue
		}
		n := root.NamedDescendantForByteRange(tag.NameRange.StartByte, tag.NameRange.EndByte)
		for depth := 0; n != nil && depth < 4; depth++ {
			if sib := n.NextSibling(); sib != nil && sib.Type(lang) == "function_body" {
				if fixes == nil {
					fixes = map[uint32]definitionFix{}
				}
				end := sib.EndPoint()
				fixes[tag.NameRange.StartByte] = definitionFix{endLine: int(end.Row) + 1, endCol: int(end.Column)}
				break
			}
			if t := n.Type(lang); t == "class_body" || t == "program" {
				break
			}
			n = n.Parent()
		}
	}
	return fixes
}

// dartImport rewrites a URI to the repository path of the file it names,
// without the extension, which is what the resolver compares with a file's.
//
//	'src/a.dart'                  relative to the importing file's directory
//	'../a.dart'                   likewise, cleaned: lib/b/x.dart gives lib/a
//	'package:shop/src/a.dart'     lib/src/a: a package's files live under lib/
//	'dart:io'                     the SDK, no file here: dropped
//
// A package URI cannot be checked against the pubspec from here, so one that
// names somebody else's package (`package:flutter/material.dart`) is rewritten
// the same way and names a lib/material file that does not exist; the resolver
// finds no module of that name and records the call as external.
func dartImport(filePath string, imp Import) []Import {
	uri := imp.Path
	switch {
	case strings.HasPrefix(uri, "dart:"):
		return nil
	case strings.HasPrefix(uri, "package:"):
		rest := strings.TrimPrefix(uri, "package:")
		_, file, found := strings.Cut(rest, "/")
		if !found {
			return nil
		}
		uri = path.Join("lib", file)
	case strings.Contains(uri, ":"):
		return nil
	default:
		uri = path.Join(path.Dir(filePath), uri)
	}
	uri = strings.TrimSuffix(path.Clean(uri), ".dart")
	if uri == "" || uri == "." || strings.HasPrefix(uri, "..") {
		return nil
	}
	imp.Path = uri
	combinators := dartCombinators(imp.Clause)
	switch imp.Directive {
	case "export":
		imp.Alias = moduleDirectives(append([]string{imp.Alias, ModuleExport}, combinators...)...)
	case "part":
		imp.Alias = moduleDirectives(imp.Alias, ModuleInclude)
	default:
		imp.Alias = moduleDirectives(append([]string{imp.Alias}, combinators...)...)
	}
	imp.Directive = ""
	imp.Clause = ""
	return []Import{imp}
}

// dartName reduces a captured constructor signature (`const factory
// Shape.circle(double r) = Circle`) to the name it declares (`Shape.circle`).
// Every other captured name is a single identifier or operator and has no
// parenthesis, so it is returned as it is.
func dartName(name string) string {
	head, _, found := strings.Cut(name, "(")
	if !found {
		return name
	}
	words := strings.Fields(head)
	if len(words) == 0 {
		return name
	}
	return words[len(words)-1]
}

// dartCallShape reads what a Dart call was made on from the source before its
// name. The call's tag spans the name only (see the patterns above), so the
// receiver is the run of identifiers and dots written before `.name`:
// `store.save(x)` is on `store`, `this.recalc()` on `this`, `a.b.c(x)` on the
// path `a.b`. What stands before the dot is anything else (a call's closing
// parenthesis, an index, a `!`) is an expression with no name, and a cascade
// (`a..f()`) is not a member call on the receiver's own type.
func dartCallShape(src []byte, at int) callShape {
	opaque := callShape{kind: model.ReceiverExpr, receiver: model.OpaqueReceiver}
	i := at
	for i > 0 && (src[i-1] == ' ' || src[i-1] == '\t' || src[i-1] == '\n' || src[i-1] == '\r') {
		i--
	}
	if i == 0 || src[i-1] != '.' {
		return callShape{kind: model.ReceiverBare}
	}
	i--
	if i > 0 && src[i-1] == '.' {
		return opaque
	}
	if i > 0 && src[i-1] == '?' {
		i--
	}
	j := i
	for j > 0 {
		c := src[j-1]
		if c == '.' || c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			j--
			continue
		}
		break
	}
	if j == i || i-j > shapeMaxReceiver {
		return opaque
	}
	return classifyReceiver("dart", string(src[j:i]), ".")
}

// dartNoise matches what is not code in a directive: comments and string
// literals (the URIs, which may hold a "show" or "hide" of their own).
var dartNoise = regexp.MustCompile(`//[^\n]*|/\*[\s\S]*?\*/|'[^']*'|"[^"]*"`)

// dartCombinator is one `show a, b` or `hide a, b` clause.
var dartCombinator = regexp.MustCompile(`\b(show|hide)\s+([\w$]+(?:\s*,\s*[\w$]+)*)`)

// dartCombinators reads the `show` and `hide` clauses of an import or export
// statement as directives: `show:f,g` and `hiding:h`. A statement with none
// gives none. Several of one kind are one list.
func dartCombinators(clause string) []string {
	if clause == "" {
		return nil
	}
	clause = dartNoise.ReplaceAllString(clause, " ")
	var show, hide []string
	for _, m := range dartCombinator.FindAllStringSubmatch(clause, -1) {
		var names []string
		for _, n := range strings.Split(m[2], ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		if m[1] == "show" {
			show = append(show, names...)
		} else {
			hide = append(hide, names...)
		}
	}
	var out []string
	if len(show) > 0 {
		out = append(out, ModuleShowPrefix+strings.Join(show, ","))
	}
	if len(hide) > 0 {
		out = append(out, ModuleHidingPrefix+strings.Join(hide, ","))
	}
	return out
}
