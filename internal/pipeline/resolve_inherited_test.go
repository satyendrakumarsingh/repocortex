package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// javaClass is one type declaration written the way the indexer writes it: a
// class symbol spanning the file, its methods nested inside, and a file named
// for the class — which is what Java guarantees and what the resolver's
// receiver tier reads as ownership.
type javaClass struct {
	path    string
	name    string
	methods []string

	// superName, when set, is the type this class declares it extends. It is
	// written as a REFERENCE, not an edge: the whole point of the deferred pass
	// is that the edge does not exist until resolution has bound this.
	superName string
	superKind string // reference kind, e.g. "reference.extends"
}

// inheritanceFixture builds a run holding a Parent/Child override case.
//
//	com.pay.base.BasePayment      declares settle()
//	com.pay.model.Payment         extends BasePayment, declares nothing
//	com.pay.billing.Invoice       declares settle() too, so the name is not
//	                              unique and pass one cannot bind by elimination
//	com.pay.service.PaymentService calls payment.settle() on a Payment
//
// Every package is its own directory and no file imports another, so none of
// pick()'s tiers can reach the definition: same-file no, receiver-owns no
// (settle is owned by BasePayment, not Payment), same-directory no, imported
// no, unique-in-repository no. The reference is parked, and only the ancestry
// walk can settle it.
func inheritanceFixture(tb testing.TB, dir string) (*registry.Registry, int64) {
	tb.Helper()
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "index.db")})
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		tb.Fatalf("migrate: %v", err)
	}

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "inherited")
	if err != nil {
		tb.Fatalf("begin run: %v", err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		tb.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	classes := []javaClass{
		{path: "src/main/java/com/pay/base/LedgerEntity.java", name: "LedgerEntity",
			methods: []string{"audit"}},
		{path: "src/main/java/com/pay/base/BasePayment.java", name: "BasePayment",
			methods: []string{"settle"}, superName: "LedgerEntity", superKind: "reference.extends"},
		{path: "src/main/java/com/pay/billing/Invoice.java", name: "Invoice",
			methods: []string{"settle", "audit"}},
		{path: "src/main/java/com/pay/model/Payment.java", name: "Payment",
			superName: "BasePayment", superKind: "reference.extends"},
	}

	for _, c := range classes {
		fileID, err := w.AddFile(ctx, &model.File{
			Path: c.path, Language: "java",
			ContentHash: "hash-" + c.name, SizeBytes: 512, ParseStatus: "ok",
		})
		if err != nil {
			tb.Fatalf("add file %s: %v", c.path, err)
		}
		if _, err := w.AddSymbol(ctx, &model.Symbol{
			Kind: model.KindClass, Name: c.name, FQN: model.DefaultFQN(c.path, c.name),
			FileID:     fileID,
			Range:      model.Range{Start: model.Position{Line: 3}, End: model.Position{Line: 40}},
			Exported:   true,
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}); err != nil {
			tb.Fatalf("add class %s: %v", c.name, err)
		}
		for i, m := range c.methods {
			if _, err := w.AddSymbol(ctx, &model.Symbol{
				Kind: model.KindMethod, Name: m,
				FQN:    model.DefaultFQN(c.path, m),
				FileID: fileID,
				Range: model.Range{
					Start: model.Position{Line: i*6 + 5}, End: model.Position{Line: i*6 + 9}},
				Exported:   true,
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
			}); err != nil {
				tb.Fatalf("add method %s.%s: %v", c.name, m, err)
			}
		}
		if c.superName != "" {
			// On the class's own declaration line, so the enclosing symbol is
			// the class — which is what makes the resulting edge an assertion
			// about the TYPE hierarchy rather than about some method.
			if err := w.AddReference(ctx, fileID, registry.Reference{
				Name: c.superName, Kind: c.superKind, Line: 3, Col: 20,
			}); err != nil {
				tb.Fatalf("add inheritance reference for %s: %v", c.name, err)
			}
		}
	}

	// The caller. `payment` is a formal parameter declared as Payment, which
	// the parser rewrites to the declared TYPE before the reference is stored
	// (see varTypeQueries in internal/parse/queries.go), so the receiver here
	// is "Payment" — the premise the whole ancestry walk rests on.
	callerPath := "src/main/java/com/pay/service/PaymentService.java"
	callerID, err := w.AddFile(ctx, &model.File{
		Path: callerPath, Language: "java",
		ContentHash: "hash-caller", SizeBytes: 512, ParseStatus: "ok",
	})
	if err != nil {
		tb.Fatalf("add caller file: %v", err)
	}
	if _, err := w.AddSymbol(ctx, &model.Symbol{
		Kind: model.KindClass, Name: "PaymentService",
		FQN: model.DefaultFQN(callerPath, "PaymentService"), FileID: callerID,
		Range:      model.Range{Start: model.Position{Line: 3}, End: model.Position{Line: 40}},
		Exported:   true,
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}); err != nil {
		tb.Fatalf("add caller class: %v", err)
	}
	if _, err := w.AddSymbol(ctx, &model.Symbol{
		Kind: model.KindMethod, Name: "process",
		FQN: model.DefaultFQN(callerPath, "process"), FileID: callerID,
		Range:      model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 9}},
		Exported:   true,
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}); err != nil {
		tb.Fatalf("add caller method: %v", err)
	}
	if err := w.AddReference(ctx, callerID, registry.Reference{
		Name: "settle", Kind: "reference.call", Receiver: "Payment", Line: 6, Col: 8,
	}); err != nil {
		tb.Fatalf("add call reference: %v", err)
	}

	// Flush, not just Close: Close ROLLS BACK an unflushed batch, and this
	// fixture is far smaller than the writer's batch size, so without this
	// every row would be discarded and the run would be empty.
	if err := w.Flush(ctx); err != nil {
		tb.Fatalf("flush writer: %v", err)
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("close writer: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		tb.Fatalf("commit run: %v", err)
	}
	return reg, run.ID
}

