package parse

import (
	"regexp"
	"strings"

	ts "github.com/odvcencio/gotreesitter"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Erlang's definitions come from the grammar's own inferred query, which is
// not replaced here. What this file adds is the clause fold: the grammar makes
// each `f(0) -> ...;` clause its own fun_decl, so a function of three clauses
// arrived as three symbols. Consecutive clauses of one name and arity are now
// one symbol; `f/1` and `f/2` stay apart. See clausemerge.go.
func init() {
	registerDefinitions("erlang", erlangDefinitions, true)
	registerClauseMerge("erlang", erlangClauseArity)
	registerImportFix("erlang", erlangImport)

	// Arity is part of an Erlang function's identity: `f/1` and `f/2` are two
	// functions. A parameter list is the named children of its expr_args, and so
	// is an argument list, so the default counting rule is the right one; Erlang
	// has no default or variadic parameters to make a count unknown.
	paramCountQueries["erlang"] = `
(fun_decl (function_clause (atom) @name . (expr_args) @params))
`
	argCountQueries["erlang"] = `
(call (atom) @name . (expr_args) @args)
`
	arityDialects["erlang"] = arityDialect{}
}

// erlangImport reads `-import(m, [f/1, g/2]).` into one row naming the module
// and the functions it brings in, arity included (`import:f/1,g/2`). Any other
// attribute the query captured names nothing a call can reach.
func erlangImport(_ string, imp Import) []Import {
	m := erlangImportAttr.FindStringSubmatch(imp.Path)
	if m == nil {
		return nil
	}
	var names []string
	for _, f := range erlangFunArity.FindAllStringSubmatch(m[2], -1) {
		names = append(names, f[1]+"/"+f[2])
	}
	if len(names) == 0 {
		return nil
	}
	imp.Path = strings.Trim(m[1], "'")
	imp.Alias = ModuleImportPrefix + strings.Join(names, ",")
	return []Import{imp}
}

var (
	erlangImportAttr = regexp.MustCompile(`(?s)^-\s*import\s*\(\s*([a-z][A-Za-z0-9_@]*|'[^']*')\s*,\s*\[(.*)\]\s*\)`)
	erlangFunArity   = regexp.MustCompile(`([a-z][A-Za-z0-9_@]*|'[^']*')\s*/\s*(\d+)`)
)

// erlangShape reads what a call was made on from the text before its name. The
// call node of a remote call begins at the module, so the text is `money:` for
// `money:zero()`, `?MODULE:` for the module the call is written in, and a
// variable (`Mod:`) for a module chosen at run time, which names nothing. A bare
// call has no text before it and never gets here.
func erlangShape(pre string) callShape {
	opaque := callShape{kind: model.ReceiverExpr, receiver: model.OpaqueReceiver}
	if !strings.HasSuffix(pre, ":") {
		return opaque
	}
	recv := strings.TrimSpace(pre[:len(pre)-1])
	switch {
	case recv == "?MODULE":
		return callShape{kind: model.ReceiverSelf, receiver: recv}
	case recv != "" && recv[0] >= 'a' && recv[0] <= 'z' && isIdentifier(recv):
		return callShape{kind: model.ReceiverName, receiver: recv}
	}
	return opaque
}

// erlangClauseArity counts the arguments of the clause the name starts:
// the named children of its expr_args.
func erlangClauseArity(name *ts.Node, lang *ts.Language) string {
	clause := name.Parent()
	if clause == nil || clause.Type(lang) != "function_clause" {
		return ""
	}
	return namedChildCount(clause, "expr_args", lang)
}

// What the index holds, see fixtures/langs/erlang/expected.tsv:
//
//   - a function is a function, its clauses folded per name and arity (above);
//   - -module(name). is a package;
//   - -type and -opaque are a type alias, -record(name, {...}) is a class;
//   - -define(NAME, value). is a constant, and -define(NAME(Args), body). is a
//     function: a macro that takes arguments is called, one that does not is a
//     value.
//
// Not definitions: -spec (it describes a function defined elsewhere),
// -callback, -export, -import, -behaviour, record fields, the -ifdef family.
const erlangDefinitions = `
; Calls: f(x) and m:f(x). The call node of a remote call starts at the module, so the text
; before the name is how shape.go reads what the call was made on. A call inside a type
; (integer() in a -spec) names a type, not a function.
((call (atom) @name) @reference.call
  (#not-has-parent? @reference.call remote)
  (#not-has-ancestor? @reference.call type_sig type_alias opaque callback record_decl))
((remote (call (atom) @name)) @reference.call
  (#not-has-ancestor? @reference.call type_sig type_alias opaque callback record_decl))

(fun_decl (function_clause (atom) @name)) @definition.function
(module_attribute . (atom) @name) @definition.package
(type_alias (type_name . (atom) @name)) @definition.type_alias
(opaque (type_name . (atom) @name)) @definition.type_alias
(record_decl . (atom) @name) @definition.class
(pp_define (macro_lhs . [(var) (atom)] @name .)) @definition.constant
(pp_define (macro_lhs . [(var) (atom)] @name . (var_args))) @definition.function
`
