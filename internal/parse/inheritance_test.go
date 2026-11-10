package parse

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Supertype references: the recall gap these tests exist to pin.
//
// Before inheritanceQueries, `queries.go` held only @definition.* patterns and
// the grammars' inferred tags emitted essentially one reference kind —
// reference.call. internal/resolve/infer.edgeKindFor maps a reference kind to
// an edge kind by substring, so its "implementation" and "extend"/"inherit"
// branches were unreachable from source and EXTENDS/IMPLEMENTS edges could
// only ever arrive from a SCIP index. internal/capability/impact traverses
// those edges, so without SCIP it expanded through an edge set nothing filled.
//
// These tests therefore assert two things a golden count cannot:
//
//   - the reference kind emitted still satisfies edgeKindFor's substring test,
//     which is the contract that makes the edge appear at all; and
//   - a file with NO inheritance emits NO supertype reference, because a
//     pattern that over-matches puts false edges on the graph, and a false
//     EXTENDS is worse than a missing one — impact analysis follows it.

// edgeKindContract mirrors the substring test in
// internal/resolve/infer.edgeKindFor. Stated here rather than imported because
// that function is unexported and lives in a package this one must not depend
// on; if the two ever disagree, the reference is emitted and silently degrades
// to EdgeReferences, which is exactly the failure worth a test.
func edgeKindContract(refKind string) string {
	switch {
	case strings.Contains(refKind, "call"):
		return "CALLS"
	case strings.Contains(refKind, "implementation"):
		return "IMPLEMENTS"
	case strings.Contains(refKind, "extend"), strings.Contains(refKind, "inherit"):
		return "EXTENDS"
	default:
		return "REFERENCES"
	}
}

// supertypeRefs returns (edgeKind, name) pairs for every reference that
// edgeKindContract classifies as a supertype relation.
func supertypeRefs(res *Result) []string {
	var out []string
	for _, r := range res.References {
		if k := edgeKindContract(r.Kind); k == "EXTENDS" || k == "IMPLEMENTS" {
			out = append(out, fmt.Sprintf("%s:%s", k, r.Name))
		}
	}
	return out
}

