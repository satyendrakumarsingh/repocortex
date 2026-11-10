package parse

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Coverage tests for the tags-query supplements.
//
// Every entry here exists because a construct was ONCE MISSING from the index —
// most of them found by the differential test against the C tree-sitter runtime
// (`go test -tags cgo_parser -run TestDifferential ./internal/parse/`), which
// compares the two backends and reports what each fails to see.
//
// This is the convention documented at the top of queries.go: a supplement
// without a coverage case is a supplement nobody will notice breaking. The
// failure mode of a tags query is silence — a wrong pattern matches nothing and
// looks exactly like a file that genuinely declares nothing.

type coverageCase struct {
	name string
	file string
	src  string
	// want maps a symbol name to the kind it must have. An empty kind means
	// "present, kind not asserted".
	want map[string]model.EntityKind
}

func TestSupplementCoverage(t *testing.T) {
	cases := []coverageCase{
		{
			name: "go_type_const_var",
			file: "svc.go",
			src: `package pay

type TransactionService struct{}

type Repository interface { Save(id string) error }

const MaxRetries = 3

var DefaultTimeout = 30

func NewService() *TransactionService { return nil }

func (s *TransactionService) UpdateStatus(id string) error { return nil }`,
			want: map[string]model.EntityKind{
				// These two asserted KindTypeAlias until the assertion was
				// found to be wrong rather than the code. A struct is not an
				// alias and an interface is not an alias; the test had locked
				// the defect in, which is why nothing ever failed while every
				// Go type in the index carried a false kind.
				"TransactionService": model.KindClass,
				"Repository":         model.KindInterface,
				"MaxRetries":         model.KindConstant,
				"DefaultTimeout":     model.KindVariable,
				"NewService":         model.KindFunction,
				"UpdateStatus":       model.KindMethod,
			},
		},
		{
			// The four Go type forms, separated. A single (type_spec ...) rule
			// answered all of them with "type_alias" — wrong for the first
			// three, and blind to the fourth, because `type X = Y` parses as
			// type_alias rather than type_spec.
			name: "go_type_forms_are_distinguished",
			file: "types.go",
			src: `package pay

type Money struct{ Cents int64 }

type Ledger interface{ Post(Money) error }

type AccountID int

type Alias = AccountID`,
			want: map[string]model.EntityKind{
				"Money":     model.KindClass,
				"Ledger":    model.KindInterface,
				"AccountID": model.KindClass,
				"Alias":     model.KindTypeAlias,
			},
		},
		{
			// A constructor arrives tagged twice — once by the inferred query
			// with an unrecognised kind that falls back to KindFunction, once by
			// our supplement as a method. Deduplication must keep the specific
			// kind. It previously kept "function" purely because f sorts before
			// m, which is the bug this case exists to prevent recurring.
			name: "java_constructor_is_a_method",
			file: "Account.java",
			src: `package p;

public class Account {
    private String id;

    public Account(String id) { this.id = id; }

    public void deposit(long amount) {}
}`,
			want: map[string]model.EntityKind{
				"Account": model.KindMethod, // the constructor, not the class
				"id":      model.KindField,
				"deposit": model.KindMethod,
			},
		},
		{
			name: "java_annotation_type",
			file: "Marker.java",
			src: `package p;

@interface Marker {}

interface Plain {}

enum Colour { RED }

record Point(int x, int y) {}`,
			want: map[string]model.EntityKind{
				"Marker": model.KindInterface,
				"Plain":  model.KindInterface,
				// An enum declares a type with members. SCIP ingestion maps
				// SymbolInformation_Enum to KindClass, and this path must agree
				// or an in-place upgrade to compiler-backed facts would change
				// what the symbol is.
				"Colour": model.KindClass,
				"Point":  model.KindClass,
			},
		},
		{
			// An exported abstract class was absent from the index entirely,
			// which for most TypeScript codebases means the base of the type
			// hierarchy was invisible.
			name: "ts_abstract_class_and_signatures",
			file: "base.ts",
			src: `export abstract class Base {
  abstract handle(id: string): void;
  concrete(): number { return 1; }
}

export interface Repo {
  save(id: string): void;
  find(id: string): string;
}

export type Status = "ok" | "failed";

export enum Level { Low, High }

export function* pager(n: number) { yield n; }`,
			want: map[string]model.EntityKind{
				"Base":     model.KindClass,
				"handle":   model.KindMethod,
				"concrete": model.KindMethod,
				"Repo":     model.KindInterface,
				"save":     model.KindMethod,
				"find":     model.KindMethod,
				// Status is a REAL alias and stays one; Level is an enum and
				// becomes a class. Holding both in one case is the point: the
				// two were indistinguishable before, and this is where a
				// regression that re-merges them shows up.
				"Status": model.KindTypeAlias,
				"Level":  model.KindClass,
				"pager":  model.KindFunction,
			},
		},
		{
			// Ambient declarations describe an external library's API. Missing
			// function_signature meant a .d.ts file indexed as declaring nothing.
			name: "ts_ambient_declare_module",
			file: "ext.d.ts",
			src: `declare module "ext" {
  export function helper(x: number): string;
  export function other(): void;
}`,
			want: map[string]model.EntityKind{
				"helper": model.KindFunction,
				"other":  model.KindFunction,
			},
		},
		{
			// The pattern must be anchored on (module ...): the grammar attaches
			// assignment directly to module with no expression_statement
			// wrapper. Assuming the wrapper matched nothing at all.
			name: "py_module_constants",
			file: "config.py",
			src: `MAX_RETRIES = 3
DEFAULT_NAME = "x"

class Service:
    def handle(self):
        local_only = 1
        return local_only`,
			want: map[string]model.EntityKind{
				"MAX_RETRIES":  model.KindConstant,
				"DEFAULT_NAME": model.KindConstant,
				"Service":      model.KindClass,
				"handle":       model.KindFunction,
			},
		},
		{
			name: "rust_struct_enum_trait_const",
			file: "lib.rs",
			src: `pub struct Service {}

pub enum Status { Ok, Failed }

pub trait Repository { fn save(&self, id: &str); }

pub const MAX: u32 = 3;`,
			want: map[string]model.EntityKind{
				"Service":    model.KindClass,
				"Status":     model.KindClass,
				"Repository": model.KindInterface,
				"MAX":        model.KindConstant,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := parse(t, c.file, c.src)

			got := map[string][]model.EntityKind{}
			for _, s := range res.Symbols {
				got[s.Name] = append(got[s.Name], s.Kind)
			}

			for name, wantKind := range c.want {
				kinds, present := got[name]
				if !present {
					t.Errorf("%s: symbol %q missing from the index", c.file, name)
					continue
				}
				if wantKind == "" {
					continue
				}
				matched := false
				for _, k := range kinds {
					if k == wantKind {
						matched = true
					}
				}
				if !matched {
					t.Errorf("%s: symbol %q has kind(s) %v, want %q", c.file, name, kinds, wantKind)
				}
			}
		})
	}
}

