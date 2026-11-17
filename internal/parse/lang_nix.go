package parse

import "strings"

// Nix. What the index holds, see fixtures/langs/nix/expected.tsv.
//
// An attribute set declares nothing as such, so the rule is narrow: the names a
// file BINDS AT ITS TOP LEVEL. The top level is the expression the file
// evaluates to, looking through the `{ args }:` and `x:` function layers that
// wrap it, and then either
//
//   - the bindings of a top-level `let ... in`, or
//   - the attributes of the attribute set (`{ ... }` or `rec { ... }`) that is
//     the file's body or the body of that let.
//
// A binding with a single-name path is a function when its value is a function
// (`double = x: x * 2;`) and a variable otherwise. Not held: attributes with a
// dotted path (`options.services.demo.enable = ...`), quoted names,
// `inherit`, bindings nested in a value (the keys of an inner set, a nested
// let) and function parameters.
func init() {
	registerDefinitions("nix", nixDefinitions(), true)
}

func nixDefinitions() string {
	const value = `[(string_expression) (indented_string_expression) (integer_expression) (float_expression)
      (path_expression) (hpath_expression) (spath_expression) (uri_expression) (variable_expression)
      (attrset_expression) (rec_attrset_expression) (list_expression) (apply_expression)
      (select_expression) (binary_expression) (unary_expression) (parenthesized_expression)
      (if_expression) (with_expression) (assert_expression) (has_attr_expression) (let_expression)]`
	binding := func(kind, val string) string {
		return "(binding (attrpath . (identifier) @name .) . " + val + ") @definition." + kind
	}
	bindings := []string{binding("function", "(function_expression)"), binding("variable", value)}

	var out strings.Builder
	wrap := func(body string) {
		out.WriteString("(source_code " + body + ")\n")
		out.WriteString("(source_code (function_expression " + body + "))\n")
		out.WriteString("(source_code (function_expression (function_expression " + body + ")))\n")
	}
	for _, b := range bindings {
		wrap("(let_expression (binding_set " + b + "))")
		for _, set := range []string{"attrset_expression", "rec_attrset_expression"} {
			wrap("(let_expression (" + set + " (binding_set " + b + ")))")
			wrap("(" + set + " (binding_set " + b + "))")
		}
	}
	return out.String()
}
