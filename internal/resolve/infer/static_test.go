package infer

import (
	"context"
	"hash/fnv"
	"reflect"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The fixture every test in this file resolves against is a handful of Python
// definitions spelled the way internal/parse/pyscope.go spells them: a
// qualified name that states the owning class, and `<locals>` between a
// function and what it defines.

const (
	baseFile    = "shop/models/base.py"
	accountFile = "shop/models/account.py"
	fmtFile     = "shop/utils/fmt.py"
	testFile    = "tests/test_payment.py"
)

// fileIDs gives each fixture file a stable id.
var fileIDs = map[string]int64{baseFile: 10, accountFile: 20, fmtFile: 30, testFile: 40}

// fileIDOf is the id of any file a test names: the fixture's, or one derived
// from the path so that two definitions in one file share it.
func fileIDOf(file string) int64 {
	if id, ok := fileIDs[file]; ok {
		return id
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(file))
	return 1000 + int64(h.Sum32()%1_000_000)
}

// pyDef builds a Python candidate from its file and qualified name.
func pyDef(id int64, file, qual, kind string) registry.SymbolIndexEntry {
	name := qual
	if i := strings.LastIndexByte(qual, '.'); i >= 0 {
		name = qual[i+1:]
	}
	return registry.SymbolIndexEntry{
		ID: id, Name: name, FQN: file + ":" + qual, Kind: kind,
		FileID: fileIDOf(file), FilePath: file, Exported: true,
	}
}

func pyFunc(id int64, file, qual string) registry.SymbolIndexEntry {
	return pyDef(id, file, qual, "function")
}

func pyClass(id int64, file, qual string) registry.SymbolIndexEntry {
	return pyDef(id, file, qual, "class")
}

// scopeAt gives a file enclosing symbols with the qualified names the registry
// would hand back, one per ten lines: the first covers lines 10-19, and so on.
func scopeAt(file string, quals ...string) []registry.EnclosingSymbol {
	out := make([]registry.EnclosingSymbol, 0, len(quals))
	for i, q := range quals {
		start := (i + 1) * 10
		out = append(out, registry.EnclosingSymbol{
			ID: int64(500 + i), Name: lastQualSegment(q), FQN: file + ":" + q,
			StartLine: start, EndLine: start + 9,
		})
	}
	return out
}

func pyCall(id int64, name, receiver string, line int) registry.Reference {
	return registry.Reference{ID: id, Name: name, Kind: "reference.call", Receiver: receiver, Line: line}
}

// resolvePython runs both passes over one Python file and returns its edges by
// the id of the reference that produced them (via the evidence line, which the
// tests keep unique per reference).
func resolvePython(
	t *testing.T,
	idx map[string][]registry.SymbolIndexEntry,
	hierarchy map[string][]string,
	fr registry.FileRefs,
) map[int]model.Edge {
	t.Helper()
	fr.Language = "python"
	opts := Options{}
	if hierarchy != nil {
		opts.Ancestry = &fakeAncestry{edges: hierarchy}
	}
	r := New(idx, opts)
	ctx := context.Background()
	var st Stats
	edges, _, err := r.ResolveFile(ctx, fr, &st)
	if err != nil {
		t.Fatal(err)
	}
	late, _, err := r.ResolveDeferred(ctx, &st)
	if err != nil {
		t.Fatal(err)
	}
	byLine := map[int]model.Edge{}
	for _, e := range append(edges, late...) {
		if _, dup := byLine[e.Evidence.Range.Start.Line]; dup {
			t.Fatalf("two edges from line %d", e.Evidence.Range.Start.Line)
		}
		byLine[e.Evidence.Range.Start.Line] = e
	}
	return byLine
}

func wantEdge(t *testing.T, edges map[int]model.Edge, line int, dst int64, conf model.Confidence) {
	t.Helper()
	e, ok := edges[line]
	if !ok {
		t.Errorf("line %d: no edge, want one to %d at %s", line, dst, conf)
		return
	}
	if e.DstID != dst || e.Confidence != conf {
		t.Errorf("line %d: bound to %d at %s (%s), want %d at %s",
			line, e.DstID, e.Confidence, e.Evidence.Detail, dst, conf)
	}
}

func wantNoEdge(t *testing.T, edges map[int]model.Edge, line int) {
	t.Helper()
	if e, ok := edges[line]; ok {
		t.Errorf("line %d: bound to %d at %s (%s), want no edge",
			line, e.DstID, e.Confidence, e.Evidence.Detail)
	}
}

func TestEnclosingClassIsReadFromTheScope(t *testing.T) {
	for scope, want := range map[string]string{
		"":                                    "",
		"top":                                 "",
		"Account":                             "",
		"Account.close":                       "Account",
		"Account.close.<locals>.retry":        "Account",
		"Account.close.<locals>.a.<locals>.b": "Account",
		"Tree.Node.walk":                      "Tree.Node",
		"build.<locals>.Item.render":          "build.<locals>.Item",
		// A function nested in a module-level function belongs to no class,
		// whatever its first parameter is called.
		"make.<locals>.method": "",
	} {
		if got := enclosingClass(scope); got != want {
			t.Errorf("enclosingClass(%q) = %q, want %q", scope, got, want)
		}
	}
}

func TestMemberOwnerIsReadFromTheQualifiedName(t *testing.T) {
	for qual, want := range map[string]string{
		"save":                       "",
		"Account":                    "",
		"Account.save":               "Account",
		"Tree.Node.walk":             "Node",
		"build.<locals>.Item":        "",
		"build.<locals>.Item.render": "Item",
		"build.<locals>.helper":      "",
	} {
		owner, ok := memberOwner(pyFunc(1, accountFile, qual))
		if owner != want || ok != (want != "") {
			t.Errorf("memberOwner(%q) = %q, %v; want %q", qual, owner, ok, want)
		}
	}
	// A qualified name this package did not write says nothing about ownership.
	scip := registry.SymbolIndexEntry{
		Name: "save", FilePath: accountFile,
		FQN: "scip-python python . . `shop.models.account`/Account#save().",
	}
	if owner, ok := memberOwner(scip); ok {
		t.Errorf("memberOwner of a compiler symbol = %q, want none", owner)
	}
}

// TestSelfBindsToItsOwnClass is the case the same-file tier could not decide:
// two classes in one file, both declaring `validate`, each calling its own.
func TestSelfBindsToItsOwnClass(t *testing.T) {
	idx := index(
		pyFunc(1, accountFile, "Account.validate"),
		pyFunc(2, accountFile, "Audit.validate"),
	)
	edges := resolvePython(t, idx, nil, registry.FileRefs{
		FileID: fileIDs[accountFile], Path: accountFile,
		Symbols: scopeAt(accountFile, "Account.close", "Audit.save"),
		Refs: []registry.Reference{
			pyCall(1, "validate", "self", 12),
			pyCall(2, "validate", "self", 22),
		},
	})
	wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
	wantEdge(t, edges, 22, 2, model.ConfidenceHigh)
}

// TestKnownClassIsNotBoundToAStubInTheSameFile is the wrong edge this file
// exists to stop: a test module declares a double with a `save`, and the only
// `save` in the file is therefore the double's. `account.save()` on an Account
// is not a call to it, and with the class known the hierarchy says where it
// goes instead.
func TestKnownClassIsNotBoundToAStubInTheSameFile(t *testing.T) {
	idx := index(
		pyClass(1, accountFile, "Account"),
		pyFunc(2, baseFile, "BaseModel.save"),
		pyFunc(3, testFile, "FakeLedger.save"),
	)
	hierarchy := map[string][]string{"account": {"BaseModel"}}
	fr := registry.FileRefs{
		FileID: fileIDs[testFile], Path: testFile,
		// Imported from the package that re-exports it, not the defining module.
		Imports: []registry.Import{{Path: "shop.models", Alias: ".Account"}},
		Symbols: scopeAt(testFile, "test_charge"),
		Refs:    []registry.Reference{pyCall(1, "save", "Account", 12)},
	}
	edges := resolvePython(t, idx, hierarchy, fr)
	wantEdge(t, edges, 12, 2, model.ConfidenceHigh)

	// With no hierarchy to consult the name is all there is, and it is
	// reported as that: one level down, never High.
	edges = resolvePython(t, idx, nil, fr)
	wantEdge(t, edges, 12, 3, model.ConfidenceMedium)
}

// TestUnknownReceiverIsNeverHigh pins the relabelling. `target.save()` where
// nothing states what `target` is used to bind to the only `save` in the file
// at High.
func TestUnknownReceiverIsNeverHigh(t *testing.T) {
	idx := index(
		pyFunc(1, accountFile, "Audit.save"),
		pyFunc(2, baseFile, "BaseModel.delete"),
	)
	for _, receiver := range []string{"target", opaqueReceiver} {
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDs[accountFile], Path: accountFile,
			Imports: []registry.Import{{Path: "shop.models.base"}},
			Symbols: scopeAt(accountFile, "run"),
			Refs: []registry.Reference{
				pyCall(1, "save", receiver, 12),   // the only save in the file
				pyCall(2, "delete", receiver, 13), // the only delete in an imported module
			},
		})
		// The only `save` in the FILE is the weakest of the name-only answers
		// and is reported two levels down: hand-checked on flask it was right
		// 2 times in 12. The only one in an imported module was right 9 times
		// in 12, and stays one level down.
		wantEdge(t, edges, 12, 1, model.ConfidenceLow)
		wantEdge(t, edges, 13, 2, model.ConfidenceMedium)
		if !strings.Contains(edges[12].Evidence.Detail, "rests on the name") {
			t.Errorf("reason %q does not say the binding is by name", edges[12].Evidence.Detail)
		}
	}
}

