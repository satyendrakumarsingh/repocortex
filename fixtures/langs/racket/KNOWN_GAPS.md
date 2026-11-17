# Racket known gaps

- Definitions inside a top-level `(begin ...)`, which Racket splices into the
  module, are not extracted; only forms directly at the top of the file or in
  a `module`, `module*` or `module+` are.
- A `module` nested inside another `module` is not extracted as a definition
  of its own, nor are the forms inside it.
- `struct` fields and the accessors a struct generates (`point-x`) are not
  held.
- Macro-defined definitions (`define-simple-macro` uses, `define-runtime-path`,
  `define-logger`) and `define-syntax-parameter` are not recognised.
