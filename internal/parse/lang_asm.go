package parse

// Assembly. What the index holds, see fixtures/langs/asm/expected.tsv.
//
//   - a label is a function: it names an address that is called or jumped to
//     (`_start:`, `main:`, `helper:`);
//   - assembler-local labels are not definitions: `.L...` and any label
//     starting with a dot, numeric labels (`1:`, used as `1b` / `1f`) and the
//     compiler's own block labels (`LBB0_1`, `Lfunc_begin0`, `Ltmp0`);
//   - directives (`.globl`, `.type`, `.size`) and instructions are not
//     definitions.
//
// The grammar keeps no section structure, so a label in a data section
// (`counter: .quad 0`) cannot be told from a code label and is held as a
// function too; fixtures/langs/asm/KNOWN_GAPS.md records that.
func init() {
	registerDefinitions("asm", asmDefinitions, true)
}

const asmDefinitions = `
(label (ident) @name
  (#match? @name "^[A-Za-z_$]")
  (#not-match? @name "^(LBB|Lfunc_|Ltmp|Lcfi)")) @definition.function
`
