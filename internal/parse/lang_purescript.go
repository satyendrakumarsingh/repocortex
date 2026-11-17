package parse

// PureScript. What the index holds, see fixtures/langs/purescript/expected.tsv.
// The shape is Haskell's.
//
//   - module M is a package;
//   - a top-level equation is a function: `f x = ...` and `f = ...`. A
//     function of several equations is written as one `function` node per
//     equation; consecutive equations of one name are folded into one symbol.
//     The type signature is not a definition of its own (the equation is), so
//     a function is held once whether or not it carries a signature;
//   - `foreign import f :: T` names a function, and `foreign import data T`
//     names a class;
//   - data and newtype are classes; type synonyms are type aliases; class is
//     an interface and the signatures in its body are methods;
//   - the fields of a record newtype and of a record type synonym are fields.
//
// Instance bodies, where-bound and let-bound names, data constructors, fixity
// declarations (`infixl 6 add as +++`), derive declarations and the signatures
// themselves are not definitions.
func init() {
	registerDefinitions("purescript", purescriptDefinitions, true)
	registerClauseMerge("purescript", nil)
}

const purescriptDefinitions = `
(purescript name: (qualified_module) @name) @definition.module

(purescript (function name: (variable) @name) @definition.function)
(purescript (foreign_import name: (variable) @name) @definition.function)
((purescript (kind_value_declaration name: (type) @name) @definition.class)
  (#match? @definition.class "^foreign"))

(purescript (data name: (type) @name) @definition.class)
(purescript (newtype name: (type) @name) @definition.class)
(purescript (type_alias name: (type) @name) @definition.type_alias)
(purescript (class_declaration (class_head (class_name) @name)) @definition.interface)
(class_body (signature name: (variable) @name) @definition.method)

(newtype (newtype_constructor (record_type_literal (row_field (field_name) @name) @definition.field)))
(type_alias (record_type_literal (row_field (field_name) @name) @definition.field))
`
