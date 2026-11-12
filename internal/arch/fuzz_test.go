package arch

import (
	"strings"
	"testing"
)

// FuzzComponentOf hunts a fabricated owner, a non-deterministic one, and the
// backtracking hang.
//
// Both halves of the input are hostile. The PATTERNS come from the repository's
// own .repocortex.yml, which RepoCortex did not write; the PATHS come from the
// index, which is a listing of files RepoCortex did not write either. That is why
// the matcher is a dynamic program rather than a regex or a recursion: a
// pattern like `**/a/**/a/**/a` costs exponential time under the obvious
// recursive matcher, and from a config file that is a hang rather than a slow
// answer. Go's fuzzing engine reports an input that stops making progress, so
// running this target IS the check that no pattern can buy unbounded work.
//
// The result property is the one a report depends on: ComponentOf either names
// a component that is really in the model, or names nothing. A string that is
// neither would appear in an architecture report as a component nobody
// declared, and every rule about it would be silently inert.
func FuzzComponentOf(f *testing.F) {
	for _, s := range [][4]string{
		{"internal/**", "internal/store/**", "internal/store/**", "internal/store/db.go"},
		{"**", "", "api/**", "api/v1/handler.go"},
		{".git/**", "", "**/*.go", ".git"},
		{"src/*", "", "src/**", "src/a/b.go"},
		{"a/**/b", "", "**/*.java", "a/x/y/z/b"},
		// The pattern shapes a backtracking matcher dies on.
		{"**/a/**/a/**/a", "", "**/**/**/**", "a/a/a/a/a/a/a/a/a/a/a/a/a/a/a/a/a/a/a/a"},
		{"*****************a", "", "?????????????????", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"**/**/**/**/**/**", "**", "**", "a/b/c/d/e/f/g/h"},
		// Degenerate spellings.
		{"", "", "", ""},
		{"/", "//", "///", "//a//"},
		{"./a", "", "a", "./a"},
	} {
		f.Add(s[0], s[1], s[2], s[3])
	}

	f.Fuzz(func(t *testing.T, include, exclude, otherInclude, path string) {
		m := Model{Components: []Component{
			{Name: "alpha", Include: []string{include}, Exclude: []string{exclude}},
			{Name: "beta", Include: []string{otherInclude}},
		}}
		// Validate reads the same patterns and must survive them too; a model
		// that cannot be validated cannot be reported on.
		_ = m.Validate()

		got := m.ComponentOf(path)
		switch got {
		case "", "alpha", "beta":
		default:
			t.Fatalf("ComponentOf(%q) = %q, which is not a component in the model", path, got)
		}
		// Determinism is what makes an architecture report diffable in CI. The
		// answer must not depend on declaration order, on a memo, or on
		// anything else that differs between two calls.
		if again := m.ComponentOf(path); again != got {
			t.Fatalf("ComponentOf(%q) = %q then %q", path, got, again)
		}
		r := NewResolver(m)
		if memoised := r.ComponentOf(path); memoised != got {
			t.Fatalf("Resolver disagrees with Model on %q: %q against %q", path, memoised, got)
		}
		if cached := r.ComponentOf(path); cached != got {
			t.Fatalf("Resolver changed its answer for %q on the second call: %q against %q", path, cached, got)
		}
		// Order must not decide ownership. Swapping the declarations swaps
		// nothing: specificity, then pattern length, then name, and every one
		// of those is a property of the components rather than of the file they
		// were typed into.
		swapped := Model{Components: []Component{m.Components[1], m.Components[0]}}
		if other := swapped.ComponentOf(path); other != got {
			t.Fatalf("declaration order changed the owner of %q: %q against %q", path, got, other)
		}

		// An owned path must have been owned by a pattern that matches it, and
		// an excluded path is never owned by the component excluding it.
		if got == "alpha" && exclude != "" {
			var buf [maxPathSegments]string
			if segs, ok := splitPath(path, buf[:0]); ok && matchSegs(exclude, segs) {
				t.Fatalf("%q is owned by alpha despite matching its exclude %q", path, exclude)
			}
		}
		// Evaluate is total: it answers for every pair of names, including the
		// empty one ComponentOf returns for an unclaimed path.
		for _, from := range []string{"", "alpha", "beta"} {
			for _, to := range []string{"", "alpha", "beta"} {
				v, rule := m.Evaluate(from, to)
				switch v {
				case VerdictAllowed, VerdictForbidden, VerdictNotAllowed:
				default:
					t.Fatalf("verdict %q from %q to %q is not one of the declared constants", v, from, to)
				}
				if v != VerdictAllowed && rule == nil {
					t.Fatalf("verdict %q from %q to %q with no rule to attribute it to", v, from, to)
				}
			}
		}
	})
}

// FuzzMatchSegs hunts a glob that never terminates, and one whose answer
// disagrees with the semantics the package documents.
//
// The `**` rules are where this gets subtle, and one of them shipped wrong
// once: `**` spans ZERO or more segments, so `.git/**` must match `.git`
// itself, and a matcher that requires at least one segment leaves the directory
// it was told to claim unclaimed. The properties below are the ones that hold
// whatever the pattern says.
func FuzzMatchSegs(f *testing.F) {
	for _, s := range [][2]string{
		{"a/**", "a"},
		{"**", "a/b/c"},
		{"**/*.go", "internal/x/y.go"},
		{"a/*/c", "a/b/c"},
		{"a?c", "abc"},
		{"a**b", "axxb"},
		{strings.Repeat("**/", 100) + "a", strings.Repeat("a/", 100) + "a"},
		{"", ""},
		{"/", "/"},
	} {
		f.Add(s[0], s[1])
	}

	f.Fuzz(func(t *testing.T, pattern, path string) {
		var buf [maxPathSegments]string
		segs, ok := splitPath(path, buf[:0])
		if !ok {
			return
		}
		got := matchSegs(pattern, segs)
		if again := matchSegs(pattern, segs); again != got {
			t.Fatalf("matchSegs(%q, %q) = %v then %v", pattern, segs, got, again)
		}
		// `**/**` describes exactly the same set as `**`, which is what makes
		// the collapse in splitPattern safe. Asserted by going the other way:
		// doubling every `**` SEGMENT — the whole segment, not the characters,
		// since `***` is an ordinary segment and not two wildcards — must not
		// change a single answer. If it does, either the collapse is dropping a
		// pattern segment that meant something or the table is mishandling two
		// adjacent spans.
		parts := strings.Split(pattern, "/")
		doubled := make([]string, 0, len(parts)*2)
		for _, p := range parts {
			doubled = append(doubled, p)
			if p == "**" {
				doubled = append(doubled, p)
			}
		}
		expanded := strings.Join(doubled, "/")
		if len(expanded) <= maxPatternBytes && len(pattern) <= maxPatternBytes {
			if other := matchSegs(expanded, segs); other != got {
				t.Fatalf("expanding %q to %q changed the answer for %q: %v against %v",
					pattern, expanded, segs, got, other)
			}
		}
		// `**` spans zero segments too, so prefixing a pattern with it can only
		// ever admit more paths, never fewer. Checked only where the longer
		// pattern still fits the bounds, because past them the answer is "no
		// match" by policy rather than by matching.
		var patBuf [maxPatternSegments]string
		prefixed := "**/" + pattern
		if _, fits := splitPattern(prefixed, patBuf[:0]); got && fits && len(prefixed) <= maxPatternBytes {
			if !matchSegs(prefixed, segs) {
				t.Fatalf("%q matches %q but %q does not", pattern, segs, prefixed)
			}
		}
	})
}
