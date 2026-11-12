package fs

import (
	"strings"
	"testing"
)

// buildRules compiles a set of (base, body) ignore files in the order given,
// which is the order the walk would layer them: root first, then deeper.
func buildRules(t *testing.T, files ...[2]string) ruleSet {
	t.Helper()
	var rs ruleSet
	for _, f := range files {
		rs = rs.extend(parseIgnoreFile([]byte(f[1]), f[0]))
	}
	return rs
}

func TestPatternMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		base    string
		path    string
		isDir   bool
		want    bool
	}{
		// Unanchored patterns match at any depth.
		{"unanchored basename", "*.log", "", "app.log", false, true},
		{"unanchored nested", "*.log", "", "a/b/app.log", false, true},
		{"unanchored non-match", "*.log", "", "app.txt", false, false},
		{"literal name any depth", "TODO", "", "src/deep/TODO", false, true},

		// A slash anywhere anchors the pattern to the ignore file's directory.
		{"anchored root only", "/build", "", "build", true, true},
		{"anchored not nested", "/build", "", "src/build", true, false},
		{"anchored via inner slash", "src/gen.go", "", "src/gen.go", false, true},
		{"anchored inner slash not nested", "src/gen.go", "", "a/src/gen.go", false, false},

		// Directory-only patterns.
		{"dir-only matches dir", "build/", "", "build", true, true},
		{"dir-only rejects file", "build/", "", "build", false, false},
		{"dir-only nested dir", "build/", "", "a/b/build", true, true},

		// ** semantics.
		{"double star spans dirs", "a/**/z.go", "", "a/b/c/z.go", false, true},
		{"double star spans zero dirs", "a/**/z.go", "", "a/z.go", false, true},
		{"leading double star", "**/vendor/x.go", "", "q/vendor/x.go", false, true},
		{"trailing double star matches inside", "dist/**", "", "dist/a/b.js", false, true},
		{"trailing double star excludes self", "dist/**", "", "dist", true, false},
		{"trailing double star covers dir", "dist/**", "", "dist", true, false},

		// Single star must not cross a separator.
		{"star does not cross slash", "a/*.go", "", "a/b/c.go", false, false},
		{"star within component", "a/*.go", "", "a/c.go", false, true},

		// Character classes and malformed patterns.
		{"char class", "f[0-9].go", "", "f7.go", false, true},
		{"char class miss", "f[0-9].go", "", "fx.go", false, false},
		{"unterminated bracket is literal", "f[0.go", "", "f[0.go", false, true},

		// Nested ignore files only govern their own subtree.
		{"base scopes rule", "build/", "src", "src/build", true, true},
		{"base excludes siblings", "build/", "src", "docs/build", true, false},
		{"base excludes root", "build/", "src", "build", true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, ok := compilePattern(tc.pattern, tc.base)
			if !ok {
				t.Fatalf("compilePattern(%q) rejected the pattern", tc.pattern)
			}
			if got := p.match(tc.path, tc.isDir); got != tc.want {
				t.Errorf("pattern %q (base %q) match(%q, isDir=%v) = %v, want %v",
					tc.pattern, tc.base, tc.path, tc.isDir, got, tc.want)
			}
		})
	}
}

func TestPatternCompileRejects(t *testing.T) {
	t.Parallel()

	for _, line := range []string{"", "   ", "# a comment", "#", "/", "!", "!/"} {
		if p, ok := compilePattern(line, ""); ok {
			t.Errorf("compilePattern(%q) = %+v, want rejected", line, p)
		}
	}
}

func TestPatternEscapes(t *testing.T) {
	t.Parallel()

	// A leading backslash quotes '!' and '#', and a backslash quotes a trailing
	// space. Getting these wrong turns a rule into a comment or drops a rule.
	if p, ok := compilePattern(`\!important`, ""); !ok || p.negate || !p.match("!important", false) {
		t.Errorf(`compilePattern("\\!important") = %+v, ok=%v; want a literal, non-negating rule`, p, ok)
	}
	if p, ok := compilePattern(`\#notacomment`, ""); !ok || !p.match("#notacomment", false) {
		t.Errorf(`compilePattern("\\#notacomment") = %+v, ok=%v; want a literal rule`, p, ok)
	}
	if got := trimUnescapedTrailingSpace(`keep\ `); got != `keep\ ` {
		t.Errorf("trimUnescapedTrailingSpace kept %q, want the escaped space preserved", got)
	}
	if got := trimUnescapedTrailingSpace("drop   "); got != "drop" {
		t.Errorf("trimUnescapedTrailingSpace(%q) = %q, want %q", "drop   ", got, "drop")
	}
}

