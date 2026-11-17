package parse

// Ada. What the index holds, see fixtures/langs/ada/expected.tsv.
//
//   - package specifications, bodies, renamings and instantiations are
//     packages (a child unit is named by its full dotted name); a generic
//     package is the package it wraps;
//   - procedures and functions are functions: specifications, bodies,
//     expression functions, generic subprograms, renamings and
//     instantiations, and nested ones. Operator functions are named with their
//     quotes ("+"). The operations of a protected type are methods, and so
//     are task and protected entries. A specification and its body are both
//     definitions, as a C prototype is not: Ada puts the specification in the
//     public .ads file, where a reader looks for it;
//   - records (tagged or not), record extensions, enumerations, task types,
//     protected types and single task and protected objects are classes;
//     interface types are interfaces; every other type and every subtype is a
//     type alias. A private type is defined by its full declaration in the
//     private part, so the "is private" view is not a second definition;
//   - record components and protected components are fields;
//   - at package level (specification or body), a number declaration or an
//     object declared "constant" is a constant, and any other object is a
//     variable. Objects local to a subprogram are not definitions. In a
//     declaration of several names (A, B : T) every name is held when the type
//     is a plain name, and only the first when it is anything else.
//
// Calls, parameters, generic formals, accept statements, task and protected
// bodies themselves, and the incomplete "type T;" are not definitions.
func init() {
	registerDefinitions("ada", adaDefinitions, true)
}

const adaDefinitions = `
(package_declaration . (_) @name) @definition.module
(package_body . (_) @name) @definition.module
(package_renaming_declaration . (_) @name) @definition.module
((generic_instantiation . (identifier) @name) @definition.module
  (#match? @definition.module "^package\\b"))
((generic_instantiation . (identifier) @name) @definition.function
  (#match? @definition.function "^(overriding\\s+)?(procedure|function)\\b"))

(package_declaration (subprogram_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(package_declaration (subprogram_body [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(package_declaration (expression_function_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(package_declaration (generic_subprogram_declaration [(procedure_specification . (_) @name) @definition.function (function_specification . (_) @name) @definition.function]))
(package_declaration (subprogram_renaming_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(non_empty_declarative_part (subprogram_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(non_empty_declarative_part (subprogram_body [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(non_empty_declarative_part (expression_function_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(non_empty_declarative_part (generic_subprogram_declaration [(procedure_specification . (_) @name) @definition.function (function_specification . (_) @name) @definition.function]))
(non_empty_declarative_part (subprogram_renaming_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(compilation_unit (subprogram_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(compilation_unit (subprogram_body [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(compilation_unit (expression_function_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)
(compilation_unit (generic_subprogram_declaration [(procedure_specification . (_) @name) @definition.function (function_specification . (_) @name) @definition.function]))
(compilation_unit (subprogram_renaming_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.function)

(protected_definition (subprogram_declaration [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.method)
(protected_body (subprogram_body [(procedure_specification . (_) @name) (function_specification . (_) @name)]) @definition.method)
(entry_declaration . (identifier) @name) @definition.method

(subtype_declaration . (identifier) @name) @definition.type_alias
(full_type_declaration . (identifier) @name . [(record_type_definition) (enumeration_type_definition)]) @definition.class
(full_type_declaration . (identifier) @name . (derived_type_definition (record_extension_part))) @definition.class
(full_type_declaration . (identifier) @name . (interface_type_definition)) @definition.interface
(full_type_declaration . (task_type_declaration . (identifier) @name)) @definition.class
(full_type_declaration . (protected_type_declaration . (identifier) @name)) @definition.class
(single_task_declaration . (identifier) @name) @definition.class
(single_protected_declaration . (identifier) @name) @definition.class
(full_type_declaration . (identifier) @name
  . [(array_type_definition) (signed_integer_type_definition) (modular_type_definition)
     (floating_point_definition) (ordinary_fixed_point_definition) (decimal_fixed_point_definition)
     (access_to_object_definition) (access_to_subprogram_definition)]) @definition.type_alias
((full_type_declaration . (identifier) @name . (derived_type_definition) @_d) @definition.type_alias
  (#not-match? @_d "\\bwith\\b"))

(component_declaration (identifier) @name) @definition.field

(package_declaration (number_declaration . (identifier) @name) @definition.constant)
((package_declaration (object_declaration . (identifier) @name) @definition.constant)
  (#match? @definition.constant "^[^:]*:\\s*(aliased\\s+)?constant\\b"))
((package_declaration (object_declaration (identifier) @name . (identifier)) @definition.constant)
  (#match? @definition.constant "^[^:]*:\\s*(aliased\\s+)?constant\\b"))
((package_declaration (object_declaration . (identifier) @name) @definition.variable)
  (#not-match? @definition.variable "^[^:]*:\\s*(aliased\\s+)?constant\\b"))
((package_declaration (object_declaration (identifier) @name . (identifier)) @definition.variable)
  (#not-match? @definition.variable "^[^:]*:\\s*(aliased\\s+)?constant\\b"))
(package_body (non_empty_declarative_part (number_declaration . (identifier) @name) @definition.constant))
((package_body (non_empty_declarative_part (object_declaration . (identifier) @name) @definition.constant))
  (#match? @definition.constant "^[^:]*:\\s*(aliased\\s+)?constant\\b"))
((package_body (non_empty_declarative_part (object_declaration (identifier) @name . (identifier)) @definition.constant))
  (#match? @definition.constant "^[^:]*:\\s*(aliased\\s+)?constant\\b"))
((package_body (non_empty_declarative_part (object_declaration . (identifier) @name) @definition.variable))
  (#not-match? @definition.variable "^[^:]*:\\s*(aliased\\s+)?constant\\b"))
((package_body (non_empty_declarative_part (object_declaration (identifier) @name . (identifier)) @definition.variable))
  (#not-match? @definition.variable "^[^:]*:\\s*(aliased\\s+)?constant\\b"))`
