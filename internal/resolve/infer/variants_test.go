package infer

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The five measured defects of 2026-11-18, against scip-ruby and scip-clang.
// Each test builds the smallest index in which the old rule bound the call at
// a tier the compiler index disagreed with, and states the tier it earned.

func rubyRef(id int64, name string, line int, block bool) registry.Reference {
	r := callRef(id, name, model.ReceiverBare, "", line)
	r.Block = block
	return r
}

func rubyFile(path string, syms []registry.EnclosingSymbol, imports []registry.Import, refs ...registry.Reference) registry.FileRefs {
	return registry.FileRefs{FileID: 20, Path: path, Language: "ruby", Imports: imports, Symbols: syms, Refs: refs}
}

func rubyMethod(id int64, file, qual string, fileID int64, singleton bool) registry.SymbolIndexEntry {
	e := def(id, "method", file, qual, fileID)
	e.Singleton = singleton
	return e
}

// A bare call inside a method that no ancestor declares is a DSL call
// (`depends_on`, `conflicts`, `resource`), and the required file that happens
// to define the name is not evidence about the object it runs against. On
// Homebrew the rule was 71.8% right at high, and at medium 2 of 378.
func TestRubyNameInARequiredFileIsLow(t *testing.T) {
	idx := index(def(1, "method", "dsl.rb", "Formula.depends_on", 10))
	r := New(idx, Options{})
	edges, _, _ := resolveOne(t, r, rubyFile("recipe.rb",
		[]registry.EnclosingSymbol{{ID: 50, Name: "build", StartLine: 1, EndLine: 9, FQN: "recipe.rb:Recipe.build", Kind: "method"}},
		[]registry.Import{{Path: "dsl"}},
		rubyRef(1, "depends_on", 5, false)))
	got, ok := edgesByLine(edges)[5]
	if !ok {
		t.Fatalf("no edge for the DSL call; edges %+v", edges)
	}
	if got.dst != 1 || got.confidence != model.ConfidenceLow {
		t.Errorf("bound to %d at %s, want the required file's method at low", got.dst, got.confidence)
	}
}

// A spec that defines `def patch` and calls `patch do ... end` is calling the
// DSL method of the class the block runs against, and the helper takes no
// block. The call that passes one is low where a class in a file that is not a
// test also defines the name (0 right of 23 on Homebrew even at medium); the
// call without one stays high.
func TestRubyHelperShadowingADslMethodWithABlockIsLow(t *testing.T) {
	idx := index(
		def(1, "function", "spec/patch_spec.rb", "patch", 10),
		rubyMethod(2, "formula.rb", "Formula.patch", 11, false),
	)
	r := New(idx, Options{})
	fr := rubyFile("spec/patch_spec.rb", enclose(99, "example"), nil,
		rubyRef(1, "patch", 5, true),
		rubyRef(2, "patch", 6, false))
	fr.FileID = 10
	edges, _, _ := resolveOne(t, r, fr)
	by := edgesByLine(edges)
	if by[5].dst != 1 || by[5].confidence != model.ConfidenceLow {
		t.Errorf("patch do ... end: %+v, want the helper at low", by[5])
	}
	if by[6].dst != 1 || by[6].confidence != model.ConfidenceHigh {
		t.Errorf("patch with no block: %+v, want the helper at high", by[6])
	}
}

// The same name defined twice at one level of a file is a choice, not a
// definition: rspec examples define helpers of one name in separate blocks.
func TestRubySameFileRedefinitionIsLow(t *testing.T) {
	a := def(1, "function", "spec/inv_spec.rb", "write_inventory", 10)
	b := def(2, "function", "spec/inv_spec.rb", "write_inventory", 10)
	b.ID = 2
	r := New(index(a, b), Options{})
	fr := rubyFile("spec/inv_spec.rb", enclose(99, "example"), nil, rubyRef(1, "write_inventory", 140, false))
	fr.FileID = 10
	edges, _, _ := resolveOne(t, r, fr)
	if got := edgesByLine(edges)[140]; got.confidence != model.ConfidenceLow {
		t.Errorf("one of two same-named helpers bound at %s, want low", got.confidence)
	}
}