// TestPythonLocalsAreNotIndexed guards the anchor on the module-level constant
// pattern. Dropping (module ...) would capture every assignment in every
// function body and bury the real declarations in local-variable noise.
func TestPythonLocalsAreNotIndexed(t *testing.T) {
	res := parse(t, "x.py", `TOP_LEVEL = 1

def work():
    buried = 2
    also_buried = 3
    return buried + also_buried`)

	names := map[string]bool{}
	for _, s := range res.Symbols {
		names[s.Name] = true
	}
	if !names["TOP_LEVEL"] {
		t.Error("module-level constant not indexed")
	}
	for _, local := range []string{"buried", "also_buried"} {
		if names[local] {
			t.Errorf("local variable %q was indexed; the (module ...) anchor is not doing its job", local)
		}
	}
}

// TestFixtureConstructorKind pins the exact case reported against the payment
// fixture: the constructor at line 17 must be a method.
func TestFixtureConstructorKind(t *testing.T) {
	path := "../../fixtures/payment-platform/src/main/java/com/pay/controller/CallbackController.java"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	p := NewRegistry().ForFile(path)
	if p == nil {
		t.Fatal("no parser for the fixture")
	}
	res, err := p.Parse(context.Background(), path, src)
	if err != nil {
		t.Fatal(err)
	}

	var sawClass, sawCtor bool
	for _, s := range res.Symbols {
		if s.Name != "CallbackController" {
			continue
		}
		switch s.Kind {
		case model.KindClass:
			sawClass = true
		case model.KindMethod:
			sawCtor = true
		case model.KindFunction:
			t.Errorf("constructor at line %d stored as KindFunction; deduplication kept the fallback kind",
				s.Range.Start.Line)
		}
	}
	if !sawClass {
		t.Error("the class declaration is missing")
	}
	if !sawCtor {
		t.Error("the constructor is not recorded with method kind")
	}
}