// TestBareCallCannotNameAMethod: Python has no implicit self, so `save()` is a
// module-level or local `save` and never a class's.
func TestBareCallCannotNameAMethod(t *testing.T) {
	idx := index(
		pyFunc(1, accountFile, "Account.save"),
		pyFunc(2, fmtFile, "save"),
		pyFunc(3, accountFile, "Account.close"),
	)
	edges := resolvePython(t, idx, nil, registry.FileRefs{
		FileID: fileIDs[accountFile], Path: accountFile,
		Imports: []registry.Import{{Path: "shop.utils.fmt"}, {Path: "shop.utils.fmt", Alias: ".save"}},
		Symbols: scopeAt(accountFile, "Account.close"),
		Refs: []registry.Reference{
			pyCall(1, "save", "", 12),
			pyCall(2, "close", "", 13), // only a method of that name exists
		},
	})
	wantEdge(t, edges, 12, 2, model.ConfidenceHigh)
	wantNoEdge(t, edges, 13)
}

// TestLocalDefinitionIsVisibleOnlyInItsFunction covers the nested-function
// case: one name defined in two functions, each calling its own.
func TestLocalDefinitionIsVisibleOnlyInItsFunction(t *testing.T) {
	const file = "shop/nested.py"
	idx := index(
		pyFunc(1, file, "outer_a.<locals>.helper"),
		pyFunc(2, file, "outer_b.<locals>.helper"),
		pyFunc(3, file, "helper"),
	)
	edges := resolvePython(t, idx, nil, registry.FileRefs{
		FileID: fileIDOf(file), Path: file,
		Symbols: scopeAt(file, "outer_a", "outer_b", "other"),
		Refs: []registry.Reference{
			pyCall(1, "helper", "", 12), // in outer_a
			pyCall(2, "helper", "", 22), // in outer_b
			pyCall(3, "helper", "", 32), // in a function that defines none
		},
	})
	wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
	wantEdge(t, edges, 22, 2, model.ConfidenceHigh)
	wantEdge(t, edges, 32, 3, model.ConfidenceHigh)

	// And from another file, a local definition is not a candidate at all.
	edges = resolvePython(t, index(pyFunc(1, file, "outer_a.<locals>.helper")), nil, registry.FileRefs{
		FileID: fileIDOf("shop/other.py"), Path: "shop/other.py",
		Symbols: scopeAt("shop/other.py", "run"),
		Refs:    []registry.Reference{pyCall(1, "helper", "", 12)},
	})
	wantNoEdge(t, edges, 12)
}

