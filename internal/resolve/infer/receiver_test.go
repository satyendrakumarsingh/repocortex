package infer

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// --- A16: a receiver must OWN the candidate, not merely occur inside its name -
//
// Tier 2 reports ConfidenceHigh whenever exactly one candidate survives it, so
// whatever that tier accepts is published as fact. The rule it used to apply was
// `strings.Contains(fqn, receiver)`, which is not a type test: it accepted any
// qualified name the receiver appeared ANYWHERE in. One accidental substring
// therefore promoted a mis-inferred receiver into a high-confidence edge to the
// wrong symbol.
//
// These tests are written so that restoring the substring rule makes them fail:
// each has exactly one candidate that contains the receiver and none that is
// owned by it, so the old rule yields a unique match and an edge, and the new
// rule yields no evidence and falls through.

// TestReceiverSubstringDoesNotOwnTheCandidate is the headline case: a receiver
// named `db` matched dbutil.go, feedback.go and anything under internal/db.
func TestReceiverSubstringDoesNotOwnTheCandidate(t *testing.T) {
	idx := index(
		sym(1, "Save", "internal/store/dbutil.go:Save", "internal/store/dbutil.go", 10, true),
		sym(2, "Save", "internal/pay/ledger.go:Save", "internal/pay/ledger.go", 11, true),
	)
	r := New(idx, Options{})

	edges, outs, st := resolveOne(t, r, registry.FileRefs{
		FileID: 50, Path: "internal/api/handler.go", Language: "go",
		Symbols: enclose(60, "Handle"),
		Refs: []registry.Reference{
			{ID: 1, Name: "Save", Kind: "reference.call", Receiver: "db", Line: 5},
		},
	})
	if len(edges) != 0 {
		t.Fatalf("a receiver that merely occurs inside a qualified name produced an edge "+
			"to %d (%q); `db` does not own dbutil.go:Save",
			edges[0].DstID, edges[0].Evidence.Detail)
	}
	if st.Ambiguous != 1 || outs[1].Resolution != registry.ResolutionAmbiguous {
		t.Errorf("expected the reference recorded as ambiguous, got %+v / %q",
			st, outs[1].Resolution)
	}
}

// TestReceiverMustMatchAWholeOwnerSegment closes the same hole one level in.
//
// A suffix test alone is not enough: `com.pay.SubTransactionService.updateStatus`
// ends with "TransactionService.updateStatus" while being owned by a DIFFERENT
// class. The owner has to occupy a whole segment.
func TestReceiverMustMatchAWholeOwnerSegment(t *testing.T) {
	idx := index(
		sym(1, "updateStatus", "com.pay.SubTransactionService.updateStatus",
			"svc/SubTransactionService.java", 10, true),
		sym(2, "updateStatus", "com.pay.OrderService.updateStatus",
			"svc/OrderService.java", 11, true),
	)
	r := New(idx, Options{})

	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 50, Path: "api/Controller.java", Language: "java",
		Symbols: enclose(60, "handleCallback"),
		Refs: []registry.Reference{
			{ID: 1, Name: "updateStatus", Kind: "reference.call",
				Receiver: "transactionService", Line: 5},
		},
	})
	if len(edges) != 0 {
		t.Fatalf("bound to %d (%q); SubTransactionService is not the TransactionService "+
			"the call site named", edges[0].DstID, edges[0].Evidence.Detail)
	}
}

