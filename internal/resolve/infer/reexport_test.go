package infer

import (
	"reflect"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

func TestReexportReach(t *testing.T) {
	files := map[string]registry.FileDirectives{
		"lib/shop":       {Exports: []string{"lib/src/a", "lib/src/b"}},
		"lib/src/a":      {Exports: []string{"lib/src/deep", "lib/shop"}, Parts: []string{"lib/src/a.g"}},
		"lib/src/deep":   {Exports: []string{"lib/src/deeper"}},
		"lib/src/deeper": {Exports: []string{"lib/src/c4"}},
		"lib/src/c4":     {Exports: []string{"lib/src/c5"}},
		"lib/src/c5":     {Exports: []string{"lib/src/c6"}},
		"lib/src/c6":     {Exports: []string{"lib/src/beyond"}},
	}
	g := newReexportGraph(files)

	// Parts and exports, depth first in source order, a cycle (a exports
	// shop) ending at the visited set, and a path the file imports itself
	// never listed as reached.
	got := g.reach([]string{"lib/shop"})
	want := []string{
		"lib/src/a", "lib/src/a.g", "lib/src/deep", "lib/src/deeper", "lib/src/c4", "lib/src/b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reach = %v\nwant    %v", got, want)
	}

	// A chain longer than maxExportDepth stops: c6's export is not followed.
	for _, p := range got {
		if p == "lib/src/beyond" || p == "lib/src/c6" {
			t.Errorf("reached %s past the depth limit", p)
		}
	}

	// A monorepo import is a suffix of the file's path; two files that end
	// the same way are no answer.
	mono := newReexportGraph(map[string]registry.FileDirectives{
		"packages/x/lib/shop": {Exports: []string{"packages/x/lib/src/a"}},
		"packages/y/lib/shop": {Exports: []string{"packages/y/lib/src/a"}},
		"packages/x/lib/only": {Exports: []string{"packages/x/lib/src/o"}},
	})
	if got := mono.reach([]string{"lib/shop"}); len(got) != 0 {
		t.Errorf("an ambiguous suffix reached %v", got)
	}
	if got := mono.reach([]string{"lib/only"}); !reflect.DeepEqual(got, []string{"packages/x/lib/src/o"}) {
		t.Errorf("a unique suffix reached %v", got)
	}
}
