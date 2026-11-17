package parse

// Agda. What the index holds, see fixtures/langs/agda/expected.tsv.
//
// An Agda function is a type signature followed by equations, and the grammar
// reads both as `function` nodes (the signature's right-hand side starts with
// a colon, an equation's with an equals sign). Agda requires the signature, so
// the signature is the definition: a function is held once however many
// equations it has, and an operator or mixfix function whose equations have no
// name of their own (`zero +++ m = m`) is held under the name its signature
// gives (`_+++_`). No clause merging is needed, and none is registered.
//
//   - module M and module M = N are packages;
//   - data and record are classes; the fields of a record are fields;
//   - a signature at the top level, in a module, or directly under private,
//     abstract, instance, mutual or postulate is a function (a postulate is a
//     function too, so a postulated type is held as one).
//
// Data constructors, pattern synonyms, equations, open and import lines, and
// anything bound in a where clause are not definitions (see KNOWN_GAPS.md for
// what the grammar does to where clauses).
func init() {
	registerDefinitions("agda", agdaDefinitions, true)
}

const agdaDefinitions = `
(module (module_name) @name) @definition.module
(module_macro . (module_name) @name) @definition.module
(data (data_name) @name) @definition.class
(record (record_name) @name) @definition.class
(fields (signature (field_name) @name) @definition.field)

(source_file (function (lhs (function_name) @name) (rhs) @_r (#match? @_r "^:")) @definition.function)
(module (function (lhs (function_name) @name) (rhs) @_r (#match? @_r "^:")) @definition.function)
(private (function (lhs (function_name) @name) (rhs) @_r (#match? @_r "^:")) @definition.function)
(postulate (function (lhs (function_name) @name) (rhs) @_r (#match? @_r "^:")) @definition.function)
(mutual (function (lhs (function_name) @name) (rhs) @_r (#match? @_r "^:")) @definition.function)
(abstract (function (lhs (function_name) @name) (rhs) @_r (#match? @_r "^:")) @definition.function)
(instance (function (lhs (function_name) @name) (rhs) @_r (#match? @_r "^:")) @definition.function)`