// TestImportedNameResolvesThroughTheImportThatNamedIt covers the two shapes of
// resolveMember and the two ways the generic import tier got them wrong.
func TestImportedNameResolvesThroughTheImportThatNamedIt(t *testing.T) {
	t.Run("the module defines it", func(t *testing.T) {
		// Two imported modules each define `render`; only one import names it.
		idx := index(pyFunc(1, fmtFile, "render"), pyFunc(2, "shop/utils/html.py", "render"))
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("shop/views.py"), Path: "shop/views.py",
			Imports: []registry.Import{
				{Path: "shop.utils.fmt"}, {Path: "shop.utils.fmt", Alias: ".render"},
				{Path: "shop.utils.html"}, {Path: "shop.utils.html", Alias: ".escape"},
			},
			Symbols: scopeAt("shop/views.py", "show"),
			Refs:    []registry.Reference{pyCall(1, "render", "", 12)},
		})
		wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
	})

	t.Run("the package re-exports it", func(t *testing.T) {
		idx := index(pyClass(1, accountFile, "Account"))
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("shop/views.py"), Path: "shop/views.py",
			Imports: []registry.Import{{Path: "shop"}, {Path: "shop", Alias: ".Account"}},
			Symbols: scopeAt("shop/views.py", "show"),
			Refs:    []registry.Reference{pyCall(1, "Account", "", 12)},
		})
		wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
	})

	t.Run("an absolute import is matched in full", func(t *testing.T) {
		// `from django.db.models import Count`. A test package's own models.py
		// also defines a Count, and shares only the last path segment.
		idx := index(
			pyClass(1, "django/db/models/aggregates.py", "Count"),
			pyClass(2, "tests/admin_utils/models.py", "Count"),
		)
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("tests/aggregation/tests.py"), Path: "tests/aggregation/tests.py",
			Imports: []registry.Import{{Path: "django.db.models"}, {Path: "django.db.models", Alias: ".Count"}},
			Symbols: scopeAt("tests/aggregation/tests.py", "test_count"),
			Refs:    []registry.Reference{pyCall(1, "Count", "", 12)},
		})
		wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
	})

	t.Run("a relative import is anchored to the importing file", func(t *testing.T) {
		// Every test package has a models.py with an Article in it.
		idx := index(
			pyClass(1, "tests/basic/models.py", "Article"),
			pyClass(2, "tests/many_to_one/models.py", "Article"),
		)
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("tests/many_to_one/tests.py"), Path: "tests/many_to_one/tests.py",
			Imports: []registry.Import{{Path: ".models"}, {Path: ".models", Alias: ".Article"}},
			Symbols: scopeAt("tests/many_to_one/tests.py", "test_create"),
			Refs:    []registry.Reference{pyCall(1, "Article", "", 12)},
		})
		wantEdge(t, edges, 12, 2, model.ConfidenceHigh)
	})

	t.Run("a method is never what an import names", func(t *testing.T) {
		idx := index(pyFunc(1, fmtFile, "Formatter.render"))
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("shop/views.py"), Path: "shop/views.py",
			Imports: []registry.Import{{Path: "shop.utils.fmt"}, {Path: "shop.utils.fmt", Alias: ".render"}},
			Symbols: scopeAt("shop/views.py", "show"),
			Refs:    []registry.Reference{pyCall(1, "render", "", 12)},
		})
		wantNoEdge(t, edges, 12)
	})
}

