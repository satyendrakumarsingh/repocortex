package web

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// keyParityFixture is ../../fixtures/keyparity/nodekeys.json — the same file
// internal/cli/exportkeyparity_test.go reads, and the reason these two
// implementations cannot drift apart silently.
//
// WHY A FILE AND NOT A CALL. The assertion this design rests on is "the portal
// mints the key export mints, for the same symbol, at any floor". The direct
// way to assert that is to run both functions over one input, and the import
// direction forbids it: internal/cli imports internal/web (ui.go), so
// internal/web must not import internal/cli, and buildExportGraph is unexported
// besides. Inverting it is no better — internal/web's structuralIndex, symInfo
// and compactEdge are unexported, so a test living in internal/cli cannot build
// this side's input. Exporting either side purely for a test would widen a
// package's API for a reason its callers do not share. So the two tests share
// the INPUT ROWS and the EXPECTED KEYS instead. Export remains the source of
// truth: the expected lists were produced by buildExportGraph, and the cli test
// re-derives them on every run, so a change there fails there and a change here
// fails here.
const keyParityFixture = "../../fixtures/keyparity/nodekeys.json"

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

func loadKeyParityIndex(t *testing.T) (*structuralIndex, keyParityFile) {
	t.Helper()
	raw, err := os.ReadFile(keyParityFixture)
	if err != nil {
		t.Fatalf("read %s: %v", keyParityFixture, err)
	}
	var fx keyParityFile
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse %s: %v", keyParityFixture, err)
	}
	if len(fx.Symbols) == 0 {
		t.Fatalf("%s holds no symbols", keyParityFixture)
	}
	idx := &structuralIndex{
		files: map[int64]fileInfo{},
		syms:  map[int64]symInfo{},
	}
	for _, s := range fx.Symbols {
		idx.files[s.FileID] = fileInfo{Path: s.Path}
		idx.syms[s.ID] = symInfo{
			Name:       s.Name,
			FQN:        s.FQN,
			Kind:       model.EntityKind(s.Kind),
			FileID:     s.FileID,
			Line:       s.Line,
			Confidence: model.Confidence(s.Confidence),
		}
	}
	return idx, fx
}

// TestMetricsGraphKeysMatchExportAtEveryFloor is the cross-package assertion.
//
// More than one floor, because the floor is where the two implementations
// diverged: applying it to edges only left a below-floor symbol inflating its
// base key's collision count, so at floor=high the portal minted
// "Floor@a/one.go" for a symbol export keys bare as "Floor".
func TestMetricsGraphKeysMatchExportAtEveryFloor(t *testing.T) {
	idx, fx := loadKeyParityIndex(t)
	for _, floor := range []model.Confidence{model.ConfidenceLow, model.ConfidenceHigh} {
		g, lookup, _, err := idx.metricsGraph(context.Background(), LevelSymbol, floor)
		if err != nil {
			t.Fatalf("floor %s: %v", floor, err)
		}
		want := fx.Expected[string(floor)]
		got := g.Keys()
		if len(got) != len(want) {
			t.Fatalf("floor %s: %d keys, export mints %d\n got: %q\nwant: %q",
				floor, len(got), len(want), got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("floor %s: key %d = %q, export mints %q", floor, i, got[i], want[i])
			}
		}
		// The lookup must cover the node set exactly: a key with no node behind
		// it is a row the reader cannot open.
		if len(lookup) != len(want) {
			t.Errorf("floor %s: lookup holds %d nodes for %d keys", floor, len(lookup), len(want))
		}
	}
}