func TestInheritanceReferencesByLanguage(t *testing.T) {
	cases := []struct {
		name string
		file string
		src  string
		want []string
	}{
		{
			// Java is the language this change is worth the most in: it states
			// both relations explicitly and is where compiler-backed indexing
			// most often is not available.
			name: "java_extends_and_implements",
			file: "TransactionService.java",
			src: `package com.pay;

public class TransactionService extends AbstractService implements Auditable, Closeable {
    public void updateStatus(String id) { repository.save(id); }
}`,
			want: []string{"EXTENDS:AbstractService", "IMPLEMENTS:Auditable", "IMPLEMENTS:Closeable"},
		},
		{
			// Generic and package-qualified supertypes are the common written
			// forms and are different grammar nodes. The qualified one must
			// arrive as the SIMPLE name, because the resolver matches a
			// reference against the symbol index by name.
			name: "java_generic_and_qualified",
			file: "Repo.java",
			src: `package com.pay;
class Repo extends com.pay.base.AbstractRepo<Txn> implements Sink<Txn>, com.pay.api.Port {}`,
			want: []string{"EXTENDS:AbstractRepo", "IMPLEMENTS:Sink", "IMPLEMENTS:Port"},
		},
		{
			// enum and record reuse the same super_interfaces node, and an
			// interface's `extends` is a different node from a class's.
			name: "java_interface_enum_record",
			file: "Kinds.java",
			src: `package com.pay;
interface Ledger extends Auditable, Closeable {}
enum Status implements Auditable {}
record Money(long cents) implements Comparable {}`,
			want: []string{
				"EXTENDS:Auditable", "EXTENDS:Closeable",
				"IMPLEMENTS:Auditable",
				"IMPLEMENTS:Comparable",
			},
		},
		{
			name: "typescript_extends_and_implements",
			file: "service.ts",
			src: `export class TransactionService extends AbstractService implements Auditable, Closeable {
  updateStatus(id: string): void {}
}`,
			want: []string{"EXTENDS:AbstractService", "IMPLEMENTS:Auditable", "IMPLEMENTS:Closeable"},
		},
		{
			// An interface's heritage is extends_type_clause, a different node
			// from a class's class_heritage; an abstract class reuses the
			// latter; and `extends ns.Base` must yield Base, not ns.
			name: "typescript_interface_abstract_namespaced",
			file: "types.ts",
			src: `export interface Repository extends Readable, Writable {}
abstract class Base extends Core<T> implements Sink<T> {}
class Ns extends outer.Component implements outer.Port {}`,
			want: []string{
				"EXTENDS:Readable", "EXTENDS:Writable",
				"EXTENDS:Core", "IMPLEMENTS:Sink",
				"EXTENDS:Component", "IMPLEMENTS:Port",
			},
		},
		{
			name: "tsx_react_component",
			file: "Widget.tsx",
			src: `class Widget extends React.Component implements Renderable {}
interface Props extends BaseProps {}`,
			want: []string{"EXTENDS:Component", "IMPLEMENTS:Renderable", "EXTENDS:BaseProps"},
		},
		{
			name: "javascript_class_heritage",
			file: "widget.js",
			src: `class Widget extends React.Component {}
class Plain extends Base {}`,
			want: []string{"EXTENDS:Component", "EXTENDS:Base"},
		},
		{
			// Python has no interface construct, so nothing here may be
			// IMPLEMENTS. A metaclass is a keyword argument, not a base class:
			// reporting it as one would put a false edge on the graph.
			name: "python_base_classes",
			file: "service.py",
			src: `class TransactionService(AbstractService, Auditable):
    pass

class Repo(base.AbstractRepo, metaclass=Meta):
    pass`,
			want: []string{
				"EXTENDS:AbstractService", "EXTENDS:Auditable",
				"EXTENDS:AbstractRepo",
			},
		},
		{
			// Go has NO implements keyword. Interface satisfaction is
			// structural and undecidable from one file's syntax tree, so no
			// IMPLEMENTS is emitted for Go at all — only embedding, which
			// promotes a method set and is the closest thing Go has to extends.
			name: "go_embedding",
			file: "service.go",
			src: `package pay

type Auditable interface {
	Closeable
	io.Reader
	Audit() error
}

type Service struct {
	AbstractService
	*Logger
	sync.Mutex
	name string
}`,
			want: []string{
				"EXTENDS:Closeable", "EXTENDS:Reader",
				"EXTENDS:AbstractService", "EXTENDS:Logger", "EXTENDS:Mutex",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parse(t, tc.file, tc.src)
			got := supertypeRefs(res)

			sortedGot := append([]string(nil), got...)
			sortedWant := append([]string(nil), tc.want...)
			sort.Strings(sortedGot)
			sort.Strings(sortedWant)
			if strings.Join(sortedGot, " ") != strings.Join(sortedWant, " ") {
				t.Errorf("supertype references mismatch\n got: %v\nwant: %v", sortedGot, sortedWant)
			}
		})
	}
}

