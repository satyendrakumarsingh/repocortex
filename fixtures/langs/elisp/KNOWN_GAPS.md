# Emacs Lisp known gaps

- Definitions wrapped in `progn`, `eval-and-compile`, `eval-when-compile`,
  `with-eval-after-load` or a `when` / `unless` guard are not extracted; only
  forms at the top of the file are. Emacs configuration files often wrap
  definitions this way.
- `defgroup`, `define-error`, `define-obsolete-function-alias` and
  `define-abbrev-table` define names but have no matching kind, and are not
  extracted. `defalias` is.
- `cl-defstruct` slots, `defclass` slots and the functions a struct generates
  (`make-NAME`, `NAME-slot`) are not held.