// TestUnconfirmedReceiverFallsThroughRatherThanExcluding pins the half that is
// easy to get wrong, and that A18 DID get wrong (see "Why candidates are not
// filtered before the tiers" in resolver.go).
//
// Failing to confirm an owner means "no evidence about this candidate", never
// "the source says the owner is something else". A tighter tier 2 must
// therefore leave the later tiers exactly as it found them — here the Go
// same-package tier, which still answers with high confidence.
func TestUnconfirmedReceiverFallsThroughRatherThanExcluding(t *testing.T) {
	idx := index(
		sym(1, "Save", "internal/pay/store.go:Save", "internal/pay/store.go", 10, true),
		sym(2, "Save", "internal/other/store.go:Save", "internal/other/store.go", 11, true),
	)
	r := New(idx, Options{})

	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 50, Path: "internal/pay/service.go", Language: "go",
		Symbols: enclose(60, "Process"),
		Refs: []registry.Reference{
			{ID: 1, Name: "Save", Kind: "reference.call", Receiver: "db", Line: 5},
		},
	})
	if len(edges) != 1 {
		t.Fatalf("an unconfirmed receiver suppressed the same-package tier as well: %+v", edges)
	}
	if edges[0].DstID != 1 || edges[0].Confidence != model.ConfidenceHigh {
		t.Errorf("bound to %d at %s; the same-package definition is still the answer",
			edges[0].DstID, edges[0].Confidence)
	}
}

// --- A5: an import's local name is a declaration, not an inference ----------

// TestImportAliasResolvesAPackageQualifiedReference pins the evidence that was
// parsed, stored and discarded.
//
// `pay.Process()` after `import pay "github.com/satyendrakumarsingh/repocortex/internal/payments"` is not a
// receiver-type guess: the file SAID where `pay` comes from. Without reading
// the alias the reference falls to the type heuristic, which cannot match
// `pay` against anything in payments/, and then to the weak tiers.
func TestImportAliasResolvesAPackageQualifiedReference(t *testing.T) {
	idx := index(
		sym(1, "Process", "internal/payments/run.go:Process", "internal/payments/run.go", 10, true),
		sym(2, "Process", "internal/billing/run.go:Process", "internal/billing/run.go", 11, true),
	)
	r := New(idx, Options{})

	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 50, Path: "cmd/app/main.go", Language: "go",
		Imports: []registry.Import{
			{Path: "github.com/satyendrakumarsingh/repocortex/internal/billing"},
			{Path: "github.com/satyendrakumarsingh/repocortex/internal/payments", Alias: "pay"},
		},
		Symbols: enclose(60, "main"),
		Refs: []registry.Reference{
			{ID: 1, Name: "Process", Kind: "reference.call", Receiver: "pay", Line: 5},
		},
	})
	if len(edges) != 1 {
		t.Fatalf("a package-qualified reference resolved to %d edges, want 1: %+v",
			len(edges), edges)
	}
	if edges[0].DstID != 1 {
		t.Errorf("bound to %d (%q); the file declared `pay` to be internal/payments",
			edges[0].DstID, edges[0].Evidence.Detail)
	}
	if edges[0].Confidence != model.ConfidenceHigh {
		t.Errorf("confidence = %s, want high — an import is a declaration",
			edges[0].Confidence)
	}
}

// TestImportAliasNamingNothingIndexedFallsThrough is the A18 lesson applied to
// A5: an alias for a third-party package matches no candidate, and that is
// absence of evidence, not evidence that the other tiers are wrong.
func TestImportAliasNamingNothingIndexedFallsThrough(t *testing.T) {
	idx := index(sym(1, "Marshal", "internal/pay/codec.go:Marshal", "internal/pay/codec.go", 10, true))
	r := New(idx, Options{})

	edges, _, _ := resolveOne(t, r, registry.FileRefs{
		FileID: 50, Path: "internal/pay/service.go", Language: "go",
		Imports: []registry.Import{{Path: "encoding/json", Alias: "stdjson"}},
		Symbols: enclose(60, "Process"),
		Refs: []registry.Reference{
			{ID: 1, Name: "Marshal", Kind: "reference.call", Receiver: "stdjson", Line: 5},
		},
	})
	// The same-package tier still answers. What must NOT happen is the alias
	// suppressing it because the package it named is not in the index.
	if len(edges) != 1 || edges[0].DstID != 1 {
		t.Fatalf("an alias naming an unindexed package suppressed a later tier: %+v", edges)
	}
}

