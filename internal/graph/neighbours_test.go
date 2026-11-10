package graph

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func neighbourIDs(ns []Neighbour) []int64 {
	out := make([]int64, len(ns))
	for i, n := range ns {
		out[i] = n.SymbolID
	}
	return out
}

func TestCallersAndCallees(t *testing.T) {
	g := build(t, []tedge{
		e(1, 2), e(3, 2), e(2, 4),
		e(5, 2).withKind(model.EdgeImports), // not a call
	})
	ctx := context.Background()

	callers, err := g.Callers(ctx, 2, "")
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	if got := neighbourIDs(callers); !equalIDs(got, []int64{1, 3}) {
		t.Fatalf("callers = %v, want [1 3]: IMPORTS is not a call", got)
	}

	callees, err := g.Callees(ctx, 2, "")
	if err != nil {
		t.Fatalf("callees: %v", err)
	}
	if got := neighbourIDs(callees); !equalIDs(got, []int64{4}) {
		t.Fatalf("callees = %v, want [4]", got)
	}
}

func TestNeighboursCarryEvidence(t *testing.T) {
	g := build(t, []tedge{e(1, 2).withConf(model.ConfidenceExact)})

	ns, err := g.Neighbours(context.Background(), 1, Outgoing, nil, "")
	if err != nil {
		t.Fatalf("neighbours: %v", err)
	}
	if len(ns) != 1 {
		t.Fatalf("got %d neighbours, want 1", len(ns))
	}
	n := ns[0]
	if n.Confidence != model.ConfidenceExact {
		t.Errorf("confidence = %q, want exact", n.Confidence)
	}
	// An exact edge must claim a compiler source; a neighbour that lost its
	// provenance on the way out would be an unverifiable assertion.
	if n.Evidence.Source != model.SourceCompiler {
		t.Errorf("evidence source = %q, want compiler", n.Evidence.Source)
	}
	if n.Evidence.FilePath == "" || n.Evidence.Range.Start.Line == 0 {
		t.Errorf("evidence = %+v, want a real file and line", n.Evidence)
	}
}

func TestNeighboursMinConfidence(t *testing.T) {
	g := build(t, []tedge{
		e(1, 2).withConf(model.ConfidenceHigh),
		e(1, 3).withConf(model.ConfidenceLow),
	})

	ns, err := g.Neighbours(context.Background(), 1, Outgoing, nil, model.ConfidenceHigh)
	if err != nil {
		t.Fatalf("neighbours: %v", err)
	}
	if got := neighbourIDs(ns); !equalIDs(got, []int64{2}) {
		t.Fatalf("neighbours = %v, want [2]", got)
	}
}

func TestNeighboursBothDirections(t *testing.T) {
	g := build(t, []tedge{e(1, 2), e(3, 2)})

	ns, err := g.Neighbours(context.Background(), 2, Both, nil, "")
	if err != nil {
		t.Fatalf("neighbours: %v", err)
	}
	if len(ns) != 2 {
		t.Fatalf("neighbours = %v, want both the outgoing and incoming edge", neighbourIDs(ns))
	}
}

func TestNeighboursNone(t *testing.T) {
	g := build(t, []tedge{e(1, 2)})

	ns, err := g.Neighbours(context.Background(), 2, Outgoing, nil, "")
	if err != nil {
		t.Fatalf("neighbours: %v", err)
	}
	if len(ns) != 0 {
		t.Fatalf("neighbours = %v, want none", neighbourIDs(ns))
	}
}

func TestAllowedConfidences(t *testing.T) {
	cases := []struct {
		min  model.Confidence
		want int
	}{
		{"", 0},         // unset: no filter
		{"nonsense", 0}, // unrecognised: no filter, not an empty allow-list
		{model.ConfidenceLow, 0},
		{model.ConfidenceMedium, 3},
		{model.ConfidenceHigh, 2},
		{model.ConfidenceExact, 1},
	}
	for _, tc := range cases {
		if got := len(allowedConfidences(tc.min)); got != tc.want {
			t.Errorf("allowedConfidences(%q) has %d values, want %d", tc.min, got, tc.want)
		}
	}
}
