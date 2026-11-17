package infer

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// # Which languages can call which
//
// A call resolves through a module system, and two languages that share none
// cannot reach each other's definitions however well a name matches. Measured
// on eight public projects (docs/verification/2026-11-12): a C macro call bound
// to a JSON key, libc `write` to a Python function, `req.text()` to a Prisma
// column named `text`, `importlib.metadata.version()` to a `version` key in a
// YAML file, and every JavaScript `low` binding to a dependency line in
// package.json.
//
// languageFamilies is the whole table, and it is the only place the answer is
// written. A call in language X may bind to a definition in X, or in a
// language listed in the same family as X, and to nothing else. A language in
// no family calls only itself. There is no "unknown, so allow": a manifest, a
// schema or a configuration file has a language too, it is in no family, and
// that is exactly what keeps a call from binding to one.
var languageFamilies = [][]string{
	// One module system: ECMAScript modules, whatever the file's envelope.
	{"javascript", "typescript", "tsx", "jsx", "vue", "svelte", "astro"},
	// One linker. A header detected as C is included from C++ and the reverse.
	{"c", "cpp", "objc", "cuda", "arduino"},
	// One class path.
	{"java", "kotlin", "scala", "groovy"},
	// One runtime and one assembly model.
	{"c_sharp", "fsharp", "vb"},
	// Swift and Objective-C share a module map on Apple platforms.
	{"swift", "objc"},
	// Hack is PHP's descendant and still loads PHP.
	{"php", "hack"},
	// Elixir, Erlang and Gleam share the BEAM and call each other directly.
	{"elixir", "erlang", "gleam"},
}