// A class method and an instance method of one name are two methods under one
// qualified name. A bare call reaches the one of its own kind.
func TestRubyBareCallReachesTheMethodOfItsOwnKind(t *testing.T) {
	idx := index(
		rubyMethod(1, "net.rb", "Net.start", 10, true),  // def self.start
		rubyMethod(2, "net.rb", "Net.start", 10, false), // def start
	)
	r := New(idx, Options{})
	syms := []registry.EnclosingSymbol{
		{ID: 50, Name: "Net", StartLine: 1, EndLine: 100, FQN: "net.rb:Net", Kind: "class"},
		{ID: 51, Name: "run", StartLine: 10, EndLine: 19, FQN: "net.rb:Net.run", Kind: "method"},
		{ID: 52, Name: "build", StartLine: 20, EndLine: 29, FQN: "net.rb:Net.build", Kind: "method", Singleton: true},
	}
	fr := rubyFile("net.rb", syms, nil,
		rubyRef(1, "start", 12, false), // in the instance method
		rubyRef(2, "start", 22, false), // in `def self.build`
	)
	fr.FileID = 10
	edges, _, _ := resolveOne(t, r, fr)
	by := edgesByLine(edges)
	if by[12].dst != 2 || by[12].confidence != model.ConfidenceHigh {
		t.Errorf("start in an instance method: %+v, want the instance method (2) at high", by[12])
	}
	if by[22].dst != 1 || by[22].confidence != model.ConfidenceHigh {
		t.Errorf("start in a class method: %+v, want the class method (1) at high", by[22])
	}
}

// Where only the other kind is declared, the name is probably supplied from
// elsewhere, and the binding says so.
func TestRubyBareCallToTheOtherKindOnlyIsLow(t *testing.T) {
	idx := index(rubyMethod(1, "net.rb", "Net.start", 10, true))
	r := New(idx, Options{})
	syms := []registry.EnclosingSymbol{
		{ID: 50, Name: "Net", StartLine: 1, EndLine: 100, FQN: "net.rb:Net", Kind: "class"},
		{ID: 51, Name: "run", StartLine: 10, EndLine: 19, FQN: "net.rb:Net.run", Kind: "method"},
	}
	fr := rubyFile("net.rb", syms, nil, rubyRef(1, "start", 12, false))
	fr.FileID = 10
	edges, _, _ := resolveOne(t, r, fr)
	if got, ok := edgesByLine(edges)[12]; ok && got.confidence != model.ConfidenceLow {
		t.Errorf("an instance method calling only a class method bound at %s, want low", got.confidence)
	}
}

// A type that defines a name twice, in one kind, has no first definition the
// call can be said to reach. One of each kind is not a redefinition.
func TestRubyRedefinedMemberIsLow(t *testing.T) {
	once := index(rubyMethod(1, "m.rb", "Money.fmt", 10, false), rubyMethod(2, "m.rb", "Money.fmt", 10, true))
	twice := index(rubyMethod(1, "m.rb", "Money.fmt", 10, false), rubyMethod(2, "m.rb", "Money.fmt", 10, false))
	syms := []registry.EnclosingSymbol{
		{ID: 50, Name: "Money", StartLine: 1, EndLine: 100, FQN: "m.rb:Money", Kind: "class"},
		{ID: 51, Name: "show", StartLine: 10, EndLine: 19, FQN: "m.rb:Money.show", Kind: "method"},
	}
	for _, tc := range []struct {
		name string
		idx  map[string][]registry.SymbolIndexEntry
		want model.Confidence
	}{
		{"an instance and a class method", once, model.ConfidenceHigh},
		{"two instance methods", twice, model.ConfidenceLow},
	} {
		fr := rubyFile("m.rb", syms, nil, rubyRef(1, "fmt", 12, false))
		fr.FileID = 10
		edges, _, _ := resolveOne(t, New(tc.idx, Options{}), fr)
		if got := edgesByLine(edges)[12]; got.confidence != tc.want {
			t.Errorf("%s: bound at %s, want %s", tc.name, got.confidence, tc.want)
		}
	}
}

func cFunc(id int64, file, name string, fileID int64, conditional bool) registry.SymbolIndexEntry {
	e := def(id, "function", file, name, fileID)
	e.Conditional = conditional
	return e
}

func cFile(path string, refs ...registry.Reference) registry.FileRefs {
	return registry.FileRefs{FileID: 10, Path: path, Language: "c", Symbols: enclose(99, "use"), Refs: refs}
}