// TestSymbolLineNumbersAreTheDeclaration guards a capture-anchoring mistake
// that names-only testing cannot catch.
//
// A tags query attaches @definition.KIND to whichever node it is written
// against. Anchoring it on an enclosing node — (module ...) rather than the
// assignment inside it — still yields the right NAME while reporting the
// enclosing node's position, so every module constant claimed to be at line 1.
// An agent following that location opens the wrong part of the file.
//
// Found by the differential test comparing positions, not just names.
func TestSymbolLineNumbersAreTheDeclaration(t *testing.T) {
	cases := []struct {
		file string
		src  string
		want map[string]int
	}{
		{
			"c.py",
			"import os\n\n\nT = 1\nMAX_RETRIES = 3\n",
			map[string]int{"T": 4, "MAX_RETRIES": 5},
		},
		{
			"d.go",
			"package p\n\ntype Alpha struct{}\n\nconst Beta = 1\n",
			map[string]int{"Alpha": 3, "Beta": 5},
		},
	}
	for _, c := range cases {
		res := parse(t, c.file, c.src)
		seen := map[string]bool{}
		for _, s := range res.Symbols {
			want, tracked := c.want[s.Name]
			if !tracked {
				continue
			}
			seen[s.Name] = true
			if s.Range.Start.Line != want {
				t.Errorf("%s: %q reported at line %d, want %d — the capture is anchored on an enclosing node",
					c.file, s.Name, s.Range.Start.Line, want)
			}
		}
		for name := range c.want {
			if !seen[name] {
				t.Errorf("%s: %q not extracted", c.file, name)
			}
		}
	}
}

// TestKindSpecificityOrdering documents why deduplication cannot sort kinds
// alphabetically: KindFunction is the catch-all kindFromTag returns for
// anything unrecognised, so it must never displace a specific kind.
func TestKindSpecificityOrdering(t *testing.T) {
	fallback := kindSpecificity(model.KindFunction)
	for _, specific := range []model.EntityKind{
		model.KindMethod, model.KindClass, model.KindInterface,
		model.KindTypeAlias, model.KindConstant, model.KindField,
	} {
		if kindSpecificity(specific) <= fallback {
			t.Errorf("kind %q ranks at or below the KindFunction fallback; deduplication would discard it",
				specific)
		}
	}

	// A general "this is a type" pattern also matches interfaces and aliases —
	// Go's (type_spec ...) does — so one declaration arrives tagged twice and
	// the more specific claim has to win. Equal ranks would leave it to tag
	// emission order, making the kind unreproducible rather than merely wrong.
	if kindSpecificity(model.KindInterface) <= kindSpecificity(model.KindClass) {
		t.Error("KindInterface must outrank KindClass: the general type pattern matches interfaces too")
	}
	if kindSpecificity(model.KindTypeAlias) <= kindSpecificity(model.KindClass) {
		t.Error("KindTypeAlias must outrank KindClass: an alias is a more specific claim than a type")
	}
}

// TestKindFromTagMatchesTheCompilerBackedVocabulary pins the agreement between
// this path and SCIP ingestion.
//
// It is not a style rule. A heuristic edge is upgraded IN PLACE when a
// compiler-backed index arrives, so if internal/resolve/scip/kinds.go and
// kindFromTag disagreed about what a struct or an enum is, the upgrade would
// silently change a symbol's kind — and the SCIP cross-check that measures
// heuristic precision would report differences that are really two
// vocabularies rather than two answers.
//
// The authority is internal/resolve/scip/kinds.go: it maps Class, Struct,
// Object, Enum, Union and Type to KindClass, and reserves KindTypeAlias for
// TypeAlias and AssociatedType. If that file changes, this test must change
// with it — and that is the point of asserting it here.
func TestKindFromTagMatchesTheCompilerBackedVocabulary(t *testing.T) {
	for tag, want := range map[string]model.EntityKind{
		"definition.class":  model.KindClass,
		"definition.struct": model.KindClass,
		"definition.enum":   model.KindClass,
		"definition.union":  model.KindClass,
		// The one that was wrong. "type" meant alias, so no Go struct was ever
		// a class and the context engine's container gate matched nothing in Go.
		"definition.type":    model.KindClass,
		"definition.typedef": model.KindClass,

		"definition.interface": model.KindInterface,
		"definition.trait":     model.KindInterface,
		"definition.protocol":  model.KindInterface,

		// Genuine aliases only, and nothing else may produce this kind.
		"definition.type_alias": model.KindTypeAlias,
		"definition.alias":      model.KindTypeAlias,
	} {
		if got := kindFromTag(tag); got != want {
			t.Errorf("kindFromTag(%q) = %q, want %q", tag, got, want)
		}
	}

	// Guard the inverse too: an unrecognised tag must not land on
	// KindTypeAlias, or the original bug returns by a different route.
	if got := kindFromTag("definition.somethingnobodyhasheardof"); got == model.KindTypeAlias {
		t.Error("the unknown-tag fallback resolves to KindTypeAlias; aliases must be claimed explicitly")
	}
}

