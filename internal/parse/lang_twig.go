package parse

// Twig. What the index holds, see fixtures/langs/twig/expected.tsv: the same
// two declarations as Jinja2, with the same rule.
//
//   - {% macro NAME(args) %} is a function named NAME, wherever it is written,
//     including a multi-line parameter list;
//   - {% block NAME %} is a function named NAME, including one nested inside
//     another tag such as {% embed %}.
//
// The grammar has the macro's name as the method of a macro statement and the
// block's name as the variable of a tag statement, so both are read from the
// tree, not from the text. `{% endmacro %}`, `{% endblock name %}`, `{% set %}`,
// `{% from %}`, `{% embed %}`, calls, and `{# comments #}` are not definitions.
func init() {
	registerDefinitions("twig", twigDefinitions, true)
}

const twigDefinitions = `
(macro_statement (method) @name) @definition.function
(tag_statement (tag) @_tag (variable) @name
  (#eq? @_tag "block")) @definition.function
`
