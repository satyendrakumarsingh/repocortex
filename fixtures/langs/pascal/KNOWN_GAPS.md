# Pascal known gaps

- Subrange types (`TSize = 0..255;`, `TDigit = '0'..'9';`). The grammar cannot
  parse the `..` after `=`: it wraps the `=` in an ERROR node and takes the
  identifier of the NEXT declaration into it, so the type itself is not
  extracted and the declaration that follows it loses its name too:

  ```
  (declType (identifier) (ERROR (kEq) (ERROR) (type (typeref (identifier)))) (kEq) (declIntf ...))
  ```

  A subrange followed by another type declaration therefore drops both. The
  fixture leaves subranges out so the other forms are tested.
- Generic types and method bodies (`TBox<T> = class ... end;` and
  `procedure TBox<T>.Put(V: T);`). The generic declaration puts the name inside
  a node of its own that the queries do not look into, so the class and the
  body are not extracted; the members declared inside the class still are.
