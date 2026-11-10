package model

import "testing"

func TestConfidenceOrdering(t *testing.T) {
	order := []Confidence{ConfidenceLow, ConfidenceMedium, ConfidenceHigh, ConfidenceExact}
	for i := 1; i < len(order); i++ {
		if order[i].Rank() <= order[i-1].Rank() {
			t.Fatalf("%s should outrank %s", order[i], order[i-1])
		}
	}
	if Confidence("bogus").Valid() {
		t.Fatal("unknown confidence must be invalid")
	}
	if Confidence("").Valid() {
		t.Fatal("empty confidence must be invalid — an unset field is a bug, not a default")
	}
}

func TestConfidenceAtLeast(t *testing.T) {
	cases := []struct {
		c, min Confidence
		want   bool
	}{
		{ConfidenceExact, ConfidenceHigh, true},
		{ConfidenceHigh, ConfidenceHigh, true},
		{ConfidenceMedium, ConfidenceHigh, false},
		{ConfidenceLow, ConfidenceMedium, false},
	}
	for _, c := range cases {
		if got := c.c.AtLeast(c.min); got != c.want {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", c.c, c.min, got, c.want)
		}
	}
}

// TestEdgeValidate guards the invariant that keeps the whole product honest:
// nothing may claim compiler-grade certainty without compiler-grade evidence.
func TestEdgeValidate(t *testing.T) {
	good := Edge{
		SrcID: 1, DstID: 2, Kind: EdgeCalls,
		Confidence: ConfidenceExact,
		Evidence:   Evidence{FilePath: "a.go", Source: SourceCompiler},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid edge rejected: %v", err)
	}

	bad := []struct {
		name string
		e    Edge
	}{
		{"missing src", Edge{DstID: 2, Kind: EdgeCalls, Confidence: ConfidenceHigh}},
		{"missing dst", Edge{SrcID: 1, Kind: EdgeCalls, Confidence: ConfidenceHigh}},
		{"missing kind", Edge{SrcID: 1, DstID: 2, Confidence: ConfidenceHigh}},
		{"unset confidence", Edge{SrcID: 1, DstID: 2, Kind: EdgeCalls}},
		{"bogus confidence", Edge{SrcID: 1, DstID: 2, Kind: EdgeCalls, Confidence: "certain"}},
		{
			"exact without compiler evidence",
			Edge{SrcID: 1, DstID: 2, Kind: EdgeCalls, Confidence: ConfidenceExact,
				Evidence: Evidence{FilePath: "a.go", Source: SourceInferred}},
		},
	}
	for _, c := range bad {
		if err := c.e.Validate(); err == nil {
			t.Errorf("%s: expected rejection, got nil", c.name)
		}
	}
}
