# Swift: known gaps

## A declaration with attributes starts on the attribute line

`@inlinable` / `@available(...)` / `@discardableResult` before a `func`, `var`,
`init` or `subscript` is part of the grammar's `modifiers` node, which is the
first child of the declaration, so the symbol's start line is the attribute's
line, not the line of `func`. The name and kind are right. Types are not
affected (`@propertyWrapper struct X` starts at `struct`). The range of a
symbol must be the whole declaration (callers inside the body are attributed
by it), so the capture cannot be narrowed to the keyword.

```
(function_declaration (modifiers (attribute ...) (visibility_modifier)) (simple_identifier) ...)
```

## The grammar loses a whole region on newer syntax

When the grammar cannot read a construct it emits `ERROR` nodes and often
swallows the declarations around it; nothing can be captured from inside an
`ERROR`. The parse status of such a file is `partial`. Seen in real code:

- typed throws, `throws(Failure)` after a parameter list;
- `nonisolated(nonsending)` as a modifier;
- an attribute applied to a parenthesised function type in a parameter, as
  in `_ factory: @escaping @Sendable (String) -> any LogHandler`, when it is
  followed by more such parameters;
- an `#if` / `#elseif` chain that declares the same property with a multi-line
  initialiser in each branch (only one branch is read).

```
(source_file (comment)... (ERROR (modifiers (visibility_modifier)) (simple_identifier) (inheritance_specifier ...) (property_declaration ...)))
```

Declarations after such a region in the same file are usually lost too. The
fix belongs in the grammar, not in a query.

## A class the grammar gives up on

The cause that was measured, an attribute on a function type
(`@escaping @Sendable (_ data: Data?) -> Void`, `(@Sendable (...) -> Void)?`),
is dealt with before the parse: `mask_swift.go` overwrites `@Sendable` and
`@MainActor` with spaces when only white space separates them from `(`, and the
file is parsed a second time under the guard the other masks use (`Fetcher.swift`
and `RequestBase.swift`: every definition, `validate` and `response` included).
The recovery below stays for any other construct that makes the parser give up,
and for a file where the guard refuses the mask. What the mask does not touch:
an attribute with its own arguments (`@MainActor(unsafe)`), and `@Sendable`
before a declaration.

What follows describes the tree when the mask is not applied.

Two methods in a row, one taking `@escaping @Sendable (_ data: Data?) -> Void`
and the next a parameter typed `(@Sendable (_ data: Data?) -> Void)?`, make the
parser give up on the whole file (Alamofire's `DownloadRequest.swift`, reduced
in `Fetcher.swift`). The root is an `ERROR` and the class is no longer a
`class_declaration`: its modifiers, `class`, name and supertypes are loose
tokens, with the members the parser could still read as sibling nodes and the
rest as tokens:

```
(source_file (ERROR (import_declaration) (modifiers) "class" (simple_identifier)
  ":" (inheritance_specifier) "," (attribute) (inheritance_specifier) "{"
  (class_declaration ...) (property_declaration ...) (function_declaration ...)
  (modifiers) "func" (simple_identifier) "(" (simple_identifier) ... (ERROR "_ data: Data?) -> Void) -> Self {...")))
```

Under an `ERROR` that holds a type keyword the query takes: the type
(`"class"` then its name), `function_declaration`, `property_declaration` and
`typealias_declaration` nodes, `"func"` followed by a name, a lone `ERROR`
child that is only modifiers, `func` and a name ("private func cancel": the
name is the last word of its text), `typealias` followed by a name, and a
generic argument list that swallowed the declarations after it
(`Protected<Downstream?>`, whose `let request:` comes out as `let` and the
label `request:`). A property is taken only with an access level or `static`,
because a local `let x = 1` of a function body that was lost looks the same.

Not recovered, and not recoverable from the tree: a method whose `func` and
name are both inside the text of a deeper `ERROR` (`validate`, `_response<...>`
and the generic `response` and `responseDecodable` overloads of
`DownloadRequest.swift`), because the name is not a node and the nesting says
nothing about the type it belongs to; and a property without an access level or
`static`. A nested local function that sits loose in an `ERROR` of a function
body (`func sendData()` in a test method) is recorded as a method.

## `extension` of a type that is not a name

`extension [Foo]`, `extension [K: V]` and `extension (A, B)` are recorded under
the type as written (`[Foo]`, `[K: V]`, `(A, B)`, with white space collapsed);
`extension Array<Foo>` is `Array`. Two extensions of the same array type are
two symbols with the same name.

## Not extracted by design

Accessors (`get`, `set`, `willSet`, `didSet`), the members of an enum case's
associated tuple, `macro` declarations (the grammar parses `macro f() = ...` as
an error), `precedencegroup`, and `import`.
