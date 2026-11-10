package mcp

import "testing"

func TestNeverWorseKeepsTheSmallerForm(t *testing.T) {
	full := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	reduced := "aaa"
	if got := NeverWorse(full, reduced); got != reduced {
		t.Errorf("got %q, want the reduced form", got)
	}
}

func TestNeverWorseFallsBackWhenReductionIsLarger(t *testing.T) {
	full := "abc"
	reduced := "abc (truncated: 1 of 400 results shown; narrow the query)"
	if got := NeverWorse(full, reduced); got != full {
		t.Errorf("a reduction larger than the original must be discarded, got %q", got)
	}
}

func TestNeverWorseKeepsFullOnATie(t *testing.T) {
	if got := NeverWorse("abcd", "wxyz"); got != "abcd" {
		t.Errorf("on equal length the full form wins, got %q", got)
	}
}

func TestNeverWorseTreatsEmptyReductionAsFailure(t *testing.T) {
	full := "some real content"
	if got := NeverWorse(full, ""); got != full {
		t.Errorf("an empty reduction is a failed reduction, not an answer of nothing; got %q", got)
	}
}

func TestNeverWorseOnEmptyFullReturnsEmpty(t *testing.T) {
	if got := NeverWorse("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
