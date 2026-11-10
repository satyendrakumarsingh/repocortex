package export

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestJSONRoundTripsDirection(t *testing.T) {
	var buf bytes.Buffer
	e, _ := ForFormat(FormatJSON)
	if err := e.Write(context.Background(), &buf, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var got JSONGraph
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, buf.String())
	}
	if len(got.Edges) != 1 {
		t.Fatalf("len(Edges) = %d, want 1", len(got.Edges))
	}
	if got.Edges[0].Source != "zebra" || got.Edges[0].Target != "alpha" {
		t.Fatalf("direction wrong: %+v", got.Edges[0])
	}
	if !got.Directed {
		t.Error("Directed = false, want true")
	}
}

func TestJSONIsByteStable(t *testing.T) {
	e, _ := ForFormat(FormatJSON)
	var first bytes.Buffer
	if err := e.Write(context.Background(), &first, reversedPairGraph()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for i := 0; i < 20; i++ {
		var again bytes.Buffer
		if err := e.Write(context.Background(), &again, reversedPairGraph()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if again.String() != first.String() {
			t.Fatalf("run %d differs", i)
		}
	}
}

// TestJSONCarriesWeight pins that Edge.Weight rides on the wire, matching the
// parity DOT and GraphML already have with each other: a graph exported three
// ways should not lose a field in exactly one of the three.
func TestJSONCarriesWeight(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: "zebra"}, {Key: "alpha"}},
		[]metrics.Edge{{From: "zebra", To: "alpha", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 7}},
	)
	var buf bytes.Buffer
	e, _ := ForFormat(FormatJSON)
	if err := e.Write(context.Background(), &buf, g); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var got JSONGraph
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, buf.String())
	}
	if len(got.Edges) != 1 || got.Edges[0].Weight != 7 {
		t.Fatalf("weight not carried: %+v", got.Edges)
	}
}

// TestJSONLossyIsEmpty pins the current Lossy() answer: every metrics.Node
// and metrics.Edge field (including Weight) is represented on the wire, so
// the exporter can truthfully claim zero loss.
func TestJSONLossyIsEmpty(t *testing.T) {
	e, _ := ForFormat(FormatJSON)
	if got := e.Lossy(); got != "" {
		t.Fatalf("Lossy() = %q, want empty", got)
	}
}