// TestModuleQualifiedCall covers `mod.name()` for the two ways a module gets a
// local name without an alias.
func TestModuleQualifiedCall(t *testing.T) {
	t.Run("a submodule imported by name", func(t *testing.T) {
		// `from django.db import models`; `models.CharField(...)`, defined two
		// levels beneath the package and re-exported. A models.py elsewhere
		// defines one too.
		idx := index(
			pyClass(1, "django/db/models/fields/__init__.py", "CharField"),
			pyClass(2, "tests/custom/models.py", "CharField"),
		)
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("tests/basic/models.py"), Path: "tests/basic/models.py",
			Imports: []registry.Import{{Path: "django.db"}, {Path: "django.db", Alias: ".models"}},
			Symbols: scopeAt("tests/basic/models.py", "Article"),
			Refs:    []registry.Reference{pyCall(1, "CharField", "models", 12)},
		})
		wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
	})

	t.Run("a package imported whole", func(t *testing.T) {
		idx := index(pyClass(1, "httpx/_client.py", "Client"), pyFunc(2, "tests/test_api.py", "Client"))
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("tests/test_api.py"), Path: "tests/test_api.py",
			Imports: []registry.Import{{Path: "httpx"}},
			Symbols: scopeAt("tests/test_api.py", "test_get"),
			// The file's own Client is not what `httpx.Client` names.
			Refs: []registry.Reference{pyCall(1, "Client", "httpx", 12)},
		})
		wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
	})

	t.Run("a relative import binds no name for its module", func(t *testing.T) {
		// `from .models import Other` does not make `models` a name, so a
		// variable called models is an unknown receiver like any other, and
		// two models.py files each defining an Article leave nothing to pick.
		idx := index(
			pyClass(1, "tests/basic/models.py", "Article"),
			pyClass(2, "tests/custom/models.py", "Article"),
		)
		edges := resolvePython(t, idx, nil, registry.FileRefs{
			FileID: fileIDOf("tests/basic/tests.py"), Path: "tests/basic/tests.py",
			Imports: []registry.Import{{Path: ".models"}, {Path: ".models", Alias: ".Other"}},
			Symbols: scopeAt("tests/basic/tests.py", "test_create"),
			Refs:    []registry.Reference{pyCall(1, "Article", "models", 12)},
		})
		if e, ok := edges[12]; ok && e.Confidence == model.ConfidenceHigh {
			t.Errorf("bound at high (%s); the receiver names no imported module", e.Evidence.Detail)
		}
	})
}

