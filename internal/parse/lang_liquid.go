package parse

// Liquid. What the index holds, see fixtures/langs/liquid/expected.tsv:
//
//   - {% capture NAME %} ... {% endcapture %} is a variable named NAME, at any
//     depth: a capture in a loop or an if block declares the name as well;
//   - {% assign NAME = value %} is a variable only when it is written at the
//     top level of the template. An assign inside a for, if or capture block
//     is a change to a variable, not a declaration the index should hold.
//
// The name is the definition node as well, so a variable is recorded on the
// line its name is on. For a capture written over several lines that is the
// line of the name, not of `{% capture`; see KNOWN_GAPS.md.
//
// The loop variable of a for tag, include and render tags, filters, output
// expressions and {% comment %} blocks are not definitions.
func init() {
	registerDefinitions("liquid", liquidDefinitions, true)
}

const liquidDefinitions = `
(program (assignment_statement . (identifier) @name @definition.variable))
(capture_statement . (identifier) @name @definition.variable)
`
