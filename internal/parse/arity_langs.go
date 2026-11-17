package parse

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// Arity for Kotlin, Scala, C# and C++.
//
// The same contract as Go, Java and TypeScript (see queries.go): a count is
// published only when it is exact. A parameter list with a default value or a
// variable number of parameters, and a call spreading a collection, publish no
// count at all (model.ArityUnknown), because the Arity type has one number and
// "exactly N" and "at least N" are different facts. A resolver that wants the
// second needs a second field on model.Symbol; until it exists the safe answer
// is the same as for TypeScript's optional parameter: unknown.
//
// What is counted, per language:
//
//	Kotlin  parameters: the `parameter` nodes of a function. A default value
//	        (the grammar writes it as a sibling of the parameter) or `vararg`
//	        makes the count unknown.
//	        arguments: the value arguments of the call's parenthesised list, a
//	        named argument (`f(b = 1)`) as one, a trailing lambda (`f(1) { }`,
//	        `f { }`) as one more, because it fills the last parameter. A
//	        spread (`f(*xs)`) makes the count unknown.
//	Scala   parameters: the first parameter list only, `def f(a: A)(b: B)` is
//	        1, which is the list overload resolution looks at first. A default
//	        value or a repeated parameter (`xs: Int*`) makes the count unknown.
//	        arguments: the first argument list of the call, `f(1)(2)` is 1 for
//	        the call of f. A call written with a block (`f { x => x }`) and an
//	        infix call (`a f b`) carry no count. A `xs: _*` spread is unknown.
//	C#      parameters: the `parameter` nodes. A default value, `params`, and the
//	        `this` of an extension method (the call does not pass it) make the
//	        count unknown.
//	        arguments: the arguments of the call; named, ref, out and in as one
//	        each.
//	C++     parameters: the parameter declarations, `f(void)` as none. A default
//	        value, `...` and a parameter pack make the count unknown.
//	        arguments: the arguments of the call; a pack expansion makes the
//	        count unknown.
//
// The match between a declaration and its count is by line and name, as for
// every other language (see callSite): a declaration whose name is not on the
// line its range starts on (a C++ return type on the line above the name)
// carries no count.
func init() {
	paramCountQueries["kotlin"] = `
(function_declaration (simple_identifier) @name (function_value_parameters) @params)
`
	argCountQueries["kotlin"] = `
(call_expression . (simple_identifier) @name (call_suffix) @args)
(call_expression (navigation_expression (navigation_suffix (simple_identifier) @name .)) (call_suffix) @args)
`
	arityDialects["kotlin"] = arityDialect{
		items:    map[string]bool{"parameter": true, "value_argument": true, "annotated_lambda": true},
		skip:     map[string]bool{"parameter_modifiers": true, "type_arguments": true},
		flatten:  map[string]bool{"value_arguments": true},
		variadic: map[string]bool{"spread_expression": true},
		opaque: func(n *ts.Node, lang *ts.Language, src []byte) bool {
			return n.Type(lang) == "parameter_modifiers" && strings.Contains(n.Text(src), "vararg")
		},
	}

	paramCountQueries["scala"] = `
(function_definition name: (identifier) @name . (parameters) @params)
(function_definition (operator_identifier) @name . (parameters) @params)
(function_definition name: (identifier) @name . (type_parameters) . (parameters) @params)
(function_definition (operator_identifier) @name . (type_parameters) . (parameters) @params)
`
	argCountQueries["scala"] = `
(call_expression function: (identifier) @name arguments: (arguments) @args)
(call_expression function: (field_expression field: (identifier) @name) arguments: (arguments) @args)
(call_expression function: (generic_function function: (identifier) @name) arguments: (arguments) @args)
(call_expression function: (generic_function function: (field_expression field: (identifier) @name)) arguments: (arguments) @args)
`
	arityDialects["scala"] = arityDialect{
		variadic: map[string]bool{"repeated_parameter_type": true, "vararg": true},
		opaque: func(n *ts.Node, lang *ts.Language, _ []byte) bool {
			if n.Type(lang) != "parameter" {
				return false
			}
			for i := 0; i < n.ChildCount(); i++ {
				if n.FieldNameForChild(i, lang) == "default_value" {
					return true
				}
			}
			return false
		},
	}

	paramCountQueries["c_sharp"] = `
(method_declaration name: (identifier) @name parameters: (parameter_list) @params)
(constructor_declaration name: (identifier) @name parameters: (parameter_list) @params)
`
	argCountQueries["c_sharp"] = `
(invocation_expression function: (identifier) @name arguments: (argument_list) @args)
(invocation_expression function: (member_access_expression name: (identifier) @name) arguments: (argument_list) @args)
(invocation_expression function: (generic_name (identifier) @name) arguments: (argument_list) @args)
(invocation_expression function: (member_access_expression name: (generic_name (identifier) @name)) arguments: (argument_list) @args)
(invocation_expression function: (conditional_access_expression (member_binding_expression name: (identifier) @name)) arguments: (argument_list) @args)
(object_creation_expression type: (identifier) @name arguments: (argument_list) @args)
(object_creation_expression type: (qualified_name (identifier) @name .) arguments: (argument_list) @args)
(object_creation_expression type: (generic_name . (identifier) @name) arguments: (argument_list) @args)
(object_creation_expression type: (qualified_name (generic_name . (identifier) @name) .) arguments: (argument_list) @args)
`
	arityDialects["c_sharp"] = arityDialect{
		items:  map[string]bool{"parameter": true, "argument": true},
		opaque: csharpOpaqueParameter,
	}

	registerScriptArity()
	registerRubyArity()
	registerPHPArity()
	registerRustArity()

	cppParams := `
(function_declarator declarator: [(identifier) (field_identifier)] @name parameters: (parameter_list) @params)
(function_declarator declarator: (qualified_identifier name: (identifier) @name) parameters: (parameter_list) @params)
(function_declarator declarator: (qualified_identifier name: (qualified_identifier name: (identifier) @name)) parameters: (parameter_list) @params)
(function_declarator declarator: (qualified_identifier name: (qualified_identifier name: (qualified_identifier name: (identifier) @name))) parameters: (parameter_list) @params)
`
	paramCountQueries["cpp"] = cppParams
	argCountQueries["cpp"] = `
(call_expression function: (identifier) @name arguments: (argument_list) @args)
(call_expression function: (field_expression field: (field_identifier) @name) arguments: (argument_list) @args)
(call_expression function: (qualified_identifier name: (identifier) @name) arguments: (argument_list) @args)
(call_expression function: (template_function name: (identifier) @name) arguments: (argument_list) @args)
`
	arityDialects["cpp"] = arityDialect{
		// An argument is any expression, so only the parameter list is closed.
		items:    map[string]bool{"parameter_declaration": true},
		closedIn: map[string]bool{"parameter_list": true},
		variadic: map[string]bool{
			"variadic_parameter_declaration": true,
			"parameter_pack_expansion":       true,
			"...":                            true,
		},
		voidList: func(list *ts.Node, lang *ts.Language, src []byte) bool {
			if list.Type(lang) != "parameter_list" {
				return false
			}
			return strings.Join(strings.Fields(list.Text(src)), "") == "(void)"
		},
	}
}

