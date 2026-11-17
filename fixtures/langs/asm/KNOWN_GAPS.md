# Assembly known gaps

- The grammar has no sections and no symbol visibility, so a `.globl` label
  and a private one look alike, and a label in a data section
  (`counter: .quad 0` after `.data`) is extracted as a function like a code
  label. The fixture leaves data labels out for that reason. Telling them
  apart would need the section directive that precedes the label, which is a
  sibling node, not part of the label.
- `.type main, @function` (GNU syntax with `@`) and AT&T operands such as
  `%rbp` are read as ERROR nodes, so a file written in that syntax is
  reported `partial`. Labels around the errors are still extracted; the
  directive is not a definition either way.
- Macros (`.macro name`) and `.equ` / `.set` symbols are not extracted.
