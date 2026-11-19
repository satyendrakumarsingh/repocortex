# Agda known gaps

The Agda grammar is layout-sensitive and loses its place on several common
forms. Most of them are recovered; `Recovery.agda` holds each with definitions
after it, and `internal/parse/mask_agda.go` explains the mechanism. Three
things the grammar reads wrongly are repaired before the definitions are read:

- an expression of three or more atoms in a row (`n +++ n`, `Vec A zero`,
  `g (h x) y`), in an equation, a signature, a constructor or a field, which
  takes in the lines after it: the expression's text is overwritten;
- a `where` block (or a `let`) in an equation, whose local signatures would be
  read as top-level functions: the equation's right-hand side is overwritten;
- `data Vec (A : Set) : Nat → Set where`, parameters together with an index:
  the parameters are overwritten.

A file with one of these is parsed twice, and the second parse is kept only
if it has every definition the first had, apart from those inside a where
block. The result then says `read with N constructs masked`, and the text of a
masked signature reads `f : _` in the index.

`field x : T` on one line stays an `ERROR "field"` in the tree (the file is
reported as partial); the query reads the signature after it as a field.

## What remains

- `field x y : T` (several names after a one-line `field`): the grammar reads
  `x y` as the arguments of a function, so neither name is a field. Only the
  first name of `field x : T` is recovered, and only when it is alone.
- A where block or let in a signature, or an expression of three atoms in a
  `data` or `record` header (`data D : Vec A n → Set`): not masked.
- Local definitions in a where block are never indexed, by design (they are
  not top level); a function that is only reachable through a where block of
  another is not found.
