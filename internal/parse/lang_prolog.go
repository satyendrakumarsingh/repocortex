package parse

import ts "github.com/odvcencio/gotreesitter"

// Prolog. What the index holds, see fixtures/langs/prolog/expected.tsv.
//
//   - a predicate is a function, named by its functor without the arity:
//     `area(circle(R), A) :- ...` is `area`. Its clauses, facts and rules, are
//     folded into one symbol when they are consecutive and agree on arity, so
//     `area/2` written as three clauses is one definition, while `describe/1`
//     and `describe/2` stay two symbols of one name (a name is not unique
//     here, as in Elixir and Erlang), and a predicate whose clauses are
//     interleaved with another's is several symbols (the compiler warns of
//     that too);
//   - a DCG rule (`greeting --> ...`) is a predicate named by its head;
//   - `:- module(Name, Exports).` is a package.
//
// Directives other than module (use_module, dynamic, discontiguous,
// initialization), the goals in a clause body and the arguments of a head are
// not definitions.
//
// The grammar reads an operator chain from left to right without precedence,
// so the head of a rule sits at the bottom of a nest of operators. The query
// finds a rule by its `:-` or `-->`, and a rule's symbol therefore covers the
// head and the first goal of its body, not the whole clause. See
// fixtures/langs/prolog/KNOWN_GAPS.md.
func init() {
	registerDefinitions("prolog", prologDefinitions, true)
	registerClauseMerge("prolog", prologClauseArity)
}

// prologClauseArity is the argument count of the head the name starts: the
// named children of the head's `args`, or 0 for a head that is a bare atom.
func prologClauseArity(name *ts.Node, lang *ts.Language) string {
	head := name.Parent()
	if head == nil || head.Type(lang) != "functional_notation" {
		return "0"
	}
	if n := namedChildCount(head, "args", lang); n != "" {
		return n
	}
	return "0"
}

const prologDefinitions = `
(clause_term
  . (compound_term . (functional_notation name: (atom) @name))
  . (end) @_e
  (#match? @_e "^[.]")) @definition.function
(clause_term
  . (atomic (atom) @name)
  . (end) @_e
  (#match? @_e "^[.]")) @definition.function

(infix_operator
  left: (compound_term . (functional_notation name: (atom) @name))
  operator: (operator) @_o
  (#any-of? @_o ":-" "-->")) @definition.function
(infix_operator
  left: (atomic (atom) @name)
  operator: (operator) @_o
  (#any-of? @_o ":-" "-->")) @definition.function

(directive_term
  (compound_term
    (functional_notation
      name: (atom) @_f
      arguments: (args . (atomic (atom) @name))))
  (#eq? @_f "module")) @definition.module
`