// `#ifdef _WIN32` and `#else` define one function twice in one file. Which a
// build sees is not in the source, so a call is low whichever definition the
// resolver picks; a function defined once and unconditionally stays high.
func TestCPreprocessorAlternativesAreLow(t *testing.T) {
	idx := index(
		cFunc(1, "compat.c", "plat", 10, true),
		cFunc(2, "compat.c", "plat", 10, true),
		cFunc(3, "compat.c", "solo", 10, false),
		cFunc(4, "compat.c", "guarded", 10, true), // one definition, under a conditional
	)
	edges, _, _ := resolveOne(t, New(idx, Options{}), cFile("compat.c",
		callRef(1, "plat", model.ReceiverBare, "", 5),
		callRef(2, "solo", model.ReceiverBare, "", 6),
		callRef(3, "guarded", model.ReceiverBare, "", 7)))
	by := edgesByLine(edges)
	if by[5].confidence != model.ConfidenceLow {
		t.Errorf("two definitions of plat: bound at %s, want low", by[5].confidence)
	}
	if by[6].confidence != model.ConfidenceHigh {
		t.Errorf("one unconditional definition: bound at %s, want high", by[6].confidence)
	}
	if by[7].confidence != model.ConfidenceLow {
		t.Errorf("a definition under #if: bound at %s, want low", by[7].confidence)
	}
}

// Two definitions of a name in one file that carry no conditional flag are
// still two (the file may select between them with a macro the parser does not
// read), for C. C++ overloads are two definitions of one name and are not.
func TestCppOverloadsAreNotAlternatives(t *testing.T) {
	sp := New(nil, Options{}).specs.For("cpp")
	c := call{spec: sp, cands: []registry.SymbolIndexEntry{
		def(1, "function", "a.cpp", "f", 10), def(2, "function", "a.cpp", "f", 10),
	}}
	d := bound(candidate{c.cands[0], model.ConfidenceHigh, "defined in the same file"})
	if got := c.capVariants(d); got.best.confidence != model.ConfidenceHigh {
		t.Errorf("overloads were capped to %s", got.best.confidence)
	}
	c.cands[1].Conditional = true
	if got := c.capVariants(d); got.best.confidence != model.ConfidenceLow {
		t.Errorf("an overload under #if was %s, want low", got.best.confidence)
	}
}

// A construction binds to the type, and a constructor under an `#if` is a
// variant of the constructor: the edge to the type is not lowered for it.
func TestConstructionOfATypeWithConditionalConstructorsStaysHigh(t *testing.T) {
	sp := New(nil, Options{}).specs.For("cpp")
	ctor := def(2, "function", "a.hpp", "Foo.Foo", 10)
	ctor.Conditional = true
	c := call{spec: sp, cands: []registry.SymbolIndexEntry{def(1, "class", "a.hpp", "Foo", 10), ctor}}
	d := bound(candidate{c.cands[0], model.ConfidenceHigh, "constructs the type the receiver names (Foo)"})
	if got := c.capVariants(d); got.best.confidence != model.ConfidenceHigh {
		t.Errorf("an instantiation of Foo was %s, want high", got.best.confidence)
	}
}

// "Multiple matches among imports" is pickImported's arbitrary choice of the
// first by path. For C and C++ it measured 6.2% right at medium, and is low.
func TestMultipleMatchesAmongImportsIsLowInTheCFamily(t *testing.T) {
	for _, lang := range []string{"c", "cpp"} {
		c := call{spec: New(nil, Options{}).specs.For(lang), cands: []registry.SymbolIndexEntry{
			def(1, "function", "a.c", "f", 10), def(2, "function", "b.c", "f", 11),
		}}
		d := bound(candidate{c.cands[0], model.ConfidenceMedium, "multiple matches among imports"})
		if got := c.capVariants(d); got.best.confidence != model.ConfidenceLow {
			t.Errorf("%s: %s, want low", lang, got.best.confidence)
		}
	}
	rb := call{spec: New(nil, Options{}).specs.For("ruby")}
	d := bound(candidate{def(1, "function", "a.rb", "f", 10), model.ConfidenceMedium, "multiple matches among imports"})
	if got := rb.capVariants(d); got.best.confidence != model.ConfidenceMedium {
		t.Errorf("ruby: %s, want it left alone", got.best.confidence)
	}
}
