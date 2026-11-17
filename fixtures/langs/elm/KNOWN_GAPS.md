# Elm known gaps

- Operator definitions: `(|>>) x f = f x` and `infix left 0 (|>>) = pipeTo`.
  The grammar reads the parenthesised operator as a unit pattern and fills the
  declaration with ERROR nodes, so nothing is extracted for either line. Elm
  0.19 only allows them in core packages, so application code rarely has them.

  ```
  (value_declaration (pattern (unit_expr (ERROR))) (ERROR ...) (function_call_expr ...))
  (infix_declaration (infix) (operator_identifier) (ERROR) (value_expr ...))
  ```
