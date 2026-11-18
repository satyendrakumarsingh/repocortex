package infer

import (
	"fmt"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

func syntheticReexportGraph(n int) (*reexportGraph, []string) {
	files := make(map[string]registry.FileDirectives, n)
	var imports []string
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("packages/pkg%d/lib/src/m%d", i%50, i)
		files[key] = registry.FileDirectives{Parts: []string{fmt.Sprintf("m%d.g", i)}}
		imports = append(imports,
			fmt.Sprintf("lib/src/m%d", i),             // a suffix of a file's path
			fmt.Sprintf("flutter/src/widgets/w%d", i), // an external package: never matches
		)
	}
	return newReexportGraph(files), imports
}

func TestReexportLookupSuffixes(t *testing.T) {
	g := newReexportGraph(map[string]registry.FileDirectives{
		"a/lib/x":       {Parts: []string{"one"}},
		"b/lib/x":       {Parts: []string{"two"}},
		"a/lib/y":       {Parts: []string{"three"}},
		"exact/path/zz": {},
	})
	if _, ok := g.lookup("lib/x"); ok {
		t.Error("lib/x ends two files and must be no answer")
	}
	if d, ok := g.lookup("lib/y"); !ok || d.Parts[0] != "three" {
		t.Errorf("lib/y = %v %v, want the one file ending with it", d, ok)
	}
	if _, ok := g.lookup("ib/y"); ok {
		t.Error("a suffix must start at a path boundary")
	}
	if _, ok := g.lookup("package:flutter/x"); ok {
		t.Error("an external package matched")
	}
	if _, ok := g.lookup("exact/path/zz"); !ok {
		t.Error("an exact key was not found")
	}
}

// BenchmarkReexportLookup shows lookup cost per import is flat as the number
// of modules grows (it scanned every key per import before, so ns/op grew with
// n): run with -bench ReexportLookup and compare the per-op times.
func BenchmarkReexportLookup(b *testing.B) {
	for _, n := range []int{1000, 3000, 9000, 27000} {
		g, imports := syntheticReexportGraph(n)
		b.Run(fmt.Sprintf("modules=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				g.lookup(imports[i%len(imports)])
			}
		})
	}
}
