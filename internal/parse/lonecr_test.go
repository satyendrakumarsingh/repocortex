package parse

import "testing"

// A bare carriage return is a line break (classic Mac files). Without it the
// whole file was one line and a definition after it was not found.
func TestLoneCarriageReturnIsALineBreak(t *testing.T) {
	res := parse(t, "a.py", "def first():\r    pass\rdef cr():\r    pass\r")
	var line int
	for _, s := range res.Symbols {
		if s.Name == "cr" {
			line = s.Range.Start.Line
		}
	}
	if line != 3 {
		t.Errorf("def cr() after a bare CR: line %d, want 3 (symbols: %v)", line, res.Symbols)
	}
	if got := normalizeLoneCR([]byte("a\r\nb\rc")); string(got) != "a\r\nb\nc" {
		t.Errorf("normalizeLoneCR = %q", got)
	}
}