// TestSuperStartsFromTheBases: `super().save()` inside Ledger.save must never
// be bound to Ledger.save, which is the only `save` in the file.
func TestSuperStartsFromTheBases(t *testing.T) {
	const ledgerFile = "shop/models/ledger.py"
	idx := index(
		pyClass(1, ledgerFile, "Ledger"),
		pyFunc(2, ledgerFile, "Ledger.save"),
		pyFunc(3, baseFile, "BaseModel.save"),
	)
	fr := registry.FileRefs{
		FileID: fileIDOf(ledgerFile), Path: ledgerFile,
		Symbols: scopeAt(ledgerFile, "Ledger.save"),
		Refs:    []registry.Reference{pyCall(1, "save", "super", 12)},
	}
	edges := resolvePython(t, idx, map[string][]string{"ledger": {"BaseModel"}}, fr)
	wantEdge(t, edges, 12, 3, model.ConfidenceHigh)

	// No indexed base declares it: the call binds to nothing rather than to
	// the method it is written in.
	edges = resolvePython(t, idx, map[string][]string{"ledger": {"External"}}, fr)
	wantNoEdge(t, edges, 12)
	edges = resolvePython(t, idx, nil, fr)
	wantNoEdge(t, edges, 12)
}

// TestInheritedThroughAKnownClassIsHighAtAnyDepth pins the one place a
// hierarchy binding is reported High, and that the hierarchy outranks the name.
func TestInheritedThroughAKnownClassIsHighAtAnyDepth(t *testing.T) {
	idx := index(
		pyFunc(1, baseFile, "BaseModel.describe"),
		pyFunc(2, accountFile, "Audit.describe"), // the only one in the calling file
	)
	hierarchy := map[string][]string{"account": {"Mixin"}, "mixin": {"BaseModel"}}
	edges := resolvePython(t, idx, hierarchy, registry.FileRefs{
		FileID: fileIDs[accountFile], Path: accountFile,
		Symbols: scopeAt(accountFile, "Account.close"),
		Refs:    []registry.Reference{pyCall(1, "describe", "self", 12)},
	})
	wantEdge(t, edges, 12, 1, model.ConfidenceHigh)
}

