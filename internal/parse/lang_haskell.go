package parse

// Haskell. What the index holds, see fixtures/langs/haskell/expected.tsv.
//
//   - module M is a package;
//   - a top-level equation is a function: `f x = ...`, `f = ...`, an operator
//     definition (`a |+| b = ...`) and a back-quoted one (x `plus` y = ...).
//     A function of several equations is written as one `function` node per
//     equation; consecutive equations of one name are folded into one symbol.
//     The type signature is not a definition of its own: the equation is, so a
//     function is held once whether or not it carries a signature, and a
//     point-free value (`total = sum . map area`) is a function too, since
//     without type inference the two cannot be told apart;
//   - `foreign import` names a function;
//   - data, newtype and data family are classes; type synonyms and type
//     families are type aliases; class is an interface, and the methods
//     declared in its body (signature lines) are methods;
//   - the fields of a record constructor are fields.
//
// Instance bodies, where-bound and let-bound names, data constructors,
// pattern synonyms and the signatures themselves are not definitions.
func init() {
	registerDefinitions("haskell", haskellDefinitions, true)
	registerClauseMerge("haskell", nil)
}

const haskellDefinitions = `
(header (module) @name) @definition.module

(declarations (function . (variable) @name) @definition.function)
(declarations (bind . (variable) @name) @definition.function)
(declarations (function . (infix (_) . (operator) @name)) @definition.function)
(declarations (function . (infix (_) . (infix_id (variable) @name))) @definition.function)
(declarations (foreign_import (signature . (variable) @name)) @definition.function)

(declarations (data_type . (name) @name) @definition.class)
(declarations (newtype . (name) @name) @definition.class)
(declarations (data_family . (name) @name) @definition.class)
(declarations (type_synomym . (name) @name) @definition.type_alias)
(declarations (type_family . (name) @name) @definition.type_alias)
(declarations (class (name) @name . (type_params)) @definition.interface)
(class_declarations (signature . (variable) @name) @definition.method)

(field (field_name (variable) @name)) @definition.field
`
