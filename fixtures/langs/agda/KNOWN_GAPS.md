# Agda known gaps

The Agda grammar is layout-sensitive and loses its place on several common
forms. None of these is in `expected.tsv`; each is a form the grammar parses
wrongly, and a query cannot repair it.

## `where` clauses with local signatures

```agda
triple : Nat → Nat
triple n = go n
  where
    go : Nat → Nat
    go m = m +++ double m

next : Nat
next = zero
```

The `where` keyword becomes a `function` node of its own (`function "where"`),
the local `go : Nat → Nat` is read as a top-level function, and the local
equation starts an `ERROR` that runs to the end of the file. `go` is therefore
indexed as if it were top level, and every definition after the `where` block
is missing. A one-line `where k = suc` does not do this.

## Indexed data with parameters

```agda
data Vec (A : Set) : Nat → Set where
  nil  : Vec A zero
  cons : {n : Nat} → A → Vec A n → Vec A (suc n)
```

A `data` with both parameters and an index type fails: the whole file becomes
one `ERROR` node (`ERROR "module F where..."`) and nothing in it is extracted.
`data Index : Nat → Set` (an index, no parameters) and `data List (A : Set) :
Set` (parameters, no index) parse.

## A right-hand side of three or more atoms swallows what follows

```agda
double : Nat → Nat
double n = n +++ n

unit : Nat
unit = zero

apply : Nat → Nat
apply f = f
```

The right-hand side `n +++ n` (also `f n n`, or a `let ... in` body) runs on as
one expression: it takes in the next lines as atoms
(`rhs "= n +++ n..."` holds `unit`, `:`, `Nat`, `unit`, `=` and so on), so the
following signature and sometimes the following definitions are not found. A
right-hand side of one or two atoms (`suc n`, `f x`, `suc (n +++ m)`) does not
do this. This is the most damaging gap: how much of a file is lost depends on
how its equations are written. The fixture keeps its equations short for that
reason.

## A record field on the same line as `field`

```agda
record Wrapper : Set where
  field unwrap : Nat
```

The grammar reports `ERROR "field"` and reads `unwrap : Nat` as a function
inside the record, so `unwrap` is not a field. `field` on its own line, with
the signatures indented below it, works.
