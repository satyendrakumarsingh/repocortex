package parse

import (
	"fmt"
	"strings"
)

// Tcl. What the index holds, see fixtures/langs/tcl/expected.tsv.
//
// A definition counts at the top level of the file or directly inside a
// `namespace eval NAME { ... }` body (to three levels of nesting). A proc
// defined inside another proc's body, or inside an `if`, is created only when
// that code runs and is not held.
//
//   - proc is a function. The name is held as written: `proc ::a::b` is
//     `::a::b`;
//   - `namespace eval NAME` is a package, as written (`::absolute::path`);
//   - `variable NAME ...` and `set NAME VALUE` at those places are variables
//     (a `set NAME` that only reads a value is not);
//   - TclOO: `oo::class create NAME { ... }` is a class. In its body, method,
//     `class method`, constructor and destructor are methods, and the names
//     of `variable a b` are fields. A `method` inside `oo::define CLASS { }`
//     or `oo::objdefine OBJ { }` is a method too.
//
// Not definitions: calls, `package`, `namespace export`, `unexport`,
// `superclass`, a proc nested in a proc or an `if`, anonymous `apply` lambdas,
// and a method whose name is a braced list (see KNOWN_GAPS.md).
func init() {
	registerDefinitions("tcl", tclDefinitions(), true)
}

// tclContext wraps a pattern in the places a definition may sit: the file, and
// the body of a namespace eval nested up to three deep.
func tclContexts(inner func(prefix string) string) []string {
	var out []string
	for depth := 0; depth <= 3; depth++ {
		pat := inner(fmt.Sprintf("d%d", depth))
		var preds []string
		for i := depth; i >= 1; i-- {
			pat = fmt.Sprintf("(namespace (word_list . (simple_word) @_e%d%d (braced_word %s)))", depth, i, pat)
			preds = append(preds, fmt.Sprintf(`(#eq? @_e%d%d "eval")`, depth, i))
		}
		pat = "(source_file " + pat + " " + strings.Join(preds, " ") + ")"
		out = append(out, pat)
	}
	return out
}

func tclDefinitions() string {
	var out []string
	add := func(f func(p string) string) { out = append(out, tclContexts(f)...) }

	// procs
	add(func(string) string { return `(procedure name: (simple_word) @name) @definition.function` })
	// namespaces
	add(func(p string) string {
		return fmt.Sprintf(`(namespace (word_list . (simple_word) @_k%[1]s . (simple_word) @name . (braced_word))
      (#eq? @_k%[1]s "eval")) @definition.module`, p)
	})
	// variables
	add(func(p string) string {
		return fmt.Sprintf(`(command name: (simple_word) @_v%[1]s arguments: (word_list . (simple_word) @name . (_))
      (#eq? @_v%[1]s "variable")) @definition.variable`, p)
	})
	add(func(string) string { return `(set . (id) @name . (_)) @definition.variable` })
	// classes
	add(func(p string) string {
		return fmt.Sprintf(`(command name: (simple_word) @_o%[1]s
      arguments: (word_list . (simple_word) @_c%[1]s . (simple_word) @name . (braced_word))
      (#eq? @_o%[1]s "oo::class") (#eq? @_c%[1]s "create")) @definition.class`, p)
	})
	// methods and fields inside a class body
	add(func(p string) string {
		return fmt.Sprintf(`(command name: (simple_word) @_o%[1]s
      arguments: (word_list . (simple_word) @_c%[1]s . (simple_word) .
        (braced_word (command name: (simple_word) @_m%[1]s arguments: (word_list . (simple_word) @name)) @definition.method))
      (#eq? @_o%[1]s "oo::class") (#eq? @_c%[1]s "create") (#eq? @_m%[1]s "method"))`, p)
	})
	add(func(p string) string {
		return fmt.Sprintf(`(command name: (simple_word) @_o%[1]s
      arguments: (word_list . (simple_word) @_c%[1]s . (simple_word) .
        (braced_word (command name: (simple_word) @_k%[1]s arguments: (word_list . (simple_word) @_s%[1]s . (simple_word) @name)) @definition.method))
      (#eq? @_o%[1]s "oo::class") (#eq? @_c%[1]s "create") (#eq? @_k%[1]s "class") (#eq? @_s%[1]s "method"))`, p)
	})
	add(func(p string) string {
		return fmt.Sprintf(`(command name: (simple_word) @_o%[1]s
      arguments: (word_list . (simple_word) @_c%[1]s . (simple_word) .
        (braced_word (command name: (simple_word) @name) @definition.method))
      (#eq? @_o%[1]s "oo::class") (#eq? @_c%[1]s "create") (#any-of? @name "constructor" "destructor"))`, p)
	})
	add(func(p string) string {
		return fmt.Sprintf(`(command name: (simple_word) @_o%[1]s
      arguments: (word_list . (simple_word) @_c%[1]s . (simple_word) .
        (braced_word (command name: (simple_word) @_v%[1]s arguments: (word_list (simple_word) @name)) @definition.field))
      (#eq? @_o%[1]s "oo::class") (#eq? @_c%[1]s "create") (#eq? @_v%[1]s "variable"))`, p)
	})
	// methods added to an existing class or object
	add(func(p string) string {
		return fmt.Sprintf(`(command name: (simple_word) @_o%[1]s
      arguments: (word_list . (simple_word) .
        (braced_word (command name: (simple_word) @_m%[1]s arguments: (word_list . (simple_word) @name)) @definition.method))
      (#any-of? @_o%[1]s "oo::define" "oo::objdefine") (#eq? @_m%[1]s "method"))`, p)
	})
	return "\n" + strings.Join(out, "\n") + "\n"
}
