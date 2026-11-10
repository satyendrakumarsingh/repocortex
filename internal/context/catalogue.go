package context

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/search"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// catalogue is the whole run's structure, held in memory for one Assemble call.
//
// Two queries build it — registry.Files and registry.Symbols — and nothing
// downstream issues a per-symbol lookup. That is the same trade internal/resolve
// makes for the same reason: a name-keyed index costs roughly 10 MB per 100,000
// symbols, while asking SQLite once per candidate turns an interactive request
// into thousands of round trips. The graph package's own comment puts the number
// on it: node-at-a-time access is two orders of magnitude slower.
type catalogue struct {
	files   map[int64]model.File
	symbols map[int64]model.Symbol

	// order is symbol ids sorted ascending, so every scan over the catalogue is
	// deterministic without re-sorting at each use.
	order []int64

	// unitOf maps a symbol to the top-level declaration that owns it — the
	// class for a method, the type for a field, itself for a free function.
	//
	// This is computed rather than read from CONTAINS edges because the
	// heuristic indexer does not emit them: resolution produces CALLS edges
	// only. Without it a pack would list `updateStatus` and `save` and never
	// name TransactionService or TransactionRepository, which is precisely what
	// the golden test asks for.
	unitOf map[int64]int64

	// unitsByFile and byName are the two lookups the later stages need. They
	// are derived once here rather than rescanned per query: without them the
	// lexical and table stages each become an O(symbols) scan per hit, which on
	// a 94,000-symbol run is the difference between a millisecond and a second.
	unitsByFile map[int64][]int64
	byName      map[string][]int64

	// nameTokens and stemTokens are the inverted index seed retrieval uses.
	//
	// WHY. findSeeds used to score EVERY symbol in the run against the task's
	// terms, which made the cheapest possible question cost a full pass over the
	// catalogue: measured at 857 ms of a 1.88 s warm assemble on a 25,000-file /
	// 500,000-symbol run — 46% of the request, and growing with the repository
	// rather than with the question. Retrieval is a lookup problem and this is
	// the standard answer to it.
	//
	// nameTokens maps a lowercase token to the symbols whose name yields it:
	// the whole lowered name, plus every identifier part (handleCallback ->
	// "handlecallback", "handle", "callback"). stemTokens maps the crude stem of
	// each of those tokens to the same symbols.
	//
	// Together the two cover every case matchesTerm can score above zero — the
	// word IS the term, the word is the term's stem, or the two share a stem —
	// so the candidate set is EXACTLY the set nameSignal would have scored above
	// zero in a full scan. This is a lookup, not a heuristic: no seed the old
	// scan found can be missed, which is what keeps the golden test honest.
	nameTokens map[string][]int64
	stemTokens map[string][]int64

	// testsBySubject maps the name a test covers to the test units that cover
	// it: CallbackController -> [CallbackControllerTest].
	//
	// Same reason as above. fromTests used to walk every symbol in the run
	// asking "is this a test, and is its subject in the pack?" — path
	// lower-casing and seven substring checks per symbol, 687 ms of the same
	// 1.88 s request. The question it is really asking is keyed by subject, so
	// it is indexed by subject, once per catalogue instead of once per query.
	testsBySubject map[string][]int64

	// modTimeRange is the observed span of file modification times, used to
	// normalise the recency signal.
	minMod, maxMod int64
}

// containerKinds are the symbol kinds that own other symbols. A method belongs
// to its class; nothing belongs to a method, because a nested closure is not a
// unit of context an agent navigates to.
var containerKinds = map[model.EntityKind]bool{
	model.KindClass:     true,
	model.KindInterface: true,
}

func loadCatalogue(ctx context.Context, reg Registry, runID int64) (*catalogue, error) {
	files, err := reg.Files(ctx, runID)
	if err != nil {
		return nil, err
	}
	symbols, err := reg.Symbols(ctx, runID)
	if err != nil {
		return nil, err
	}

	c := &catalogue{
		files:          make(map[int64]model.File, len(files)),
		symbols:        make(map[int64]model.Symbol, len(symbols)),
		order:          make([]int64, 0, len(symbols)),
		unitOf:         make(map[int64]int64, len(symbols)),
		unitsByFile:    make(map[int64][]int64, len(files)),
		byName:         make(map[string][]int64, len(symbols)),
		nameTokens:     make(map[string][]int64, len(symbols)),
		stemTokens:     make(map[string][]int64),
		testsBySubject: make(map[string][]int64),
	}

	for _, f := range files {
		c.files[f.ID] = f
		if c.minMod == 0 || f.ModTimeUnix < c.minMod {
			c.minMod = f.ModTimeUnix
		}
		if f.ModTimeUnix > c.maxMod {
			c.maxMod = f.ModTimeUnix
		}
	}

	// Group by file so enclosure is resolved with one pass per file rather than
	// a scan of every symbol per symbol.
	byFile := make(map[int64][]model.Symbol, len(files))
	for _, s := range symbols {
		c.symbols[s.ID] = s
		c.order = append(c.order, s.ID)
		byFile[s.FileID] = append(byFile[s.FileID], s)
	}
	sort.Slice(c.order, func(i, j int) bool { return c.order[i] < c.order[j] })

	for fileID := range byFile {
		c.resolveEnclosure(byFile[fileID])
	}
	// Built from c.order, which is already sorted, so every index below comes
	// out in a stable order without a second sort.
	for _, id := range c.order {
		s := c.symbols[id]
		lower := strings.ToLower(s.Name)
		c.byName[lower] = append(c.byName[lower], id)
		c.indexTokens(id, s.Name, lower)
		if c.unit(id) != id {
			continue
		}
		c.unitsByFile[s.FileID] = append(c.unitsByFile[s.FileID], id)
		// Enclosure is resolved above, so "is this a unit" and "is this a test"
		// are both answerable here — once — rather than per query.
		if c.isTestUnit(id) {
			if subject := testSubject(s.Name); subject != "" {
				c.testsBySubject[subject] = append(c.testsBySubject[subject], id)
			}
		}
	}
	return c, nil
}