func TestRuleSetLastMatchWins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files [][2]string
		path  string
		isDir bool
		want  bool
	}{
		{
			name:  "negation re-includes",
			files: [][2]string{{"", "*.log\n!keep.log\n"}},
			path:  "keep.log",
			want:  false,
		},
		{
			name:  "negation only applies to the named file",
			files: [][2]string{{"", "*.log\n!keep.log\n"}},
			path:  "other.log",
			want:  true,
		},
		{
			name:  "re-negation flips back",
			files: [][2]string{{"", "*.log\n!keep.log\nkeep.log\n"}},
			path:  "keep.log",
			want:  true,
		},
		{
			name:  "deeper file overrides shallower",
			files: [][2]string{{"", "*.md\n"}, {"docs", "!*.md\n"}},
			path:  "docs/readme.md",
			want:  false,
		},
		{
			name:  "shallower still governs elsewhere",
			files: [][2]string{{"", "*.md\n"}, {"docs", "!*.md\n"}},
			path:  "src/readme.md",
			want:  true,
		},
		{
			name:  "repocortexignore layers onto gitignore",
			files: [][2]string{{"", "*.log\n"}, {"", "*.tmp\n"}},
			path:  "x.tmp",
			want:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs := buildRules(t, tc.files...)
			if got := rs.ignored(tc.path, tc.isDir); got != tc.want {
				t.Errorf("ignored(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
			}
		})
	}
}

func TestRuleSetPruning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		dir  string
		want bool
	}{
		// "dir/**" must stop the walk at dir rather than descending and
		// rejecting each entry, which is what makes .git cheap to exclude.
		{"contents glob prunes its directory", "dist/**\n", "dist", true},
		{"dot-git default shape", ".git/**\n", ".git", true},
		{"nested contents glob", "**/testdata/**\n", "a/b/testdata", true},
		{"directory-only prunes", "build/\n", "build", true},
		{"unrelated directory survives", "build/\n", "src", false},

		// "build/*" does NOT prune build, which is exactly why that form is
		// the one that lets a later negation re-include a child.
		{"single star does not prune", "build/*\n", "build", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rs := buildRules(t, [2]string{"", tc.body})
			if got := rs.prunes(tc.dir); got != tc.want {
				t.Errorf("prunes(%q) with rules %q = %v, want %v", tc.dir, tc.body, got, tc.want)
			}
		})
	}
}

func TestRuleSetExtendDoesNotAliasParent(t *testing.T) {
	t.Parallel()

	// Sibling directories extend the same parent concurrently. If extend
	// appended in place, one sibling's rules would leak into the other's — a
	// data race that would also silently mis-ignore files.
	parent := ruleSet{pats: parseIgnoreFile([]byte("*.log\n"), "")}
	a := parent.extend(parseIgnoreFile([]byte("a.txt\n"), "a"))
	b := parent.extend(parseIgnoreFile([]byte("b.txt\n"), "b"))

	if len(parent.pats) != 1 {
		t.Fatalf("parent grew to %d rules; extend must copy", len(parent.pats))
	}
	if a.ignored("b/b.txt", false) {
		t.Error("rules from sibling b leaked into a")
	}
	if !b.ignored("b/b.txt", false) {
		t.Error("b's own rule did not apply")
	}
}

// TestMatchCompsGeneralPathAgreesWithFastPath guards the optimisation in
// matchComps: the two fast paths and the DP must never disagree.
func TestMatchCompsGeneralPathAgreesWithFastPath(t *testing.T) {
	t.Parallel()

	patterns := []string{"a/b", "*/b", "**/b", "a/**/b", "**/a/**/b", "a/**", "**"}
	paths := []string{"a", "b", "a/b", "a/x/b", "x/a/b", "a/x/y/b", "q/a/x/b", ""}

	for _, pat := range patterns {
		p, ok := compilePattern(pat, "")
		if !ok {
			t.Fatalf("compilePattern(%q) rejected", pat)
		}
		for _, s := range paths {
			if s == "" {
				continue
			}
			parts := strings.Split(s, "/")
			fast := matchComps(p.comps, parts)
			slow := matchCompsDP(p.comps, parts)
			if fast != slow {
				t.Errorf("pattern %q path %q: fast=%v dp=%v", pat, s, fast, slow)
			}
		}
	}
}

// matchCompsDP is the general algorithm with the fast paths removed, used only
// as a differential oracle in tests.
func matchCompsDP(comps []seg, parts []string) bool {
	n, m := len(comps), len(parts)
	if n == 0 {
		return false
	}
	next := make([]bool, m+1)
	cur := make([]bool, m+1)
	next[m] = true
	for i := n - 1; i >= 0; i-- {
		if comps[i].s == "**" {
			cur[m] = next[m]
			for j := m - 1; j >= 0; j-- {
				cur[j] = next[j] || cur[j+1]
			}
		} else {
			cur[m] = false
			for j := m - 1; j >= 0; j-- {
				cur[j] = next[j+1] && matchSeg(comps[i], parts[j])
			}
		}
		cur, next = next, cur
	}
	return next[0]
}
