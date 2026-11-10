package web

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestFileNodeReportsAPartialParse covers the affirmative branch of
// FileNode.PartialParse, which no real repository could demonstrate.
//
// The run reports only HOW MANY files parsed partially, never which. Without
// this flag a reader is told 69 of them exist and given no way to find one, and
// every symbol count on a screen showing one is a floor the reader cannot
// locate. The false branch renders on any index; the true branch does not,
// because a file whose parse failed usually yields no symbols at all and a file
// with no symbols never reaches the file rung of the graph. So the fixture has
// to construct what the world will not hand over.
func TestFileNodeReportsAPartialParse(t *testing.T) {
	t.Parallel()

	syms := []captest.Sym{
		// Two files in one container. Only the second is damaged, so the test
		// proves the flag DISCRIMINATES rather than merely appearing.
		captest.S(1, "clean", "internal/core/clean.go").Export(),
		captest.S(2, "damaged", "internal/core/damaged.go").Export().PartialParse(),
	}
	edges := []captest.Edge{captest.E(1, 2).WithConf(model.ConfidenceHigh)}

	deps, _ := captest.Build(t, syms, edges, captest.Refs{Resolved: 1})
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	srv := newServer(t, reg, deps, config.Config{ProjectName: "fixture"})

	var body GraphResponse
	rec := get(t, srv, "/api/graph?level=files&container=internal/core", &body)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(body.Files) == 0 {
		t.Fatal("no files were drawn; the fixture did not reach the file rung")
	}

	got := map[string]bool{}
	for _, f := range body.Files {
		got[f.Path] = f.PartialParse
	}

	tests := []struct {
		path string
		want bool
		why  string
	}{
		{
			path: "internal/core/damaged.go",
			want: true,
			why:  "the parser could not read this file completely, and the node must say so",
		},
		{
			path: "internal/core/clean.go",
			want: false,
			why:  "an undamaged file must not be marked, or the flag means nothing",
		},
	}
	for _, tc := range tests {
		v, seen := got[tc.path]
		if !seen {
			t.Errorf("%s was not drawn at all (drawn: %v)", tc.path, got)
			continue
		}
		if v != tc.want {
			t.Errorf("%s partial_parse = %v, want %v — %s", tc.path, v, tc.want, tc.why)
		}
	}
}

// TestSymbolNodeCarriesItsLine guards the other half of the same promise: a node
// a reader cannot open is a node they have to go and find by hand. The line was
// present on model.Symbol and dropped when the structural index was built, so
// every symbol on the graph reported line 0.
func TestSymbolNodeCarriesItsLine(t *testing.T) {
	t.Parallel()

	syms := []captest.Sym{
		captest.S(1, "target", "internal/core/target.go").Export(),
		captest.S(2, "caller", "internal/core/caller.go").Export(),
	}
	edges := []captest.Edge{captest.E(2, 1).WithConf(model.ConfidenceHigh)}

	deps, _ := captest.Build(t, syms, edges, captest.Refs{Resolved: 1})
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T", deps.Registry)
	}
	srv := newServer(t, reg, deps, config.Config{ProjectName: "fixture"})

	var body GraphResponse
	get(t, srv, "/api/graph?level=symbols&container=internal/core", &body)
	if len(body.Nodes) == 0 {
		t.Fatal("no symbol nodes were drawn")
	}
	for _, n := range body.Nodes {
		if n.Line < 0 {
			t.Errorf("%s reported a negative line %d", n.Name, n.Line)
		}
	}
	// The fixture seeds lines from 1 upward, so at least one node must carry a
	// real position. A payload of all-zeroes is the bug this test exists for.
	positioned := 0
	for _, n := range body.Nodes {
		if n.Line > 0 {
			positioned++
		}
	}
	if positioned == 0 {
		t.Errorf("every one of the %d symbol nodes reported line 0; the declaration "+
			"position is being dropped between the store and the wire", len(body.Nodes))
	}
}
