package parse

import (
	"sort"

	ts "github.com/odvcencio/gotreesitter"
)

// Tags-query supplements.
//
// gotreesitter ships 206 grammars but ZERO explicit tags queries — every query
// is inferred from grammar symbols at runtime. Inference is a reasonable
// default, but its coverage is uneven, and the gaps are not cosmetic.
//
// Measured example (gotreesitter v0.50.1): for Go, the inferred query captures
// funcs and methods but misses type, const and var declarations entirely. On
// this input:
//
//	type TransactionService struct{}
//	type Repository interface { Save(id string) error }
//	const MaxRetries = 3
//	func NewService() *TransactionService { return nil }
//	func (s *TransactionService) UpdateStatus(id string) error { ... }
//
// inference yields only [NewService, UpdateStatus]. A code intelligence tool
// that cannot find the type `TransactionService` is not useful, so RepoCortex
// appends its own patterns for priority languages.
//
// These are ADDITIVE. The inferred query is kept and supplemented, so a future
// gotreesitter release that improves inference does not regress us — at worst
// a symbol is captured twice, and de-duplication handles that.
//
// Adding a language here is a well-bounded contribution: write the patterns,
// add a case to the coverage test proving the previously-missing symbols now
// appear.

// supplements maps a grammar name to extra tree-sitter tag patterns.
//
// Capture convention (tree-sitter tags):
//
//	@name              the symbol's identifier
//	@definition.KIND   the enclosing node, KIND becoming the symbol kind
//	@reference.KIND    a use of a name
var supplements = map[string]string{
	// Verified: recovers type/const/var, which inference drops.
	//
	// The four type patterns are deliberate. A single (type_spec ...) rule was
	// wrong in both directions: it tagged EVERY Go type declaration as an
	// alias, so no Go struct was ever a class and no Go interface was ever an
	// interface — and it missed real aliases entirely, because `type X = Y`
	// parses as `type_alias`, a different node than `type_spec`. Probed against
	// the grammar rather than assumed.
	//
	// The struct rule is redundant with the general one now that
	// definition.type means class, and is kept because it states the intent at
	// the point a reader looks for it. The interface rule is NOT redundant: the
	// general rule matches interfaces too, so the two compete and specificity
	// decides — see kindSpecificity, where KindInterface outranks KindClass for
	// exactly this reason.
	//
	// A const or var is a definition only at the package level, as a child of
	// the file: `var local = 1` inside a function, a closure or a block is a
	// local, and cobra had 13 of them indexed as variables in two files. The
	// grouped form `var ( ... )` wraps its specs in a var_spec_list; a grouped
	// const does not.
	"go": `
(type_spec name: (type_identifier) @name type: (struct_type)) @definition.class
(type_spec name: (type_identifier) @name type: (interface_type)) @definition.interface
(type_spec name: (type_identifier) @name) @definition.type
(type_alias name: (type_identifier) @name) @definition.type_alias
(source_file (const_declaration (const_spec name: (identifier) @name) @definition.constant))
(source_file (var_declaration (var_spec name: (identifier) @name) @definition.variable))
(source_file (var_declaration (var_spec_list (var_spec name: (identifier) @name) @definition.variable)))
`,

	// Interfaces, enums, records and fields are how Java expresses most
	// structure; inference concentrates on classes and methods.
	"java": `
(interface_declaration name: (identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.enum
(record_declaration name: (identifier) @name) @definition.class
(field_declaration declarator: (variable_declarator name: (identifier) @name)) @definition.field
(constructor_declaration name: (identifier) @name) @definition.method
(annotation_type_declaration name: (identifier) @name) @definition.interface
`,

	// Type-level declarations carry the domain model in TypeScript and are
	// exactly what an agent needs when reasoning about a change.
	//
	// function_signature covers declarations inside `declare module`, which is
	// how ambient type definitions describe an external library's API. Missing
	// them meant a .d.ts file indexed as if it declared nothing.
	// The two lexical/var patterns below capture `const Button = () => {}` and
	// `const helper = function () {}` — the dominant React and Vue component
	// form, and previously invisible to the index entirely: no symbol, so
	// nothing to find and nothing for a reference to bind to.
	//
	// The `value:` alternation is load-bearing. Without it, EVERY const and
	// let binding in the codebase becomes a function symbol, flooding the
	// graph with config constants. Only a binding whose value is literally an
	// arrow function or a function expression matches.
	"typescript": `
(interface_declaration name: (type_identifier) @name) @definition.interface
(type_alias_declaration name: (type_identifier) @name) @definition.type_alias
(enum_declaration name: (identifier) @name) @definition.enum
(public_field_definition name: (property_identifier) @name) @definition.field
(abstract_class_declaration name: (type_identifier) @name) @definition.class
(method_signature name: (property_identifier) @name) @definition.method
(abstract_method_signature name: (property_identifier) @name) @definition.method
(generator_function_declaration name: (identifier) @name) @definition.function
(function_signature name: (identifier) @name) @definition.function
(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function
(variable_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function
`,

	// tsx IS the typescript grammar plus JSX — every node type typescript's
	// supplement names also exists in the tsx grammar, so this is the same
	// pattern list verbatim, not a reduced one. It shipped with only 2 of
	// typescript's 10 captures, which meant every enum, field and method
	// signature in a .tsx file was invisible — not a language difference,
	// just a row nobody wrote. TestTSXSupplementCoversEverythingTypeScriptDoes
	// (coverage_test.go) fails if the two drift again.
	//
	// Duplicated rather than shared as one constant: the two entries are keyed
	// by grammar name, and a future JSX-only capture (e.g. a pattern over
	// jsx_element) belongs in tsx alone, not in a shared block that typescript
	// would also inherit.
	"tsx": `
(interface_declaration name: (type_identifier) @name) @definition.interface
(type_alias_declaration name: (type_identifier) @name) @definition.type_alias
(enum_declaration name: (identifier) @name) @definition.enum
(public_field_definition name: (property_identifier) @name) @definition.field
(abstract_class_declaration name: (type_identifier) @name) @definition.class
(method_signature name: (property_identifier) @name) @definition.method
(abstract_method_signature name: (property_identifier) @name) @definition.method
(generator_function_declaration name: (identifier) @name) @definition.function
(function_signature name: (identifier) @name) @definition.function
(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function
(variable_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function
`,

	// javascript had no supplement entry at all before this. The two patterns
	// are identical to typescript/tsx because the JS, TS and TSX grammars
	// share these node types for variable declarations and function values.
	"javascript": `
(lexical_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function
(variable_declaration
  (variable_declarator
    name: (identifier) @name
    value: [(arrow_function) (function_expression)])) @definition.function
`,

	// Decorated functions are the norm in FastAPI/Flask/Django, and assignment
	// targets at module scope are how Python declares configuration.
	//
	// Two details in the module-constant pattern, both learned the hard way:
	//
	// It is anchored on (module ...) with NO expression_statement wrapper —
	// this grammar attaches `assignment` directly to `module`. An earlier
	// version assumed the wrapper and silently matched nothing, which is how a
	// wrong tags query fails: it looks exactly like a file that declares
	// nothing. The anchor is also what keeps locals inside functions out of the
	// index.
	//
	// @definition.constant sits on the `assignment` node, INSIDE the parentheses
	// — not on the outer (module ...). Placing it outside made every constant
	// report the module's start line, so an agent following the location was
	// sent to line 1 instead of the declaration. Caught by the differential
	// test, which compared positions and not merely names.
	// There is deliberately NO rule for decorated_definition. The grammar's own
	// tags already capture the function_definition nested inside it, so adding
	// one produced TWO symbols for every decorated function — one at the
	// decorator, one at the `def`. On a real Python service that was 4,224
	// duplicate symbol rows: `repocortex symbol` listed each test twice, and a
	// context pack spent its budget saying the same thing twice, which is the
	// waste this product exists to remove. Undecorated functions were never
	// affected, which is why it survived the fixtures.
	"python": `
(module (assignment left: (identifier) @name) @definition.constant)
`,

	"rust": `
(struct_item name: (type_identifier) @name) @definition.class
(enum_item name: (type_identifier) @name) @definition.enum
(trait_item name: (type_identifier) @name) @definition.interface
(const_item name: (identifier) @name) @definition.constant
`,

	"cpp": `
(class_specifier name: (type_identifier) @name body: (field_declaration_list)) @definition.class
(struct_specifier name: (type_identifier) @name body: (field_declaration_list)) @definition.class
(enum_specifier name: (type_identifier) @name body: (enumerator_list)) @definition.enum
`,

	"c_sharp": `
(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(struct_declaration name: (identifier) @name) @definition.class
(enum_declaration name: (identifier) @name) @definition.enum
(record_declaration name: (identifier) @name) @definition.class
(invocation_expression (member_access_expression (identifier) (identifier) @name)) @reference.call
(invocation_expression (identifier) @name) @reference.call
(invocation_expression (member_access_expression "this" (identifier) @name)) @reference.call
(invocation_expression (member_access_expression "base" (identifier) @name)) @reference.call
`,

	"ruby": `
(class name: (constant) @name) @definition.class
(module name: (constant) @name) @definition.module
(call receiver: (_) method: (identifier) @name) @reference.call
(call !receiver method: (identifier) @name (#not-any-of? @name "include" "extend" "prepend")) @reference.call
`,

	// Solidity: the definition query (lang_solidity.go) tags no reference, so a
	// call, a construction and a `is` clause bound nothing. The call tag spans the
	// whole call expression, which is how the receiver of `Math.max(a, b)` is read
	// back from the text before the name (shape.go). `emit Sent(...)`, `revert
	// NotOwner(...)` and a modifier written after a function's parameters
	// (`onlyOwner`, or a base constructor `Base(x)`) are calls of the event, the
	// error and the modifier. `new Vault(x)` constructs a contract, and
	// `contract A is B, C` names its bases (see inheritanceQueries). A type written where a
	// value of it is declared, and the library of a `using L for T`, is a use of it.
	"solidity": `
(call_expression (expression (identifier) @name)) @reference.call
(call_expression (expression (member_expression (identifier) . (identifier) @name .))) @reference.call
(call_expression (expression (member_expression (expression) (identifier) @name .))) @reference.call
(emit_statement (expression (identifier) @name @reference.call))
(revert_statement (expression (identifier) @name @reference.call))
(modifier_invocation (identifier) @name @reference.call)
(new_expression (type_name (user_defined_type (identifier) @name @reference.call.new .)))
(parameter (type_name (user_defined_type (identifier) @name .))) @reference.type
(variable_declaration (type_name (user_defined_type (identifier) @name .))) @reference.type
(state_variable_declaration (type_name (user_defined_type (identifier) @name .))) @reference.type
(using_directive (type_alias (identifier) @name)) @reference.type
`,

	// Apex constructs with `new`, which its inferred tags have no pattern for, so a
	// class that is only ever constructed had no reference anywhere. Each pattern
	// tags the type the expression names with the kind Java's construction has
	// (see lang_java.go): the resolver reads it as a construction, and a
	// constructor, which is indexed as a method called after its class, is not
	// mistaken for a call. A type written where a value of it is declared (a
	// field, a parameter, a local, a return type, a type argument) is a use of
	// that type: `reference.type` binds only to a type and, where the org holds
	// none of the name (String, Account, a standard SObject), is ignored.
	"apex": `
(object_creation_expression . (type_identifier) @name @reference.call.new)
(object_creation_expression . (generic_type . (type_identifier) @name) @reference.call.new)
(object_creation_expression . (scoped_type_identifier (type_identifier) @name .) @reference.call.new)
(field_declaration (type_identifier) @name @reference.type)
(field_declaration (generic_type . (type_identifier) @name) @reference.type)
(formal_parameter (type_identifier) @name @reference.type)
(formal_parameter (generic_type . (type_identifier) @name) @reference.type)
(local_variable_declaration (type_identifier) @name @reference.type)
(local_variable_declaration (generic_type . (type_identifier) @name) @reference.type)
(method_declaration (type_identifier) @name @reference.type)
(method_declaration (generic_type . (type_identifier) @name) @reference.type)
(type_arguments (type_identifier) @name @reference.type)
`,
}