// indexTokens records one symbol under EVERY token its name yields, and under
// the stem of each. lower must be strings.ToLower(name); it is passed in
// because the caller already computed it.
//
// There is deliberately no cap on tokens per symbol. A cap would be the one
// thing that could make this index return fewer seeds than the scan it
// replaces — a generated Python test name splits into a dozen parts, and
// dropping the twelfth would silently cost recall on exactly the identifiers
// that name what a task is about. It is bounded anyway, and by construction:
// the index holds one entry per identifier part, so its size is proportional to
// the symbol-name bytes the catalogue already has in memory.
func (c *catalogue) indexTokens(id int64, name, lower string) {
	// Small slices rather than maps: a name yields a handful of tokens, and a
	// map allocation per symbol is the cost this index exists to avoid.
	seen := make([]string, 0, 8)
	seenStems := make([]string, 0, 8)

	add := func(tok string) {
		if tok == "" || contains(seen, tok) {
			return
		}
		seen = append(seen, tok)
		c.nameTokens[tok] = append(c.nameTokens[tok], id)
		if st := stem(tok); st != "" && st != tok && !contains(seenStems, st) {
			seenStems = append(seenStems, st)
			c.stemTokens[st] = append(c.stemTokens[st], id)
		}
	}

	add(lower)
	parts := search.SplitIdentifier(name)
	for _, part := range parts {
		add(part)
	}
	for _, form := range compoundForms(parts) {
		add(form)
	}
}

// compoundForms returns the ways an identifier's words are written as one word.
//
// SplitIdentifier turns DataSourceHealthIndicator into
// [data source health indicator], and matchesTerm compares whole tokens, so a
// task saying "datasource" matched none of them — not "data", not "source",
// and not the full name. On spring-boot, "add retry to the datasource health
// check" returned zero mentions of DataSourceHealthIndicator while "data
// source" returned five. The class is indexed, obviously relevant, and was
// unreachable by the phrasing every Spring user writes, because the framework
// itself spells it `spring.datasource.url`.
//
// Adjacent PAIRS plus the whole name, not every contiguous run. Pairs are how
// people actually compound — datasource, healthcheck, classloader, webserver —
// and they cost one extra token per word rather than the quadratic number of
// runs, which matters because this index is held in memory for the whole
// catalogue. A three-of-four run like "sourcehealthindicator" is not reachable;
// that is a deliberate limit, not an oversight.
//
// Callers MUST agree on this set. It is used by the scorer to decide whether a
// name matches and by the token index to decide which names to offer, and a
// disagreement between those two silently drops candidates.
func compoundForms(parts []string) []string {
	if len(parts) < 2 {
		return nil
	}
	out := make([]string, 0, len(parts))
	for i := 0; i+1 < len(parts); i++ {
		out = append(out, parts[i]+parts[i+1])
	}
	if len(parts) > 2 {
		out = append(out, strings.Join(parts, ""))
	}
	return out
}

