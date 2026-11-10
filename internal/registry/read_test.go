package registry

import (
	"context"
	"reflect"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// classFile is one type declaration, as the symbol index holds it: a class
// symbol whose file is named for it, which is the shape Java guarantees and the
// resolver's receiver tier relies on.
func classFile(dir, name string) fileSpec {
	path := "src/main/java/com/pay/" + dir + "/" + name + ".java"
	return fileSpec{
		path: path, lang: "java", hash: "hash-" + name,
		syms: []symSpec{{fqn: model.DefaultFQN(path, name), name: name, line: 3, kind: model.KindClass}},
	}
}

// inherits builds the EXTENDS or IMPLEMENTS edge resolution writes for a
// `class Child extends Parent` reference: source is the DECLARING type, because
// the reference's enclosing symbol is the class itself.
func inherits(childDir, child, parentDir, parent string, kind model.EdgeKind) edgeSpec {
	childPath := "src/main/java/com/pay/" + childDir + "/" + child + ".java"
	parentPath := "src/main/java/com/pay/" + parentDir + "/" + parent + ".java"
	return edgeSpec{
		srcFQN: model.DefaultFQN(childPath, child),
		dstFQN: model.DefaultFQN(parentPath, parent),
		kind:   kind, conf: model.ConfidenceLow, source: model.SourceSourceCode,
		evFile: childPath, evLine: 3,
	}
}

// hierarchy is the fixture every case here shares:
//
//	Payment    extends BasePayment, implements Auditable
//	BasePayment extends LedgerEntity
//	Invoice     (nothing)
//
// plus a SECOND Payment in another package, so the union a name-keyed query
// necessarily takes is exercised rather than assumed away.
func hierarchy() ([]fileSpec, []edgeSpec) {
	files := []fileSpec{
		classFile("model", "Payment"),
		classFile("base", "BasePayment"),
		classFile("base", "LedgerEntity"),
		classFile("audit", "Auditable"),
		classFile("billing", "Invoice"),
		// A homonym: com.pay.legacy.Payment, extending something else entirely.
		{
			path: "src/main/java/com/pay/legacy/Payment.java", lang: "java", hash: "hash-legacy",
			syms: []symSpec{{
				fqn:  model.DefaultFQN("src/main/java/com/pay/legacy/Payment.java", "Payment"),
				name: "Payment", line: 3, kind: model.KindClass}},
		},
	}
	edges := []edgeSpec{
		inherits("model", "Payment", "base", "BasePayment", model.EdgeExtends),
		inherits("model", "Payment", "audit", "Auditable", model.EdgeImplements),
		inherits("base", "BasePayment", "base", "LedgerEntity", model.EdgeExtends),
		{
			srcFQN: model.DefaultFQN("src/main/java/com/pay/legacy/Payment.java", "Payment"),
			dstFQN: model.DefaultFQN("src/main/java/com/pay/billing/Invoice.java", "Invoice"),
			kind:   model.EdgeExtends, conf: model.ConfidenceLow, source: model.SourceSourceCode,
			evFile: "src/main/java/com/pay/legacy/Payment.java", evLine: 3,
		},
	}
	return files, edges
}

// TestSupertypesReadsBothInheritanceChannels is the query the deferred
// inherited-dispatch pass is built on.
//
// EXTENDS and IMPLEMENTS both count: a method reached through a superclass and
// one reached through an implemented interface are equally inherited as far as
// a call site is concerned, and answering only the first would leave every
// interface-declared method unreachable.
func TestSupertypesReadsBothInheritanceChannels(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	files, edges := hierarchy()
	writeContent(t, r, run.ID, files, edges)

	got, err := r.Supertypes(ctx, run.ID, []string{"Payment", "BasePayment", "Invoice", "Nowhere"})
	if err != nil {
		t.Fatalf("supertypes: %v", err)
	}

	// Payment unions both of its own supertypes AND the homonym's, because the
	// question a name-keyed index can answer is about the NAME. The walk above
	// this is tolerant of that — a supertype that defines nothing contributes
	// nothing — and the alternative, guessing which Payment was meant, is the
	// coin flip this project refuses.
	want := map[string][]string{
		"Payment":     {"Auditable", "BasePayment", "Invoice"},
		"BasePayment": {"LedgerEntity"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("supertypes = %v, want %v", got, want)
	}

	// Invoice has no supertypes and Nowhere is not a type at all. Both must be
	// ABSENT rather than present-and-empty: the port states the two mean the
	// same thing, and the walk records an absent name as "walked, has none".
	if _, present := got["Invoice"]; present {
		t.Errorf("a type with no supertypes was returned present: %v", got)
	}
	if _, present := got["Nowhere"]; present {
		t.Errorf("an unknown name was returned present: %v", got)
	}
}

// TestSupertypesIsRunScoped is invariant 3 applied to this query.
//
// The edges table holds every run at once, so a supertype query that forgot the
// run would let a staging hierarchy — or the debris of a crashed index — decide
// how a committed run resolves.
func TestSupertypesIsRunScoped(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	files, edges := hierarchy()

	run1, err := r.BeginRun(ctx, "auto", "rev1")
	if err != nil {
		t.Fatalf("begin run 1: %v", err)
	}
	writeContent(t, r, run1.ID, files, edges)

	// Run 2 holds the same types with the hierarchy REMOVED — the shape a
	// working tree takes after someone deletes an `extends` clause.
	run2, err := r.BeginRun(ctx, "auto", "rev2")
	if err != nil {
		t.Fatalf("begin run 2: %v", err)
	}
	writeContent(t, r, run2.ID, files, nil)

	got1, err := r.Supertypes(ctx, run1.ID, []string{"Payment"})
	if err != nil {
		t.Fatalf("supertypes run 1: %v", err)
	}
	if len(got1["Payment"]) == 0 {
		t.Fatalf("run 1 lost its own hierarchy: %v", got1)
	}

	got2, err := r.Supertypes(ctx, run2.ID, []string{"Payment"})
	if err != nil {
		t.Fatalf("supertypes run 2: %v", err)
	}
	if len(got2) != 0 {
		t.Errorf("run 2 has no inheritance edges, yet reported %v", got2)
	}
}

// TestSupertypesAreDeduplicatedAndOrdered pins the two properties the deferred
// pass's determinism rests on.
//
// It walks these slices, and the registry digest is computed over the edges
// that walk produces. An order left to the query plan, or a duplicate that
// made one ancestor look like two, would break the determinism gate in a way
// that only shows up as a digest mismatch on somebody else's machine.
func TestSupertypesAreDeduplicatedAndOrdered(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	files := []fileSpec{
		classFile("model", "Payment"),
		classFile("base", "BasePayment"),
		classFile("audit", "Auditable"),
	}
	// The same supertype claimed twice, from two evidence lines — which the
	// edge table's UNIQUE key permits, because the evidence line is part of it.
	edges := []edgeSpec{
		inherits("model", "Payment", "audit", "Auditable", model.EdgeImplements),
		inherits("model", "Payment", "base", "BasePayment", model.EdgeExtends),
		{
			srcFQN: model.DefaultFQN("src/main/java/com/pay/model/Payment.java", "Payment"),
			dstFQN: model.DefaultFQN("src/main/java/com/pay/base/BasePayment.java", "BasePayment"),
			kind:   model.EdgeExtends, conf: model.ConfidenceLow, source: model.SourceSourceCode,
			evFile: "src/main/java/com/pay/model/Payment.java", evLine: 9,
		},
	}
	writeContent(t, r, run.ID, files, edges)

	want := []string{"Auditable", "BasePayment"}
	for i := 0; i < 8; i++ {
		got, err := r.Supertypes(ctx, run.ID, []string{"Payment"})
		if err != nil {
			t.Fatalf("supertypes: %v", err)
		}
		if !reflect.DeepEqual(got["Payment"], want) {
			t.Fatalf("attempt %d: supertypes of Payment = %v, want %v", i, got["Payment"], want)
		}
	}
}

// TestSupertypesIgnoresNonInheritanceEdges keeps the answer a claim about the
// TYPE hierarchy. A CALLS edge between two classes says nothing about
// inheritance, and treating it as one would hand the walk a fabricated chain.
func TestSupertypesIgnoresNonInheritanceEdges(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	files := []fileSpec{classFile("model", "Payment"), classFile("billing", "Invoice")}
	edges := []edgeSpec{{
		srcFQN: model.DefaultFQN("src/main/java/com/pay/model/Payment.java", "Payment"),
		dstFQN: model.DefaultFQN("src/main/java/com/pay/billing/Invoice.java", "Invoice"),
		kind:   model.EdgeCalls, conf: model.ConfidenceHigh, source: model.SourceSourceCode,
		evFile: "src/main/java/com/pay/model/Payment.java", evLine: 12,
	}}
	writeContent(t, r, run.ID, files, edges)

	got, err := r.Supertypes(ctx, run.ID, []string{"Payment"})
	if err != nil {
		t.Fatalf("supertypes: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a CALLS edge was read as inheritance: %v", got)
	}
}

// TestSupertypesOfNothingIsNotAQuery: the drain asks about whatever names a
// chunk mentions, and an empty frontier must cost no round trip and no SQL with
// an empty IN () list, which SQLite would refuse outright.
func TestSupertypesOfNothingIsNotAQuery(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	for _, names := range [][]string{nil, {}, {""}} {
		got, err := r.Supertypes(ctx, run.ID, names)
		if err != nil {
			t.Fatalf("supertypes(%v): %v", names, err)
		}
		if len(got) != 0 {
			t.Errorf("supertypes(%v) = %v, want empty", names, got)
		}
	}
}
