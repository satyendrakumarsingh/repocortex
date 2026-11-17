package parse

// Fennel. What the index holds, see fixtures/langs/fennel/expected.tsv.
//
// Only a form at the top level of the file counts; a `fn`, `local` or `var`
// inside a function, a `let`, a `when` or an `each` is a local.
//
//   - fn, lambda and λ with a name are functions, and so is macro (macros are
//     functions). A dotted name (`(fn M.method [self] ...)`) is named by its
//     last part, as a Lua `function M.method` is;
//   - `(local f (fn ...))`, the same with var and global, and the same with a
//     lambda are functions;
//   - any other `(local x value)`, `(var x value)` or `(global x value)` that
//     binds one plain name is a variable;
//   - `(set M.f (fn ...))` is a function named f. A set of a plain value is
//     not a definition.
//
// Not definitions: `(fn Obj:method ...)` (the grammar misreads the name, see
// KNOWN_GAPS.md), destructuring bindings (`(local {: a} ...)`), the macros
// table of `(macros {...})`, import-macros, require, calls, and anything in a
// nested form.
func init() {
	registerDefinitions("fennel", fennelDefinitions, true)
}

const fennelDefinitions = `
(program (fn_form name: (symbol) @name) @definition.function)
(program (fn_form name: (multi_symbol member: (symbol_fragment) @name .)) @definition.function)
(program (lambda_form name: (symbol) @name) @definition.function)
(program (lambda_form name: (multi_symbol member: (symbol_fragment) @name .)) @definition.function)
(program (macro_form name: (symbol) @name) @definition.function)

(program (local_form (binding_pair lhs: (symbol_binding) @name rhs: [(fn_form) (lambda_form)])) @definition.function)
(program (var_form (binding_pair lhs: (symbol_binding) @name rhs: [(fn_form) (lambda_form)])) @definition.function)
(program (global_form (binding_pair lhs: (symbol_binding) @name rhs: [(fn_form) (lambda_form)])) @definition.function)
(program (local_form (binding_pair lhs: (symbol_binding) @name) @_p (#not-match? @_p "^[^\\s]+\\s+\\((fn|lambda|λ)[\\s\\[]")) @definition.variable)
(program (var_form (binding_pair lhs: (symbol_binding) @name) @_p (#not-match? @_p "^[^\\s]+\\s+\\((fn|lambda|λ)[\\s\\[]")) @definition.variable)
(program (global_form (binding_pair lhs: (symbol_binding) @name) @_p (#not-match? @_p "^[^\\s]+\\s+\\((fn|lambda|λ)[\\s\\[]")) @definition.variable)

(program
  (set_form lhs: (multi_symbol member: (symbol_fragment) @name .) rhs: [(fn_form) (lambda_form)]) @definition.function)
`
