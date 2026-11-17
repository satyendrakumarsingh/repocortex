# Ada known gaps

- In an object declaration of several names, `Left, Right : Integer;`, every
  name is held only when the type is a plain name. `A, B : array (1 .. 3) of
  Integer;` holds `A` and not `B`: the grammar gives the declaration one
  identifier per name followed by the type node, and the query can pair an
  identifier with a following identifier but not with an arbitrary node.
- Numbers declared in a list (`A, B : constant := 1;`) hold only the first
  name, for the same reason.
- The private view of a type (`type T is private;`) is deliberately not a
  definition: the full declaration in the private part is, so a type is held
  once. A private type with no full declaration in the same file (it is in the
  body of another unit) is therefore not held.
- Subprogram bodies given as `separate` stubs and `task body` / `protected
  body` units are not definitions of their own; the task or protected type
  they implement is.
