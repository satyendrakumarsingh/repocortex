package parse

// Emacs Lisp. What the index holds, see fixtures/langs/elisp/expected.tsv.
//
// Only a form at the top level of the file counts. A defun inside another
// function, a `when`, a `progn` or an `eval-and-compile` is not held, and nor
// is a defvar inside a function body.
//
//   - defun, defsubst and defmacro are functions (macros are functions), and
//     so are define-inline, cl-defun, cl-defsubst, cl-defmacro, cl-defgeneric,
//     `(defalias 'name ...)`, define-minor-mode, define-derived-mode and
//     define-globalized-minor-mode, which each define a function;
//   - cl-defmethod is a method, named by the generic function it specialises;
//   - defvar, defvar-local, defcustom and defface are variables; defconst is a
//     constant. A bare `(defvar x)` only declares the name special and is not
//     a definition;
//   - cl-defstruct and defclass are classes (cl-defstruct takes its name from
//     `(name options...)` too).
//
// Not definitions: defgroup, define-error, require, provide, setq, hooks and
// calls.
func init() {
	registerDefinitions("elisp", elispDefinitions, true)
}

const elispDefinitions = `
(source_file (function_definition name: (symbol) @name) @definition.function)
(source_file (macro_definition name: (symbol) @name) @definition.function)

(source_file
  (list . (symbol) @_k . (symbol) @name
    (#any-of? @_k "define-inline" "cl-defun" "cl-defsubst" "cl-defmacro" "cl-defgeneric"
      "define-minor-mode" "define-derived-mode" "define-globalized-minor-mode")) @definition.function)
(source_file
  (list . (symbol) @_k . (quote (symbol) @name)
    (#eq? @_k "defalias")) @definition.function)
(source_file
  (list . (symbol) @_k . (symbol) @name
    (#eq? @_k "cl-defmethod")) @definition.method)

(source_file
  (list . (symbol) @_k . (symbol) @name
    (#any-of? @_k "defvar-local" "defcustom" "defface")) @definition.variable)
(source_file (special_form "defvar" . (symbol) @name . (_)) @definition.variable)
(source_file (special_form "defconst" . (symbol) @name . (_)) @definition.constant)

(source_file
  (list . (symbol) @_k . (symbol) @name
    (#any-of? @_k "cl-defstruct" "defclass")) @definition.class)
(source_file
  (list . (symbol) @_k . (list . (symbol) @name)
    (#eq? @_k "cl-defstruct")) @definition.class)
`
