package parse

import "strings"

// COBOL. What the index holds, see fixtures/langs/cobol/expected.tsv.
//
//   - PROGRAM-ID names a program, held as a package: a program is the unit
//     other programs CALL, and it owns the paragraphs below it;
//   - every paragraph and section header of the PROCEDURE DIVISION is a
//     function (PERFORM targets). The grammar lexes the header with its
//     full stop, and a section with its SECTION keyword, so the name is cut
//     back to the first word;
//   - level 01 and level 77 data items are variables (a group item and each
//     elementary 77). Subordinate items (05, 10, ...), 88-level condition
//     names, FD file names and SELECT names are not definitions.
//
// PERFORM, CALL, MOVE and the other statements are not definitions.
func init() {
	registerDefinitions("cobol", cobolDefinitions, true)
	registerNameFix("cobol", cobolName)
}

// cobolName cuts a paragraph or section header back to its name: the first
// word, without the full stop.
func cobolName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		name = f[0]
	}
	return strings.TrimSuffix(name, ".")
}

const cobolDefinitions = `
(program_definition (identification_division (program_name) @name)) @definition.module
(paragraph_header) @name @definition.function
(section_header) @name @definition.function
(data_description (level_number) @_l (entry_name) @name (#match? @_l "^(0?1|77)$")) @definition.variable
`
