package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func sym(name string, line, col int) model.Symbol {
	return model.Symbol{
		Kind: model.KindFunction, Name: name,
		Range: model.Range{
			Start: model.Position{Line: line, Col: col},
			End:   model.Position{Line: line + 1},
		},
	}
}

// TestCollapseDuplicateDeclarations pins the fix for a total index failure.
//
// The registry treats (index run, fqn, file, start line) as one symbol. A tags
// query can legitimately emit two declarations agreeing on all four, differing
// only in column, which the constraint does not include. From a real
// TypeScript test file:
//
//	async function json<T>(res: { ...; json(): Promise<any>; ... }, what: string)
//
// Line 152 declares a function `json` and, inside its own parameter type, a
// method signature `json`. Before this, the second insert failed the UNIQUE
// constraint and took the whole run with it: `repocortex index` exited 1 at 80%
// of an 8,094-file repository and committed nothing.
func TestCollapseDuplicateDeclarations(t *testing.T) {
	t.Run("the real case: same name and line, different column", func(t *testing.T) {
		in := []model.Symbol{sym("json", 152, 15), sym("json", 152, 64)}
		resolveFQNs("a.ts", in)

		out, dropped := collapseDuplicateDeclarations(in)
		if dropped != 1 {
			t.Errorf("dropped = %d, want 1", dropped)
		}
		if len(out) != 1 {
			t.Fatalf("kept %d symbols, want 1", len(out))
		}
		// The outermost declaration comes first in tags order and is the one a
		// user searching the name means.
		if out[0].Range.Start.Col != 15 {
			t.Errorf("kept the symbol at column %d, want the first (15)", out[0].Range.Start.Col)
		}
	})

	t.Run("same name on different lines is not a duplicate", func(t *testing.T) {
		in := []model.Symbol{sym("handler", 10, 1), sym("handler", 40, 1)}
		resolveFQNs("a.ts", in)
		out, dropped := collapseDuplicateDeclarations(in)
		if dropped != 0 || len(out) != 2 {
			t.Errorf("dropped %d, kept %d; two declarations on different lines are "+
				"distinct and the schema allows both", dropped, len(out))
		}
	})

	t.Run("different names on one line are not duplicates", func(t *testing.T) {
		in := []model.Symbol{sym("a", 5, 1), sym("b", 5, 20)}
		resolveFQNs("a.ts", in)
		out, dropped := collapseDuplicateDeclarations(in)
		if dropped != 0 || len(out) != 2 {
			t.Errorf("dropped %d, kept %d; want both", dropped, len(out))
		}
	})

	t.Run("an explicit FQN is respected, not overwritten", func(t *testing.T) {
		in := []model.Symbol{
			{Kind: model.KindMethod, Name: "json", FQN: "a.ts#Outer.json",
				Range: model.Range{Start: model.Position{Line: 152}}},
			sym("json", 152, 64),
		}
		resolveFQNs("a.ts", in)
		if in[0].FQN != "a.ts#Outer.json" {
			t.Fatalf("resolveFQNs overwrote an explicit FQN: %q", in[0].FQN)
		}
		out, dropped := collapseDuplicateDeclarations(in)
		if dropped != 0 || len(out) != 2 {
			t.Errorf("dropped %d, kept %d; distinct FQNs are distinct symbols", dropped, len(out))
		}
	})

	t.Run("nothing to do", func(t *testing.T) {
		if out, dropped := collapseDuplicateDeclarations(nil); out != nil || dropped != 0 {
			t.Errorf("nil input produced %v, %d", out, dropped)
		}
	})
}
