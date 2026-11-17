package parse

import "strings"

// Blade. What the index holds, see fixtures/langs/blade/expected.tsv: each
// @section('name') declares a function named name. That is the block form,
// `@section('name') ... @endsection`, and the inline form,
// `@section('name', 'value')`, which declares the same name on one line. The
// name is the first argument, and only a quoted literal counts: a name read
// from a variable (`@section($name)`) is not extracted.
//
// @component, @include, @extends, @yield, @push, @slot, echo expressions and
// the text of a `{{-- comment --}}` are not definitions.
func init() {
	registerDefinitions("blade", bladeDefinitions, true)
	registerNameFix("blade", bladeSectionName)
}

const bladeDefinitions = `
(section (directive_start) @_d . (parameter) @name
  (#eq? @_d "@section")
  (#match? @name "^(['][^']+[']|[\"][^\"]+[\"])$")) @definition.function
(section (directive) @_d . (parameter) @name
  (#eq? @_d "@section")
  (#match? @name "^(['][^']+[']|[\"][^\"]+[\"])$")) @definition.function
`

// bladeSectionName removes the quotes around a literal section name.
func bladeSectionName(name string) string {
	return strings.Trim(name, `'"`)
}
