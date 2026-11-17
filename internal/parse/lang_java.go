package parse

// Java constructs an object with `new`, and the inferred tags query has no
// pattern for it: a class that is only ever constructed had no reference
// anywhere, and looked unused.
//
// Each pattern tags the TYPE the expression names, and gives the reference the
// kind `reference.call.new`. The kind is how the resolver tells a construction
// from a method call of the same name (a constructor is itself indexed as a
// method called after its class); see constructedKind in internal/resolve/infer.
//
//	new Widget(3)                    the type_identifier is the name and the tag
//	new com.shop.Widget(3)           the name is the last identifier; the tag
//	                                 spans the qualified name, so the qualifier
//	                                 is the receiver, as for any other call
//	new ArrayList<String>()          the generic type is the tag, the name is its
//	new Runnable() { ... }           an anonymous class constructs the named type
//	outer.new Inner()                the type is Inner
//
// An anonymous class is recorded as a construction of the type it names, and
// is bound only when that type is a class: `new Runnable() {}` names an
// interface, which is never instantiated, so it binds to nothing. Its body's
// methods are indexed as ordinary methods of the enclosing class.
//
// A type written where a value of it is declared is a use of that type too:
// a field, a parameter, a local variable, a return type, a type argument
// (`List<Order>`). They are `reference.type`, which binds only to a type
// (resolve/infer typeCandidates) and, when the repository defines no such
// type (String, List, a framework's class), is recorded as ignored rather
// than unresolved. Without them a class that is only held and passed, never
// constructed or called, had no reference at all.
func init() {
	registerDefinitions("java", `
(object_creation_expression
  type: (type_identifier) @name @reference.call.new)
(object_creation_expression
  type: (scoped_type_identifier (type_identifier) @name .) @reference.call.new)
(object_creation_expression
  type: (generic_type . (type_identifier) @name) @reference.call.new)
(object_creation_expression
  type: (generic_type . (scoped_type_identifier (type_identifier) @name .)) @reference.call.new)

(field_declaration type: (type_identifier) @name @reference.type)
(field_declaration type: (generic_type . (type_identifier) @name) @reference.type)
(formal_parameter type: (type_identifier) @name @reference.type)
(formal_parameter type: (generic_type . (type_identifier) @name) @reference.type)
(local_variable_declaration type: (type_identifier) @name @reference.type)
(local_variable_declaration type: (generic_type . (type_identifier) @name) @reference.type)
(method_declaration type: (type_identifier) @name @reference.type)
(method_declaration type: (generic_type . (type_identifier) @name) @reference.type)
(type_arguments (type_identifier) @name @reference.type)
`, false)
}
