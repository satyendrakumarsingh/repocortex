# Haxe: forms that are not extracted, and why

The Haxe grammar is the limiting factor. It has no node for an enum or an
abstract, no name node for a constructor, and when it meets a construct it
cannot read it does not recover locally: the parse of everything after it, up
to the whole file, changes shape (`(ERROR ...)` wrapping, class members left
floating at module level). On the five real files used to measure this
(hscript), most of the loss is of that kind. The query cannot repair a tree
that does not contain the name.

Read before the parse, since the construct is the whole cause (`mask_haxe.go`,
second parse under the shared guard; `Expression.hx`, `Generics.hx`):

- a short-form object typedef (`typedef Argument = { name : String, ?t : CType };`,
  `{> Base, var z : Int }`): the inside of the braces is overwritten, the typedef
  is still a `type_alias`, and the declarations after it are read;
- metadata before a type (`@:autoBuild(...) extern interface Live`): overwritten
  when a type keyword follows it. The metadata of a class, a function and a
  variable that the grammar does read is taken by the query;
- a constraint on a type parameter (`class Box<T:Dynamic>`, `function f<K:Foo>()`);
- an empty `default:` that ends a switch, and an empty `case A(_), B(_):`;
- an `abstract` after other declarations (a query on the ERROR node that holds
  the keyword), its constructor, and its methods, which are read as a block
  directly in the module and recorded as methods.

Constructs that break the parse and are still not read, each seen in real code
(the first of these is only partly true on the fixture: the enums of
`Expression.hx` are read once the typedefs are masked, but a file the mask is
refused for still comes back as one ERROR):

- **Object-literal typedefs and enum constructors with arguments** in the same
  file as other declarations: `typedef Argument = { name : String, ?t : CType };`
  and `enum Expr { EConst( c : Const ); ... }`. A file with enums, `#if`
  blocks and `{> Base, ...}` typedefs (hscript's `Expr.hx`) comes back as one
  ERROR node; its `enum` and `class` declarations are no longer separate
  nodes, only the two block-form typedefs are found. The same enums parse
  correctly when the file is cut short before the first object-literal
  typedef, which is how the fixture keeps them (`types.hx`).
- **A class header with `extends` after other errors**: the class name is not
  in the tree (`class Colorizer extends Parser {` becomes
  `(ERROR (type_name) (variable_declaration ...) ...)`), so the class and its
  fields are not extracted. Methods inside are found, as methods.
- **Members of a class whose closing brace the parser lost** are left as
  `function_declaration` children of `module`. They are recorded as `function`
  rather than `method`, since a function at module level is also legal Haxe
  and the two cannot be told apart. A method of an `abstract` is in the same
  position and is also recorded as `function`.
- **Functions after a `try`/`catch` or `switch` with patterns** in the same
  class may be lost entirely (`executeScriptFile` in hscript's `RunScript.hx`).
- **Constructors**: `function new` has no identifier node; it is read from the
  anonymous `new` token, and only inside a class body.
- **Expression-bodied functions** (`function f():Int return 1;`), `macro`
  functions and `enum abstract` are read when they stand alone in a class
  (`Expression.hx`), and are listed here because in the real file they were
  seen to break the parse around them; no case of that is kept in the fixture,
  and nothing is recovered from one if it happens.
- **`extern interface` with metadata above it** (`@:autoBuild(...)`): the
  interface is lost to an ERROR node (hscript `Live.hx`).

How the extraction recovers what it can: an enum, from the `enum` keyword node
followed by the name; an abstract, from an ERROR node whose text starts with
`abstract`; a class or interface whose own node is missing, from an ERROR node
whose text starts with `class` or `interface`. All three select on the text, so
they match nothing else.
