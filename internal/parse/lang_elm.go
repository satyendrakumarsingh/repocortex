package parse

// Elm. What the index holds, see fixtures/langs/elm/expected.tsv.
//
//   - module (also `port module`) is a package;
//   - a top-level value or function is a function, once: the type annotation
//     above it is not a definition of its own, and a constant (`limit = 10`)
//     is held as a function too, since a zero-argument function and a value
//     are one thing in Elm. Names bound by let are local and are not held;
//   - `type` is a class (a custom type; its variants are not held), and
//     `type alias` is a type alias;
//   - `port name : ...` is a function.
//
// Record fields, union variants and imports are not definitions.
func init() {
	registerDefinitions("elm", elmDefinitions, true)
}

const elmDefinitions = `
(module_declaration (upper_case_qid) @name) @definition.module
(file (value_declaration (function_declaration_left . (lower_case_identifier) @name)) @definition.function)
(file (port_annotation . (port) . (lower_case_identifier) @name) @definition.function)
(file (type_declaration . (type) . (upper_case_identifier) @name) @definition.class)
(file (type_alias_declaration . (type) . (alias) . (upper_case_identifier) @name) @definition.type_alias)
`
