package parse

import "strings"

// Swift: types, functions, initialisers, subscripts, properties, type aliases,
// enum cases and operator declarations.
//
// The inferred query is replaced. It names enum cases `enum_member`, which
// becomes a function, and gives every function the kind `function` even
// inside a type, so methods were indistinguishable from free functions. It
// also finds no initialiser, deinitialiser, subscript, property, typealias,
// associatedtype or extension.
//
// Kinds: struct, enum, actor, class and extension are `class`; a protocol is
// `interface`; a function in a type or protocol body is `method`, anywhere
// else `function` (a free function, or one nested in another function).
// init, deinit and subscript are `method` named by their keyword (several
// initialisers in one type share the name `init`; they differ by line).
// Properties declared in a type body, computed or stored, static or not, are
// `field`. At file level `let` is `constant` and `var` is `variable`; locals
// are not recorded. Enum cases are `constant`. A typealias and an
// associatedtype are `type_alias`. An operator declaration
// (`infix operator **`) is a `function` named by the operator; an operator
// implementation (`static func ==`) is a method named by the operator.
//
// An extension is recorded as a `class` under the name of the type it
// extends, at the extension's own line, so that `Array` has an entry for each
// place it is extended. Its members are methods and fields as in any type. A
// type that is not a name is the name as written: `extension [Foo]` is
// `[Foo]`, `extension [String: Int]` is `[String: Int]`, `extension (A, B)`
// is `(A, B)`; a bound generic (`extension Array<Foo>`) is `Array`.
//
// A declaration with attributes (`@MainActor`, `@discardableResult`) starts at
// the attribute, because the grammar makes the attributes part of the
// declaration; that is the line recorded.
func init() {
	registerDefinitions("swift", `
; Calls. A call is the callee name and everything written before it: Pricing.discount(for: x)
; is the name discount in a call that begins at Pricing, which is how shape.go reads the
; receiver. An initialiser call Item(name: "a") is a bare call of the type's name, and the
; resolver binds a call to a class as an instantiation.
(call_expression . (simple_identifier) @name) @reference.call
(call_expression . (navigation_expression (navigation_suffix (simple_identifier) @name) .)) @reference.call

(protocol_declaration (type_identifier) @name) @definition.interface
(class_declaration "class" (type_identifier) @name) @definition.class
(class_declaration "struct" (type_identifier) @name) @definition.class
(class_declaration "actor" (type_identifier) @name) @definition.class
(class_declaration "enum" (type_identifier) @name) @definition.class
(class_declaration "extension" (user_type (type_identifier) @name .)) @definition.class
(class_declaration "extension" (user_type (type_identifier) @name . (type_arguments))) @definition.class
(class_declaration "extension" [(array_type) (dictionary_type) (tuple_type)] @name) @definition.class

(typealias_declaration (type_identifier) @name) @definition.type_alias
(associatedtype_declaration (type_identifier) @name) @definition.type_alias

(enum_entry (simple_identifier) @name) @definition.constant

; functions: free and nested ones are functions, those in a type are methods
(source_file (function_declaration (simple_identifier) @name) @definition.function)
(function_body (statements (function_declaration (simple_identifier) @name) @definition.function))
(class_body (function_declaration (simple_identifier) @name) @definition.method)
(enum_class_body (function_declaration (simple_identifier) @name) @definition.method)
(protocol_function_declaration (simple_identifier) @name) @definition.method

; operator implementations: a custom operator is a node, the built-in ones are tokens
(class_body (function_declaration "func" [(custom_operator) "==" "!=" "<" ">" "<=" ">=" "+" "-" "*" "/" "%" "&" "|" "^" "!" "~" "&&" "||" "<<" ">>" "+=" "-=" "*=" "/=" "??" "..." "..<" "~=" "==="] @name) @definition.method)
(enum_class_body (function_declaration "func" [(custom_operator) "==" "!=" "<" ">" "<=" ">=" "+" "-" "*" "/" "%" "&" "|" "^" "!" "~" "&&" "||" "<<" ">>" "+=" "-=" "*=" "/=" "??" "..." "..<" "~=" "==="] @name) @definition.method)
(source_file (function_declaration "func" [(custom_operator) "==" "!=" "<" ">" "<=" ">=" "+" "-" "*" "/" "%" "&" "|" "^" "!" "~" "&&" "||" "<<" ">>" "+=" "-=" "*=" "/=" "??" "..." "..<" "~=" "==="] @name) @definition.function)

(init_declaration "init" @name) @definition.method
(deinit_declaration "deinit" @name) @definition.method
(subscript_declaration "subscript" @name) @definition.method

(operator_declaration (custom_operator) @name) @definition.function
(macro_declaration (simple_identifier) @name) @definition.function

; properties
(source_file (property_declaration (value_binding_pattern) @_b (pattern (simple_identifier) @name)) @definition.constant
  (#match? @_b "^let"))
(source_file (property_declaration (value_binding_pattern) @_b (pattern (simple_identifier) @name)) @definition.variable
  (#match? @_b "^var"))
(source_file (property_declaration (value_binding_pattern) @_b (pattern (pattern (simple_identifier) @name))) @definition.constant
  (#match? @_b "^let"))
(source_file (property_declaration (value_binding_pattern) @_b (pattern (pattern (simple_identifier) @name))) @definition.variable
  (#match? @_b "^var"))
(class_body (property_declaration (pattern (simple_identifier) @name)) @definition.field)
(enum_class_body (property_declaration (pattern (simple_identifier) @name)) @definition.field)
(protocol_property_declaration (pattern (simple_identifier) @name)) @definition.field

; Recovery. When the grammar cannot read a construct inside a type it gives up on the type: the
; header is no longer a class_declaration but loose tokens (the modifiers, "class", the name,
; the supertypes) directly under an ERROR, and the members that it could still read follow as
; siblings, with no class_body around them. These patterns only match under an ERROR that
; holds a type keyword, which no clean tree has, and they take the declarations whose own
; nodes are intact. A function whose parameters were shredded keeps "func" and its name as
; two tokens, which is enough for the name.
(ERROR ["class" "struct" "enum" "actor"] . (simple_identifier) @name @definition.class)
(ERROR "protocol" . (simple_identifier) @name @definition.interface)
(ERROR ["class" "struct" "enum" "actor" "protocol" "extension"] (function_declaration (simple_identifier) @name) @definition.method)
(ERROR ["class" "struct" "enum" "actor" "protocol" "extension"] "func" . (simple_identifier) @name @definition.method)
; Only a property with an access level or static is taken: a local let in a function
; whose body was shredded looks the same without one, and locals are not definitions.
(ERROR ["class" "struct" "enum" "actor" "protocol" "extension"] (property_declaration (modifiers (visibility_modifier)) (pattern (simple_identifier) @name)) @definition.field)
(ERROR ["class" "struct" "enum" "actor" "protocol" "extension"] (property_declaration (modifiers (property_modifier) @_m) (pattern (simple_identifier) @name)) @definition.field
  (#match? @_m "^(static|class)$"))
(ERROR ["class" "struct" "enum" "actor" "protocol" "extension"] (typealias_declaration (type_identifier) @name) @definition.type_alias)
(class_body (ERROR "func" . (simple_identifier) @name @definition.method))
(ERROR "typealias" . (simple_identifier) @name @definition.type_alias)

; A generic argument list the parser cannot close (Protected<Downstream?>, a type argument with
; a postfix "?") swallows the declarations that follow it in the type: "private let request:
; DataRequest" becomes the tokens "let" and the label "request:", an initialiser the token
; "init". Neither can occur inside a type argument list in a tree that was read properly.
(class_body (property_declaration (type_annotation (user_type (type_arguments (ERROR ["let" "var"] . (statement_label) @name @definition.field))))))
(class_body (property_declaration (type_annotation (user_type (type_arguments (ERROR "init" @name @definition.method))))))
; ... or only "func" is a node and the name stays in the error's text ("private func cancel"):
; the whole error is the head, modifiers then "func" then the name, and nothing else. The name
; is the last word of that text; see swiftName.
(ERROR ["class" "struct" "enum" "actor" "protocol" "extension"] (ERROR "func") @name @definition.method
  (#match? @name "^([@A-Za-z_][A-Za-z0-9_]*(\\([^)]*\\))?\\s+)*func\\s+[A-Za-z_][A-Za-z0-9_]*$"))
`, true)
	registerNameFix("swift", swiftName)
	registerImportFix("swift", swiftImport)
}

// swiftImport reduces an import to the module it names. Swift imports modules,
// never files: `import Core` and `@testable import Core` are the module Core,
// and `import struct Core.Money` or `import Core.Models` is still Core, the
// first word, because a submodule or a declaration is reached through it.
func swiftImport(_ string, imp Import) []Import {
	module, _, _ := strings.Cut(imp.Path, ".")
	if module == "" {
		return nil
	}
	imp.Path = module
	return []Import{imp}
}

// swiftName cleans up three names that come from text the grammar did not
// make into nodes:
//
//   - "private func cancel", a function head left in an error node: `cancel`;
//   - "request:", the label a declaration `let request: T` was read as: `request`;
//   - "[String: Int]", the type an extension extends, as written, with its
//     white space collapsed.
//
// A name from a clean tree has none of these shapes.
func swiftName(name string) string {
	switch {
	case strings.HasPrefix(name, "[") || strings.HasPrefix(name, "("):
		return strings.Join(strings.Fields(name), " ")
	case strings.ContainsAny(name, " \t\r\n"):
		f := strings.Fields(name)
		return f[len(f)-1]
	case len(name) > 1 && strings.HasSuffix(name, ":"):
		return strings.TrimSuffix(name, ":")
	}
	return name
}
