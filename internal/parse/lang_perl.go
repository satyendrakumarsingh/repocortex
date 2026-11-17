package parse

// Perl. What the index holds, see fixtures/langs/perl/expected.tsv.
//
//   - sub NAME is a function wherever it is written: at file level, after a
//     `package` statement, inside a `package NAME { }` block. Perl has no
//     method syntax, so a sub reached through `$self->` is still a function.
//     A prototype or attribute list does not matter;
//   - package NAME, in statement or block form, is a package;
//   - `use constant NAME => value` and every key of
//     `use constant { A => 1, B => 2 }` are constants. In the hash form the
//     keys are the bare words that follow a value, so a bare-word value is
//     read as one more key;
//   - `our $x`, `our @x`, `our %x` and `our ($a, $b)` at file level or
//     directly inside a package block are variables (`my` is not: it is
//     private to the file or block).
//
// Anonymous subs, `my` and `local` variables, calls and `use Module` are not
// definitions.
func init() {
	registerDefinitions("perl", perlDefinitions, true)
}

const perlDefinitions = `
(subroutine_declaration_statement . (bareword) @name) @definition.function
(package_statement . (package) @name) @definition.module

(use_statement
  . (package) @_kw
  (list_expression . (autoquoted_bareword) @name @definition.constant)
  (#eq? @_kw "constant"))
(use_statement
  . (package) @_kw
  (anonymous_hash_expression (list_expression . (autoquoted_bareword) @name @definition.constant))
  (#eq? @_kw "constant"))
(use_statement
  . (package) @_kw
  (anonymous_hash_expression (list_expression (list_expression (_) . (autoquoted_bareword) @name @definition.constant)))
  (#eq? @_kw "constant"))

(source_file
  (expression_statement
    (assignment_expression . (variable_declaration (scalar (varname) @name) @definition.variable)) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (variable_declaration (scalar (varname) @name) @definition.variable) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (assignment_expression . (variable_declaration (array (varname) @name) @definition.variable)) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (variable_declaration (array (varname) @name) @definition.variable) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (assignment_expression . (variable_declaration (hash (varname) @name) @definition.variable)) @_d
    (#match? @_d "^our\\b")))
(source_file
  (expression_statement
    (variable_declaration (hash (varname) @name) @definition.variable) @_d
    (#match? @_d "^our\\b")))
(package_statement
  (block
    (expression_statement
      (assignment_expression . (variable_declaration (scalar (varname) @name) @definition.variable)) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (variable_declaration (scalar (varname) @name) @definition.variable) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (assignment_expression . (variable_declaration (array (varname) @name) @definition.variable)) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (variable_declaration (array (varname) @name) @definition.variable) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (assignment_expression . (variable_declaration (hash (varname) @name) @definition.variable)) @_d
      (#match? @_d "^our\\b"))))
(package_statement
  (block
    (expression_statement
      (variable_declaration (hash (varname) @name) @definition.variable) @_d
      (#match? @_d "^our\\b"))))
`
