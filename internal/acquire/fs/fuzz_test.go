package fs

import (
	"strings"
	"testing"
	"time"
)

// FuzzIgnoreMatcher drives the ignore-rule parser and matcher with arbitrary
// input, because both are reached directly from attacker-controlled bytes: a
// cloned repository's .gitignore and .cortexignore. CONTRIBUTING.md requires
// a fuzz target for anything reading untrusted input.
//
// The properties asserted are the ones a hostile repository could otherwise
// violate: no panic, no unbounded work, and no dependence on how many times a
// rule is evaluated (the walk consults the same ruleSet from several
// goroutines, and a matcher that answered differently on the second call would
// break the determinism gate).
func FuzzIgnoreMatcher(f *testing.F) {
	seeds := []struct{ body, path string }{
		{"*.log\n!keep.log\n", "keep.log"},
		{"build/\n", "build"},
		{"**/gen/**\n", "a/b/gen/x.go"},
		{"a/**/b/**/c/**/d\n", "a/x/b/y/c/z/d"},

		// Shapes chosen to be hostile rather than realistic.
		{"**/**/**/**/**/**/**/**/**/**/x\n", strings.Repeat("a/", 64) + "x"},
		{"*******************\n", strings.Repeat("a", 200)},
		{"[[[[[[[[\n", "[[[[[[[["},
		{"[a-\n", "[a-"},
		{"[!]\n", "x"},
		{"\\\\\\\\\n", "\\\\"},
		{"!\n!!\n!!!\n", "x"},
		{"/\n//\n///\n", "a"},
		{"a\\ \\ \\ \n", "a   "},
		{"\x00\x00\x00\n", "\x00"},
		{"\xff\xfe invalid utf8\n", "\xff\xfe"},
		{strings.Repeat("!a/", 300) + "\n", strings.Repeat("a/", 300) + "b"},
		{"a/" + strings.Repeat("*", 500) + "/b\n", "a/" + strings.Repeat("x", 500) + "/b"},
		{"#comment\n   \n\t\n\r\n", ""},
	}
	for _, s := range seeds {
		f.Add(s.body, s.path)
	}

	f.Fuzz(func(t *testing.T, body, p string) {
		// Bound the corpus so a pathological input is reported as a hang in the
		// matcher rather than as the fuzzer exploring a megabyte of text.
		if len(body) > 8192 || len(p) > 4096 {
			t.Skip()
		}
		// Path components are what the walk actually produces; empty and
		// absolute forms never reach the matcher.
		p = strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
		if p == "" {
			t.Skip()
		}

		for _, base := range []string{"", "src", "a/b"} {
			rs := ruleSet{pats: parseIgnoreFile([]byte(body), base)}

			start := time.Now()
			first := rs.ignored(p, false)
			firstDir := rs.ignored(p, true)
			firstPrune := rs.prunes(p)
			elapsed := time.Since(start)

			// A single match is a table walk over the rules; anything near a
			// second means the matcher backtracked or looped, which is a
			// denial of service reachable from a committed file.
			if elapsed > 2*time.Second {
				t.Fatalf("matching %q against %d rules took %v", p, len(rs.pats), elapsed)
			}

			if got := rs.ignored(p, false); got != first {
				t.Fatalf("ignored(%q, false) changed between calls: %v then %v", p, first, got)
			}
			if got := rs.ignored(p, true); got != firstDir {
				t.Fatalf("ignored(%q, true) changed between calls: %v then %v", p, firstDir, got)
			}
			if got := rs.prunes(p); got != firstPrune {
				t.Fatalf("prunes(%q) changed between calls: %v then %v", p, firstPrune, got)
			}

			// A directory-only rule can never exclude a file, so a path that is
			// ignored as a file must also be ignored as a directory. The
			// converse does not hold.
			if first && !firstDir {
				t.Fatalf("path %q ignored as a file but not as a directory; rules:\n%s", p, body)
			}

			for i := range rs.pats {
				// coversDir is a weakening of match for directories: anything
				// the rule matches as a directory it must also cover. The walk
				// relies on this to prune safely.
				if rs.pats[i].match(p, true) && !rs.pats[i].coversDir(p) {
					t.Fatalf("rule %q matches dir %q but does not cover it", rs.pats[i].raw, p)
				}
				// Scoped rules must never reach outside their own subtree.
				if b := rs.pats[i].base; b != "" && rs.pats[i].match(p, false) {
					if !strings.HasPrefix(p, b+"/") {
						t.Fatalf("rule %q from base %q matched %q outside its subtree", rs.pats[i].raw, b, p)
					}
				}
			}
		}
	})
}

// FuzzCompilePattern narrows the target to a single line, which is where the
// escaping and trailing-whitespace rules live and where a malformed glob would
// otherwise become a silently-matches-nothing rule.
func FuzzCompilePattern(f *testing.F) {
	for _, s := range []string{
		"", "!", "#", "/", "//", "\\", "\\!", "\\#", "!/", "a/", "**", "**/**",
		"a\\ ", "   ", "[", "[]", "[^a]", "?*[", "a**b", "**a**b**",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > 4096 {
			t.Skip()
		}
		p, ok := compilePattern(line, "")
		if !ok {
			return
		}
		if len(p.comps) == 0 {
			t.Fatalf("compilePattern(%q) accepted a rule with no components", line)
		}
		for i, c := range p.comps {
			if strings.Contains(c.s, "/") {
				t.Fatalf("compilePattern(%q): component %d = %q contains a separator", line, i, c.s)
			}
			if c.s == "" {
				t.Fatalf("compilePattern(%q): component %d is empty", line, i)
			}
			// Runs of "**" must have been collapsed, or an adversarial pattern
			// buys a DP row per repetition.
			if i > 0 && c.s == "**" && p.comps[i-1].s == "**" {
				t.Fatalf("compilePattern(%q): adjacent ** at %d", line, i)
			}
		}
		if p.contentsOnly {
			n := len(p.comps)
			if n < 2 || p.comps[n-1].s != "*" || p.comps[n-2].s != "**" {
				t.Fatalf("compilePattern(%q): contentsOnly set but tail is %v", line, p.comps)
			}
			// The point of the synthetic "*": "a/**" must not match "a".
			if base := p.comps[:n-2]; len(base) > 0 {
				if matchComps(p.comps, []string{"a"}) && matchComps(base, []string{"a"}) {
					t.Fatalf("compilePattern(%q): trailing ** matched zero components", line)
				}
			}
		}
	})
}