// inheritanceQueries capture SUPERTYPE relationships as references.
//
// The gap this closes: every pattern in `supplements` above is a
// @definition.*, and the grammars' own inferred tags emit essentially one
// reference kind — reference.call. So the only edge kind heuristic indexing
// could ever produce was CALLS. internal/resolve/infer maps a reference kind
// to an edge kind by substring — "implementation" becomes EdgeImplements,
// "extend" or "inherit" becomes EdgeExtends — and both of those branches were
// unreachable from source, leaving IMPLEMENTS and EXTENDS edges available only
// from a SCIP index. internal/capability/impact traverses exactly those edges,
// so without SCIP, impact analysis expanded through an edge set nothing
// populated. Java is where inheritance carries the most meaning and is also
// where compiler-backed indexing most often is not available, which is what
// made this the highest-value recall gap rather than a nicety.
//
// Capture names are chosen to land on the right side of that substring match
// and MUST NOT be renamed casually:
//
//	@reference.extends         -> EdgeExtends       (contains "extend")
//	@reference.inherit         -> EdgeExtends       (contains "inherit")
//	@reference.implementation  -> EdgeImplements    (contains "implementation")
//
// These are separate from `supplements` for one reason: a tags query that
// names a node type the grammar does not have is REJECTED outright, not
// silently ignored — probed, not assumed — and a rejected query sets initErr
// and costs the language every symbol it would otherwise yield. Keeping these
// patterns in their own string lets newGenericParser drop them and retry, so a
// grammar update that renames `super_interfaces` degrades inheritance edges
// instead of deleting Java from the index.
//
// Every capture puts @name on the SIMPLE type name, never the qualified path:
// the resolver matches a reference by name against the symbol index, so
// `extends com.pay.Base` must arrive as "Base". The scoped patterns use a
// trailing anchor to take the last segment for that reason.
var inheritanceQueries = map[string]string{
	// Go has no `implements` keyword, and interface satisfaction is structural:
	// whether a type implements an interface is a property of its method set,
	// decidable only with full type information that a single file's syntax
	// tree does not carry. So NO reference.implementation is emitted for Go at
	// all. Inventing one from a name would be a confident guess presented as a
	// parsed fact, which is the failure the confidence model exists to prevent.
	//
	// What IS syntactic, and what these patterns capture, is EMBEDDING —
	// interface embedding (`type A interface { B }`) and struct embedding
	// (`type S struct { D }`). Both promote the embedded type's method set into
	// the embedder, which is the closest thing Go has to an extends relation,
	// so both are reported as reference.extends.
	//
	// The type_elem patterns are anchored on BOTH sides (`. ... .`) so the
	// element has exactly one child. Without the anchors, a generic constraint
	// interface — `interface { ~int | string }` — parses as one type_elem with
	// several children and contributed a bogus "extends string" edge. Measured
	// on the probe, not reasoned about.
	//
	// The field_declaration patterns use `!name` because that is the only thing
	// separating an embedded field from an ordinary one: `D` is
	// (field_declaration type: (type_identifier)) while `named G` adds a
	// name: field. Dropping the negation would report every struct field as a
	// supertype.
	"go": `
(interface_type (type_elem . (type_identifier) @name @reference.extends .))
(interface_type (type_elem . (qualified_type name: (type_identifier) @name) @reference.extends .))
(interface_type (type_elem . (generic_type type: (type_identifier) @name) @reference.extends .))
(field_declaration !name type: (type_identifier) @name @reference.extends)
(field_declaration !name type: (pointer_type (type_identifier) @name @reference.extends))
(field_declaration !name type: (qualified_type name: (type_identifier) @name @reference.extends))
(field_declaration !name type: (pointer_type (qualified_type name: (type_identifier) @name @reference.extends)))
(field_declaration !name type: (generic_type type: (type_identifier) @name @reference.extends))
`,

	// Java states both relations explicitly, which is why this is the language
	// the change is worth the most in.
	//
	//	class A extends B          -> (superclass (type_identifier))
	//	class A implements C, D    -> (super_interfaces (type_list ...))
	//	interface I extends J, K   -> (extends_interfaces (type_list ...))
	//
	// enum and record declarations reuse super_interfaces, so `enum N
	// implements C` and `record R(...) implements C` are covered by the same
	// patterns with no extra rules.
	//
	// Each relation needs three shapes because a supertype may be written
	// plain (`Base`), generic (`Base<T>`) or qualified (`com.pay.Base`), and
	// the three are different nodes. An interface list mentioning several types
	// yields one match per type, so `implements C, D` produces two references.
	// Solidity names its bases after `is`, interfaces and contracts alike, so every
	// one is an `extends`. A qualified base (`Lib.Base`) is the last identifier.
	"solidity": `
(inheritance_specifier (user_defined_type (identifier) @name @reference.extends .))
`,

	// Apex's `extends` and `implements` are Java's, with the grammar's own node
	// names (`interfaces` for the implements list). A class has one superclass; an
	// interface extends several.
	"apex": `
(superclass (type_identifier) @name @reference.extends)
(superclass (generic_type . (type_identifier) @name @reference.extends))
(superclass (scoped_type_identifier (type_identifier) @name @reference.extends .))
(interfaces (type_list (type_identifier) @name @reference.implementation))
(interfaces (type_list (generic_type . (type_identifier) @name @reference.implementation)))
(interfaces (type_list (scoped_type_identifier (type_identifier) @name @reference.implementation .)))
(extends_interfaces (type_list (type_identifier) @name @reference.extends))
(extends_interfaces (type_list (generic_type . (type_identifier) @name @reference.extends)))
(extends_interfaces (type_list (scoped_type_identifier (type_identifier) @name @reference.extends .)))
`,

	"java": `
(superclass (type_identifier) @name @reference.extends)
(superclass (generic_type (type_identifier) @name @reference.extends))
(superclass (scoped_type_identifier (type_identifier) @name @reference.extends .))
(superclass (generic_type (scoped_type_identifier (type_identifier) @name @reference.extends .)))
(super_interfaces (type_list (type_identifier) @name @reference.implementation))
(super_interfaces (type_list (generic_type (type_identifier) @name @reference.implementation)))
(super_interfaces (type_list (scoped_type_identifier (type_identifier) @name @reference.implementation .)))
(extends_interfaces (type_list (type_identifier) @name @reference.extends))
(extends_interfaces (type_list (generic_type (type_identifier) @name @reference.extends)))
(extends_interfaces (type_list (scoped_type_identifier (type_identifier) @name @reference.extends .)))
`,

	// TypeScript splits the two clauses the same way Java does, but hangs them
	// off class_heritage:
	//
	//	class A extends B          -> (class_heritage (extends_clause ...))
	//	class A implements C       -> (class_heritage (implements_clause ...))
	//	interface I extends J      -> (extends_type_clause ...)
	//
	// The extends_clause holds a VALUE expression, not a type — TypeScript
	// permits `class A extends mixin(B)` — so its child is an identifier or a
	// member_expression, and `extends R<T>` puts the type arguments in a
	// SIBLING type_arguments node rather than wrapping R. That is why there is
	// no generic_type rule for extends_clause and there is one for
	// implements_clause, where the child genuinely is a type.
	//
	// abstract_class_declaration reuses class_heritage, so abstract classes
	// need no separate rule.
	"typescript": `
(extends_clause (identifier) @name @reference.extends)
(extends_clause (member_expression property: (property_identifier) @name @reference.extends))
(implements_clause (type_identifier) @name @reference.implementation)
(implements_clause (generic_type (type_identifier) @name @reference.implementation))
(implements_clause (nested_type_identifier (type_identifier) @name @reference.implementation .))
(extends_type_clause (type_identifier) @name @reference.extends)
(extends_type_clause (generic_type (type_identifier) @name @reference.extends))
(extends_type_clause (nested_type_identifier (type_identifier) @name @reference.extends .))
`,

	// Identical to typescript: .tsx is the same grammar plus JSX, and a React
	// class component extending a base class is the common case here.
	"tsx": `
(extends_clause (identifier) @name @reference.extends)
(extends_clause (member_expression property: (property_identifier) @name @reference.extends))
(implements_clause (type_identifier) @name @reference.implementation)
(implements_clause (generic_type (type_identifier) @name @reference.implementation))
(implements_clause (nested_type_identifier (type_identifier) @name @reference.implementation .))
(extends_type_clause (type_identifier) @name @reference.extends)
(extends_type_clause (generic_type (type_identifier) @name @reference.extends))
(extends_type_clause (nested_type_identifier (type_identifier) @name @reference.extends .))
`,

	// JavaScript has no implements clause and no separate extends_clause: the
	// superclass expression hangs directly off class_heritage. `class C extends
	// React.Component` is the shape that matters in practice, hence the
	// member_expression rule capturing the property, not the namespace.
	"javascript": `
(class_heritage (identifier) @name @reference.extends)
(class_heritage (member_expression property: (property_identifier) @name @reference.extends))
`,

	// Python's base-class list is an argument_list on the class_definition's
	// superclasses field. reference.inherit rather than reference.extends
	// because Python's own vocabulary is inheritance, and both land on
	// EdgeExtends.
	//
	// Python has no interface construct — an ABC is a base class — so nothing
	// here emits reference.implementation.
	//
	// keyword arguments are a distinct node, so `class D(E, metaclass=M)`
	// yields E and not M: a metaclass is not a base class, and reporting it as
	// one would put a false edge on the graph.
	//
	// A base written with a subscript, `class C(ParamType[str])`, names ParamType
	// (click: 17 of 30 declared supertypes were missing). `Generic[T]` and
	// `Protocol[T]` are not bases of the program's own: they declare a type
	// parameter list and a structural type, and are recorded for no form, plain
	// or subscripted, bare or reached through typing, typing_extensions or t.
	"python": `
(class_definition superclasses: (argument_list (identifier) @name @reference.inherit (#not-any-of? @name "Generic" "Protocol")))
(class_definition superclasses: (argument_list (attribute object: (_) @_o attribute: (identifier) @name) @reference.inherit (#not-any-of? @_o "typing" "typing_extensions" "t")))
(class_definition superclasses: (argument_list (subscript value: (identifier) @name @reference.inherit (#not-any-of? @name "Generic" "Protocol"))))
(class_definition superclasses: (argument_list (subscript value: (attribute object: (_) @_o attribute: (identifier) @name) @reference.inherit (#not-any-of? @_o "typing" "typing_extensions" "t"))))
`,

	// C# writes one base_list after the colon and the grammar does not say which
	// entry is the base class and which are interfaces:
	//
	//	class A : B, IFoo, IBar<T>   -> (base_list (identifier) (identifier) (generic_name ...))
	//	struct S : IFoo              -> (struct_declaration (base_list (identifier)))
	//	interface I : IBase          -> (interface_declaration (base_list (identifier)))
	//	record R(int X) : Base(X), IFoo
	//	                             -> (base_list (primary_constructor_base_type ...) (identifier))
	//
	// Where the declaration decides, the pattern follows it: a struct can only
	// implement interfaces, an interface can only extend them, and a record's
	// `Base(...)` call is a base class. In a class (or record) list nothing in
	// the syntax decides, so the .NET naming convention does: a name shaped
	// `I` + capital + lower-case (IDisposable, IFoo) is an interface and is
	// IMPLEMENTS, anything else is EXTENDS. The limitation is that a class
	// implementing an interface not named that way is recorded as EXTENDS to it,
	// and a base class named like `IFoo` as IMPLEMENTS. Both channels are
	// consulted together when a call is bound through the ancestry, so the
	// label changes the edge kind and not the answer.
	//
	// An enum's base_list is its underlying integral type and is not captured.
	// Each shape is spelled four ways: plain (B), generic (B<T>), and either
	// qualified (Ns.B, Ns.B<T>). The last segment is captured, because the
	// resolver matches a reference by simple name.
	"c_sharp": `
(class_declaration (base_list (identifier) @name @reference.implementation (#match? @name "^I[A-Z][a-z]")))
(class_declaration (base_list (generic_name . (identifier) @name @reference.implementation) (#match? @name "^I[A-Z][a-z]")))
(class_declaration (base_list (qualified_name (identifier) @name @reference.implementation .) (#match? @name "^I[A-Z][a-z]")))
(class_declaration (base_list (qualified_name (generic_name . (identifier) @name @reference.implementation) .) (#match? @name "^I[A-Z][a-z]")))
(class_declaration (base_list (identifier) @name @reference.extends (#not-match? @name "^I[A-Z][a-z]")))
(class_declaration (base_list (generic_name . (identifier) @name @reference.extends) (#not-match? @name "^I[A-Z][a-z]")))
(class_declaration (base_list (qualified_name (identifier) @name @reference.extends .) (#not-match? @name "^I[A-Z][a-z]")))
(class_declaration (base_list (qualified_name (generic_name . (identifier) @name @reference.extends) .) (#not-match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (identifier) @name @reference.implementation (#match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (generic_name . (identifier) @name @reference.implementation) (#match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (qualified_name (identifier) @name @reference.implementation .) (#match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (qualified_name (generic_name . (identifier) @name @reference.implementation) .) (#match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (identifier) @name @reference.extends (#not-match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (generic_name . (identifier) @name @reference.extends) (#not-match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (qualified_name (identifier) @name @reference.extends .) (#not-match? @name "^I[A-Z][a-z]")))
(record_declaration (base_list (qualified_name (generic_name . (identifier) @name @reference.extends) .) (#not-match? @name "^I[A-Z][a-z]")))
(struct_declaration (base_list (identifier) @name @reference.implementation))
(struct_declaration (base_list (generic_name . (identifier) @name @reference.implementation)))
(struct_declaration (base_list (qualified_name (identifier) @name @reference.implementation .)))
(struct_declaration (base_list (qualified_name (generic_name . (identifier) @name @reference.implementation) .)))
(interface_declaration (base_list (identifier) @name @reference.extends))
(interface_declaration (base_list (generic_name . (identifier) @name @reference.extends)))
(interface_declaration (base_list (qualified_name (identifier) @name @reference.extends .)))
(interface_declaration (base_list (qualified_name (generic_name . (identifier) @name @reference.extends) .)))
(primary_constructor_base_type (identifier) @name @reference.extends)
(primary_constructor_base_type (generic_name . (identifier) @name @reference.extends))
(primary_constructor_base_type (qualified_name (identifier) @name @reference.extends .))
(primary_constructor_base_type (qualified_name (generic_name . (identifier) @name @reference.extends) .))
`,

	// C++ lists its bases after the colon, each optionally preceded by an access
	// specifier and `virtual`:
	//
	//	class A : public B, private C   -> (base_class_clause (access_specifier) (type_identifier) ...)
	//	struct S : B                    -> the same node under struct_specifier
	//	class A : public Base<T>        -> (template_type (type_identifier) (template_argument_list))
	//	class A : public ns::B          -> (qualified_identifier (namespace_identifier) (type_identifier))
	//
	// All of them are EXTENDS: C++ has no interface construct, and an abstract
	// class used as one is a base class to the grammar. One clause serves class
	// and struct, so no pattern names the parent.
	//
	// A qualified base is captured by its last segment, at up to three levels
	// (`a::b::C`, which nests as qualified_identifier inside qualified_identifier),
	// and only when the qualifier is a namespace: `Base<T>::type`, a member of a
	// dependent type, names nothing a symbol could be bound to, and capturing its
	// `type` would be a false edge. Out-of-line method definitions `A::m()` are a
	// separate gap and not read here.
	"cpp": `
(base_class_clause (type_identifier) @name @reference.extends)
(base_class_clause (template_type . (type_identifier) @name @reference.extends))
(base_class_clause (qualified_identifier . (type_identifier) @name @reference.extends .))
(base_class_clause (qualified_identifier . (namespace_identifier) (type_identifier) @name @reference.extends .))
(base_class_clause (qualified_identifier . (namespace_identifier) (template_type . (type_identifier) @name @reference.extends) .))
(base_class_clause (qualified_identifier . (namespace_identifier) (qualified_identifier . (namespace_identifier) (type_identifier) @name @reference.extends .) .))
(base_class_clause (qualified_identifier . (namespace_identifier) (qualified_identifier . (namespace_identifier) (template_type . (type_identifier) @name @reference.extends) .) .))
(base_class_clause (qualified_identifier . (namespace_identifier) (qualified_identifier . (namespace_identifier) (qualified_identifier . (namespace_identifier) (type_identifier) @name @reference.extends .) .) .))
`,

	// Rust has no classes; the hierarchy is traits.
	//
	//	trait A: B + C       -> (trait_item (trait_bounds (type_identifier) (type_identifier)))
	//	trait A: ns::B<T>    -> (trait_bounds (generic_type (scoped_type_identifier ...)))
	//
	// A supertrait is EXTENDS, as an interface that extends another is in Java.
	// Lifetimes (`'static`) are a different node and are not captured, and a
	// `where Self: B` clause is not a supertrait list.
	//
	// The standard library's traits are not captured. A bound like `Debug +
	// Send + Sync` is nearly always the prelude's, and the resolver binds by
	// name: ripgrep's `trait Flag: Debug` was bound, at medium, to an unrelated
	// struct called `Debug` in the next file of the same directory, because a
	// sibling module is a same-directory candidate. The list is the prelude,
	// the marker and operator traits, and the std::io, std::fmt and std::error
	// ones that are written without a `std::` prefix. A repository that defines
	// its own trait under one of these names loses that supertrait edge, which is
	// the cheaper error.
	//
	// `impl Trait for Type` declares IMPLEMENTS from Type to Trait. The
	// reference sits on the impl block, which is no symbol, and the resolver
	// attributes a reference to the innermost symbol on its line, so the edge
	// would have no source. The pattern therefore also captures the type named
	// by `type:` as @implementer; the parser carries it on the reference
	// (Receiver, with model.ReceiverImplementer) and the resolver uses it as the
	// edge's source. The trait and the type each take any of four spellings:
	// `T`, `T<A>`, `path::T` and `path::T<A>`, captured by their last segment.
	//
	// Not captured, and why:
	//   - `impl Type { }` has no `trait:` field; it is not inheritance.
	//   - `impl<T: Bound> Trait for T` is blanket: the type is one of the impl's
	//     own type parameters, so there is no single type. The parser drops it
	//     (isImplTypeParameter), because a query cannot compare two nodes.
	//   - `impl Trait for &T`, `[T]`, `(A, B)` and `fn(..)` write no type name.
	//   - The standard-library traits, by the same list and for the same reason
	//     as the supertraits above: `impl Display for Foo` is nearly always the
	//     prelude's trait, and the name alone would bind it to a repository
	//     struct of that name.
	"rust": `
((impl_item
  trait: [
    (type_identifier) @name @reference.implementation
    (generic_type type: (type_identifier) @name @reference.implementation)
    (scoped_type_identifier name: (type_identifier) @name @reference.implementation)
    (generic_type type: (scoped_type_identifier name: (type_identifier) @name @reference.implementation))
  ]
  type: [
    (type_identifier) @implementer
    (generic_type type: (type_identifier) @implementer)
    (scoped_type_identifier name: (type_identifier) @implementer)
    (generic_type type: (scoped_type_identifier name: (type_identifier) @implementer))
  ]) @impl
  (#not-match? @name "^(Debug|Display|Clone|Copy|Send|Sync|Sized|Unpin|Default|Eq|PartialEq|Ord|PartialOrd|Hash|Iterator|IntoIterator|DoubleEndedIterator|ExactSizeIterator|From|Into|TryFrom|TryInto|AsRef|AsMut|Borrow|ToString|ToOwned|Drop|Fn|FnMut|FnOnce|Deref|DerefMut|Any|Error|Read|Write|BufRead|Seek|UnwindSafe|RefUnwindSafe|Future|Extend|FromStr)$"))
(trait_item (trait_bounds (type_identifier) @name @reference.extends (#not-match? @name "^(Debug|Display|Clone|Copy|Send|Sync|Sized|Unpin|Default|Eq|PartialEq|Ord|PartialOrd|Hash|Iterator|IntoIterator|DoubleEndedIterator|ExactSizeIterator|From|Into|TryFrom|TryInto|AsRef|AsMut|Borrow|ToString|ToOwned|Drop|Fn|FnMut|FnOnce|Deref|DerefMut|Any|Error|Read|Write|BufRead|Seek|UnwindSafe|RefUnwindSafe|Future|Extend|FromStr)$")))
(trait_item (trait_bounds (generic_type type: (type_identifier) @name @reference.extends (#not-match? @name "^(Debug|Display|Clone|Copy|Send|Sync|Sized|Unpin|Default|Eq|PartialEq|Ord|PartialOrd|Hash|Iterator|IntoIterator|DoubleEndedIterator|ExactSizeIterator|From|Into|TryFrom|TryInto|AsRef|AsMut|Borrow|ToString|ToOwned|Drop|Fn|FnMut|FnOnce|Deref|DerefMut|Any|Error|Read|Write|BufRead|Seek|UnwindSafe|RefUnwindSafe|Future|Extend|FromStr)$"))))
(trait_item (trait_bounds (scoped_type_identifier name: (type_identifier) @name @reference.extends (#not-match? @name "^(Debug|Display|Clone|Copy|Send|Sync|Sized|Unpin|Default|Eq|PartialEq|Ord|PartialOrd|Hash|Iterator|IntoIterator|DoubleEndedIterator|ExactSizeIterator|From|Into|TryFrom|TryInto|AsRef|AsMut|Borrow|ToString|ToOwned|Drop|Fn|FnMut|FnOnce|Deref|DerefMut|Any|Error|Read|Write|BufRead|Seek|UnwindSafe|RefUnwindSafe|Future|Extend|FromStr)$"))))
(trait_item (trait_bounds (generic_type type: (scoped_type_identifier name: (type_identifier) @name @reference.extends (#not-match? @name "^(Debug|Display|Clone|Copy|Send|Sync|Sized|Unpin|Default|Eq|PartialEq|Ord|PartialOrd|Hash|Iterator|IntoIterator|DoubleEndedIterator|ExactSizeIterator|From|Into|TryFrom|TryInto|AsRef|AsMut|Borrow|ToString|ToOwned|Drop|Fn|FnMut|FnOnce|Deref|DerefMut|Any|Error|Read|Write|BufRead|Seek|UnwindSafe|RefUnwindSafe|Future|Extend|FromStr)$")))))
`,

	// Ruby: a single superclass and any number of mixins.
	//
	//	class A < B           -> (class (constant) (superclass (constant)))
	//	class A < Mod::B      -> (superclass (scope_resolution (constant) (constant)))
	//	include M, extend M,
	//	prepend M             -> (call (identifier) (argument_list (constant)))
	//
	// The superclass is EXTENDS. A mixin is IMPLEMENTS, the relation Java gives
	// an interface and Rust a trait: the module contributes methods without
	// being the class's parent. `include` and `prepend` put the module in the
	// instance ancestry. `extend` puts it in the CLASS's singleton ancestry
	// (its methods become class methods), which this graph cannot tell apart,
	// so a call to a method that only an extended module defines may bind to it
	// from an instance method too; the resolver only ever consults ancestry for
	// a call it could not bind by name, which bounds the damage.
	//
	// `!receiver` keeps the mixin patterns to the bare statement. `foo.include
	// M` and `Class.new { include M }` on a receiver are not a class body
	// declaring its ancestry. Only a constant is captured: `include
	// Comparable` names a module, `include some_helper` does not.
	//
	// `class A < Struct.new(:x)` and `class A < Class.new(B)` put a CALL where
	// the superclass is, and capture nothing, deliberately: the base is built at
	// run time, not named.
	"ruby": `
(superclass (constant) @name @reference.extends)
(superclass (scope_resolution name: (constant) @name @reference.extends))
(call !receiver method: (identifier) @_m arguments: (argument_list (constant) @name @reference.implementation) (#match? @_m "^(include|extend|prepend)$"))
(call !receiver method: (identifier) @_m arguments: (argument_list (scope_resolution name: (constant) @name @reference.implementation)) (#match? @_m "^(include|extend|prepend)$"))
`,

	// PHP: `class A extends B implements C, D`, `interface I extends J, K`, and
	// `use SomeTrait;` in a class body, which brings the trait's methods into
	// the class the way a parent's are, so `$this->m()` reaches them. A name
	// written with namespaces keeps its last segment, as in Java.
	"php": `
(class_declaration (base_clause (name) @name @reference.extends))
(class_declaration (base_clause (qualified_name (name) @name @reference.extends .)))
(class_declaration (class_interface_clause (name) @name @reference.implementation))
(class_declaration (class_interface_clause (qualified_name (name) @name @reference.implementation .)))
(interface_declaration (base_clause (name) @name @reference.extends))
(interface_declaration (base_clause (qualified_name (name) @name @reference.extends .)))
(use_declaration (name) @name @reference.inherit)
(use_declaration (qualified_name (name) @name @reference.inherit .))
`,

	// Kotlin: the first supertype of `class A : B(), C` is written with a
	// constructor call and is the superclass; the others are interfaces. A
	// supertype may be qualified (`a.B`, last segment) or generic (`B<T>`).
	"kotlin": `
(class_declaration (delegation_specifier (constructor_invocation (user_type (type_identifier) @name @reference.extends .))))
(class_declaration (delegation_specifier (constructor_invocation (user_type (type_identifier) @name @reference.extends . (type_arguments)))))
(class_declaration (delegation_specifier (user_type (type_identifier) @name @reference.implementation .)))
(class_declaration (delegation_specifier (user_type (type_identifier) @name @reference.implementation . (type_arguments))))
`,

	// Scala: `extends A with B with C`. The first is the superclass or a trait
	// and the rest are traits, and all of them are what a call inside the class
	// may reach, so all are EXTENDS. A parent may be qualified (`p.A`, last
	// segment) or generic (`A[T]`).
	"scala": `
(extends_clause (type_identifier) @name @reference.extends)
(extends_clause (generic_type type: (type_identifier) @name @reference.extends))
(extends_clause (stable_type_identifier (type_identifier) @name @reference.extends .))
`,
}

