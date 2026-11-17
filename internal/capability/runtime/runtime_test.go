package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/trace"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

type fakeReg struct {
	files []model.File
	syms  []model.Symbol
}

func (f fakeReg) Files(context.Context, int64) ([]model.File, error)     { return f.files, nil }
func (f fakeReg) Symbols(context.Context, int64) ([]model.Symbol, error) { return f.syms, nil }

func rng(a, b int) model.Range {
	return model.Range{Start: model.Position{Line: a}, End: model.Position{Line: b}}
}

// fixture: two Java classes that both have a charge() method, as in a real
// service where "charge" is the name of half the payment code.
func fixture() fakeReg {
	return fakeReg{
		files: []model.File{
			{ID: 1, Path: "src/main/java/com/shop/OrderService.java"},
			{ID: 2, Path: "src/main/java/com/shop/RefundService.java"},
		},
		syms: []model.Symbol{
			{ID: 10, Kind: model.KindClass, Name: "OrderService", FQN: "src/main/java/com/shop/OrderService.java:OrderService", FileID: 1, Range: rng(1, 40)},
			{ID: 11, Kind: model.KindMethod, Name: "charge", FQN: "src/main/java/com/shop/OrderService.java:charge", FileID: 1, Range: rng(5, 12)},
			{ID: 12, Kind: model.KindMethod, Name: "cancel", FQN: "src/main/java/com/shop/OrderService.java:cancel", FileID: 1, Range: rng(14, 20)},
			{ID: 20, Kind: model.KindClass, Name: "RefundService", FQN: "src/main/java/com/shop/RefundService.java:RefundService", FileID: 2, Range: rng(1, 30)},
			{ID: 21, Kind: model.KindMethod, Name: "charge", FQN: "src/main/java/com/shop/RefundService.java:charge", FileID: 2, Range: rng(5, 12)},
		},
	}
}

func span(name string, attrs map[string]string) trace.Span {
	return trace.Span{TraceID: "t", Name: name, Service: "svc", Attributes: attrs}
}

func run(t *testing.T, spans ...trace.Span) Report {
	t.Helper()
	rep, err := Correlate(context.Background(), fixture(), 1, spans)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestCodeFunctionAndNamespaceJoinTheMethod(t *testing.T) {
	rep := run(t, span("OrderService.charge", map[string]string{
		"code.function": "charge", "code.namespace": "com.shop.OrderService",
	}))
	if len(rep.Exercised) != 1 || rep.Exercised[0].SymbolID != 11 || rep.Exercised[0].Match != MatchCodeAttribute {
		t.Fatalf("want charge in OrderService joined by code-attribute, got %+v unmatched=%v", rep.Exercised, rep.Unmatched)
	}
}

func TestCodeFunctionAloneIsAmbiguousWhenTwoMethodsShareTheName(t *testing.T) {
	rep := run(t, span("charge", map[string]string{"code.function": "charge"}))
	if len(rep.Exercised) != 0 {
		t.Fatalf("must not guess between two charge methods, got %+v", rep.Exercised)
	}
	if len(rep.Unmatched) != 1 || !strings.Contains(rep.Unmatched[0], "ambiguous") {
		t.Fatalf("want an ambiguous report, got %v", rep.Unmatched)
	}
}

func TestCodeFilepathAndLinenoPickTheInnermostSymbol(t *testing.T) {
	rep := run(t, span("work", map[string]string{
		"code.filepath": "/build/agent/src/main/java/com/shop/RefundService.java", "code.lineno": "7",
	}))
	if len(rep.Exercised) != 1 || rep.Exercised[0].SymbolID != 21 {
		t.Fatalf("line 7 of RefundService.java is charge (21), got %+v", rep.Exercised)
	}
}