// symbolNamesFor parses src as the given language and returns the names of
// every symbol extracted. The registry dispatches on file extension, so the
// filename passed to parse() is a stand-in chosen per language.
func symbolNamesFor(t *testing.T, lang, src string) []string {
	t.Helper()
	ext := map[string]string{
		"typescript": "x.ts",
		"tsx":        "x.tsx",
		"javascript": "x.js",
	}[lang]
	if ext == "" {
		t.Fatalf("symbolNamesFor: no fixture extension registered for language %q", lang)
	}
	res := parse(t, ext, src)
	return symbolNames(res)
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// export const Button = () => {} is the dominant React and Vue component form.
// No supplement captured a lexical_declaration whose value is a function, so
// the most common way to declare a frontend component produced no symbol at
// all — nothing to find, and nothing for a reference to bind to.
func TestArrowFunctionBindingsBecomeSymbols(t *testing.T) {
	cases := []struct {
		lang string
		src  string
		want []string
	}{
		{
			lang: "typescript",
			src: `export const load = async () => { return 1 }
const helper = function () { return 2 }
export const notAFunction = 42
`,
			want: []string{"load", "helper"},
		},
		{
			lang: "tsx",
			src: `export const Button = () => <button />
const useThing = () => { return null }
export const NOT_A_FUNCTION = "x"
`,
			want: []string{"Button", "useThing"},
		},
		{
			lang: "javascript",
			src: `export const handler = async () => { return 1 }
const other = function () { return 2 }
`,
			want: []string{"handler", "other"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			names := symbolNamesFor(t, tc.lang, tc.src)
			for _, want := range tc.want {
				if !containsName(names, want) {
					t.Errorf("%s: %q produced no symbol; got %v", tc.lang, want, names)
				}
			}
			// A constant that is not a function must NOT become a function
			// symbol. Over-capturing here would fill the graph with every
			// config value in the codebase.
			for _, unwanted := range []string{"notAFunction", "NOT_A_FUNCTION"} {
				if containsName(names, unwanted) {
					t.Errorf("%s: %q is not a function and must not be captured; got %v",
						tc.lang, unwanted, names)
				}
			}
		})
	}
}

// kindSet is a set of @definition.<kind> names, used to compare the
// definition kinds two supplement queries capture without caring about query
// text formatting.
type kindSet map[string]bool

// difference returns the kinds in s that are not in other.
func (s kindSet) difference(other kindSet) kindSet {
	out := kindSet{}
	for k := range s {
		if !other[k] {
			out[k] = true
		}
	}
	return out
}

var definitionCaptureRE = regexp.MustCompile(`@definition\.(\w+)`)

// definitionKinds scans a tags-query string for @definition.<kind> captures
// and returns the distinct set of kinds it declares.
func definitionKinds(query string) kindSet {
	out := kindSet{}
	for _, m := range definitionCaptureRE.FindAllStringSubmatch(query, -1) {
		out[m[1]] = true
	}
	return out
}

// tsx IS the typescript grammar plus JSX. Any construct typescript's supplement
// captures, the tsx grammar can parse identically — so a capture present in one
// and missing from the other is an oversight, not a language difference. It
// shipped with 2 captures against typescript's 10, which meant every enum,
// field and method signature in a .tsx file was invisible.
func TestTSXSupplementCoversEverythingTypeScriptDoes(t *testing.T) {
	ts := Supplement("typescript")
	tsx := Supplement("tsx")

	if strings.TrimSpace(tsx) == "" {
		t.Fatal("tsx has no supplement at all")
	}

	// Compare on the captured definition kinds rather than on the query text,
	// so a formatting difference does not fail the test and a genuinely missing
	// construct does.
	missing := definitionKinds(ts).difference(definitionKinds(tsx))
	if len(missing) > 0 {
		t.Errorf("tsx supplement is missing these definition kinds that typescript captures: %v", missing)
	}
}
