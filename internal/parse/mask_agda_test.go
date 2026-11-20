package parse

import (
	"strings"
	"testing"
)

func TestAgdaMaskKeepsOffsetsAndLeavesShortFormsAlone(t *testing.T) {
	short := []byte("module M where\n\nnot : Bool -> Bool\nnot b = if b\n\ndouble : Nat -> Nat\ndouble n = suc (suc n)\n")
	if spans := findAgdaConstructs(short); len(spans) != 0 {
		t.Errorf("a file in the short form must not be masked, got %d spans", len(spans))
	}

	src := []byte(`module M where

data Vec (A : Set) : Nat → Set where
  nil : Vec A zero -- Vec A zero

double : Nat → Nat
double n = n +++ n -- a comment
triple n = go n
  where
    go : Nat → Nat
    go m = m

short : Nat
short = suc zero
`)
	spans := findAgdaConstructs(src)
	masked := applyMaskSpans(src, spans)
	if len(masked) != len(src) {
		t.Fatalf("length changed: %d -> %d", len(src), len(masked))
	}
	if strings.Count(string(masked), "\n") != strings.Count(string(src), "\n") {
		t.Error("a newline was removed or added")
	}
	for _, want := range []string{"data Vec", ": Nat → Set where", "nil :", "double n =", "triple n =", "short = suc zero"} {
		if !strings.Contains(string(masked), want) {
			t.Errorf("masked text lost %q:\n%s", want, masked)
		}
	}
	for _, gone := range []string{"(A : Set)", "+++", "go :", "go m"} {
		if strings.Contains(string(masked), gone) {
			t.Errorf("masked text still has %q:\n%s", gone, masked)
		}
	}
}