// TestNoInheritanceEmitsNoSupertypeReference is the other half of the contract.
// A pattern that over-matches is worse than one that under-matches: a false
// EXTENDS edge is followed by impact analysis and reported as a consequence of
// a change that has none.
func TestNoInheritanceEmitsNoSupertypeReference(t *testing.T) {
	cases := []struct{ name, file, src string }{
		{"java", "Plain.java", `package com.pay;
class Plain {
    private String status;
    void updateStatus(String id) { repository.save(id); }
}
interface Bare { void save(String id); }`},
		{"typescript", "plain.ts", `export class Plain {
  updateStatus(id: string): void { this.repo.save(id); }
}
export interface Bare { save(id: string): void; }
export type Status = "ok" | "failed";`},
		{"tsx", "plain.tsx", `export class Plain { render() { return null; } }
export interface Bare { save(): void; }`},
		{"javascript", "plain.js", `class Plain { save(id) { return this.repo.save(id); } }`},
		{"python", "plain.py", `class Plain:
    def update_status(self, txn_id):
        return self.repo.save(txn_id)

def helper(a, b):
    return a(b)`},
		{"go_named_fields_only", "plain.go", `package pay

type Service struct {
	name string
	repo Repository
	next *Service
	m    sync.Mutex
}

type Repository interface {
	Save(id string) error
	Load(id string) (string, error)
}

func (s *Service) Update(id string) error { return s.repo.Save(id) }`},
		{
			// Go generic constraints parse as interface elements too. An
			// unanchored type_elem pattern reported `string` as a supertype;
			// the anchors are what stop that, and this is the case that proves
			// it.
			"go_generic_constraint", "constraint.go", `package pay

type Number interface {
	~int | ~int64 | float64 | string
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parse(t, tc.file, tc.src)
			if got := supertypeRefs(res); len(got) != 0 {
				t.Errorf("file declares no inheritance but produced %v", got)
			}
		})
	}
}

// TestInheritanceCaptureNamesReachTheIntendedEdgeKind guards the one thing that
// makes this whole change load-bearing: the capture NAME. Rename
// @reference.implementation to @reference.implements and edgeKindFor stops
// matching it — the reference is still emitted, still resolves, and silently
// becomes a plain REFERENCES edge that impact analysis does not traverse for
// inheritance. Nothing fails; recall just quietly returns to zero.
func TestInheritanceCaptureNamesReachTheIntendedEdgeKind(t *testing.T) {
	for _, lang := range InheritanceLanguages() {
		q := InheritanceQuery(lang)
		if strings.TrimSpace(q) == "" {
			t.Errorf("%s: listed but empty", lang)
			continue
		}
		found := false
		for _, field := range strings.Fields(q) {
			field = strings.TrimRight(field, ")")
			if !strings.HasPrefix(field, "@reference.") {
				continue
			}
			found = true
			switch k := edgeKindContract(field); k {
			case "EXTENDS", "IMPLEMENTS":
			default:
				t.Errorf("%s: capture %q maps to %s, not an inheritance edge", lang, field, k)
			}
		}
		if !found {
			t.Errorf("%s: query contains no @reference capture", lang)
		}
	}
}

// TestInheritanceQueriesCompileForEveryLanguage is the guard that matters most
// operationally. A tags query naming a node type a grammar does not have is
// REJECTED, not ignored. newGenericParser drops the inheritance patterns and
// retries so a rejection costs edges rather than every symbol in the language —
// but a silent drop is still a silent recall loss, so it is asserted here.
func TestInheritanceQueriesCompileForEveryLanguage(t *testing.T) {
	probe := map[string]string{
		"go": "x.go", "java": "X.java", "typescript": "x.ts",
		"tsx": "x.tsx", "javascript": "x.js", "python": "x.py",
	}
	for _, lang := range InheritanceLanguages() {
		file, ok := probe[lang]
		if !ok {
			t.Errorf("%s has an inheritance query but no probe file in this test", lang)
			continue
		}
		p := NewRegistry().ForFile(file)
		if p == nil {
			t.Errorf("%s: no parser for %s", lang, file)
			continue
		}
		gp, ok := p.(*genericParser)
		if !ok {
			continue
		}
		if gp.initErr != nil {
			t.Errorf("%s: parser construction failed: %v", lang, gp.initErr)
			continue
		}
		if !strings.Contains(gp.tagsQuery, "@reference.") {
			t.Errorf("%s: inheritance patterns were dropped from the tags query — "+
				"the grammar rejected them and EXTENDS/IMPLEMENTS recall is now zero for this language", lang)
		}
	}
}

// TestSupertypeReferencesCarryNoReceiver: a receiver is a property of a call —
// the object a method was invoked on — and a supertype has none. Receivers are
// matched by (line, name), so a class declaration sharing a line with a call of
// the same name could otherwise acquire one, and the resolver would use it to
// narrow candidates for a relation that has no receiver at all.
func TestSupertypeReferencesCarryNoReceiver(t *testing.T) {
	res := parse(t, "Mixed.java", `package com.pay;
class A extends Builder { void m() { Builder.of(); b.build(); } }`)
	for _, r := range res.References {
		if k := edgeKindContract(r.Kind); (k == "EXTENDS" || k == "IMPLEMENTS") && r.Receiver != "" {
			t.Errorf("supertype reference %q carries receiver %q", r.Name, r.Receiver)
		}
	}
}

// TestInheritanceReferencesAreDeterministic underpins the registry digest gate:
// references are appended in tag order and never sorted, so the ORDER is part
// of the output contract, not only the set.
func TestInheritanceReferencesAreDeterministic(t *testing.T) {
	srcs := map[string]string{
		"d.java": `package p;
class A extends B implements C, D {}
class E extends F implements G {}
interface H extends I, J {}`,
		"d.ts": `class A extends B implements C, D {}
interface I extends J, K {}`,
		"d.py": `class A(B, C):
    pass
class D(E, F):
    pass`,
		"d.go": `package p
type A interface { B; C }
type S struct {
	D
	*E
	f G
}`,
	}
	for file, src := range srcs {
		t.Run(file, func(t *testing.T) {
			fingerprint := func() string {
				res := parse(t, file, src)
				var b strings.Builder
				for _, r := range res.References {
					fmt.Fprintf(&b, "%s|%s|%d:%d;", r.Kind, r.Name, r.Range.Start.Line, r.Range.Start.Col)
				}
				return b.String()
			}
			first := fingerprint()
			if !strings.Contains(first, "reference.") {
				t.Fatalf("no references at all for %s", file)
			}
			for i := 0; i < 20; i++ {
				if got := fingerprint(); got != first {
					t.Fatalf("run %d differs:\n first=%s\n got  =%s", i, first, got)
				}
			}
		})
	}
}

// TestSupertypeReferencesAreNotDuplicated: the shapes within one language's
// query are meant to be mutually exclusive — a supertype is written plain, or
// generic, or qualified, never two at once. Two patterns matching one clause
// would double every inheritance edge in the graph.
func TestSupertypeReferencesAreNotDuplicated(t *testing.T) {
	cases := []struct{ file, src string }{
		{"Dup.java", `package p;
class A extends com.x.Base<T> implements com.x.Port<T>, Plain {}`},
		{"dup.ts", `class A extends ns.Base implements ns.Port, Plain {}`},
		{"dup.py", `class A(mod.Base, Plain):
    pass`},
		{"dup.go", `package p
type S struct {
	*pkg.Base
	pkg.Other
	Plain
}
type I interface { pkg.Reader; Plain }`},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			res := parse(t, tc.file, tc.src)
			seen := map[string]int{}
			for _, r := range res.References {
				if k := edgeKindContract(r.Kind); k != "EXTENDS" && k != "IMPLEMENTS" {
					continue
				}
				key := fmt.Sprintf("%s@%d:%d", r.Name, r.Range.Start.Line, r.Range.Start.Col)
				seen[key]++
			}
			for k, n := range seen {
				if n > 1 {
					t.Errorf("supertype reference %s emitted %d times", k, n)
				}
			}
			if len(seen) == 0 {
				t.Errorf("expected supertype references, got none")
			}
		})
	}
}

// FuzzInheritanceCaptures exercises the new capture path the way FuzzParseGo
// exercises definitions: the point is not that the output is right, it is that
// a hostile or merely broken heritage clause cannot panic, cannot return a
// run-level error, and cannot emit a supertype reference with an empty name —
// which would become an edge to nothing.
func FuzzInheritanceCaptures(f *testing.F) {
	f.Add("A.java", "class A extends B implements C {}")
	f.Add("A.java", "class A extends {")
	f.Add("A.java", "interface I extends ,,, {}")
	f.Add("a.ts", "class A extends B implements C, D {}")
	f.Add("a.ts", "interface I extends")
	f.Add("a.tsx", "class A extends React.Component {}")
	f.Add("a.js", "class A extends B {}")
	f.Add("a.py", "class A(B, C):\n    pass")
	f.Add("a.py", "class A(:")
	f.Add("a.go", "package p\ntype S struct{ A\n*B\npkg.C }")
	f.Add("a.go", "package p\ntype I interface{ ~int | string }")
	f.Add("a.go", "package p\ntype S struct {")

	files := []string{"A.java", "a.ts", "a.tsx", "a.js", "a.py", "a.go"}
	allowed := make(map[string]bool, len(files))
	for _, f := range files {
		allowed[f] = true
	}

	reg := NewRegistry()
	f.Fuzz(func(t *testing.T, file, src string) {
		if !allowed[file] {
			t.Skip()
		}
		p := reg.ForFile(file)
		if p == nil {
			t.Skip()
		}
		res, err := p.Parse(context.Background(), file, []byte(src))
		if err != nil {
			t.Fatalf("run-level error on fuzz input: %v", err)
		}
		if res == nil {
			t.Fatal("nil result")
		}
		for _, r := range res.References {
			k := edgeKindContract(r.Kind)
			if k != "EXTENDS" && k != "IMPLEMENTS" {
				continue
			}
			if r.Name == "" {
				t.Fatalf("supertype reference with an empty name in %s", file)
			}
			if r.Receiver != "" {
				t.Fatalf("supertype reference %q carries receiver %q", r.Name, r.Receiver)
			}
			if r.Range.Start.Line < 1 {
				t.Fatalf("supertype reference %q at line %d", r.Name, r.Range.Start.Line)
			}
		}
	})
}