// TestSeveralClassesOfOneNameAreNotChosenBetween: a receiver that names a class
// is static only when it names ONE.
func TestSeveralClassesOfOneNameAreNotChosenBetween(t *testing.T) {
	idx := index(
		pyClass(1, "tests/basic/models.py", "Article"),
		pyClass(2, "tests/many_to_one/models.py", "Article"),
		pyFunc(3, "tests/many_to_one/models.py", "Article.publish"),
	)
	edges := resolvePython(t, idx, nil, registry.FileRefs{
		FileID: fileIDOf("tests/other/tests.py"), Path: "tests/other/tests.py",
		Symbols: scopeAt("tests/other/tests.py", "test_publish"),
		Refs:    []registry.Reference{pyCall(1, "publish", "Article", 12)},
	})
	if e, ok := edges[12]; ok && e.Confidence == model.ConfidenceHigh {
		t.Errorf("bound at high (%s); nothing says which Article this is", e.Evidence.Detail)
	}
}

// TestCallThroughALocalValueBindsNothing: a parameter called `render` is not
// the repository's `render`.
func TestCallThroughALocalValueBindsNothing(t *testing.T) {
	idx := index(pyFunc(1, fmtFile, "render"))
	_, outs, st := resolveOne(t, New(idx, Options{}), registry.FileRefs{
		FileID: fileIDs[fmtFile], Path: fmtFile, Language: "python",
		Symbols: scopeAt(fmtFile, "show"),
		Refs: []registry.Reference{{
			ID: 1, Name: "render", Kind: "reference.call" + indirectCallSuffix, Line: 12,
		}},
	})
	if got := outs[1].Resolution; got != registry.ResolutionUnresolved {
		t.Errorf("resolution = %s, want unresolved", got)
	}
	if st.Unresolved != 1 || st.Resolved != 0 {
		t.Errorf("stats = %+v, want one unresolved", st)
	}
}

// TestBareNameTheFileDidNotImportIsNotHigh: the file imports a module, calls a
// bare name that module defines, and never imported that NAME. A wildcard
// import does that legitimately; so does a builtin of the same name. Measured
// at 28 right in 48, it is bound and reported one level down.
func TestBareNameTheFileDidNotImportIsNotHigh(t *testing.T) {
	idx := index(pyFunc(1, fmtFile, "render"))
	edges := resolvePython(t, idx, nil, registry.FileRefs{
		FileID: fileIDOf("shop/views.py"), Path: "shop/views.py",
		Imports: []registry.Import{{Path: "shop.utils.fmt"}, {Path: "shop.utils.fmt", Alias: ".money"}},
		Symbols: scopeAt("shop/views.py", "show"),
		Refs:    []registry.Reference{pyCall(1, "render", "", 12)},
	})
	wantEdge(t, edges, 12, 1, model.ConfidenceMedium)
	if !strings.Contains(edges[12].Evidence.Detail, "does not import this name") {
		t.Errorf("reason %q does not say the name was never imported", edges[12].Evidence.Detail)
	}
}

// TestProximityIsNotEvidenceInPython: measured at 1 right in 213, the
// same-directory tier is off for Python and the reference stays ambiguous.
func TestProximityIsNotEvidenceInPython(t *testing.T) {
	idx := index(
		pyFunc(1, "tests/test_a.py", "get_crawler"),
		pyFunc(2, "scrapy/utils/test.py", "get_crawler"),
	)
	edges := resolvePython(t, idx, nil, registry.FileRefs{
		FileID: fileIDOf("tests/test_b.py"), Path: "tests/test_b.py",
		Symbols: scopeAt("tests/test_b.py", "test_x"),
		Refs:    []registry.Reference{pyCall(1, "get_crawler", "", 12)},
	})
	wantNoEdge(t, edges, 12)
}

func TestPackageInitFileIsItsDirectorysModule(t *testing.T) {
	spec := NewSpecs().For("python")
	if got, want := fileSegments(spec, "shop/models/__init__.py"),
		[]string{"shop", "models"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fileSegments = %v, want %v", got, want)
	}
	if got := moduleStem(spec, "shop/models/__init__.py"); got != "models" {
		t.Errorf("moduleStem = %q, want models", got)
	}
	if got := moduleStem(spec, "shop/utils/fmt.py"); got != "fmt" {
		t.Errorf("moduleStem = %q, want fmt", got)
	}
	// Only where the language says so: a TypeScript file called __init__ is a
	// module of that name.
	if got, want := fileSegments(NewSpecs().For("typescript"), "src/__init__.ts"),
		[]string{"src", "__init__"}; !reflect.DeepEqual(got, want) {
		t.Errorf("typescript fileSegments = %v, want %v", got, want)
	}
}