// familyOf returns the languages that may call into each other with language,
// itself included. Never empty.
func familyOf(language string) []string {
	var out []string
	for _, fam := range languageFamilies {
		for _, l := range fam {
			if l != language {
				continue
			}
			for _, member := range fam {
				if !containsString(out, member) {
					out = append(out, member)
				}
			}
			break
		}
	}
	if !containsString(out, language) {
		out = append(out, language)
	}
	return out
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ImportStyle says how a language's import statements bind local names, which
// decides how a call qualified by a name is traced to a package.
type ImportStyle uint8

const (
	// ImportsNameNothing: the import table does not say what a qualifier
	// denotes. Rust's `use` and C#'s `using` bring names into scope without
	// the import row recording which; C and C++ include text. A qualifier in
	// these languages is identified by what it NAMES — a type, or a module by
	// its file — never by an import.
	ImportsNameNothing ImportStyle = iota

	// ImportsNameDirectories: an import path names a directory and binds its
	// last segment, or a written alias. Go.
	ImportsNameDirectories

	// ImportsNameClasses: an import path names a class by its file and binds
	// the class's own name. Java, Kotlin, Scala.
	ImportsNameClasses

	// ImportsNameModules: an import binds a module to an alias, or members of
	// a module to their names, and the rows say which. Python and ECMAScript.
	ImportsNameModules
)

// CallTraits is what resolution needs to know about how a language's calls
// reach definitions. One row per language, in callTraitsByLanguage.
type CallTraits struct {
	// Family lists the languages this one may bind a call into, itself
	// included. See languageFamilies.
	Family []string

	// ImplicitSelf reports that a bare name inside a type may denote a member
	// of that type: `save()` in a Java method is `this.save()`. Where false, a
	// bare call names a free function or a local definition and never a
	// method — Go, Rust, Python, JavaScript.
	ImplicitSelf bool

	// DynamicSelf reports that what a bare name is sent to is decided at run
	// time. In Ruby `get '/'` inside a block is a call on whatever `self` the
	// block is evaluated against, and a class gains methods from every module
	// it includes — none of which the index records. So a bare name may reach
	// a member of a type the call is not written in, and where the enclosing
	// type and its known ancestors do not declare it, the name's own evidence
	// (the same file, a required file) is weighed over every method, as a
	// weaker claim. In Java the same call can only be a member, an inherited
	// member or a static import, and anything else is not bound.
	DynamicSelf bool

	// CallConstructs reports that calling a type by its name constructs one:
	// Python's `Account(...)`, Kotlin's `Queue()`, a Go conversion, a Rust
	// tuple struct. Where it does not hold a bare call can never denote a
	// type — Java and C# need `new`, JavaScript throws, and C has no such
	// expression at all — and a type of the call's name is not a candidate.
	// Measured on redis, where 37 C calls were bound to a `struct` of the
	// function's name.
	CallConstructs bool

	// NewConstructs reports that the language constructs with a keyword and
	// the parser records each construction as a reference of kind
	// reference.call.new whose name is the TYPE: Java's and PHP's
	// `new Widget(3)`. Such a reference may bind to a class even though a bare
	// call in the language never can (CallConstructs is false), and binds to
	// nothing else. A language where a bare call constructs (Kotlin, Scala,
	// Python) needs no flag for it, and records its `new` the same way.
	NewConstructs bool

	// ClassBodyCode reports that code may stand directly in a type's body, with
	// no member around it: Kotlin's property initialisers and `init` blocks,
	// Scala's statements in an object or class (`test("x") { ... }`,
	// `val p = parser()`). The enclosing definition of such a call is the type
	// itself, and a bare name in it denotes a member of that type.
	ClassBodyCode bool

	// ExtensionCalls reports that a function declared outside any type can be
	// written as a call on a value: Kotlin's `fun String.shout()` called as
	// `"x".shout()`. The index records neither that a function is an extension
	// nor what it extends, so where no type declares a member of the called
	// name, the one free function of that name is what the call reaches.
	ExtensionCalls bool

	// NoFreeFunctions reports that every function is a member of a type: Java
	// and C#. A bare call there is a member of the enclosing type, an
	// inherited member, or a static import — and a candidate that seems to
	// belong to no type is an extraction artefact, not a function the call
	// could reach through an import or a neighbouring file. Measured on
	// Newtonsoft.Json, where the import and same-directory tiers bound bare
	// calls to such candidates and were right 10 times in 31.
	NoFreeFunctions bool

	// ValuesAreCallable reports that a constant or a variable may hold a
	// function, so that a call may name one: `export const parse =
	// _parse(Err)` is called as `parse(x)`. ECMAScript only. The cost is a
	// call that names a constant which holds something else, and the tier of
	// every rule that reaches one is the one measured for the language.
	ValuesAreCallable bool

	// ObjectsAreCallable reports that a singleton object can be called like a
	// function, as its `apply`: Scala's `os.exists(p)` for `object exists`. The
	// index holds an object as a variable, which a call cannot otherwise
	// denote; see object.go.
	ObjectsAreCallable bool

	// Overloads reports that one type may declare several different methods
	// under one name, told apart by their parameters: Java, C#, C++. Where it
	// holds, knowing the type and the name still leaves a choice, and unless
	// the call's argument count makes it the binding names the first
	// declaration and is reported one tier down. Where it does not hold —
	// TypeScript's overload signatures are several declarations of ONE
	// function — the first declaration is the definition.
	Overloads bool

	// OpenTypes reports that a type's members may be declared outside the
	// file that declares the type: Go methods anywhere in the package, Rust
	// impl blocks, C++ definitions in a .cc beside the header, C# partial
	// classes, Ruby reopened classes. Where false a member lives in its type's
	// file and nowhere else.
	OpenTypes bool

	// TypesAreCapitalised reports that a name beginning with a capital letter
	// denotes a type or a constant, by the language's rules (Ruby) or by a
	// convention its tooling enforces (Java, C#, Rust, TypeScript). It lets a
	// capitalised qualifier that the repository defines nowhere be recorded as
	// external. Go and C, where case means something else or nothing, are
	// false.
	TypesAreCapitalised bool

	// ScopeNamesModule reports that `m::f()` may be a function `f` in the
	// module the FILE `m` defines: Rust's `messages::errored()` and
	// messages.rs.
	ScopeNamesModule bool

	// Imports says how the import table binds names.
	Imports ImportStyle

	// ImportAliases reports that an import may rename the class it names, and the
	// parser records both rows: Groovy's `import a.b.Formatter as Fmt` is the
	// import of a.b.Formatter and an alias row, and `Fmt.format(x)` is a call on
	// Formatter. Where unset a row with an alias is not read as a class's rename,
	// which is what Java (which cannot write one), Kotlin and Scala have always had.
	ImportAliases bool

	// WildcardImports reports that `import a.b.*` brings every type of package
	// a.b into scope, and the import row is the package path. Where unset a
	// package import names no type, as it never has for Java, Kotlin and Scala.
	WildcardImports bool

	// ExternalRoots are the first segments of a qualified path that always
	// lie outside any repository: Rust's `std`, C++'s `std`, C#'s `System`.
	ExternalRoots []string

	// Measured holds the tiers this language's measurements earned for the
	// rules whose precision depends on the language. See ruleTiers.
	Measured ruleTiers

	// Builtins are type and namespace names the LANGUAGE defines, which need
	// no import and are in no repository: JavaScript's `Object` and `Math`,
	// Ruby's `String` and `Mutex`, Java's `String` and `System`. A call made
	// on one of them, where the repository defines no type of that name, is
	// external. Deliberately short: the names every program in the language
	// can use without declaring anything, not a model of a standard library.
	Builtins []string
}

// ruleTiers are the confidence tiers of the rules whose precision is a fact
// about the LANGUAGE rather than about the rule. Each was assigned from a
// hand-checked sample on a public project (docs/accuracy.md has the counts),
// against the floors that document states: `high` at least 86.8%, `medium` at
// least 60%. The zero value of each field is the tier a language with no
// measurement gets, which is the weakest the rule is ever given.
type ruleTiers struct {
	// UnknownReceiver is the tier of "exactly one type in the repository
	// declares a member of this name" for a call on a value whose type the
	// source does not state. Zero means low.
	//
	//	go          25/25 by hand, 458/458 against scip-go   high
	//	c_sharp     22/24                                     medium
	//	java        11/12                                     medium
	//	rust        9/12                                      medium
	//	typescript  7/12, then 24/30 on zod (docs/accuracy.md,
	//	            "JavaScript, TypeScript and TSX")         medium
	//	javascript  1/5 on express                            low
	//	ruby        4/8                                       low
	//	cpp         3/12                                      low
	//
	// Go is alone at the top for a reason the rule itself explains: a Go
	// value's methods are declared by a package and never by the language, so
	// a name the repository declares once is seldom anyone else's.
	UnknownReceiver model.Confidence

	// NamedAfterType is the tier of "the receiver is named after the type that
	// declares the member". Zero means medium. Measured 54 right of 55 across
	// Go, Java, C++, Rust and Ruby, and high in those five; 33 right of 33 on
	// express, where the owner of a prototype or object method is the name its
	// callers give the object (`res.send`, `app.use`), and high in JavaScript.
	//
	// Low in C#: 9 right of 21 on serilog (the whole tier, read edge by edge),
	// and every one of the 12 wrong is `log.Warning(...)` on an `ILogger log`
	// bound to the members of the static class `Log`. A C# type that cannot be
	// instantiated is named for what it does, and a variable of that name is
	// something else. The index does not say that a class is static.
	NamedAfterType model.Confidence

	// UniqueTypeName is the tier of a member binding whose type was identified
	// only as the one type of that name in the repository. Zero means medium.
	// Measured 31 of 33 on C# and 14 of 14 on Rust, where a type is routinely
	// used from another crate or namespace with no import the index can read.
	UniqueTypeName model.Confidence

	// Neighbour is the tier of "a free function of this name in the same
	// directory, though the language requires an import". Zero means medium.
	// Low for C++, where it measured 6 of 12.
	Neighbour model.Confidence

	// Extension is the tier of "a method-style call, no type declares a member
	// of that name, and exactly one free function has it". Zero means low.
	Extension model.Confidence
}

func (t ruleTiers) extension() model.Confidence {
	if t.Extension == "" {
		return model.ConfidenceLow
	}
	return t.Extension
}

func (t ruleTiers) unknownReceiver() model.Confidence {
	if t.UnknownReceiver == "" {
		return model.ConfidenceLow
	}
	return t.UnknownReceiver
}

func (t ruleTiers) namedAfterType() model.Confidence {
	if t.NamedAfterType == "" {
		return model.ConfidenceMedium
	}
	return t.NamedAfterType
}

func (t ruleTiers) uniqueTypeName() model.Confidence {
	if t.UniqueTypeName == "" {
		return model.ConfidenceMedium
	}
	return t.UniqueTypeName
}

func (t ruleTiers) neighbour() model.Confidence {
	if t.Neighbour == "" {
		return model.ConfidenceMedium
	}
	return t.Neighbour
}

// isObjectMember reports that a candidate is a method or accessor no type
// owns: in ECMAScript, one written in an object literal (`module.exports = {
// vary (field) {} }`, `get URL () {}`), which the index qualifies by the file
// alone. In a language with no implicit `this` such a member is a property of a
// value, and a call written with no receiver cannot reach it: the bare name is
// a function, a constant holding one, a class, or an import. Measured on koa,
// where 5 of 30 `high` edges bound the `vary`, `fresh`, `accepts` and `URL` of
// a package or of the platform to one of these.
//
// An import is different. `const { vary } = require('./response')` does bring a
// module's exported member into scope, and bindInModule still sees it.
func (s LanguageSpec) isObjectMember(c registry.SymbolIndexEntry) bool {
	return !s.Calls.ImplicitSelf && !s.Calls.DynamicSelf && c.Kind == string(model.KindMethod) && isModuleLevel(c)
}

// isBuiltinType reports whether the language itself defines a type or
// namespace of this name. Asked only after the repository was found not to.
func (s LanguageSpec) isBuiltinType(name string) bool {
	return containsString(s.Calls.Builtins, name)
}

var (
	scriptBuiltins = []string{
		"Object", "Array", "Math", "JSON", "Promise", "Number", "String", "Boolean", "Symbol",
		"Reflect", "Date", "Map", "Set", "WeakMap", "WeakSet", "RegExp", "Error", "TypeError",
		"RangeError", "BigInt", "Intl", "Proxy", "console", "process", "globalThis", "window",
		"document", "Buffer", "URL", "URLSearchParams", "Function", "Atomics", "ArrayBuffer",
		"Uint8Array", "Int32Array", "Float64Array", "DataView", "performance", "navigator",
		"localStorage", "sessionStorage", "crypto", "Response", "Request", "Headers",
	}
	javaBuiltins = []string{
		"String", "Math", "Object", "System", "Integer", "Long", "Double", "Float", "Boolean",
		"Character", "Byte", "Short", "Thread", "StringBuilder", "StringBuffer", "Class", "Enum",
		"Number", "Runtime", "Throwable", "Exception", "RuntimeException", "Error", "Iterable",
		"Comparable", "CharSequence", "Void", "StrictMath", "ThreadLocal", "ClassLoader",
	}
	rubyBuiltins = []string{
		"String", "Array", "Hash", "Mutex", "Time", "File", "Integer", "Float", "Struct", "Class",
		"Module", "Kernel", "Thread", "Set", "Proc", "Regexp", "IO", "Dir", "Symbol", "Range",
		"Comparable", "Enumerable", "Exception", "StandardError", "Object", "BasicObject",
		"Process", "Marshal", "Math", "Random", "Rational", "Complex", "Method", "Encoding",
		"Queue", "ConditionVariable", "Monitor", "Fiber", "ObjectSpace", "GC", "Signal", "ENV",
		"ARGV", "Numeric", "NilClass", "TrueClass", "FalseClass", "Data", "Enumerator",
	}
	rustBuiltins = []string{
		"String", "Vec", "Box", "Option", "Result", "Some", "Ok", "Err", "Rc", "Arc", "Cell",
		"RefCell", "HashMap", "HashSet", "BTreeMap", "BTreeSet", "VecDeque", "Path", "PathBuf",
		"OsString", "OsStr", "Cow", "Default", "Iterator", "Mutex", "RwLock", "Duration",
		"Instant", "u8", "u16", "u32", "u64", "u128", "usize", "i8", "i16", "i32", "i64", "i128",
		"isize", "f32", "f64", "char", "str", "bool",
	}
	csharpBuiltins = []string{
		"String", "Math", "Console", "Convert", "Object", "Enum", "Array", "Type", "Guid",
		"DateTime", "TimeSpan", "Int32", "Int64", "Double", "Decimal", "Boolean", "Char",
		"Activator", "Environment", "GC", "Nullable", "Tuple", "Task", "Interlocked",
		"string", "int", "long", "double", "decimal", "bool", "char", "object", "byte", "float",
	}
	swiftBuiltins = []string{
		"String", "Int", "Double", "Float", "Bool", "Array", "Dictionary", "Set", "Character",
		"Optional", "Result", "Date", "Data", "URL", "UUID", "Error", "Substring", "Range",
		"JSONEncoder", "JSONDecoder", "NotificationCenter", "UserDefaults", "FileManager",
		"DispatchQueue", "Bundle", "Task", "Thread", "Timer", "ProcessInfo",
	}
	dartBuiltins = []string{
		"String", "int", "double", "num", "bool", "List", "Map", "Set", "Object", "Duration",
		"DateTime", "Future", "Stream", "Iterable", "Exception", "Error", "StateError",
		"ArgumentError", "Uri", "RegExp", "Runes", "Symbol", "Type", "Function", "Null",
		"Completer", "Timer", "Random", "Stopwatch", "StringBuffer", "BigInt", "Directory", "File",
	}
	objcBuiltins = []string{
		"NSObject", "NSString", "NSMutableString", "NSArray", "NSMutableArray", "NSDictionary",
		"NSMutableDictionary", "NSSet", "NSMutableSet", "NSNumber", "NSDate", "NSData", "NSURL",
		"NSError", "NSException", "NSFileManager", "NSUserDefaults", "NSBundle", "NSThread",
		"NSNotificationCenter", "NSProcessInfo", "NSJSONSerialization", "NSTimer", "NSRunLoop",
	}
	luaBuiltins = []string{
		"string", "table", "math", "os", "io", "coroutine", "debug", "utf8", "package", "_G", "_ENV",
	}
	goBuiltins = []string{
		"string", "error", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16",
		"uint32", "uint64", "uintptr", "byte", "rune", "float32", "float64", "bool", "any",
		"complex64", "complex128",
	}
)

// callTraitsByLanguage is the table. A language absent from it gets
// defaultCallTraits, which claims the least: nothing implicit, nothing
// capitalised, no import knowledge.
var callTraitsByLanguage = map[string]CallTraits{
	"go":         {Measured: ruleTiers{UnknownReceiver: model.ConfidenceHigh, NamedAfterType: model.ConfidenceHigh}, CallConstructs: true, OpenTypes: true, Imports: ImportsNameDirectories, Builtins: goBuiltins},
	"java":       {NewConstructs: true, NoFreeFunctions: true, Measured: ruleTiers{UnknownReceiver: model.ConfidenceMedium, NamedAfterType: model.ConfidenceHigh}, ImplicitSelf: true, Overloads: true, TypesAreCapitalised: true, Imports: ImportsNameClasses, ExternalRoots: []string{"java", "javax"}, Builtins: javaBuiltins},
	"kotlin":     {Measured: ruleTiers{Extension: model.ConfidenceHigh}, ClassBodyCode: true, ExtensionCalls: true, CallConstructs: true, ImplicitSelf: true, Overloads: true, TypesAreCapitalised: true, Imports: ImportsNameClasses, ExternalRoots: []string{"java", "javax", "kotlin"}, Builtins: javaBuiltins},
	"scala":      {Measured: ruleTiers{UnknownReceiver: model.ConfidenceMedium, NamedAfterType: model.ConfidenceHigh}, ObjectsAreCallable: true, ClassBodyCode: true, CallConstructs: true, ImplicitSelf: true, Overloads: true, TypesAreCapitalised: true, Imports: ImportsNameClasses, ExternalRoots: []string{"java", "javax", "scala"}, Builtins: javaBuiltins},
	"python":     {CallConstructs: true, TypesAreCapitalised: true, Imports: ImportsNameModules},
	"javascript": {Measured: ruleTiers{NamedAfterType: model.ConfidenceHigh}, CallConstructs: true, ValuesAreCallable: true, TypesAreCapitalised: true, Imports: ImportsNameModules, Builtins: scriptBuiltins},
	"typescript": {Measured: ruleTiers{UnknownReceiver: model.ConfidenceMedium}, CallConstructs: true, ValuesAreCallable: true, TypesAreCapitalised: true, Imports: ImportsNameModules, Builtins: scriptBuiltins},
	"tsx":        {CallConstructs: true, ValuesAreCallable: true, TypesAreCapitalised: true, Imports: ImportsNameModules, Builtins: scriptBuiltins},
	"vue":        {TypesAreCapitalised: true, Imports: ImportsNameModules, Builtins: scriptBuiltins},
	"svelte":     {TypesAreCapitalised: true, Imports: ImportsNameModules, Builtins: scriptBuiltins},
	"astro":      {TypesAreCapitalised: true, Imports: ImportsNameModules, Builtins: scriptBuiltins},
	"rust":       {Measured: ruleTiers{UnknownReceiver: model.ConfidenceMedium, NamedAfterType: model.ConfidenceHigh, UniqueTypeName: model.ConfidenceHigh}, CallConstructs: true, OpenTypes: true, TypesAreCapitalised: true, ScopeNamesModule: true, ExternalRoots: []string{"std", "core", "alloc"}, Builtins: rustBuiltins},
	"ruby":       {Measured: ruleTiers{NamedAfterType: model.ConfidenceHigh}, ImplicitSelf: true, DynamicSelf: true, OpenTypes: true, TypesAreCapitalised: true, Builtins: rubyBuiltins},
	"c_sharp":    {NewConstructs: true, NoFreeFunctions: true, Measured: ruleTiers{UnknownReceiver: model.ConfidenceMedium, NamedAfterType: model.ConfidenceLow, UniqueTypeName: model.ConfidenceHigh}, ImplicitSelf: true, Overloads: true, OpenTypes: true, TypesAreCapitalised: true, ExternalRoots: []string{"System", "Microsoft"}, Builtins: csharpBuiltins},
	"cpp":        {Measured: ruleTiers{NamedAfterType: model.ConfidenceHigh, Neighbour: model.ConfidenceLow}, CallConstructs: true, ImplicitSelf: true, Overloads: true, OpenTypes: true, ScopeNamesModule: true, ExternalRoots: []string{"std"}},
	"c":          {OpenTypes: true},
	"php":        {Measured: ruleTiers{NamedAfterType: model.ConfidenceHigh, UniqueTypeName: model.ConfidenceHigh}, NewConstructs: true, TypesAreCapitalised: true, Imports: ImportsNameClasses},
	"swift":      {CallConstructs: true, ImplicitSelf: true, Overloads: true, OpenTypes: true, TypesAreCapitalised: true, Builtins: swiftBuiltins},
	"dart":       {CallConstructs: true, ImplicitSelf: true, TypesAreCapitalised: true, Imports: ImportsNameModules, Builtins: dartBuiltins},
	"objc":       {OpenTypes: true, TypesAreCapitalised: true, Builtins: objcBuiltins},
	"lua":        {Imports: ImportsNameModules, Builtins: luaBuiltins},
	"bash":       {},
	"powershell": {Measured: ruleTiers{UniqueTypeName: model.ConfidenceLow}},
	"elixir":     {ExternalRoots: []string{"Kernel", "Enum", "Map", "MapSet", "List", "String", "Keyword", "Tuple", "Integer", "Float", "Atom", "Regex", "Range", "Stream", "IO", "File", "Path", "System", "Process", "Task", "Agent", "GenServer", "Supervisor", "DynamicSupervisor", "Application", "Logger", "Code", "Module", "Macro", "Access", "Date", "DateTime", "Time", "NaiveDateTime", "Calendar", "URI", "Base", "Bitwise", "Inspect", "Node", "Port", "Registry", "Version", "Record", "Exception", "ExUnit", "Mix", "Ecto", "Phoenix", "Plug", "Jason"}},
	"r":          {ExternalRoots: []string{"base", "stats", "utils", "methods", "graphics", "grDevices", "datasets", "tools", "parallel", "compiler", "grid", "splines", "stats4"}},
	"erlang":     {ExternalRoots: []string{"erlang", "lists", "io", "io_lib", "maps", "string", "binary", "proplists", "gen_server", "gen_statem", "gen_event", "gen_fsm", "supervisor", "application", "ets", "dets", "mnesia", "file", "filename", "os", "timer", "calendar", "crypto", "rand", "random", "sets", "ordsets", "gb_trees", "gb_sets", "dict", "orddict", "queue", "array", "digraph", "logger", "code", "net_kernel", "rpc", "inet", "gen_tcp", "gen_udp", "ssl", "unicode", "re", "sys", "proc_lib", "init", "eunit", "ct", "math", "base64", "zlib", "erl_scan", "erl_parse", "epp", "persistent_term", "atomics", "counters", "cowboy", "ranch", "lager", "jsx", "jiffy", "meck"}},
	"clojure":    {ExternalRoots: []string{"clojure", "java", "javax", "cljs", "js", "goog", "Math", "System", "String", "Integer", "Long", "Double", "Thread", "Character", "Boolean", "Object", "Class", "Runtime", "StringBuilder", "Pattern", "UUID", "Instant", "LocalDate", "Duration", "File", "Files", "Paths"}},
	"julia":      {CallConstructs: true, ExternalRoots: []string{"Base", "Core", "Main", "LinearAlgebra", "Statistics", "Random", "Dates", "Test", "Printf", "Distributed", "SparseArrays", "Pkg", "Markdown", "Logging", "Libdl", "Sockets", "Serialization", "Unicode", "UUIDs", "REPL", "InteractiveUtils", "Mmap", "Downloads", "TOML", "DelimitedFiles", "Profile", "SHA", "Base64", "Threads", "JSON", "JSON3", "DataFrames", "CSV", "Plots", "HTTP"}},
	"groovy":     {ImportAliases: true, WildcardImports: true, NewConstructs: true, Measured: ruleTiers{NamedAfterType: model.ConfidenceLow, UniqueTypeName: model.ConfidenceLow}, ImplicitSelf: true, Overloads: true, TypesAreCapitalised: true, Imports: ImportsNameClasses, ExternalRoots: []string{"java", "javax", "groovy"}, Builtins: javaBuiltins},
}

// callTraitsFor returns a language's row with its family filled in.
func callTraitsFor(language string) CallTraits {
	t := callTraitsByLanguage[language]
	t.Family = familyOf(language)
	return t
}

// languageOfCandidate is the language a candidate's file was indexed as, or
// failing that what its extension says. "" is unknown.
func languageOfCandidate(c registry.SymbolIndexEntry) string {
	if c.Language != "" {
		return c.Language
	}
	return languageOfPath(c.FilePath)
}

// canBind reports whether a reference in this language may bind to a
// candidate at all.
//
// Two tests, and neither is a guess about likelihood. The candidate is
// written in a language this one can call (languageFamilies). And for a CALL
// it is something a call can denote: a function, a method, or a type — whose
// call is its construction. A field, a constant, a table column, a
// configuration key and a dependency line are not callable, and a call bound
// to one of them was never a statement about the program. A supertype
// reference likewise denotes a type and nothing else.
//
// This filters candidates, which the notes at the top of resolver.go warn
// about: removing candidates removes the ambiguity that was protecting a
// caller (A19). The difference is that A19 removed candidates that were
// UNLIKELY and these are candidates that are IMPOSSIBLE, and that the effect
// was measured by hand on eight projects rather than argued; see
// docs/accuracy.md.
func (s LanguageSpec) canBind(c registry.SymbolIndexEntry, kind model.EdgeKind) bool {
	lang := languageOfCandidate(c)
	if lang != s.Name && !containsString(s.Calls.Family, lang) {
		return false
	}
	switch kind {
	case model.EdgeCalls:
		return isCallableKind(c.Kind) || (isConstructibleKind(c.Kind) && (s.Calls.CallConstructs || s.Calls.NewConstructs)) ||
			(s.Calls.ValuesAreCallable && isValueKind(c.Kind)) || isMacroDefinition(c)
	case model.EdgeExtends, model.EdgeImplements:
		return isTypeKind(c.Kind)
	default:
		return true
	}
}

// isCallableKind reports whether a symbol kind is something a call invokes.
func isCallableKind(kind string) bool {
	return kind == string(model.KindFunction) || kind == string(model.KindMethod)
}

// isValueKind reports whether a symbol kind is a constant or a variable.
func isValueKind(kind string) bool {
	return kind == string(model.KindConstant) || kind == string(model.KindVariable)
}

// isConstructibleKind reports whether calling a symbol of this kind constructs
// one: `Account(...)`, `Point{}` written as a call, a Go conversion. An
// interface and a module are types and are not constructed by a call.
func isConstructibleKind(kind string) bool {
	return kind == string(model.KindClass)
}

// isTypeKind reports whether a symbol kind is a type: something a receiver may
// name and a supertype clause may list. A Ruby module is indexed as a package
// and is a type in this sense — it is included, extended and called on.
func isTypeKind(kind string) bool {
	switch kind {
	case string(model.KindClass), string(model.KindInterface), string(model.KindTypeAlias), string(model.KindPackage):
		return true
	}
	return false
}

// capitalised reports whether a name begins with an upper-case ASCII letter.
func capitalised(name string) bool {
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}

// receiverSegments splits a receiver written as a path — `a.b`, `a::b`,
// `Rack::Mutex` — into its identifiers.
func receiverSegments(receiver string) []string {
	return strings.FieldsFunc(receiver, func(r rune) bool { return r == '.' || r == ':' })
}

// lastSegment is the final identifier of a receiver path: the thing the call
// was actually made on.
func lastSegment(receiver string) string {
	if i := strings.LastIndexAny(receiver, ".:"); i >= 0 {
		return receiver[i+1:]
	}
	return receiver
}