// csharpOpaqueParameter reports a C# parameter whose declaration cannot be set
// against a call's argument count: one with a default value, which the grammar
// writes as a further child of the parameter, and the `this` parameter of an
// extension method, which the call does not pass.
func csharpOpaqueParameter(n *ts.Node, lang *ts.Language, src []byte) bool {
	if n.Type(lang) != "parameter" {
		return false
	}
	body := 0
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c == nil || !c.IsNamed() || c.IsExtra() {
			continue
		}
		switch c.Type(lang) {
		case "modifier":
			if strings.TrimSpace(c.Text(src)) == "this" {
				return true
			}
		case "attribute_list":
		default:
			body++
		}
	}
	// A parameter is a type and a name. Anything more is `= default`.
	return body > 2
}

// Arity for JavaScript, Ruby, PHP and Rust, under the same contract: a count is
// published only when it is exact, and anything the count cannot state makes it
// unknown.
//
// What is counted, per language:
//
//	JavaScript  parameters: plain identifiers and destructuring patterns, one
//	            each. A default value (`a = 1`) and a rest parameter make the
//	            count unknown, as TypeScript's optional parameter cannot be told
//	            from a required one by this dialect. A function written
//	            `x => x`, with no parenthesised list, carries no count.
//	            arguments: the arguments of the call; a spread makes it unknown.
//	Ruby        parameters: plain and destructured parameters, one each. An
//	            optional, keyword, splat, hash-splat or forwarding parameter
//	            makes the count unknown, and a block parameter (`&blk`) is not
//	            counted because a call passes its block outside the list. A
//	            method written with no list at all carries no count.
//	            arguments: the positional arguments of the call. A keyword or
//	            hash argument, a splat and a hash splat make the count unknown,
//	            because they fill an unknown number of parameters; a block
//	            argument (`&blk`) is not counted.
//	PHP         parameters: simple and promoted constructor parameters, one
//	            each. A default value and a variadic parameter make the count
//	            unknown.
//	            arguments: the arguments of the call; a named argument and an
//	            unpacking (`...$xs`) make the count unknown.
//	Rust        parameters: the typed parameters, not the receiver: a method's
//	            `&self` is written in its parameter list and not at a call made
//	            with `.`. A parameter that names `self` with a type
//	            (`self: &Self`) makes the count unknown.
//	            arguments: the arguments of the call.
//
// The match between a declaration and its count is by line and name, as for
// every other language (see callSite).
func registerScriptArity() {
	paramCountQueries["javascript"] = `
(function_declaration name: (identifier) @name parameters: (formal_parameters) @params)
(generator_function_declaration name: (identifier) @name parameters: (formal_parameters) @params)
(method_definition name: (property_identifier) @name parameters: (formal_parameters) @params)
(variable_declarator name: (identifier) @name value: (function_expression parameters: (formal_parameters) @params))
(variable_declarator name: (identifier) @name value: (arrow_function parameters: (formal_parameters) @params))
(assignment_expression left: (member_expression property: (property_identifier) @name) right: (function_expression parameters: (formal_parameters) @params))
(assignment_expression left: (member_expression property: (property_identifier) @name) right: (arrow_function parameters: (formal_parameters) @params))
`
	argCountQueries["javascript"] = `
(call_expression function: (identifier) @name arguments: (arguments) @args)
(call_expression function: (member_expression property: (property_identifier) @name) arguments: (arguments) @args)
`
	arityDialects["javascript"] = arityDialect{
		items:    map[string]bool{"identifier": true, "object_pattern": true, "array_pattern": true},
		closedIn: map[string]bool{"formal_parameters": true},
		variadic: map[string]bool{"rest_pattern": true, "spread_element": true},
	}
}

