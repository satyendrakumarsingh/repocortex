package parse

// Haxe. What the index holds, see fixtures/langs/haxe/expected.tsv and
// KNOWN_GAPS.md.
//
// The grammar is weak: it has no node for an enum or an abstract, no name for
// a constructor, and a file with a construct it cannot read (a switch with
// patterns, a conditional-compilation block) is wrapped in ERROR nodes. The
// query therefore does not anchor on `module`, and reads an enum or an
// abstract from the sibling tokens the parser leaves behind (a `keyword` node
// with the text "enum", or an identifier with the text "abstract", directly
// followed by the name). Those patterns select on the keyword text, so they do
// not match other declarations.
//
//   - class and interface are classes and interfaces; enum and abstract are
//     classes; typedef is a type_alias;
//   - a function in a class or interface body, or one the parser left loose in
//     an ERROR node, is a method; `new` is a method named new; a function
//     anywhere else is a function;
//   - a var in a class or interface body is a field (static or not, final or
//     not);
//   - enum constructors, local variables and function arguments are not
//     definitions.
func init() {
	registerDefinitions("haxe", haxeDefinitions, true)
}

const haxeDefinitions = `
(class_declaration . (identifier) @name) @definition.class
(interface_declaration . (identifier) @name) @definition.interface
(typedef_declaration . (identifier) @name) @definition.type_alias
; metadata (@:keep, @:generic) is the first child of the declaration it applies to
(class_declaration (metadata) . (identifier) @name) @definition.class
(interface_declaration (metadata) . (identifier) @name) @definition.interface
(typedef_declaration (metadata) . (identifier) @name) @definition.type_alias

(function_declaration . (identifier) @name) @definition.function
(function_declaration (metadata) . (identifier) @name) @definition.function
(class_declaration (block (function_declaration . (identifier) @name) @definition.method))
(class_declaration (block (function_declaration (metadata) . (identifier) @name) @definition.method))
(interface_declaration (block (function_declaration . (identifier) @name) @definition.method))
(interface_declaration (block (function_declaration (metadata) . (identifier) @name) @definition.method))
(class_declaration (block (function_declaration "new" @name) @definition.method))

(class_declaration (block (variable_declaration . (identifier) @name) @definition.field))
(class_declaration (block (variable_declaration (metadata) . (identifier) @name) @definition.field))
(interface_declaration (block (variable_declaration . (identifier) @name) @definition.field))

((keyword) @kw . (identifier) @name @definition.class (#eq? @kw "enum"))
((keyword) @kw . (ERROR . (identifier) @name @definition.class .) (#eq? @kw "enum"))
((ERROR . (call_expression . (identifier) @name @definition.class)) @e
  (#match? @e "^(private[ \t]+|extern[ \t]+|enum[ \t]+)*abstract[ \t]"))

; an abstract after other declarations: the keyword is an ERROR node of its own, the name
; the identifier that follows it
((ERROR) @e . (identifier) @name @definition.class
  (#match? @e "^((private|extern|enum)[ \t]+)*abstract$"))

((ERROR . (identifier) @name @definition.class) @e
  (#match? @e "^(private[ \t]+|final[ \t]+|extern[ \t]+)*class[ \t]"))
((ERROR . (identifier) @name @definition.interface) @e
  (#match? @e "^(private[ \t]+|extern[ \t]+)*interface[ \t]"))
(ERROR (function_declaration . (identifier) @name) @definition.method)
; the constructor of an abstract is left loose in the module, or in an ERROR; "new" is not a
; function name anywhere else, so it is a method wherever it is
(module (function_declaration "new" @name) @definition.method)
(module (block (function_declaration "new" @name) @definition.method))
; the body of an abstract, read as a block directly in the module (nothing else is one)
(module (block (function_declaration . (identifier) @name) @definition.method))
(ERROR (function_declaration "new" @name) @definition.method)
`