// TestUnaliasedImportIsNotTreatedAsALocalName keeps A5 to what the parser can
// prove. An unaliased import binds the PACKAGE CLAUSE, which is not the last
// path segment in general, and inferring it would be a guess carrying a
// declaration's confidence.
func TestUnaliasedImportIsNotTreatedAsALocalName(t *testing.T) {
	set := buildImportSet(NewSpecs().For("go"), []registry.Import{{Path: "github.com/satyendrakumarsingh/repocortex/internal/payments"}})
	if len(set.byLocalName) != 0 {
		t.Errorf("an unaliased import claimed the local name %v", set.byLocalName)
	}
}

// TestReceiverMatchesTable covers the predicate directly, in both directions.
//
// The "want false" rows are the ones that carry the change: every one of them
// was accepted by the substring rule.
func TestReceiverMatchesTable(t *testing.T) {
	tests := []struct {
		name     string
		language string
		receiver string
		entry    registry.SymbolIndexEntry
		want     bool
	}{
		{
			name:     "owner is the qualified name's last segment before the member",
			language: "java", receiver: "transactionService",
			entry: sym(1, "updateStatus", "com.pay.TransactionService.updateStatus",
				"svc/Transaction.java", 1, true),
			want: true,
		},
		{
			name:     "receiver already rewritten to its declared type",
			language: "java", receiver: "Builder",
			entry: sym(1, "build", "com.pay.Builder.build", "svc/Whatever.java", 1, true),
			want:  true,
		},
		{
			name:     "nested class separated by a JVM dollar",
			language: "java", receiver: "Entry",
			entry: sym(1, "getKey", "java.util.Map$Entry.getKey", "j/Map.java", 1, true),
			want:  true,
		},
		{
			name:     "SCIP-shaped hash between type and member",
			language: "typescript", receiver: "Client",
			entry: sym(1, "send", "src/net.ts`Client#send", "src/net.ts", 1, true),
			want:  true,
		},
		{
			name:     "double colon where that is the member separator",
			language: "rust", receiver: "Parser",
			entry: sym(1, "parse", "crate::syn::Parser::parse", "src/syn.rs", 1, true),
			want:  true,
		},
		{
			name:     "file named for the receiver, which is how path:name FQNs are owned",
			language: "go", receiver: "Ledger",
			entry: sym(1, "Save", "internal/pay/ledger.go:Save", "internal/pay/ledger.go", 1, true),
			want:  true,
		},
		{
			name:     "receiver is a substring of the owner",
			language: "java", receiver: "transactionService",
			entry: sym(1, "updateStatus", "com.pay.SubTransactionService.updateStatus",
				"svc/SubTransactionService.java", 1, true),
			want: false,
		},
		{
			name:     "receiver occurs in the package, not the owner",
			language: "java", receiver: "order",
			entry: sym(1, "save", "com.order.Ledger.save", "svc/Ledger.java", 1, true),
			want:  false,
		},
		{
			name:     "receiver occurs inside the member name",
			language: "java", receiver: "status",
			entry: sym(1, "updateStatus", "com.pay.Order.updateStatus", "svc/Order.java", 1, true),
			want:  false,
		},
		{
			name:     "receiver is a substring of a path segment",
			language: "go", receiver: "db",
			entry: sym(1, "Save", "internal/store/dbutil.go:Save", "internal/store/dbutil.go", 1, true),
			want:  false,
		},
		{
			name:     "owner separated by something that is not a member separator",
			language: "go", receiver: "internal/pay/ledger.go",
			entry: sym(1, "Save", "internal/pay/ledger.go:Save", "internal/pay/ledger.go", 1, true),
			want:  false,
		},
		{
			name:     "this tells us nothing the same-file tier has not",
			language: "java", receiver: "this",
			entry: sym(1, "save", "com.pay.this.save", "svc/this.java", 1, true),
			want:  false,
		},
		{
			name:     "self likewise",
			language: "python", receiver: "self",
			entry: sym(1, "save", "app.self.save", "app/self.py", 1, true),
			want:  false,
		},
		{
			name:     "empty receiver is not a claim about anything",
			language: "go", receiver: "",
			entry: sym(1, "Save", "internal/pay/ledger.go:Save", "internal/pay/ledger.go", 1, true),
			want:  false,
		},
		{
			name:     "a pointer receiver is still the type it points at",
			language: "go", receiver: "*Ledger",
			entry: sym(1, "Save", "pkg.Ledger.Save", "internal/pay/store.go", 1, true),
			want:  true,
		},
		{
			// The file-name channel is EQUALITY, not containment: OrderService.java
			// is not the file a receiver of `Order` names, even though the old rule
			// took it (and the whole package path with it).
			name:     "file named for a longer type is not the receiver's file",
			language: "java", receiver: "Order",
			entry: sym(1, "save", "com.pay.OrderService.save", "svc/OrderService.java", 1, true),
			want:  false,
		},
		{
			// Its converse: the channel deliberately does not ask the qualified
			// name for an owner, because the FQNs RepoCortex synthesises do not
			// carry one. A candidate in a file named for the receiver matches on
			// that evidence alone.
			name:     "file named for the receiver needs no owner in the qualified name",
			language: "go", receiver: "Order",
			entry: sym(1, "Total", "internal/pay/order.go:Total", "internal/pay/order.go", 1, true),
			want:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := NewSpecs().For(tc.language)
			if got := receiverMatches(spec, tc.receiver, tc.entry); got != tc.want {
				t.Errorf("receiverMatches(%q, %q, %q/%s) = %v, want %v",
					tc.language, tc.receiver, tc.entry.FQN, tc.entry.FilePath, got, tc.want)
			}
		})
	}
}

