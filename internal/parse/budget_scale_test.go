package parse

import (
	"testing"
	"time"
)

// The wall-clock allowance grows with the file: a small file gets seconds, a
// 120 KB one never the full 60 s ceiling, and only the largest reach it.
func TestParseAllowanceScalesWithSize(t *testing.T) {
	c := defaultParserConfig
	if got := c.budget("css", 1<<10); got != budgetBase+budgetPerKiB {
		t.Errorf("1 KiB css: %s", got)
	}
	if got := c.budget("css", 120_000); got >= 10*time.Second {
		t.Errorf("120 KB css is allowed %s; it held a worker 60 s before", got)
	}
	if got := c.budget("c_sharp", 2<<20); got <= c.budget("c_sharp", 1<<20) {
		t.Errorf("allowance does not grow: %s for 2 MiB, %s for 1 MiB", got, c.budget("c_sharp", 1<<20))
	}
	if got := c.budget("c", 100<<20); got != DefaultParseCeiling {
		t.Errorf("a huge file is allowed %s, want the ceiling %s", got, DefaultParseCeiling)
	}
	if got := c.budget("haskell", 20<<10); got >= c.budget("css", 20<<10) {
		t.Errorf("haskell (superlinear) is allowed %s, no less than css %s", got, c.budget("css", 20<<10))
	}
}

// A ceiling the caller set is the allowance for every file, whatever its size:
// below the default because a test wants a short one, above because an
// operator wants every file to have it.
func TestAnExplicitCeilingIsNotScaled(t *testing.T) {
	for _, d := range []time.Duration{time.Millisecond, 5 * time.Minute} {
		c := parserConfig{ceiling: d}
		for _, n := range []int{10, 10 << 10, 5 << 20} {
			if got := c.budget("c", n); got != d {
				t.Errorf("ceiling %s, %d bytes: allowance %s", d, n, got)
			}
		}
	}
}
