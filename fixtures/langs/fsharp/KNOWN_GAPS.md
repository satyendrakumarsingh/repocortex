# F#: known gaps

F# is whitespace-sensitive and the grammar reads it with an approximation of
the offside rule. On real code it often gives up part-way through a file; what
follows is what that costs and what the query cannot recover.

## Files the grammar reads wrongly

A type with an `internal` or `private` constructor and `as self`
(`type JsonPickleFormatProvider internal (indent, omitHeader) as self =`), a
`new` constructor with attributes on its arguments, or a long multi-line
expression in a class `let` can make the grammar flatten what follows. The
members and the type header are then siblings at file level, with no
`type_definition` around them:

```
(ERROR (namespace ...) (line_comment) (ERROR) (type_name (identifier))
  (primary_constr_args ...) (type_extension_elements (member_defn ...)) ...)
```

or, without an `ERROR` node at all:

```
(file (long_identifier ..) (import_decl ..) (type_definition ..)
  (type_name (identifier)) (primary_constr_args) (type_extension_elements ..))
```

A `type_name` that is a direct child of the file or of an `ERROR` could be a
union, a record, a class or an interface, so it is not extracted: its kind
would be a guess. Members that survive inside `member_defn` are extracted;
members that end up as a bare `property_or_ident` followed by patterns (for
example `member __.CreateWriter (stream, encoding) =` after such a failure) are
not. In Linguist's `JsonSerializer.fs` this loses all but one definition.

A module that follows another one in a file the grammar mangled can lose its
header entirely (`module Bson =` after `module Json =` in `Combinators.fs`),
and a `let` in it can come out as a bare `value_declaration_left` with no
`function_or_value_defn`, which is not extracted.

## What is read around, and what is not

`internal/parse/mask_fsharp.go` rewrites, before a second parse that the guard
must accept, the constructs the grammar cannot read, and one before the first
parse; `Interop.fsx`, `Modifiers.fsx` and `ConstraintCalls.fsx` hold each form.

- a statically resolved member call, `(^T: (member M: sig) args)`;
- a `when ^T: (member ...)` or `when 'a: struct` clause after a return type,
  up to the `=`;
- an `extern` P/Invoke declaration, rewritten as a function of the same name;
- `let (|Case|_|) = function` with the cases in the column of the `let`;
- `abstract M: a -> b * c`, whose tuple result is read as `_`;
- `#if ... #else ... #endif` inside a body (cond.go's mask, one branch kept:
  a definition only in the other branch is not indexed);
- a bare `///` line, rewritten to `//` and a space before the first parse: the
  grammar reads it as an operator, loses the declaration after it with no
  error, and takes time that grows with the square of the number of such lines.

Not read, measured:

- `let inline f<'T when 'T: (static member Zero: 'T)> () : 'T = 'T.Zero`
  (a static abstract member used as `'T.Zero`) gives error nodes;
- an `and` group (`let rec f ... and g ...`) is one `function_or_value_defn` for
  the grammar. Each function after the first is now tagged at its own pattern
  and every function of the group ends at its own body (`AndGroup.fsx`); an
  `and` value (`and x = 1`) is not separated from the first function's
  definition;
- a few deeply nested expressions in one function (FAKE's `FSIHelper.fs`
  `getCacheInfoFromScript`, Analyzers' `TASTCollecting.fs` `visitExpr`) leave
  the declarations after them unread with no error node;
- a `#if` group the guard refuses (FAKE's `CoreCache.fs`);
- the bare `///` rewrite has no guard, since it is made before the first
  parse: in FAKE's `PaketTemplateHelper.fs` (4 error nodes, one definition
  recovered by luck) the rewritten text reads as 6 error nodes and none, the
  the only definition this rewrite cost in the six repositories measured;
- `module rec A.B` is not a module header for the grammar; the name is read
  (the refiner replaces `rec`), the declarations after it are not;
- about a dozen large files in Fable (`Seq.fs`, `ilwrite.fs`, `Optimizer.fs`)
  and four in Paket still run into the 60 s wall-clock ceiling, in a stall
  that was not isolated.

## Indented namespaces

When the body of `namespace A.B` is indented, the grammar drops the namespace
node and leaves its name as the first `long_identifier` in the file. That is
read as the namespace/module header (kind `package`). It is the only way a
bare `long_identifier` appears there; if a script starts with a lone dotted
name for another reason it would be recorded too.

## Interfaces written as abstract-only classes

`type IShape = abstract Area: unit -> float` has the same tree as a class whose
members are all abstract, so it is a `class`. Only the explicit form
(`interface ... end`) is an `interface`.

## Single-case unions

`type Id = int` is parsed as a union with one case. A field-less union with a
single case and no bar is therefore read as an abbreviation (`type_alias`). A
genuine `type T = A` single-case union is recorded as an alias too.

## Not extracted

`new(...)` additional constructors (no name), class-private `let` bindings,
union cases, record fields, enum members, `let` inside functions, and patterns
that bind several names (`let (a, b) = ...`). Operators and active patterns are
recorded with their punctuation, as written: `(+++)` and `(|Even|Odd|)`.

## Detection

`.fs` files are routed to the Forth grammar, not F#; only `.fsx` and `.fsi`
reach F#. This tree does not change detection.