// TestReceiverValidationIsDeterministic: the tier must reach the same answer on
// every run, since the registry digest is computed over the edges it produces.
func TestReceiverValidationIsDeterministic(t *testing.T) {
	idx := index(
		sym(1, "updateStatus", "com.pay.TransactionService.updateStatus", "svc/A.java", 10, true),
		sym(2, "updateStatus", "com.pay.SubTransactionService.updateStatus", "svc/B.java", 11, true),
		sym(3, "updateStatus", "com.pay.OrderService.updateStatus", "svc/C.java", 12, true),
	)
	fr := registry.FileRefs{
		FileID: 50, Path: "api/Controller.java", Language: "java",
		Symbols: enclose(60, "handle"),
		Refs: []registry.Reference{
			{ID: 1, Name: "updateStatus", Kind: "reference.call",
				Receiver: "transactionService", Line: 5},
		},
	}
	var first int64 = -1
	for i := 0; i < 20; i++ {
		edges, _, _ := resolveOne(t, New(idx, Options{}), fr)
		var got int64
		if len(edges) > 0 {
			got = edges[0].DstID
		}
		if first == -1 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d bound to %d, previously %d", i, got, first)
		}
	}
	if first != 1 {
		t.Errorf("bound to %d, want the TransactionService that owns the method (1)", first)
	}
}

