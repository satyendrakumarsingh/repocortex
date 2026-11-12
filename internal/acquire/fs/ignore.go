package fs

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Why this file exists rather than a dependency:
//
// The obvious candidate, github.com/sabhiram/go-gitignore, was last pushed in
// February 2024 and has 166 stars with 8 open issues — stale by the standard in
// CONTRIBUTING.md ("new dependencies without a clear justification"). More
// importantly it models a single flat pattern list, whereas correct .gitignore
// semantics require every rule to remember the directory of the file it came
// from: `build/` in `src/.gitignore` must not touch `docs/build/`. Bolting that
// on from outside means reimplementing the matcher anyway. Ignore rules are also
// attacker-controlled input — a cloned repository writes them — so this is
// exactly the code we want to own and fuzz. Hence ~250 lines here, no new
// dependency, and CGO_ENABLED=0 stays trivially true.

// maxIgnoreFileBytes caps how much of an ignore file we will read. A hostile
// repository can commit a multi-gigabyte .gitignore; refusing to buffer it is
// cheaper than discovering the limit through an OOM on a build agent.
const maxIgnoreFileBytes = 1 << 20

// ignoreFileNames are the per-directory rule files, in increasing precedence.
// .repocortexignore is read last so a repository can carve out RepoCortex-specific
// exclusions without perturbing what git sees.
var ignoreFileNames = []string{".gitignore", ".repocortexignore"}

// seg is one '/'-delimited component of a compiled pattern.
type seg struct {
	s string
	// lit compares with == instead of path.Match. Set for components with no
	// metacharacters (the overwhelming majority, and much cheaper) and also for
	// malformed ones such as "[abc": git treats an unterminated bracket as a
	// literal, and so must we, or a typo in a .gitignore silently degrades into
	// a rule that matches nothing and indexes what the author meant to hide.
	lit bool
}

// pattern is one compiled ignore rule.
type pattern struct {
	comps   []seg
	negate  bool // leading '!' — re-includes a path excluded by an earlier rule
	dirOnly bool // trailing '/' — matches directories only

	// contentsOnly records that the pattern ended in "/**", i.e. "everything
	// inside". comps then carries a synthetic trailing "*"; see compilePattern.
	contentsOnly bool

	// base is the repo-relative directory of the ignore file this rule came
	// from ("" for the repository root). Rules apply only at or below their own
	// directory; this is what makes nested .gitignore files behave like git
	// rather than like one concatenated list.
	base string

	raw string // original line, for debugging and test failure messages
}

// compilePattern parses one ignore-file line. ok is false for blank lines,
// comments, and lines that carry no rule.
func compilePattern(line, base string) (pattern, bool) {
	line = strings.TrimSuffix(line, "\r")
	line = trimUnescapedTrailingSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}

	p := pattern{base: base, raw: line}
	switch {
	case strings.HasPrefix(line, "!"):
		p.negate = true
		line = line[1:]
	case strings.HasPrefix(line, `\`) && len(line) > 1 && (line[1] == '!' || line[1] == '#'):
		// `\!foo` and `\#foo` are literal names, not a negation and not a
		// comment.
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	// A pattern containing a slash anywhere is anchored to the directory of its
	// ignore file; one without is matched against every path component. We
	// express the second case by prepending "**" so a single matcher handles
	// both shapes.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return pattern{}, false
	}

	parts := strings.Split(line, "/")
	if !anchored {
		parts = append([]string{"**"}, parts...)
	}
	for _, part := range parts {
		switch part {
		case "":
			continue // "a//b" — git collapses empty components
		case "**":
			// Collapse runs of "**". Without this, "**/**/**/**/x" costs a DP
			// row per component for no added expressiveness: a cheap denial of
			// service from a committed file.
			if n := len(p.comps); n > 0 && p.comps[n-1].s == "**" {
				continue
			}
			p.comps = append(p.comps, seg{s: "**"})
		default:
			p.comps = append(p.comps, seg{s: part, lit: literalSeg(part)})
		}
	}
	if len(p.comps) == 0 {
		return pattern{}, false
	}

	// A trailing "**" means "everything inside": git's `a/**` matches `a/b` but
	// not `a` itself. Our "**" spans zero or more components, so append a "*"
	// to force at least one. Doing it here keeps the matcher free of a special
	// case that would otherwise have to be repeated at every call site.
	if n := len(p.comps); p.comps[n-1].s == "**" {
		p.contentsOnly = true
		p.comps = append(p.comps, seg{s: "*"})
	}
	return p, true
}