// TestResolveBindsInheritedDispatch is the end-to-end proof that the deferred
// pass is wired up, and that it runs at the only point where it can work.
//
// `payment.settle()` on a Payment, where settle is declared on BasePayment.
// Pass one cannot bind it: at the moment the call is seen, whether Payment IS a
// BasePayment is a question the index cannot yet answer, because the EXTENDS
// edge that answers it is produced by resolving a reference in a different
// file. Resolve must therefore drain the parked queue AFTER the last page and
// AFTER the flush that puts those edges in the table. Both orderings are
// asserted here by consequence: the inherited edge exists, and it names the
// ancestor it was found on.
func TestResolveBindsInheritedDispatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	reg, runID := inheritanceFixture(t, dir)

	stats, err := Resolve(ctx, reg, runID, ResolveOptions{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The counters must describe WHERE the second pass acted without
	// double-counting: one reference parked, one bound through inheritance, and
	// the ambiguity pass one recorded taken back.
	if stats.Deferred != 1 {
		t.Errorf("stats %+v, want exactly one reference parked for the second pass", stats)
	}
	if stats.Inherited != 1 || stats.InheritedAmbiguous != 0 {
		t.Errorf("stats %+v, want one inherited binding and no recorded ambiguity", stats)
	}
	if stats.Ambiguous != 0 {
		t.Errorf("stats %+v, want the parked reference no longer counted ambiguous", stats)
	}

	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}

	// The EXTENDS edge the ancestry walk depends on. Its presence is what makes
	// the ordering claim testable: if the deferred pass ran before this landed,
	// the walk would find no supertypes and produce nothing.
	if !hasEdge(t, reg, runID, edges, model.EdgeExtends, "Payment", "BasePayment") {
		t.Fatalf("no EXTENDS edge Payment -> BasePayment; the fixture never built a hierarchy")
	}

	inherited := findEdge(t, reg, runID, edges, model.EdgeCalls, "process", "settle")
	if inherited == nil {
		t.Fatalf("no CALLS edge process -> settle; the inherited method was never bound")
	}

	// Confidence tier. One inheritance step earns Medium and nothing here may
	// ever earn High: the binding rests on the receiver token naming the type
	// it appears to AND on an EXTENDS edge this same heuristic resolver
	// produced, neither of which is compiler-backed evidence.
	if inherited.Confidence != model.ConfidenceMedium {
		t.Errorf("inherited edge confidence = %q, want medium at one level up", inherited.Confidence)
	}

	// Evidence. An edge whose justification a reader cannot check is the thing
	// this package exists not to emit, so the detail must name the ancestor the
	// definition was found on and how far up it was.
	if !strings.Contains(inherited.Evidence.Detail, "BasePayment") ||
		!strings.Contains(inherited.Evidence.Detail, "1 level") {
		t.Errorf("evidence %q does not name the ancestor and the distance",
			inherited.Evidence.Detail)
	}
	if inherited.Evidence.FilePath != "src/main/java/com/pay/service/PaymentService.java" ||
		inherited.Evidence.Range.Start.Line != 6 {
		t.Errorf("evidence points at %s:%d, want the call site",
			inherited.Evidence.FilePath, inherited.Evidence.Range.Start.Line)
	}
	if inherited.Evidence.Source != model.SourceSourceCode {
		t.Errorf("evidence source = %q, want source code", inherited.Evidence.Source)
	}

	// It bound to the ancestor's definition, not to the unrelated Invoice.settle
	// that made the name ambiguous in the first place.
	dst := symbolByID(t, reg, runID, inherited.DstID)
	if dst.FileID == 0 || !strings.Contains(fileOf(t, reg, runID, dst.FileID), "BasePayment.java") {
		t.Errorf("bound settle in %s, want the definition on BasePayment",
			fileOf(t, reg, runID, dst.FileID))
	}
}

