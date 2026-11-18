package parse

import "strings"

// Erlang, Clojure and Julia reach a function through the MODULE it is in, named
// by what it is called and not by where it is kept: `money:zero()`,
// `(d/total ...)` after `[shop.data :as d]`, `Money.zero()` after `using
// .Money`. Their import rows therefore carry the module name in Path and what
// the directive does with it in Alias, the way Elixir's do (lang_elixir.go), and
// the resolver (internal/resolve/infer/modules.go) reads the two.
//
// A directive can say several things about one module at once (`[a.b :as b
// :refer [f]]` binds a qualifier AND brings a name in), and a file can name one
// module in several directives, so Alias is a list of directives joined with
// ModuleDirectiveSeparator, and these languages keep a row per distinct Alias
// (see keepsEveryDirective).
//
//	alias:b        the qualifier `b` names the module
//	import         every function of the module is in scope under its own name
//	import:f,g/2   only the listed names are (Erlang writes the arity: g/2)
//	parent         Path names a package this one inherits from (Perl's
//	               `use parent` and `use base`)
//	hiding:f,g     every function of the module EXCEPT the listed (Haskell's
//	               `import A hiding (f)`)
//	include        Path names a FILE whose definitions join this file's module
//	               (Julia's include, Dart's `part`)
//	export         Path names a library this file re-exports: whoever imports
//	               this file reaches its names too (Dart's `export`)
//
// The prefixes are read by the resolver; keep the two in step.
const (
	ModuleAliasPrefix        = "alias:"
	ModuleImportAll          = "import"
	ModuleImportPrefix       = "import:"
	ModuleParent             = "parent"
	ModuleHidingPrefix       = "hiding:"
	ModuleInclude            = "include"
	ModuleExport             = "export"
	ModuleDirectiveSeparator = ";"
)

// moduleDirectives joins directives into the Alias of one import row.
func moduleDirectives(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ModuleDirectiveSeparator)
}

// keepsEveryDirective is the set of languages whose import rows are directives
// about a module and not paths: two rows of one Path with different Alias are
// two statements, and collapsing them (dedupeImports keeps the first of a run
// of one Path, which is right for an import written twice and wrong here) would
// lose one. Groovy keeps both rows of `import a.b.C as D` (the class, and the
// alias) for the same reason. Every other language keeps the behaviour it has
// always had.
var keepsEveryDirective = map[string]bool{"erlang": true, "clojure": true, "julia": true, "groovy": true, "haskell": true, "ocaml": true, "perl": true, "zig": true, "fsharp": true, "dart": true}

// dedupeDirectives drops only exact repeats of a row, in the order given.
func dedupeDirectives(in []Import) []Import {
	type key struct{ path, alias string }
	seen := make(map[key]struct{}, len(in))
	out := in[:0]
	for _, imp := range in {
		k := key{imp.Path, imp.Alias}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, imp)
	}
	return out
}
