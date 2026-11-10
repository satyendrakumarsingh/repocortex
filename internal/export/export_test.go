package export

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// bridgeExportGraph is a five-node, two-cluster graph shared by exporter
// tests that need more than the two-node reversedPairGraph fixture. It
// mirrors metrics' own bridgeGraph shape (two clusters joined by a single
// "bridge" node) without depending on the metrics test package.
func bridgeExportGraph() metrics.Graph {
	e := func(f, t string) metrics.Edge {
		return metrics.Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}
	}
	return metrics.NewGraph(
		[]metrics.Node{{Key: "a1"}, {Key: "a2"}, {Key: "bridge"}, {Key: "b1"}, {Key: "b2"}},
		[]metrics.Edge{e("a1", "a2"), e("a2", "bridge"), e("bridge", "b1"), e("b1", "b2")},
	)
}

func TestForFormatResolvesEveryAdvertisedFormat(t *testing.T) {
	formats := Formats()
	if len(formats) == 0 {
		t.Fatal("Formats() is empty")
	}
	for _, f := range formats {
		e, err := ForFormat(f)
		if err != nil {
			t.Errorf("ForFormat(%q): %v", f, err)
			continue
		}
		if e.Format() != f {
			t.Errorf("ForFormat(%q).Format() = %q", f, e.Format())
		}
	}
}

func TestForFormatRejectsUnknown(t *testing.T) {
	if _, err := ForFormat("parquet"); err == nil {
		t.Fatal("ForFormat(\"parquet\") = nil error, want an error naming the supported formats")
	}
}