// InheritanceQuery returns the supertype-reference patterns for a language, or
// "". Appended to the tags query; see newGenericParser for the fallback that
// keeps a rejected one from costing the language its symbols.
func InheritanceQuery(language string) string { return inheritanceQueries[language] }

// InheritanceLanguages lists the languages that emit supertype references.
func InheritanceLanguages() []string {
	out := make([]string, 0, len(inheritanceQueries))
	for k := range inheritanceQueries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// importQueries extract a file's declared dependencies.
//
// Tags queries capture definitions and references but not imports, so these run
// as a separate query against the same syntax tree.
//
// Imports are what let the resolver distinguish a high-confidence binding from
// a bare name match: knowing a file imports com.pay.repository narrows
// `repository.save(...)` to one package instead of every `save` in the repo.
// Without them, cross-file resolution degrades to name matching, which the
// design explicitly rejects.
//
// Captures:
//
//	@path   the module/package as written
//	@alias  the local binding, where the language allows renaming
var importQueries = map[string]string{
	"go": `
(import_spec path: (interpreted_string_literal) @path)
(import_spec name: (package_identifier) @alias path: (interpreted_string_literal) @path)
`,

	"java": `
(import_declaration (scoped_identifier) @path)
`,

	// The relative_import pattern is not a nicety. `from .helpers import X` is
	// ordinary Python and it parses as
	// (import_from_statement module_name: (relative_import (import_prefix) (dotted_name))),
	// a DIFFERENT node from the absolute `module_name: (dotted_name)` above — so
	// before this pattern existed a file whose imports were entirely relative
	// arrived at the resolver declaring NOTHING. Probed, not assumed: on
	//
	//	from scrapy.utils.test import get_crawler
	//	from .helpers import Helper
	//	from ..pkg.mod import Thing
	//
	// the query without it captured one import out of three.
	//
	// That is the worst possible shape of gap for this resolver. Python is a
	// file-is-a-module language, so LanguageSpec.PackageIsDirectory is false and
	// imports are consulted BEFORE proximity — precisely because adjacency
	// asserts nothing there. A file with no imports has no import tier at all,
	// so every reference in it falls through to the same-directory tier the
	// language ordering exists to demote. docs/accuracy.md records what that
	// costs when it happens: `tests/test_addons.py` bound `get_crawler` to an
	// unrelated helper in a neighbouring test file, 144 wrong edges from one
	// name and ~45% of every high-confidence disagreement in the repository.
	//
	// The captured path is the relative import AS WRITTEN, leading dots
	// included (".helpers", "..pkg.mod"). It used to be the dotted name alone,
	// on the grounds that resolving `..` needs the referencing file's own
	// directory, which buildImportSet does not have. That is still true of the
	// import TIER: buildImportSet trims the dots and matches the rest as an
	// unanchored module name, exactly as before, so a unique match is High and
	// several are Medium. Keeping the dots is for the two rules that can use
	// them — a name imported from the module, and a call qualified by it —
	// which do know the referencing file and anchor the path to its directory.
	// Without the dots a repository with a `models.py` in every package matches
	// `from .models import Article` against all of them.
	//
	// The aliased pattern is anchored on import_statement, and the anchor is a
	// correction. `aliased_import` is also how the grammar spells a renamed
	// MEMBER — `from shop.models import Account as Acct` — and unanchored, the
	// pattern recorded that as an import of a module called "Account" under the
	// local name "Acct": a path that is not a module, matched against file
	// stems by the import tier, and a module alias the resolver then read
	// `Acct.close()` as package-qualified by. The module of a from-import is
	// captured by the two patterns above it; the names it brings in are read by
	// pyscope.go, which can tell the two shapes apart.
	"python": `
(import_statement name: (dotted_name) @path)
(import_from_statement module_name: (dotted_name) @path)
(import_from_statement module_name: (relative_import (dotted_name)) @path)
(import_statement name: (aliased_import name: (dotted_name) @path alias: (identifier) @alias))
`,

	"typescript": `
(import_statement source: (string) @path)
(export_statement source: (string (string_fragment) @path)) @import
` + scriptRequireImports,

	"tsx": `
(import_statement source: (string) @path)
(export_statement source: (string (string_fragment) @path)) @import
` + scriptRequireImports,

	"javascript": `
(import_statement source: (string) @path)
(export_statement source: (string (string_fragment) @path)) @import
` + scriptRequireImports,

	"rust": `
(use_declaration argument: (scoped_identifier) @path)
(use_declaration argument: (identifier) @path)
`,

	// Ruby has no import statement; `require` and `require_relative` are
	// ordinary method calls, so the query matches the call shape and the
	// resolver treats the string argument as the imported path.
	"ruby": `
(call
  method: (identifier) @_m
  arguments: (argument_list (string (string_content) @path))
  (#match? @_m "^(require|require_relative|load|autoload)$")) @import
`,

	// PHP: `use` for namespace imports, plus require/include which take an
	// expression rather than a bare string and are matched loosely.
	//
	// The `use` declarations are not here: they are read from the tree by
	// phpUses (lang_php.go), which writes their paths with dots so the
	// resolver can split them, and reads aliases and the group form.
	"php": `
(expression_statement
  (require_expression (string) @path)) @import

(expression_statement
  (include_expression (string) @path)) @import
`,

	// C#: `using` directives, including the aliased form whose path is the
	// right-hand qualified name rather than the whole clause.
	"c_sharp": `
(using_directive
  (qualified_name) @path) @import

(using_directive
  (identifier) @path) @import
`,

	// Kotlin: an import directive, with the alias of `import a.B as C`. The
	// plain pattern also matches the aliased form, and dedupeImports keeps the
	// row that carries the alias. A wildcard (`import a.*`) is the package
	// itself.
	"kotlin": `
(import_header
  (identifier) @path) @import

(import_header
  (identifier) @path
  (import_alias (type_identifier) @alias)) @import
`,

	// Scala: the rows come from scalaImports (lang_scala.go), which reads the
	// tree. A dotted path is spread over sibling identifiers with no node that
	// spans it, so no query can capture `a.b.C` — the pattern that stood here
	// captured the last identifier alone, which names no package, and renames
	// and wildcards were not read at all. This pattern binds no @path and so
	// adds no row; it is the statement that the language's imports are read.
	"scala": `
(import_declaration) @import
`,

	// Swift imports a MODULE, not a file: `import Core`, `@testable import Core`,
	// `import struct Core.Money`. swiftImport (lang_swift.go) reduces each to its
	// first word, the module.
	"swift": `
(import_declaration (identifier) @path)
`,

	// Dart: import, export and part directives all name a file. The captured
	// path is rewritten by dartImport (lang_dart.go), which has the importing
	// file's directory to resolve a relative URI against.
	"dart": `
(import_specification (configurable_uri (uri (string_literal) @path)) (identifier) @alias)
(import_specification (configurable_uri (uri (string_literal) @path)))
(library_export "export" @directive (configurable_uri (uri (string_literal) @path)))
(part_directive "part" @directive (uri (string_literal) @path))
(part_of_directive (uri (string_literal) @path))
`,

	// Objective-C: a quoted #import or #include names a header of the repository.
	// A <Framework/Header.h> is a system or framework header and an `@import
	// Module;` names a module: neither is repository code, and neither is
	// captured. objcImport (lang_objc.go) drops the extension.
	"objc": `
(preproc_include path: (string_literal) @path)
`,

	// Apex has no imports. A class is global to the org by its name: `Foo` in
	// classes/Foo.cls is visible from every other class, a namespace's classes
	// are reached as `ns.Foo`, and nothing in a file says where a name comes
	// from. This pattern binds no @path and so adds no row; it is the statement
	// that the language's imports are read, and that there are none to read. What
	// the resolver does in their place is the spec's (resolve/infer, "apex").
	"apex": `
(class_declaration) @import
`,

	// Solidity: `import "./A.sol";`, `import "./A.sol" as A;`, `import * as L from
	// "./L.sol";` and `import {A, B as C} from "./x.sol";` each name a file by
	// the string that ends the directive. solidityImport (lang_solidity.go)
	// rewrites the path to the repository path of the file it names, and the
	// names the directive binds are read from the tree (solidityBindings).
	"solidity": `
(import_directive (string) @path)
`,

	// Lua: `require("a.b")`, `require "a.b"` and `local m = require("a.b")`. The
	// path is the dotted name as written, which the resolver reads as the
	// segments of a file (a/b.lua or a/b/init.lua) under some source root; the
	// alias is the local the module is bound to, which is what `m.f()` is
	// qualified by.
	"lua": `
(function_call . (identifier) @_f (arguments . (string (string_content) @path) .)
  (#eq? @_f "require")) @import
(variable_declaration (assignment_statement (variable_list . (identifier) @alias .)
  (expression_list . (function_call . (identifier) @_f (arguments . (string (string_content) @path) .))))
  (#eq? @_f "require")) @import
(chunk (assignment_statement (variable_list . (identifier) @alias .)
  (expression_list . (function_call . (identifier) @_f (arguments . (string (string_content) @path) .))))
  (#eq? @_f "require")) @import
`,

	// Bash: `source x.sh` and `. ./lib/x.sh` name a file. The word is captured
	// whole, whatever it is made of (a quoted string, a `$(dirname "$0")/x.sh`
	// concatenation), because which spellings name a file of the repository is a
	// question about text that bashImport (lang_bash.go) answers.
	//
	// bats's `load helper` names a file beside the test too. The command name is
	// captured as the alias so that bashImport can tell the two apart (and keeps
	// `load` only in a .bats file, where it is not some script's own function);
	// it is cleared there, because an alias would read as a module qualifier.
	"bash": `
(command (command_name (word) @alias) . [(word) (string) (raw_string) (concatenation)] @path
  (#any-of? @alias "source" "." "load"))
`,

	// R: `source("x.R")` and `sys.source("x.R", env)` name a file; `library(pkg)`,
	// `require(pkg)` and `requireNamespace("pkg")` name a package. The first
	// argument is captured whole (a string, a bare name, `file.path("R", "x.R")`)
	// and its function as the alias, and rImport (lang_r.go) reads the two.
	"r": `
(call function: (identifier) @alias arguments: (arguments . (argument value: (_) @path))
  (#any-of? @alias "source" "sys.source" "library" "require" "requireNamespace" "loadNamespace" "attachNamespace"))
`,

	// Elixir: `alias`, `import`, `use` and `require` are calls. The whole directive
	// is captured, because what it says (a group of aliases, `as:`, `only:`) is
	// text that elixirImport (lang_elixir.go) reads, and each row names a module.
	"elixir": `
((call (identifier) @_d) @path
  (#any-of? @_d "alias" "import" "use" "require"))
`,

	// PowerShell: a dot-source (`. ./x.ps1`), `Import-Module` and `using module`.
	// All three are commands, and the whole command is captured: which of its
	// words is the file, and how a path built from $PSScriptRoot reads, is text
	// that powershellImport (lang_powershell.go) answers.
	"powershell": `
((command (command_name) @_c) @path
  (#match? @_c "^(?i:import-module|ipmo|using)$"))
((command (command_invokation_operator) @_op) @path
  (#eq? @_op "."))
`,

	// Erlang: `-import(m, [f/1, g/2]).` brings functions of a module in under
	// their own names. The whole attribute is captured, and erlangImport
	// (lang_erlang.go) reads the module and the names out of its text. Nothing
	// else names a module: `m:f()` needs no import, and an -include is a header
	// of records and macros.
	"erlang": `
(import_attribute) @path
`,

	// Clojure: `(ns a.b (:require [c.d :as d :refer [f]]) (:use ...))` and a
	// top-level `(require '[c.d :as d])` or `(use ...)`. The whole form is
	// captured because what it says (a prefix list, `:as`, `:refer`, `:only`)
	// is text that clojureImport (lang_clojure.go) reads, and each row names a
	// namespace.
	"clojure": `
((source (list_lit . (sym_lit (sym_name) @_h)) @path)
  (#any-of? @_h "ns" "require" "use"))
`,

	// Julia: `using`, `import` and `include("x.jl")`. Each whole statement is
	// captured because what it says (a module, a list of names, an alias) is
	// text that juliaImport (lang_julia.go) reads, and each row names a module,
	// or for an include a file.
	"julia": `
(using_statement) @path
(import_statement) @path
((call_expression . (identifier) @_f (argument_list . [(string_literal) (call_expression)])) @path
  (#eq? @_f "include"))
`,

	// Groovy: `import a.b.C`, `import a.b.*`, `import static a.b.C.m` and
	// `import a.b.C as D`. The qualified name is the class (or package, or static
	// member) as written, and is read as Java's is; an alias is the identifier
	// after it. The plain pattern also matches the aliased form, and dedupeImports
	// keeps the row that carries the alias.
	"groovy": `
(groovy_import (qualified_name) @path)
(groovy_import (qualified_name) @path (identifier) @alias)
`,

	// Haskell: `import A.B`, `import qualified A.B as C`, `import A.B (f, g)` and
	// `import A.B hiding (f)`. The whole statement is captured, because what it
	// says (a qualifier, a list of names, hiding) is text that haskellImport
	// (lang_haskell.go) reads, and each row names a module.
	"haskell": `
(import) @path
`,

	// OCaml: `open A`, `include A` and `module X = A.B`, each captured whole
	// because what it says is text that ocamlImport (lang_ocaml.go) reads, and
	// each row names a module. A file `a.ml` is the module `A`, so no statement
	// is needed for `A.f` to be written: this one is for what brings a module's
	// names in bare, or renames it.
	//
	// An `open` inside an expression (`let open A in`) is a different node under
	// a different parent, and names a module for that expression alone: only the
	// statements of a file or of a module's structure are read.
	"ocaml": `
(compilation_unit (open_module) @path)
(compilation_unit (include_module) @path)
(structure (open_module) @path)
(structure (include_module) @path)
(module_definition (module_binding (module_name) body: (module_path))) @path
`,

	// Perl: `use A::B qw(f g);`, `use parent 'A::B';` and `require A::B;`. The
	// whole statement is captured, because what it says (a list of names, a
	// parent) is text that perlImport (lang_perl.go) reads, and each row names a
	// package. A pragma (`use strict`) names none and is dropped there.
	"perl": `
(use_statement) @path
(require_expression (bareword)) @path
`,

	// Zig: `const x = @import("x.zig");`, `const f = @import("x.zig").f;` and a
	// bare `_ = @import("x.zig");` name a file relative to the importing one. The
	// whole declaration is captured, because which local name it binds, and
	// whether it is the file or one member of it, is text that zigImport
	// (lang_zig.go) reads.
	"zig": `
((variable_declaration) @path
  (#match? @path "@import"))
((assignment_expression) @path
  (#match? @path "^_\\s*=\\s*@import"))
`,

	// F#: `open A.B` and `module X = A.B`, each captured whole because what it says
	// is text that fsharpImport (lang_fsharp.go) reads, and each row names a module.
	// A module is declared and not a file, so no statement is needed for `A.f` to be
	// written: these are for what brings a module's names in bare, or renames it.
	"fsharp": `
(import_decl) @path
(module_defn (identifier) block: (long_identifier_or_op)) @path
`,

	"c": `
(preproc_include path: (system_lib_string) @path)
(preproc_include path: (string_literal) @path)
`,

	"cpp": `
(preproc_include path: (system_lib_string) @path)
(preproc_include path: (string_literal) @path)
`,
}

// Supplement returns RepoCortex's extra tag patterns for a language, or "".
func Supplement(language string) string { return supplements[language] }

// ImportQuery returns the import-extraction query for a language, or "".
func ImportQuery(language string) string { return importQueries[language] }

// SupplementedLanguages lists languages carrying a RepoCortex supplement. Used by
// tests and by `repocortex doctor` to report where coverage is strongest.
func SupplementedLanguages() []string {
	out := make([]string, 0, len(supplements))
	for k := range supplements {
		out = append(out, k)
	}
	return out
}

// receiverLanguages are the languages whose call receivers have been part of
// the `typed` tier's claim since that tier was first derived.
//
// It used to be the key set of a map of receiver QUERIES, one tree-sitter
// pattern per language, each capturing a simple identifier in front of a call.
// Those queries are gone: shape.go reads the receiver of every call in every
// language from the tag's own ranges, and tells a bare call from one made on a
// path, on `self`, or on an expression no query could express — which the
// queries could not, and which cost every chained Go call its receiver (A18 in
// internal/resolve/infer).
//
// The set is kept because TierFor derives a published tier from it. Every
// language has receivers now; which languages have had their receiver handling
// MEASURED is a narrower claim, and it is the one the tier makes. Adding a
// language here is a statement about a measurement, not about the parser.
var receiverLanguages = map[string]bool{
	"java": true, "go": true, "python": true, "typescript": true, "javascript": true,
	"kotlin": true, "rust": true, "c_sharp": true, "cpp": true, "ruby": true,
	"php": true, "scala": true,
}

// HasMeasuredReceivers reports whether a language's receiver handling is part
// of the `typed` tier's claim. See receiverLanguages.
func HasMeasuredReceivers(language string) bool { return receiverLanguages[language] }

// # A4 — arity queries
//
// paramCountQueries and argCountQueries are the two halves of one fact: how
// many parameters a definition declares, and how many arguments a call site
// passes. Together they let internal/resolve/infer prefer the `Save(id)` over
// the `Save(ctx, id, opts)` when a file references both by name.
//
// Each query captures @name on the SAME identifier node the tags query already
// reports, so the results can be matched to the symbol or reference at that
// position without either query knowing about the other. See the callSite type
// in generic.go for why the match is on line plus name and never on column.
//
// # Python is not listed, and that is a measurement statement rather than a gap
//
// Kotlin, Scala, C# and C++ register theirs from arity_langs.go, with the rule
// each one counts by.
//
// The counting itself is language-specific in a way that is easy to get subtly
// wrong, and a wrong count is worse than no count:
//
//   - Go declares several parameters in one group — `func f(a, b int)` is ONE
//     parameter_declaration carrying two `name:` fields, so counting nodes
//     reports 1 for a function of arity 2. arityDialects handles that.
//   - Python's `self` is written in the parameter list and NOT at the call
//     site, so every method would appear to be off by one; its default and
//     keyword arguments break the correspondence further.
//   - TypeScript's optional and defaulted parameters mean a declared count of
//     three is satisfied by a call passing one.
//   - Java is the language overload resolution matters most in, and it is the
//     one language docs/accuracy.md has no compiler ground truth for.
//
// Python was built and measured, and is not here. The count was made honest —
// a method's `self` taken off, a signature with a default, a splat or a bare
// `*` refused outright — and it then decided almost nothing: across four
// repositories adjudicated against scip-python it chose between candidates 56
// times, every one in the lowest tier, and was right once. Switching it off
// left two MORE call sites right, not fewer. docs/accuracy.md has the table.
// An arity query is a grammar-shaped contract to keep through every grammar
// bump, and this one bought nothing, so it was removed — the same decision,
// for the same reason, the TypeScript attempt records there.
//
// Variadic forms are handled everywhere they appear by refusing to count at
// all — see arityDialect. What is NOT acceptable is adding a language here on
// the strength of the grammar looking right. The rule this package keeps is
// that a language ships when a measurement against a compiler index says it
// helps, and Go is the language this repository can measure.
var paramCountQueries = map[string]string{
	"go": `
(function_declaration name: (identifier) @name parameters: (parameter_list) @params)
(method_declaration name: (field_identifier) @name parameters: (parameter_list) @params)
`,
	"java": `
(method_declaration name: (identifier) @name parameters: (formal_parameters) @params)
(constructor_declaration name: (identifier) @name parameters: (formal_parameters) @params)
`,
	"typescript": `
(function_declaration name: (identifier) @name parameters: (formal_parameters) @params)
(method_definition name: (property_identifier) @name parameters: (formal_parameters) @params)
`,
	"tsx": `
(function_declaration name: (identifier) @name parameters: (formal_parameters) @params)
(method_definition name: (property_identifier) @name parameters: (formal_parameters) @params)
`,
}

var argCountQueries = map[string]string{
	"go": `
(call_expression function: (identifier) @name arguments: (argument_list) @args)
(call_expression function: (selector_expression field: (field_identifier) @name) arguments: (argument_list) @args)
`,
	"java": `
(method_invocation name: (identifier) @name arguments: (argument_list) @args)
`,
	"typescript": `
(call_expression function: (identifier) @name arguments: (arguments) @args)
(call_expression function: (member_expression property: (property_identifier) @name) arguments: (arguments) @args)
`,
	"tsx": `
(call_expression function: (identifier) @name arguments: (arguments) @args)
(call_expression function: (member_expression property: (property_identifier) @name) arguments: (arguments) @args)
`,
}

// ParamCountQuery returns the parameter-list query for a language, or "".
func ParamCountQuery(language string) string { return paramCountQueries[language] }

// ArgCountQuery returns the argument-list query for a language, or "".
func ArgCountQuery(language string) string { return argCountQueries[language] }

// arityDialect says how to turn a parameter or argument list node into a count.
//
// A tree-sitter list node is not simply "one child per item", and the two ways
// it is not are exactly the two ways an arity can be wrong:
//
//   - a GROUP node declares several parameters at once. Go's
//     `parameter_declaration` for `a, b int` carries two `name:` fields under
//     one node. Counting nodes would report 1.
//   - a VARIADIC node makes the count open-ended. `func v(xs ...int)` accepts
//     any number, and `v(xs...)` passes an unknown number. Neither side can be
//     compared to the other, so both are reported as unknown.
//
// Unknown is the answer to every case not understood, which is what keeps this
// from manufacturing evidence. A node type in neither map that is not a plain
// item makes the whole list unknown rather than contributing a guess.
type arityDialect struct {
	// variadic node types abandon the count entirely when present.
	variadic map[string]bool

	// group node types may declare SEVERAL parameters, counted by how many
	// children carry the `name:` field, with a floor of one for an unnamed
	// parameter — `func h(int, string)` declares two parameters and names
	// neither.
	group map[string]bool

	// items, when non-nil, closes the dialect: the only named children of a
	// list that count as one item each are these types, and any other named
	// child (a default value the grammar writes beside its parameter, a
	// `params` array it leaves bare) makes the whole list unknown. A nil items
	// keeps the older, open behaviour of the Go, Java and TypeScript dialects,
	// where every named child is one item.
	items map[string]bool

	// closedIn narrows items to the lists of these types, for a language whose
	// argument lists hold any expression and so cannot be closed (C++'s
	// parameter_list is closed, its argument_list is not). Empty means every
	// list the dialect counts.
	closedIn map[string]bool

	// skip types are named children that are neither items nor a reason to
	// give up: a modifier list that belongs to the next parameter, the type
	// arguments of a call.
	skip map[string]bool

	// flatten types are wrappers whose own children are counted in their place
	// (Kotlin's `value_arguments` inside a `call_suffix`).
	flatten map[string]bool

	// opaque reports an item whose presence makes the count unknown although
	// it is a plain item by type: a C# parameter with a default value, or the
	// `this` of an extension method.
	opaque func(item *ts.Node, lang *ts.Language, src []byte) bool

	// voidList reports a list that declares nothing although it has a child:
	// C and C++ write `f(void)` for a function of no parameters.
	voidList func(list *ts.Node, lang *ts.Language, src []byte) bool
}

// arityDialects is keyed by the same language names as the queries above. A
// language with a query and no dialect counts nothing, which is safe rather
// than convenient.
var arityDialects = map[string]arityDialect{
	"go": {
		variadic: map[string]bool{
			"variadic_parameter_declaration": true,
			"variadic_argument":              true,
		},
		group: map[string]bool{
			"parameter_declaration": true,
		},
	},
	"java": {
		variadic: map[string]bool{
			"spread_parameter": true,
		},
	},
	"typescript": {
		variadic: map[string]bool{
			"rest_pattern":   true,
			"spread_element": true,
		},
	},
	"tsx": {
		variadic: map[string]bool{
			"rest_pattern":   true,
			"spread_element": true,
		},
	},
}

// arityDialectFor returns the counting rules for a language, and whether it has
// any. Unexported along with the type: these rules are how this package reads a
// syntax tree, not part of the fact it publishes.
func arityDialectFor(language string) (arityDialect, bool) {
	d, ok := arityDialects[language]
	return d, ok
}
