package parse

// awk. What the index holds, see fixtures/langs/awk/expected.tsv: each
// `function NAME(...)` and `func NAME(...)` is a function, named by NAME. A
// parameter list over several lines does not move the declaration: it starts
// on the line the keyword is on.
//
// BEGIN and END blocks, pattern-action rules, calls, variables (a global, or a
// local in a parameter list) and comments are not definitions.
func init() {
	registerDefinitions("awk", awkDefinitions, true)
}

const awkDefinitions = `
(func_def . (identifier) @name) @definition.function
`
