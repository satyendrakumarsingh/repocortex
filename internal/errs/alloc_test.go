//go:build !race

package errs

import "testing"

// TestClassificationDoesNotAllocate guards the property the whole design
// rests on: errors cost something to BUILD, but nothing to ASK ABOUT. A retry
// loop or an MCP handler inspecting every error it sees must not add GC
// pressure to the failure path.
//
// This is why classify is a hand-rolled walk rather than repeated errors.As
// calls — errors.As goes through reflect and would show up here.
//
// Excluded under -race, which instruments allocation accounting and makes the
// count unreliable rather than wrong.
func TestClassificationDoesNotAllocate(t *testing.T) {
	classified := deepChain()
	unclassified := unclassifiedChain()

	cases := []struct {
		name string
		fn   func()
	}{
		{"KindOf/classified", func() { sinkKind = KindOf(classified) }},
		{"KindOf/unclassified", func() { sinkKind = KindOf(unclassified) }},
		{"IsRetryable/classified", func() { sinkBool = IsRetryable(classified) }},
		{"IsRetryable/unclassified", func() { sinkBool = IsRetryable(unclassified) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if n := testing.AllocsPerRun(1000, tc.fn); n != 0 {
				t.Fatalf("%s allocated %.1f times per call, want 0", tc.name, n)
			}
		})
	}
}