func registerRubyArity() {
	paramCountQueries["ruby"] = `
(method name: (identifier) @name parameters: (method_parameters) @params)
(singleton_method name: (identifier) @name parameters: (method_parameters) @params)
`
	argCountQueries["ruby"] = `
(call method: (identifier) @name arguments: (argument_list) @args)
`
	arityDialects["ruby"] = arityDialect{
		items:    map[string]bool{"identifier": true, "destructured_parameter": true},
		closedIn: map[string]bool{"method_parameters": true},
		skip:     map[string]bool{"block_parameter": true, "block_argument": true},
		variadic: map[string]bool{"pair": true, "splat_argument": true, "hash_splat_argument": true},
	}
}

func registerPHPArity() {
	paramCountQueries["php"] = `
(function_definition name: (name) @name parameters: (formal_parameters) @params)
(method_declaration name: (name) @name parameters: (formal_parameters) @params)
`
	argCountQueries["php"] = `
(function_call_expression function: (name) @name arguments: (arguments) @args)
(function_call_expression function: (qualified_name (name) @name .) arguments: (arguments) @args)
(member_call_expression name: (name) @name arguments: (arguments) @args)
(nullsafe_member_call_expression name: (name) @name arguments: (arguments) @args)
(scoped_call_expression name: (name) @name arguments: (arguments) @args)
`
	arityDialects["php"] = arityDialect{
		items:    map[string]bool{"simple_parameter": true, "property_promotion_parameter": true, "argument": true},
		closedIn: map[string]bool{"formal_parameters": true},
		variadic: map[string]bool{"variadic_parameter": true, "variadic_unpacking": true},
		opaque:   phpOpaqueItem,
	}
}

// phpOpaqueItem reports a parameter with a default value and an argument passed
// by name, neither of which lines up positionally with the other side.
func phpOpaqueItem(n *ts.Node, lang *ts.Language, _ []byte) bool {
	switch n.Type(lang) {
	case "simple_parameter", "property_promotion_parameter":
		for i := 0; i < n.ChildCount(); i++ {
			if n.FieldNameForChild(i, lang) == "default_value" {
				return true
			}
		}
	case "argument":
		for i := 0; i < n.ChildCount(); i++ {
			if n.FieldNameForChild(i, lang) == "name" {
				return true
			}
		}
	}
	return false
}

func registerRustArity() {
	paramCountQueries["rust"] = `
(function_item name: (identifier) @name parameters: (parameters) @params)
(function_signature_item name: (identifier) @name parameters: (parameters) @params)
`
	argCountQueries["rust"] = `
(call_expression function: (identifier) @name arguments: (arguments) @args)
(call_expression function: (field_expression field: (field_identifier) @name) arguments: (arguments) @args)
(call_expression function: (scoped_identifier name: (identifier) @name) arguments: (arguments) @args)
(call_expression function: (generic_function function: (identifier) @name) arguments: (arguments) @args)
(call_expression function: (generic_function function: (field_expression field: (field_identifier) @name)) arguments: (arguments) @args)
(call_expression function: (generic_function function: (scoped_identifier name: (identifier) @name)) arguments: (arguments) @args)
`
	arityDialects["rust"] = arityDialect{
		items:    map[string]bool{"parameter": true},
		closedIn: map[string]bool{"parameters": true},
		skip:     map[string]bool{"self_parameter": true, "attribute_item": true},
		opaque: func(n *ts.Node, lang *ts.Language, _ []byte) bool {
			// `self: &Self` is a receiver the grammar writes as a parameter.
			if n.Type(lang) != "parameter" || n.ChildCount() == 0 {
				return false
			}
			first := n.Child(0)
			return first != nil && first.Type(lang) == "self"
		},
	}
}
