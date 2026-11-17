package parse

import "regexp"

// Jinja2. What the index holds, see fixtures/langs/jinja2/expected.tsv:
//
//   - {% macro NAME(args) %} is a function named NAME;
//   - {% block NAME %} is a function named NAME.
//
// The grammar does not give a tag's parts as nodes: a whole `{% ... %}` tag is
// one jinja_expression node, so the query matches the tag's text and the name
// is read from it. A tag is matched only when its text begins at the tag and
// starts with the keyword and an identifier, so `{% endmacro %}`,
// `{% endblock name %}`, expressions (`{{ NAME() }}`), calls and
// `{# comments #}` are not definitions.
//
// The text begins at the tag only when no newline lies between it and the tag
// before it: after another tag on its own line the grammar starts the node at
// the end of that previous tag, so the line would be wrong. Such a tag is left
// out rather than recorded on the wrong line; see KNOWN_GAPS.md.
func init() {
	registerDefinitions("jinja2", jinja2Definitions, true)
	registerNameFix("jinja2", jinja2TagName)
}

const jinja2Definitions = `
(jinja_expression) @name @definition.function
  (#match? @name "^\\{%-?\\s*(macro|block)\\s+[A-Za-z_][A-Za-z0-9_]*")
`

var jinja2TagPattern = regexp.MustCompile(`^\{%-?\s*(?:macro|block)\s+([A-Za-z_][A-Za-z0-9_]*)`)

// jinja2TagName returns the name a macro or block tag declares. The query has
// already matched the tag, so the pattern always matches; the text is returned
// unchanged otherwise, which the conformance fixture would show.
func jinja2TagName(text string) string {
	if m := jinja2TagPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return text
}