// seedCandidates returns, in ascending id order, every symbol whose name can
// score above zero against terms.
//
// It is a filter, not a ranker: the caller still scores each candidate with
// nameSignal, which remains the only place a name is turned into a number. The
// index only decides which symbols are worth asking about.
func (c *catalogue) seedCandidates(terms []Term) []int64 {
	seen := make(map[int64]struct{}, 64)
	var out []int64

	take := func(ids []int64) {
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	for _, t := range terms {
		// matchesTerm scores above zero in exactly three cases, and each one is
		// a lookup here: word == t.Text, word == t.Stem, stem(word) == either.
		take(c.nameTokens[t.Text])
		take(c.stemTokens[t.Text])
		if t.Stem != "" {
			take(c.nameTokens[t.Stem])
			take(c.stemTokens[t.Stem])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// resolveEnclosure assigns every symbol in one file to its owning unit.
//
// The owner is the SMALLEST containing declaration, so an inner class wins over
// the outer one. Ties are impossible in practice and broken by id if they occur,
// which keeps the result independent of scan order.
func (c *catalogue) resolveEnclosure(syms []model.Symbol) {
	var containers []model.Symbol
	for _, s := range syms {
		if containerKinds[s.Kind] {
			containers = append(containers, s)
		}
	}
	sort.Slice(containers, func(i, j int) bool { return containers[i].ID < containers[j].ID })

	for _, s := range syms {
		owner := s.ID
		best := -1
		for _, ct := range containers {
			if ct.ID == s.ID {
				continue
			}
			if ct.Range.Start.Line > s.Range.Start.Line || ct.Range.End.Line < s.Range.End.Line {
				continue
			}
			span := ct.Range.End.Line - ct.Range.Start.Line
			if best < 0 || span < best {
				best, owner = span, ct.ID
			}
		}
		c.unitOf[s.ID] = owner
	}
}

// unit returns the owning declaration's id, falling back to the symbol itself.
func (c *catalogue) unit(symbolID int64) int64 {
	if u, ok := c.unitOf[symbolID]; ok {
		return u
	}
	return symbolID
}

func (c *catalogue) path(symbolID int64) string {
	return c.files[c.symbols[symbolID].FileID].Path
}

// isTestUnit reports whether a symbol looks like test code.
//
// Path convention first, then name convention. Both are heuristics, and they
// are labelled as such wherever they reach the output — nothing in the index
// marks a file as a test, because no language backend has claimed that
// capability yet.
func (c *catalogue) isTestUnit(symbolID int64) bool {
	p := strings.ToLower(c.path(symbolID))
	if strings.Contains(p, "/test/") || strings.Contains(p, "/tests/") ||
		strings.HasPrefix(p, "test/") || strings.HasPrefix(p, "tests/") ||
		strings.Contains(p, "_test.") || strings.Contains(p, ".test.") ||
		strings.Contains(p, ".spec.") {
		return true
	}
	name := c.symbols[symbolID].Name
	return isTestName(name)
}

// isTestName recognises the near-universal naming conventions for a test type.
func isTestName(name string) bool {
	for _, suffix := range []string{"Test", "Tests", "Spec", "_test", "IT"} {
		if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			return true
		}
	}
	return strings.HasPrefix(name, "Test") && len(name) > 4
}

// testSubject strips a test naming convention to recover the name of the thing
// under test: CallbackControllerTest -> CallbackController.
//
// This is how tests reach a pack at all on the heuristic path. The fixture's
// CallbackControllerTest calls controller.handleCallback through a field the
// parser never sees declared, so resolution binds nothing and the call graph
// contains no edge at all between the tests and the code they cover. Naming
// convention is the only evidence available, and the resulting items say so.
func testSubject(name string) string {
	for _, suffix := range []string{"Tests", "Test", "Spec", "_test", "IT"} {
		if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	if strings.HasPrefix(name, "Test") && len(name) > 4 {
		return name[4:]
	}
	return ""
}

// nameSignal scores a symbol name against the task's terms, in [0,1].
//
// A whole-name match is worth more than a part match, because "the task named
// this symbol" and "the task named a word this symbol contains" are different
// strengths of evidence. Saturating at nameSaturation keeps a symbol that
// happens to contain three task words from dwarfing every other signal.
func nameSignal(name string, terms []Term) (score float64, matched []string) {
	const partDiscount = 0.7
	const nameSaturation = 1.5

	lower := strings.ToLower(name)
	parts := search.SplitIdentifier(name)

	var raw float64
	for _, t := range terms {
		q := matchesTerm(lower, t)
		if q == 0 {
			for _, p := range parts {
				if v := partDiscount * matchesTerm(p, t); v > q {
					q = v
				}
			}
		}
		// A term naming several of the identifier's words run together —
		// "datasource" for DataSource — is at least as specific as one of them
		// alone, so it scores at the same discount rather than a deeper one.
		// See compoundForms: the token index offers exactly this set, and the
		// two must not disagree.
		if q == 0 {
			for _, form := range compoundForms(parts) {
				if v := partDiscount * matchesTerm(form, t); v > q {
					q = v
				}
			}
		}
		if q > 0 {
			raw += t.Weight * q
			matched = append(matched, t.Text)
		}
	}
	if raw <= 0 {
		return 0, nil
	}
	score = raw / nameSaturation
	if score > 1 {
		score = 1
	}
	sort.Strings(matched)
	return score, matched
}

// packagePrefix is the leading path segments used for module affinity.
//
// Three segments is a compromise across layouts: it distinguishes
// src/main/java/com/pay/service from src/main/java/com/pay/controller only
// through the shared-prefix length, which is what samePackage measures.
func packagePrefix(p string) string { return path.Dir(p) }

// affinity scores how close two paths are, in [0,1]: same directory is 1, a
// shared parent decays with how much of the path they agree on.
func affinity(a, b string) float64 {
	if a == b {
		return 1
	}
	da, db := packagePrefix(a), packagePrefix(b)
	if da == db {
		return 1
	}
	sa, sb := strings.Split(da, "/"), strings.Split(db, "/")
	n := 0
	for n < len(sa) && n < len(sb) && sa[n] == sb[n] {
		n++
	}
	if n == 0 {
		return 0
	}
	longer := len(sa)
	if len(sb) > longer {
		longer = len(sb)
	}
	return float64(n) / float64(longer)
}