// literalSeg reports whether a component must be compared byte-for-byte.
func literalSeg(part string) bool {
	if !strings.ContainsAny(part, `*?[\`) {
		return true
	}
	// path.Match reports ErrBadPattern only for input it cannot parse; those
	// degrade to literal comparison rather than to "matches nothing".
	if _, err := path.Match(part, "x"); err != nil {
		return true
	}
	return false
}

// trimUnescapedTrailingSpace drops trailing spaces, which git ignores unless
// they were quoted with a backslash.
func trimUnescapedTrailingSpace(s string) string {
	i := len(s)
	for i > 0 && s[i-1] == ' ' {
		back := 0
		for j := i - 2; j >= 0 && s[j] == '\\'; j-- {
			back++
		}
		if back%2 == 1 {
			break
		}
		i--
	}
	return s[:i]
}

// scope trims rel to the portion this rule governs, reporting false when rel
// lies outside the directory that owns the rule.
func (p *pattern) scope(rel string) (string, bool) {
	if p.base == "" {
		return rel, true
	}
	if !strings.HasPrefix(rel, p.base) || len(rel) <= len(p.base) || rel[len(p.base)] != '/' {
		return "", false
	}
	return rel[len(p.base)+1:], true
}

// match reports whether the rule applies to rel, a repo-relative slash path.
func (p *pattern) match(rel string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	sub, ok := p.scope(rel)
	if !ok || sub == "" {
		return false
	}
	return matchComps(p.comps, strings.Split(sub, "/"))
}

// coversDir reports whether the rule excludes a directory outright — either by
// matching it, or by being of the form "dir/**", which can only ever match
// things beneath it. Recognising the second form is what stops the walk at
// `.git` instead of descending and rejecting every loose object individually.
func (p *pattern) coversDir(rel string) bool {
	if p.match(rel, true) {
		return true
	}
	if !p.contentsOnly {
		return false
	}
	// Drop the trailing "**" and its synthetic "*": what remains names the
	// directory whose contents the rule covers.
	base := p.comps[:len(p.comps)-2]
	if len(base) == 0 {
		return true // a bare "**" covers every directory
	}
	sub, ok := p.scope(rel)
	if !ok || sub == "" {
		return false
	}
	return matchComps(base, strings.Split(sub, "/"))
}

// matchComps matches compiled components against path components, where "**"
// spans zero or more components.
func matchComps(comps []seg, parts []string) bool {
	// Two fast paths that avoid allocating DP rows. Between them they cover
	// every pattern shape that occurs in practice; the general case below is
	// the correctness backstop for forms like "a/**/b/**/c".
	switch {
	case len(comps) == 0:
		return false
	case !hasDoubleStar(comps):
		if len(comps) != len(parts) {
			return false
		}
		// len(comps) == len(parts) was established immediately above; indexing
		// parts by a comps index is therefore in range. Ranging over the
		// shorter slice explicitly makes that legible to a reader and to
		// gosec's bounds analysis.
		if len(comps) != len(parts) {
			return false
		}
		// Iterate parts, not comps, and index comps: gosec tracks the bound on
		// the ranged slice, so ranging the one being indexed is what makes the
		// safety visible to it as well as to a reader.
		for i, part := range parts {
			if !matchSeg(comps[i], part) {
				return false
			}
		}
		return true
	case comps[0].s == "**" && !hasDoubleStar(comps[1:]):
		// A leading "**" followed by fixed-arity components: only one
		// alignment is possible, so no search is needed.
		tail := comps[1:]
		if len(parts) < len(tail) {
			return false
		}
		off := len(parts) - len(tail)
		for i := range tail {
			if !matchSeg(tail[i], parts[off+i]) {
				return false
			}
		}
		return true
	}

	// next[j] reports whether comps[i+1:] matches parts[j:]. The table is
	// O(len(comps)*len(parts)) and never backtracks, so an adversarial pattern
	// costs time linear in its own size rather than exponential.
	n, m := len(comps), len(parts)
	next := make([]bool, m+1)
	cur := make([]bool, m+1)
	next[m] = true
	for i := n - 1; i >= 0; i-- {
		if comps[i].s == "**" {
			cur[m] = next[m]
			for j := m - 1; j >= 0; j-- {
				// Either "**" matches nothing here, or it swallows parts[j].
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

func hasDoubleStar(comps []seg) bool {
	for i := range comps {
		if comps[i].s == "**" {
			return true
		}
	}
	return false
}

func matchSeg(s seg, name string) bool {
	if s.lit {
		return s.s == name
	}
	// path.Match never lets '*' cross '/', which is exactly the per-component
	// semantics we want, and its star handling is iterative rather than
	// recursive so a hostile glob cannot blow the stack or hang.
	ok, err := path.Match(s.s, name)
	return err == nil && ok
}

// ruleSet is an ordered, immutable list of rules. Later rules win, which is
// both git's within-file rule (the last match decides) and its across-file rule
// (a deeper .gitignore overrides a shallower one), given rules are appended in
// root-to-leaf order.
type ruleSet struct {
	pats []pattern
}

// extend returns a ruleSet with extra appended. It always copies: a child
// directory's rules are built while sibling goroutines still read the parent's
// slice, and appending in place would be a data race.
func (r ruleSet) extend(extra []pattern) ruleSet {
	if len(extra) == 0 {
		return r
	}
	out := make([]pattern, 0, len(r.pats)+len(extra))
	out = append(out, r.pats...)
	out = append(out, extra...)
	return ruleSet{pats: out}
}

// ignored reports whether rel is excluded. It scans backwards and stops at the
// first hit, because the last matching rule is the one that decides.
func (r ruleSet) ignored(rel string, isDir bool) bool {
	for i := len(r.pats) - 1; i >= 0; i-- {
		if r.pats[i].match(rel, isDir) {
			return !r.pats[i].negate
		}
	}
	return false
}

// prunes reports whether a directory can be skipped without descending.
//
// This deliberately implements git's documented rule that "it is not possible
// to re-include a file if a parent directory of that file is excluded" — a
// negation below an excluded directory does not buy the subtree a walk. Authors
// who want re-inclusion write "build/*" rather than "build/", and that form is
// handled correctly here because "build" itself then fails to match.
func (r ruleSet) prunes(rel string) bool {
	for i := len(r.pats) - 1; i >= 0; i-- {
		if r.pats[i].coversDir(rel) {
			return !r.pats[i].negate
		}
	}
	return false
}

// parseIgnoreFile compiles every rule in an ignore file that lives in base.
func parseIgnoreFile(data []byte, base string) []pattern {
	var out []pattern
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), maxIgnoreFileBytes)
	for sc.Scan() {
		if p, ok := compilePattern(sc.Text(), base); ok {
			out = append(out, p)
		}
	}
	return out
}

// loadDirRules reads the ignore files in dir and layers them onto parent.
func loadDirRules(parent ruleSet, dir, rel string) ruleSet {
	var extra []pattern
	for _, name := range ignoreFileNames {
		data, err := readCapped(filepath.Join(dir, name), maxIgnoreFileBytes)
		if err != nil {
			// A missing or unreadable ignore file must not abort indexing. The
			// worst outcome is indexing more than the author wanted, which the
			// user can see and correct; failing the whole run is not.
			continue
		}
		extra = append(extra, parseIgnoreFile(data, rel)...)
	}
	return parent.extend(extra)
}

func readCapped(name string, limit int64) ([]byte, error) {
	f, err := os.Open(name) //nolint:gosec // name is composed from a walked directory entry
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}