// TestResolveIsDeterministicWithInheritedDispatch is the determinism gate
// applied to the second pass.
//
// The pass walks maps keyed by type name, and a map ranged over instead of a
// slice would produce the same edges in a different ORDER — which reaches the
// registry digest and makes the same tree hash differently on two machines.
// Resolving twice over identical content must agree exactly.
func TestResolveIsDeterministicWithInheritedDispatch(t *testing.T) {
	ctx := context.Background()

	state := func() string {
		dir := t.TempDir()
		reg, runID := inheritanceFixture(t, dir)
		if _, err := Resolve(ctx, reg, runID, ResolveOptions{}); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		edges, err := reg.Edges(ctx, runID)
		if err != nil {
			t.Fatalf("read edges: %v", err)
		}
		var b strings.Builder
		for _, e := range edges {
			// Endpoints by content identity, not by id: ids are allocation
			// artefacts that legitimately differ between two identical runs.
			b.WriteString(fileOf(t, reg, runID, symbolByID(t, reg, runID, e.SrcID).FileID))
			b.WriteString(" -")
			b.WriteString(string(e.Kind))
			b.WriteString("-> ")
			b.WriteString(symbolByID(t, reg, runID, e.DstID).FQN)
			b.WriteString(" ")
			b.WriteString(string(e.Confidence))
			b.WriteString(" ")
			b.WriteString(e.Evidence.Detail)
			b.WriteString("\n")
		}
		return b.String()
	}

	first := state()
	if !strings.Contains(first, "inheritance chain") {
		t.Fatalf("no inherited edge in the resolved state:\n%s", first)
	}
	for i := 0; i < 3; i++ {
		if got := state(); got != first {
			t.Fatalf("run %d differs:\n--- want ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

// --- assertions over the registry's own reads ------------------------------
//
// The registry deliberately exposes no *sql.DB (CLAUDE.md §1/§2), so these go
// through its read queries rather than reaching past them.

func symbolByID(t *testing.T, reg *registry.Registry, runID, id int64) model.Symbol {
	t.Helper()
	syms, err := reg.Symbols(context.Background(), runID)
	if err != nil {
		t.Fatalf("read symbols: %v", err)
	}
	for _, s := range syms {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no symbol %d in run %d", id, runID)
	return model.Symbol{}
}

func fileOf(t *testing.T, reg *registry.Registry, runID, fileID int64) string {
	t.Helper()
	files, err := reg.Files(context.Background(), runID)
	if err != nil {
		t.Fatalf("read files: %v", err)
	}
	for _, f := range files {
		if f.ID == fileID {
			return f.Path
		}
	}
	return ""
}

func findEdge(
	t *testing.T,
	reg *registry.Registry,
	runID int64,
	edges []model.Edge,
	kind model.EdgeKind,
	srcName, dstName string,
) *model.Edge {
	t.Helper()
	for i := range edges {
		e := edges[i]
		if e.Kind != kind {
			continue
		}
		if symbolByID(t, reg, runID, e.SrcID).Name == srcName &&
			symbolByID(t, reg, runID, e.DstID).Name == dstName {
			return &edges[i]
		}
	}
	return nil
}

func hasEdge(
	t *testing.T,
	reg *registry.Registry,
	runID int64,
	edges []model.Edge,
	kind model.EdgeKind,
	srcName, dstName string,
) bool {
	t.Helper()
	return findEdge(t, reg, runID, edges, kind, srcName, dstName) != nil
}
