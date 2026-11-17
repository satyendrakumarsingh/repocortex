# Prolog known gaps

## A rule's symbol covers only its head and first goal

The grammar reads an operator chain left to right with no precedence, so
`area(circle(R), A) :- pi_approx(Pi), A is Pi * R * R.` becomes a nest of
`infix_operator` nodes, one per operator, with the head at the bottom:

```
clause_term
  compound_term
    infix_operator            (* R)
      left: compound_term
        infix_operator        (* R)
          left: compound_term
            ... 
              infix_operator  (:-)
                left: compound_term  area(circle(R), A)
                right: compound_term pi_approx(Pi)
```

No query can reach the head from the clause at an unknown depth, so a rule is
found by the `:-` (or `-->`) whose left operand is the head. The node that
carries the definition is that innermost `infix_operator`: the symbol starts
at the right line with the right name, and ends after the first goal of the
body. Facts are matched from the clause itself and cover the whole clause.

## User-defined operators

```prolog
:- op(700, xfx, likes).
X likes Y :- colour(X, Y).
```

The grammar does not know `likes` is an operator. It breaks the clause into
`X`, `likes` and `Y :- colour(X, Y).` as three clauses, so the rule is not
found as a definition of `likes` (the fragments end with a zero-width `end`
node, which the query rejects, so no wrong symbol is made either).

## Quoted-atom functors

```prolog
'Quoted Name'(1).
```

The grammar reads this as the atom `'Quoted Name'` followed by the bracketed
term `(1)`, two clauses, so a predicate whose name is quoted is not found.

## Module-qualified heads

`lists:append(A, B, C) :- ...` is read as an operator chain on `:`; the head is
not found.
