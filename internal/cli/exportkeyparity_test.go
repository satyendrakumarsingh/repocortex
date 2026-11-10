package cli

import (
	stdctx "context"
	"encoding/json"
	"os"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// keyParityFixture is ../../fixtures/keyparity/nodekeys.json, the one fixture
// this test and internal/web's metrics_keyparity_test.go both read.
//
// The portal's metricsGraphBySymbol re-implements this function's keying rule
// against the portal's compact index, and nothing in the type system stops the
// two from drifting. The import direction forbids the obvious pin — internal/cli
// imports internal/web (see ui.go), so internal/web cannot call buildExportGraph
// — and internal/web's structuralIndex is unexported, so this package cannot
// build the projection's input either. What both packages CAN share is a file.
// Each test derives its own input from the same rows and asserts the same
// expected keys, so a change to either implementation fails one of the two.
const keyParityFixture = "../../fixtures/keyparity/nodekeys.json"

// keyParitySymbol is one fixture row, in the shape both tests read.
type keyParitySymbol struct {
	ID         int64  `json:"id"`
	FileID     int64  `json:"file_id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	FQN        string `json:"fqn"`
	Line       int    `json:"line"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
}

type keyParityFile struct {
	Note     string              `json:"note"`
	Symbols  []keyParitySymbol   `json:"symbols"`
	Expected map[string][]string `json:"expected"`
}

func loadKeyParityFixture(t *testing.T) keyParityFile {
	t.Helper()
	raw, err := os.ReadFile(keyParityFixture)
	if err != nil {
		t.Fatalf("read %s: %v", keyParityFixture, err)
	}
	var f keyParityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse %s: %v", keyParityFixture, err)
	}
	if len(f.Symbols) == 0 {
		t.Fatalf("%s holds no symbols", keyParityFixture)
	}
	return f
}

// TestExportKeysMatchTheSharedParityFixture pins export's half of the contract.
//
// Export is the source of truth for these keys; this test is what makes that
// claim checkable rather than a doc comment.
func TestExportKeysMatchTheSharedParityFixture(t *testing.T) {
	fx := loadKeyParityFixture(t)
	paths := map[int64]string{}
	symbols := make([]model.Symbol, 0, len(fx.Symbols))
	for _, s := range fx.Symbols {
		paths[s.FileID] = s.Path
		sym := model.Symbol{
			ID:         s.ID,
			Kind:       model.EntityKind(s.Kind),
			Name:       s.Name,
			FQN:        s.FQN,
			FileID:     s.FileID,
			Confidence: model.Confidence(s.Confidence),
		}
		sym.Range.Start.Line = s.Line
		symbols = append(symbols, sym)
	}

	for _, floor := range []model.Confidence{model.ConfidenceLow, model.ConfidenceHigh} {
		g, _, err := buildExportGraph(stdctx.Background(), paths, symbols, nil, floor)
		if err != nil {
			t.Fatalf("floor %s: %v", floor, err)
		}
		want := fx.Expected[string(floor)]
		got := g.Keys()
		if len(want) != len(got) {
			t.Fatalf("floor %s: %d keys, fixture expects %d\n got: %q\nwant: %q",
				floor, len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("floor %s: key %d = %q, fixture expects %q", floor, i, got[i], want[i])
			}
		}
	}
}
