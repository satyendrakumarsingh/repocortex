package parse

// Verilog and SystemVerilog. What the index holds, see
// fixtures/langs/verilog/expected.tsv.
//
//   - module is a class: it is the unit that is instantiated, with ports
//     (parameters) and members; interface is an interface; package is a
//     package; class is a class;
//   - function and task are functions, and methods when declared inside a
//     class. `function new()` has no name of its own and is not held;
//   - parameter and localparam are constants, wherever they are declared:
//     in the module's parameter port list, in its body, in a package or an
//     interface. Type parameters are not held;
//   - typedef is a type alias (the enumeration's members are not held);
//   - the properties of a class are fields.
//
// Ports, nets, registers and variables of a module, instances (`alu a0 (...)`)
// and calls are not definitions.
func init() {
	registerDefinitions("verilog", verilogDefinitions, true)
}

const verilogDefinitions = `
(module_declaration (module_header (module_keyword) . (simple_identifier) @name)) @definition.class
(interface_declaration (_ (interface_identifier (simple_identifier) @name))) @definition.interface
(package_declaration (package_identifier (simple_identifier) @name)) @definition.module
(class_declaration (class_identifier (simple_identifier) @name)) @definition.class

(function_declaration (function_body_declaration (function_identifier (function_identifier (simple_identifier) @name)))) @definition.function
(task_declaration (task_body_declaration (task_identifier (task_identifier (simple_identifier) @name)))) @definition.function
(class_method (function_declaration (function_body_declaration (function_identifier (function_identifier (simple_identifier) @name)))) @definition.method)
(class_method (task_declaration (task_body_declaration (task_identifier (task_identifier (simple_identifier) @name)))) @definition.method)

(param_assignment (parameter_identifier (simple_identifier) @name)) @definition.constant

(type_declaration (data_type) . (simple_identifier) @name .) @definition.type_alias

(class_property (data_declaration (list_of_variable_decl_assignments (variable_decl_assignment (simple_identifier) @name) @definition.field)))
`
