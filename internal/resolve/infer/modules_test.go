package infer

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A Clojure namespace is a path: dashes are underscores, and the namespace's
// segments are the path's last ones, in a file Clojure loads.
func TestClojureNamespaceFile(t *testing.T) {
	cases := []struct {
		path, ns string
		want     bool
	}{
		{"src/shop/money.clj", "shop.money", true},
		{"src/shop/line_items.clj", "shop.line-items", true},
		{"src/shop/store.cljc", "shop.store", true},
		{"money.clj", "shop.money", false},
		{"src/billing/money.clj", "shop.money", false},
		{"src/shop/money.clj", "money", true},
		{"src/shop/money.edn", "shop.money", false},
		{"src/shop/money.cljs.map", "shop.money", false},
	}
	for _, c := range cases {
		if got := clojureNamespaceFile(c.path, c.ns); got != c.want {
			t.Errorf("clojureNamespaceFile(%q, %q) = %v, want %v", c.path, c.ns, got, c.want)
		}
	}
}

// A .clj file loads .clj and .cljc, a .cljs file .cljs and .cljc.
func TestClojureLoads(t *testing.T) {
	cases := []struct {
		caller, file string
		want         bool
	}{
		{"a.clj", "b.clj", true}, {"a.clj", "b.cljc", true}, {"a.clj", "b.cljs", false},
		{"a.cljs", "b.cljs", true}, {"a.cljs", "b.cljc", true}, {"a.cljs", "b.clj", false},
		{"a.cljc", "b.clj", true}, {"a.cljc", "b.cljs", true},
	}
	for _, c := range cases {
		if got := clojureLoads(c.caller, c.file); got != c.want {
			t.Errorf("clojureLoads(%q, %q) = %v, want %v", c.caller, c.file, got, c.want)
		}
	}
}

// Erlang's `-import(m, [f/1])` brings f at arity 1 and no other; a list written
// without arities (Clojure's `:refer [f]`, Julia's `using M: f`) names the name.
func TestModuleImportBrings(t *testing.T) {
	dirs := moduleDirectivesOf([]registry.Import{
		{Path: "util", Alias: "import:slug/1,pad/2"},
		{Path: "shop.tax", Alias: "alias:tax;import:rate,apply-tax"},
		{Path: "shop.all", Alias: "import"},
	})
	if len(dirs.imports) != 3 || dirs.aliases["tax"] != "shop.tax" {
		t.Fatalf("directives = %+v", dirs)
	}
	ref := func(name string, args int) registry.Reference {
		r := registry.Reference{Name: name, ArgCount: model.ArityUnknown}
		if args >= 0 {
			r.ArgCount = model.KnownArity(args)
		}
		return r
	}
	cases := []struct {
		imp  moduleImported
		ref  registry.Reference
		want bool
	}{
		{dirs.imports[0], ref("slug", 1), true},
		{dirs.imports[0], ref("slug", 2), false},
		{dirs.imports[0], ref("slug", -1), true},
		{dirs.imports[0], ref("trim", 1), false},
		{dirs.imports[1], ref("rate", 3), true},
		{dirs.imports[1], ref("other", 1), false},
		{dirs.imports[2], ref("anything", 0), true},
	}
	for i, c := range cases {
		if got := c.imp.brings(c.ref); got != c.want {
			t.Errorf("case %d: %s/%v brought by %s = %v, want %v", i, c.ref.Name, c.ref.ArgCount, c.imp.module, got, c.want)
		}
	}
}

// Only Groovy reads an import as bringing a package's types in whole, or as
// renaming a class; Java, Kotlin and Scala have always done neither, and
// callNamed, typeNamed and AppendNames ask for these before changing anything.
func TestImportReadingsAreGroovyOnly(t *testing.T) {
	specs := NewSpecs()
	for _, lang := range []string{"java", "kotlin", "scala", "python", "typescript", "go", "ruby", "lua"} {
		traits := specs.For(lang).Calls
		if traits.ImportAliases || traits.WildcardImports {
			t.Errorf("%s reads wildcard or aliased class imports: %+v", lang, traits)
		}
	}
	if g := specs.For("groovy").Calls; !g.ImportAliases || !g.WildcardImports {
		t.Errorf("groovy does not read wildcard and aliased class imports: %+v", g)
	}
}

// Only Erlang, Clojure and Julia are resolved by module name.
func TestModularLanguages(t *testing.T) {
	for lang := range modularLanguages {
		if got := NewSpecs().For(lang).Name; got != lang {
			t.Errorf("%s resolves by module name and has no spec", lang)
		}
	}
	if modularLanguages["elixir"] {
		t.Error("elixir has a resolver of its own (elixir.go)")
	}
}
