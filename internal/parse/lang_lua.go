package parse

// Lua. What the index holds, see fixtures/langs/lua/expected.tsv.
//
// The inferred query reads `function M.f` as a method, misses `function M:m`
// entirely and knows no assignment form, so it is replaced.
//
//   - function f(), local function f() and `local f = function` are functions,
//     at any depth (a nested local function is still a named function);
//   - function M.f() and function M.a.b.f() are functions named f;
//   - function M:m() is a method named m (the colon passes self);
//   - `M.f = function` and `Name = function` are functions when the statement
//     is at the top level of the file (inside a function body they assign to
//     an existing field, such as self.callback, and declare nothing);
//   - a function-valued field of a table that is assigned or returned
//     (`M = { f = function() end }`, `return { f = function() end }`) is a
//     function, whether the key is a name or a string. A table passed as an argument (setmetatable(t, { __index =
//     function ... })) is not a declaration of anything. Tables nested up to
//     three levels deep count;
//   - a top-level `local T = {}` or global `T = {}` is a variable: it is the
//     module or class object the functions hang off.
//
// Other locals, constants and fields that are not functions are not
// definitions.
func init() {
	registerDefinitions("lua", luaDefinitions, true)
}

const luaDefinitions = `
; Calls. f(x), a.b.f(x) and o:m(x); the call node begins at the receiver, so the
; text before the name is how shape.go reads what the call was made on.
(function_call . (identifier) @name (#not-eq? @name "require")) @reference.call
(function_call . (dot_index_expression (identifier) @name .)) @reference.call
(function_call . (method_index_expression (identifier) @name .)) @reference.call

(function_declaration . (identifier) @name) @definition.function
(function_declaration . (dot_index_expression (identifier) @name .)) @definition.function
(function_declaration . (method_index_expression (identifier) @name .)) @definition.method

(variable_declaration
  (assignment_statement
    (variable_list . (identifier) @name .)
    (expression_list . (function_definition))) @definition.function)
(chunk
  (assignment_statement
    (variable_list . (identifier) @name .)
    (expression_list . (function_definition))) @definition.function)
(chunk
  (assignment_statement
    (variable_list . (dot_index_expression (identifier) @name .) .)
    (expression_list . (function_definition))) @definition.function)

(chunk
  (assignment_statement
    (variable_list . (identifier) @name .)
    (expression_list . (table_constructor))) @definition.variable)
(chunk
  (variable_declaration
    (assignment_statement
      (variable_list . (identifier) @name .)
      (expression_list . (table_constructor)))) @definition.variable)

(assignment_statement
  (expression_list
    (table_constructor
      (field . [(identifier) @name (string (string_content) @name)] . (function_definition)) @definition.function)))
(assignment_statement
  (expression_list
    (table_constructor
      (field (table_constructor
        (field . [(identifier) @name (string (string_content) @name)] . (function_definition)) @definition.function)))))
(assignment_statement
  (expression_list
    (table_constructor
      (field (table_constructor
        (field (table_constructor
        (field . [(identifier) @name (string (string_content) @name)] . (function_definition)) @definition.function)))))))
(return_statement
  (expression_list
    (table_constructor
      (field . [(identifier) @name (string (string_content) @name)] . (function_definition)) @definition.function)))
(return_statement
  (expression_list
    (table_constructor
      (field (table_constructor
        (field . [(identifier) @name (string (string_content) @name)] . (function_definition)) @definition.function)))))
`
