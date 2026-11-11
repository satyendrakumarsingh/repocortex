package scip

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestToolDetailNamesTheIndexer: the detail string is the provenance a reader
// sees on every compiler-proven edge, so it must name the indexer and say that
// the fact came from SCIP.
func TestToolDetailNamesTheIndexer(t *testing.T) {
	cases := []struct {
		tool Tool
		want string
	}{
		{Tool{Name: "scip-go", Version: "v0.3.1"}, "scip: scip-go v0.3.1"},
		{Tool{Name: "scip-go"}, "scip: scip-go"},
		{Tool{}, "scip: unknown indexer"},
	}
	for _, c := range cases {
		got := c.tool.Detail()
		if got != c.want {
			t.Errorf("Detail(%+v) = %q, want %q", c.tool, got, c.want)
		}
		if !strings.HasPrefix(got, "scip: ") {
			t.Errorf("Detail(%+v) = %q, must carry the scip: prefix", c.tool, got)
		}
	}
}

// TestRelationKeyIdentifiesOneRelationship: two facts about the same pair with
// the same kind are the same relationship, whatever their evidence, and a
// different kind or endpoint is a different one.
func TestRelationKeyIdentifiesOneRelationship(t *testing.T) {
	var a Relation
	a.SrcFQN, a.DstFQN, a.Kind = "pkg.A", "pkg.B", model.EdgeCalls
	sameWithOtherEvidence := a
	sameWithOtherEvidence.Evidence.Detail = "a different occurrence"

	if keyOf(a) != keyOf(sameWithOtherEvidence) {
		t.Error("the same relationship with different evidence produced different keys")
	}
	if keyOf(a) == keyOf(Relation{SrcFQN: "pkg.A", DstFQN: "pkg.B", Kind: model.EdgeReads}) {
		t.Error("a different kind between the same symbols produced the same key")
	}
	if keyOf(a) == keyOf(Relation{SrcFQN: "pkg.A", DstFQN: "pkg.C", Kind: model.EdgeCalls}) {
		t.Error("a different target produced the same key")
	}
	if got := keyOf(a); got != (Key{SrcFQN: "pkg.A", DstFQN: "pkg.B", Kind: model.EdgeCalls}) {
		t.Errorf("Key() = %+v, want the fields copied through", got)
	}
}

// keyOf is the lookup a caller uses to deduplicate relations.
func keyOf(r Relation) Key { return r.Key() }
