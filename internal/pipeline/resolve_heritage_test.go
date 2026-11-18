package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestOneLineClassHeritage checks that a supertype written in the header of a
// class whose methods share the line starts at the class and not at the last
// method on the line. Heritage is attributed by position, not by line alone.
func TestOneLineClassHeritage(t *testing.T) {
	cases := map[string]map[string]string{
		"java":       {"A.java": "class B {}\nclass A extends B { void m() {} void k() {} }\n"},
		"c_sharp":    {"A.cs": "class B {}\nclass A : B { void M() {} void K() {} }\n"},
		"typescript": {"a.ts": "class B {}\nclass A extends B { m() {} k() {} }\n"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			got := langEdges(t, files)
			var file string
			for f := range files {
				file = f
			}
			found := false
			for _, e := range got {
				if e.Kind != model.EdgeExtends {
					continue
				}
				found = true
				if e.Src != file+":A" || e.Dst != file+":B" {
					t.Errorf("extends edge %s -> %s (%s), want %s:A -> %s:B", e.Src, e.Dst, e.Confidence, file, file)
				}
			}
			if !found {
				t.Errorf("no extends edge")
			}
		})
	}
}
