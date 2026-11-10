package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBasisValuesAreStable(t *testing.T) {
	cases := map[Basis]string{
		BasisMeasured:  "measured",
		BasisInferred:  "inferred",
		BasisEstimated: "estimated",
	}
	for b, want := range cases {
		if string(b) != want {
			t.Errorf("Basis %v = %q, want %q", b, string(b), want)
		}
	}
}

// The label must survive serialisation — a basis that is dropped at the wire is
// the same as no basis at all.
func TestBasisSurvivesJSON(t *testing.T) {
	resp := Response{Summary: "x", Basis: BasisEstimated}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"basis":"estimated"`) {
		t.Errorf("basis missing from %s", out)
	}
}

// Token counts come from a bytes/4 heuristic that internal/context/pack.go:92
// documents as running low. They may never be labelled measured.
func TestTokenCountsAreNeverLabelledMeasured(t *testing.T) {
	if BasisTokenCount == BasisMeasured {
		t.Fatal("token counts use a bytes/4 estimate; labelling them measured is a false claim")
	}
}

// The label and the estimator must move together. If bytesPerToken is ever
// removed in favour of a real tokenizer, this test fails and forces someone to
// decide what the new basis is, rather than leaving a stale "estimated" behind.
func TestTokenBasisMatchesTheEstimatorInUse(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "context", "pack.go"))
	if err != nil {
		t.Fatalf("read pack.go: %v", err)
	}
	usesHeuristic := strings.Contains(string(src), "bytesPerToken")
	if usesHeuristic && BasisTokenCount != BasisEstimated {
		t.Errorf("pack.go still uses bytesPerToken but BasisTokenCount is %q", BasisTokenCount)
	}
	if !usesHeuristic && BasisTokenCount == BasisEstimated {
		t.Error("pack.go no longer uses bytesPerToken; revisit BasisTokenCount")
	}
}
