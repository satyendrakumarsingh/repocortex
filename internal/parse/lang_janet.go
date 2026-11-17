package parse

// Janet. What the index holds, see fixtures/langs/janet/expected.tsv.
//
// The grammar reads every form as a plain tuple of symbols, so a definition is
// recognised by the symbol that opens the form. Only a form at the top level of
// the file counts: a `def` or `defn` in a function body, a `let` or a `when`
// is a local.
//
//   - defn, defn-, defmacro and defmacro- are functions (macros are
//     functions);
//   - `(def f (fn ...))` and `(def- f (fn ...))` are functions;
//   - any other def, def-, var, var-, defglobal, varglobal or defdyn that
//     binds one plain name is a variable. Janet's def is an immutable binding,
//     but the index does not tell the two apart.
//
// Not definitions: destructuring (`(def {:a x} ...)`, `(def [a b] ...)`),
// import, use, require, calls, and anything in a nested form.
func init() {
	registerDefinitions("janet", janetDefinitions, true)
}

const janetDefinitions = `
(source
  (par_tup_lit . (sym_lit) @_k . (sym_lit) @name
    (#any-of? @_k "defn" "defn-" "defmacro" "defmacro-")) @definition.function)
(source
  (par_tup_lit . (sym_lit) @_k . (sym_lit) @name . (par_tup_lit . (sym_lit) @_v)
    (#any-of? @_k "def" "def-")
    (#eq? @_v "fn")) @definition.function)
(source
  (par_tup_lit . (sym_lit) @_k . (sym_lit) @name
    (#any-of? @_k "def" "def-" "var" "var-" "defglobal" "varglobal" "defdyn")
    (#not-match? @definition.variable "^\\(def-?\\s+[^\\s()]+\\s*\\(\\s*fn[\\s)\\[]")) @definition.variable)
`
