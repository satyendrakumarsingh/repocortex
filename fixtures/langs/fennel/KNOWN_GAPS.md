# Fennel known gaps

## `(macros {...})`

```fennel
(macros {:twice (fn [x] `(* 2 ,x))
         :thrice (fn [x] `(* 3 ,x))})
```

The macros of a `macros` table are keyed by a string node (`":twice"`), and
the grammar keeps the colon in that node's text, so the only name a query can
capture is `:twice` rather than `twice`. They are not extracted; `(macro name
...)` is.

## Destructuring bindings

`(local {: left : right} (values 1 2))` and `(local [a b] [1 2])` bind names
from the shape of a value. Most of them are `require` imports, and the shape
of the right-hand side does not say which names are definitions, so none is
extracted.

## `fn Obj:method` is misread

```fennel
(fn Account:deposit [amount] ...)
(fn Account:balance-of [] ...)
```

The grammar has no symbol for `Account:deposit`. It reads the first as an
`fn_form` named `Account` followed by `ERROR ":deposit"`, and the second as an
`ERROR "Account:"` followed by the name `balance-of`. The two shapes differ by
the name's spelling, so no pattern can name the method reliably. The form is
not extracted (the first would be recorded as a function called `Account`, so
the query is not made to accept it), and the file's parse status is `partial`.
