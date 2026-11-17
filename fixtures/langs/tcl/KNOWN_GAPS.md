# Tcl known gaps

- A method whose name is a braced list, `method {set name} {n} { ... }`, is not
  extracted: the grammar reads the braces as a `braced_word` and parses the
  words inside it as commands, so there is no plain name to capture.
- `oo::define CLASS method NAME args body` (the form without a braced script)
  is not extracted; the braced form `oo::define CLASS { method ... }` is.
- A proc written inside an `if`, `foreach` or `catch` block at the top of the
  file, or inside another proc, is not extracted. Tcl creates it only when that
  code runs, so the index cannot say it exists.
- Qualified names are held as written (`::shapes::qualified`). A `proc make`
  inside `namespace eval shapes` is `make`, with no namespace in the name, so
  the same proc can be known by two spellings.
- `set NAME VALUE` is a variable wherever it stands at the top or in a
  namespace body, including a plain assignment in script code (`set handler
  [...]`); Tcl has no declaration that tells the two apart.