// TestMisInferredReceiverYieldsNoEdge is the safety property A3 rests on, and
// it holds in a WEAKER form than A3 was designed against. Both halves are
// pinned here, because the gap is the whole risk of the feature.
//
// A3 builds a file-local type environment for languages that do not write their
// types everywhere, so a receiver can now arrive as an INFERRED type rather than
// a declared one. The claim that made that look safe was: a type name the
// program never had matches nothing, so a mis-inference produces NO edge.
//
// WHAT ACTUALLY HOLDS: a mis-inferred receiver never produces a HIGH-confidence
// edge. receiverMatches validates rather than contains (A16) — an owner must
// occupy a whole segment of the candidate's qualified name, or be its file stem
// exactly — and tier 2 reports High only when exactly ONE candidate survives.
//
// WHAT DOES NOT HOLD is the stronger "no edge at all", and the case below shows
// why: a receiver of `Store` matches BOTH internal/orders/store.go and
// internal/payments/store.go on the file-stem channel, so tier 2 takes its
// `len(byReceiver) > 1` branch and returns a deterministic PREFERENCE at
// ConfidenceMedium. That is a coin flip wearing a tier, and it is the same
// shape as the failure A19 was reverted for — a wrong narrowing converting
// honest ambiguity into an answer — one tier lower down.
//
// So the residual exposure of A3 is confined to `medium`, and the measurement
// that decides whether A3 ships has to look at `medium` and not only at `high`.
func TestMisInferredReceiverYieldsNoEdge(t *testing.T) {
	// Two same-named candidates in different packages: enough that nothing
	// weaker than the receiver can settle it, so any edge produced here is the
	// receiver tier's doing.
	cands := []registry.SymbolIndexEntry{
		entry(1, "Save", "internal/orders/store.go"),
		entry(2, "Save", "internal/payments/store.go"),
	}
	fr := registry.FileRefs{FileID: 99, Path: "internal/app/service.go", Language: "go"}

	for _, receiver := range []string{
		"OrderStore",  // plausible, and owns nothing here
		"orders",      // names a DIRECTORY, not a file stem or an owner
		"internal",    // a path segment shared by both candidates
		"Sav",         // a prefix of the member name
		"StoreOrders", // the segments of a real path, reordered
		"ordersStore", // the two joined, which a containment test would accept
	} {
		r := New(nil, Options{})
		spec := NewSpecs().For(fr.Language)
		got, ok := r.pick(spec, fr, buildImportSet(spec, nil),
			registry.Reference{Name: "Save", Receiver: receiver}, cands)
		if ok {
			t.Errorf("receiver %q bound to %s (%s, %s); a type this program does not have "+
				"must yield NO edge, or a mis-inference becomes a confident wrong answer",
				receiver, got.entry.FilePath, got.confidence, got.reason)
		}
	}

	// The documented gap, pinned so that closing it is a deliberate change and
	// widening it is a test failure.
	r := New(nil, Options{})
	spec := NewSpecs().For(fr.Language)
	got, ok := r.pick(spec, fr, buildImportSet(spec, nil),
		registry.Reference{Name: "Save", Receiver: "Store"}, cands)
	if !ok {
		t.Skip("the multi-stem-match gap has been closed; tighten this test rather than deleting it")
	}
	if got.confidence == model.ConfidenceHigh {
		t.Errorf("a receiver matching TWO candidates' file stems produced %s (%s); "+
			"the one property A3 may not break is that a mis-inference never reaches high",
			got.confidence, got.reason)
	}
}

// TestCorrectlyInferredReceiverStillBinds is the other half: the property above
// is worthless if it holds by never matching anything at all.
func TestCorrectlyInferredReceiverStillBinds(t *testing.T) {
	cands := []registry.SymbolIndexEntry{
		entry(1, "Save", "internal/orders/orderstore.go"),
		entry(2, "Save", "internal/payments/paymentstore.go"),
	}
	fr := registry.FileRefs{FileID: 99, Path: "internal/app/service.go", Language: "go"}
	r := New(nil, Options{})
	spec := NewSpecs().For(fr.Language)
	got, ok := r.pick(spec, fr, buildImportSet(spec, nil),
		registry.Reference{Name: "Save", Receiver: "OrderStore"}, cands)
	if !ok {
		t.Fatal("a receiver naming the candidate's own file bound nothing")
	}
	if got.entry.FilePath != "internal/orders/orderstore.go" {
		t.Errorf("bound to %s, want internal/orders/orderstore.go", got.entry.FilePath)
	}
}