// TestImportSetSeparatesMembersFromModules pins what a member row is and is
// not: it names what was imported, adds no entry of its own when its module is
// already there, and is never a module alias.
func TestImportSetSeparatesMembersFromModules(t *testing.T) {
	spec := NewSpecs().For("python")
	s := buildImportSet(spec, []registry.Import{
		{Path: "numpy", Alias: "np"},
		{Path: "shop.models"},
		{Path: "shop.models", Alias: ".Account"},
		{Path: "shop.models", Alias: ".Ledger"},
		{Path: "..pkg.mod"},
		{Path: "..pkg.mod", Alias: ".Thing"},
		{Path: ".", Alias: ".sibling"},
	})
	if len(s.imports) != 4 {
		t.Fatalf("got %d entries, want 4 (numpy, shop.models, ..pkg.mod, .): %+v", len(s.imports), s.imports)
	}
	if _, ok := s.byLocalName["account"]; ok {
		t.Error("an imported member was registered as a module alias")
	}
	if _, ok := s.byLocalName["np"]; !ok {
		t.Error("the module alias np was lost")
	}
	for _, name := range []string{"Account", "Ledger", "Thing", "sibling"} {
		if len(s.members[name]) != 1 {
			t.Errorf("member %q: %v, want one declaring import", name, s.members[name])
		}
	}
	if e := s.imports[s.members["Thing"][0]]; e.relative != 2 || !reflect.DeepEqual(e.segments, []string{"pkg", "mod"}) {
		t.Errorf("..pkg.mod = %+v, want two dots and [pkg mod]", e)
	}
	if e := s.imports[s.members["sibling"][0]]; e.relative != 1 || len(e.segments) != 0 {
		t.Errorf(". = %+v, want one dot and no segments", e)
	}
	// A JavaScript relative path is not a dotted relative import.
	if n := leadingDots("./sibling"); n != 0 {
		t.Errorf("leadingDots(./sibling) = %d, want 0", n)
	}
}

