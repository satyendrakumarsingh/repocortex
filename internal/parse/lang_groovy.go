package parse

import "strings"

// Groovy (and Gradle build scripts, Jenkinsfiles, Spock specifications).
//
// The grammar reads only part of the language. A class body is a `closure`; a
// `trait` and an `enum` are not nodes of their own but the sequence
// identifier, identifier, closure; `implements` is an ERROR node, which moves
// the class name into it when the class has no `extends`; a constructor is
// parsed as a call with a closure argument. The patterns below follow what the
// tree really is, and each was checked against files for what it also matches.
//
// The inferred query tagged every identifier child of a class (so the
// superclass was a second class, and `implements Serializable` produced the
// class `erializable`) and found neither constructors, generic methods,
// Spock's string-named methods, enums nor traits.
//
// Kinds: class and enum are `class`; interface and trait are `interface`;
// methods and constructors in a class body are `method`; a `def` or typed
// function at the top level of a script is `function`; a Gradle task
// (`task name {}`, `task name(type: T) {}`, `task('name') {}`,
// `tasks.register('name') {}`, `tasks.create('name')`, `project.task('name')`)
// is `function` named by the task. A field whose value is a closure
// (`def handler = { x -> ... }`) is a `method` in a class and a `function` at
// the top level of a script. A Spock feature method is named by its quoted
// title, without the quotes.
//
// Recovery. The grammar fails on common Groovy (a list as an annotation argument, a map keyed
// by parenthesised expressions), and then reads a class as loose tokens or closes it early. A
// one-line doc comment, which it never closes, is dealt with before the parse: see
// mask_groovy.go. The patterns marked "recovery" below only match shapes that
// a tree read properly does not have: a doc comment holding a class keyword, a class followed
// by an ERROR and then members. See fixtures/langs/groovy/KNOWN_GAPS.md.
//
// Plain fields and script variables are not recorded: a misread constructor
// leaves its parameters behind as declarations, so a field pattern would also
// record parameters.
func init() {
	registerDefinitions("groovy", `
; Calls. f(x), a.b.f(x), obj?.f(x) and `+"`f x`"+` are function_call, access_op and juxt_function_call
; nodes whose callee is an identifier or a dotted_identifier. A call node begins at its
; receiver, so the text before the name is how shape.go reads what the call was made on;
; the callee of `+"`obj?.f()`"+` is the function_call inside an access_op, so that node is the
; tagged one. `+"`new Widget(1)`"+` is a unary_op around the same function_call, and is
; recorded as a construction (reference.call.new) of the type and not as a call of a method.
((function_call . (identifier) @name) @reference.call
  (#not-has-parent? @reference.call unary_op access_op))
(function_call . (dotted_identifier (identifier) @name .)) @reference.call
(access_op (function_call . (identifier) @name)) @reference.call
(juxt_function_call . (identifier) @name) @reference.call
(juxt_function_call . (dotted_identifier (identifier) @name .)) @reference.call
(unary_op "new" . (function_call . (identifier) @name @reference.call.new))
(unary_op "new" . (function_call . (dotted_identifier . (identifier) (identifier) @name @reference.call.new .)))

; class, interface, @interface: the first identifier after the keyword. A class that has
; "implements" and no "extends" is read with the keyword inside an ERROR node and its name
; mangled; that form is the ERROR followed by an identifier (the mangled name).
(class_definition "class" . (identifier) @name) @definition.class
(class_definition "interface" . (identifier) @name) @definition.interface
(class_definition "@interface" . (identifier) @name) @definition.interface
(class_definition (ERROR . (identifier) @name) . (identifier)) @definition.class

; trait and enum are the sequence identifier, identifier, closure; with "implements" the name
; is read as a declaration
(source_file (identifier) @_k . (identifier) @name @definition.interface . (closure) (#eq? @_k "trait"))
(source_file (identifier) @_k . (identifier) @name @definition.class . (closure) (#eq? @_k "enum"))
(closure (identifier) @_k . (identifier) @name @definition.interface . (closure) (#eq? @_k "trait"))
(closure (identifier) @_k . (identifier) @name @definition.class . (closure) (#eq? @_k "enum"))
(source_file (identifier) @_k . (declaration . (identifier) @name @definition.interface . (identifier)) (#eq? @_k "trait"))
(source_file (identifier) @_k . (declaration . (identifier) @name @definition.class . (identifier)) (#eq? @_k "enum"))

; a trait or enum with type parameters or a supertype list: the name sits inside a
; type_with_generics (trait Repo<D> implements Base<D> {, trait Two<T> extends Foo {)
(source_file (identifier) @_k . (declaration . (type_with_generics . (identifier) @name @definition.interface) . (identifier)) (#eq? @_k "trait"))
; a generic trait with no supertype is read as the call "trait" with a comparison and a closure
; for its argument; the closure is its body
(juxt_function_call (identifier) @_k (argument_list (binary_op (binary_op . (identifier) @name @definition.interface)))
  (#eq? @_k "trait"))
(juxt_function_call (identifier) @_k (argument_list (binary_op (closure (function_definition (identifier) @name . (parameter_list)) @definition.method)))
  (#eq? @_k "trait"))
(juxt_function_call (identifier) @_k (argument_list (binary_op (closure (declaration) @_d . (function_call (identifier) @name) @definition.method)))
  (#eq? @_k "trait") (#not-match? @_d "="))

; functions at the top of a script, methods in a body
(source_file (function_definition (identifier) @name . (parameter_list)) @definition.function)
(class_definition (closure (function_definition (identifier) @name . (parameter_list)) @definition.method))
(class_definition (closure (function_definition (quoted_identifier) @name . (parameter_list)) @definition.method))
(class_definition (closure (function_declaration (identifier) @name . (parameter_list)) @definition.method))
(source_file (closure (function_definition (identifier) @name . (parameter_list)) @definition.method))
(source_file (closure (function_declaration (identifier) @name . (parameter_list)) @definition.method))

; constructors, generic methods and bodiless methods with a class-typed parameter are read as a
; call, which has no other meaning directly in a class body
(class_definition (closure (function_call (identifier) @name) @definition.method))
; the same in a trait body, where the type before the name is the only thing that tells a method
; from a statement: a method whose return type is a bare type variable (static D create() {) or
; that has its own type parameters (static <T> T with(...) {) is read as a declaration of the
; modifiers and the type followed by a call whose closure is the body. A declaration with an
; initialiser is a field, not a return type.
(source_file (closure (declaration) @_d . (function_call (identifier) @name) @definition.method)
  (#not-match? @_d "="))

; an abstract method whose return type is a bare type variable (abstract T snapshot()) is the
; identifiers abstract and T, then a call
((closure (identifier) @_m . (identifier) . (function_call (identifier) @name) @definition.method)
  (#eq? @_m "abstract"))

; a constructor with modifiers (private Name(a) {): an ERROR holding the modifier and the name
(class_definition (closure (ERROR (access_modifier) (identifier) @name .) @definition.method . (parenthesized_expression) . (closure)))

; a method with modifiers and no return type (private static name(a) {) is a declaration of
; the modifiers and the name, then the parameters as a parenthesised expression, then the body
(class_definition (closure (declaration (identifier) @name .) @definition.method . (parenthesized_expression) . (closure)))
(source_file (declaration (identifier) @name .) @definition.function . (parenthesized_expression) . (closure))

; A class that the grammar closes early (a map literal with parenthesised keys does it) leaves
; the rest of its members at the top of the file, after an ERROR. Those are methods. Neither
; pattern can match in a tree without an ERROR at the top, so a script that really has a
; function after a class keeps its kind.
(source_file (class_definition) (ERROR) (function_definition (identifier) @name . (parameter_list)) @definition.method)
(source_file (class_definition) (ERROR) (function_declaration (identifier) @name . (parameter_list)) @definition.method)
(source_file (class_definition) (ERROR) (declaration (identifier) @name .) @definition.method . (parenthesized_expression) . (closure))
(source_file (class_definition) (ERROR) (declaration [(access_modifier) (modifier)] .) . (function_call (identifier) @name (argument_list (closure) .)) @definition.method)

; The class header itself when an error earlier in the file left the tokens loose under a doc
; comment that never closed (a one-line doc comment is masked before the parse, but the mask
; is refused when it does not help): "class", then the name (with its supertypes, in an ERROR).
; A doc comment cannot hold a class keyword in a tree that was read properly.
(groovy_doc "class" . (ERROR . (identifier) @name) @definition.class)
(groovy_doc "class" . (identifier) @name @definition.class)

; closures assigned to a field or script variable
(class_definition (closure (declaration name: (identifier) @name (closure)) @definition.method))
(source_file (declaration name: (identifier) @name (closure)) @definition.function)

; Gradle tasks
(source_file (identifier) @_t . (juxt_function_call (identifier) @name) @definition.function (#eq? @_t "task"))
(source_file (identifier) @_t . (function_call (identifier) @name) @definition.function (#eq? @_t "task"))
(function_call (identifier) @_t (argument_list . (string (string_content) @name))) @definition.function
  (#eq? @_t "task")
(function_call (dotted_identifier (identifier) @_o (identifier) @_m) (argument_list . (string (string_content) @name))) @definition.function
  (#eq? @_o "tasks")
  (#any-of? @_m "register" "create" "maybeCreate")
(function_call (dotted_identifier (identifier) @_o (identifier) @_m) (argument_list . (string (string_content) @name))) @definition.function
  (#eq? @_m "task")
`, true)
	registerNameFix("groovy", groovyName)
	registerImportFix("groovy", groovyImport)
}

// groovyImport keeps the class an aliased import names. `import a.b.Formatter as
// Fmt` is two facts, that the file imports the class a.b.Formatter and that it
// writes it `Fmt`, and the row for each is kept: the first is read as Java's
// import of a class is, and the second says what a call qualified by `Fmt` is
// qualified by.
func groovyImport(_ string, imp Import) []Import {
	if imp.Alias == "" {
		return []Import{imp}
	}
	plain := imp
	plain.Alias = ""
	return []Import{plain, imp}
}

// groovyName removes the quotes of a Spock feature title (`"adds two numbers"`),
// which is a quoted_identifier node that includes them.
func groovyName(name string) string {
	if len(name) >= 2 && (name[0] == '"' || name[0] == '\'') && name[len(name)-1] == name[0] {
		return strings.TrimSpace(name[1 : len(name)-1])
	}
	return name
}