func TestSubprojectOf(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"backend/python/auth/main.py", "backend/python/auth"},
		{"backend/go/gateway/main.go", "backend/go/gateway"},
		{"backend/api.py", "backend/api.py"}, // less than 3 parts: backend/api.py
		{"services/payment/service.py", "services/payment"},
		{"packages/ui/button.tsx", "packages/ui"},
		{"apps/web/page.tsx", "apps/web"},
		{"modules/shared/util.py", "modules/shared"},
		{"mobile/android/MainActivity.kt", "mobile/android"},
		{"mobile/ios/AppDelegate.swift", "mobile/ios"},
		{"components/table/data.tsx", "components/table"},
		{"plugins/auth/index.js", "plugins/auth"},
		{"clients/python/client.py", "clients/python"},
		{"tools/migrator/run.go", "tools/migrator"},
		{"single_service/main.py", ""},
		{"main.py", ""},
	}
	for _, tc := range cases {
		got := subprojectOf(tc.path)
		if got != tc.want {
			t.Errorf("subprojectOf(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestSubprojectCandidateIsolation(t *testing.T) {
	authCaller := "backend/python/auth/handler.py"
	idx := index(
		pyFunc(1, "backend/python/auth/token.py", "process_token"),
		pyFunc(2, "backend/python/billing/token.py", "process_token"),
	)
	fr := registry.FileRefs{
		FileID:  fileIDOf(authCaller),
		Path:    authCaller,
		Symbols: scopeAt(authCaller, "handle_request"),
		Refs: []registry.Reference{
			pyCall(10, "process_token", "", 15),
		},
	}
	edges := resolvePython(t, idx, nil, fr)
	edge, ok := edges[15]
	if !ok {
		t.Fatalf("expected edge for process_token call, got none: %+v", edges)
	}
	if edge.DstID != 1 {
		t.Fatalf("expected target to be auth token (ID 1), got %d", edge.DstID)
	}
	if edge.Confidence != model.ConfidenceLow {
		t.Fatalf("expected confidence low, got %s", edge.Confidence)
	}
}

// A service calling a name it and a shared library both define most likely
// means the library's and lost the import that would say so: the folder
// alone does not choose the local one.
func TestSubprojectPickDefersToASharedLibrary(t *testing.T) {
	caller := "services/orders/handler.py"
	idx := index(
		pyFunc(1, "services/orders/money.py", "format_amount"),
		pyFunc(2, "libs/common/money.py", "format_amount"),
	)
	fr := registry.FileRefs{
		FileID:  fileIDOf(caller),
		Path:    caller,
		Symbols: scopeAt(caller, "handle"),
		Refs:    []registry.Reference{pyCall(10, "format_amount", "", 15)},
	}
	if edge, ok := resolvePython(t, idx, nil, fr)[15]; ok && edge.DstID == 1 {
		t.Fatalf("bound to the service's own format_amount on folder alone: %+v", edge)
	}
}

func TestConftestFixtureResolution(t *testing.T) {
	testCaller := "backend/services/auth/tests/integration/test_login.py"
	idx := index(
		pyFunc(101, "backend/services/auth/tests/conftest.py", "auth_headers"),
		pyFunc(102, "backend/services/billing/tests/conftest.py", "auth_headers"),
	)
	fr := registry.FileRefs{
		FileID:  fileIDOf(testCaller),
		Path:    testCaller,
		Symbols: scopeAt(testCaller, "test_admin_login"),
		Refs: []registry.Reference{
			pyCall(20, "auth_headers", "", 12),
		},
	}
	edges := resolvePython(t, idx, nil, fr)
	edge, ok := edges[12]
	if !ok {
		t.Fatalf("expected edge for auth_headers fixture call, got none: %+v", edges)
	}
	if edge.DstID != 101 {
		t.Fatalf("expected target to be auth conftest (ID 101), got %d", edge.DstID)
	}
	if edge.Confidence != model.ConfidenceHigh {
		t.Fatalf("expected confidence high for conftest fixture, got %s", edge.Confidence)
	}
}

func TestPolyglotParentFolderLayoutCandidateIsolation(t *testing.T) {
	caller := "custom_service_auth/handler.py"
	files := []string{
		"custom_service_auth/pyproject.toml",
		"custom_service_auth/handler.py",
		"custom_service_auth/token.py",
		"custom_service_billing/pyproject.toml",
		"custom_service_billing/token.py",
	}
	layout := NewLayout(files, nil)
	idx := index(
		pyFunc(1, "custom_service_auth/token.py", "process_token"),
		pyFunc(2, "custom_service_billing/token.py", "process_token"),
	)
	fr := registry.FileRefs{
		FileID:  fileIDOf(caller),
		Path:    caller,
		Symbols: scopeAt(caller, "handle_request"),
		Refs: []registry.Reference{
			pyCall(10, "process_token", "", 15),
		},
	}
	edges := resolvePythonWithLayout(t, idx, layout, fr)
	edge, ok := edges[15]
	if !ok {
		t.Fatalf("expected edge for process_token call, got none: %+v", edges)
	}
	if edge.DstID != 1 {
		t.Fatalf("expected target to be auth token (ID 1), got %d", edge.DstID)
	}
	if edge.Confidence != model.ConfidenceLow {
		t.Fatalf("expected confidence low, got %s", edge.Confidence)
	}
}

func resolvePythonWithLayout(
	t *testing.T,
	idx map[string][]registry.SymbolIndexEntry,
	layout *Layout,
	fr registry.FileRefs,
) map[int]model.Edge {
	t.Helper()
	fr.Language = "python"
	opts := Options{Layout: layout}
	r := New(idx, opts)
	ctx := context.Background()
	var st Stats
	edges, _, err := r.ResolveFile(ctx, fr, &st)
	if err != nil {
		t.Fatal(err)
	}
	late, _, err := r.ResolveDeferred(ctx, &st)
	if err != nil {
		t.Fatal(err)
	}
	byLine := map[int]model.Edge{}
	for _, e := range append(edges, late...) {
		if _, dup := byLine[e.Evidence.Range.Start.Line]; dup {
			t.Fatalf("two edges from line %d", e.Evidence.Range.Start.Line)
		}
		byLine[e.Evidence.Range.Start.Line] = e
	}
	return byLine
}
