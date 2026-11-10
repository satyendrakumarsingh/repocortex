package parse

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// DefaultParseTimeout bounds a single file's parse.
//
// Not theoretical: a file of 10,000 unbalanced braces took ~2s in testing.
// Pathological inputs exist in real repositories (minified bundles, generated
// code, fuzzer corpora) and one of them must not stall an indexing run.
const DefaultParseTimeout = 5 * time.Second

// PoolReturnLimit is the largest source a Tagger may have parsed and still be
// returned to the pool. See tagWithGuards for why.
const PoolReturnLimit = 128 << 10

// MaxSourceBytes caps what is handed to a parser.
//
// 256 KiB, chosen from measurement rather than intuition. Peak heap indexing a
// 2,110-file Go corpus, varying only this cap:
//
//	 64 KiB   1807 files    962 MB
//	256 KiB   2088 files  2,260 MB
//	  2 MiB   2110 files  6,667 MB
//
// Twenty-two files cost 4.4 GB. Tree-sitter's node arenas scale super-linearly
// with input, and that cost is internal to the library — it cannot be bounded
// from outside, only avoided.
//
// 256 KiB is chosen because it is far above any hand-written source file and
// below essentially all generated code. A quarter-megabyte source file is a
// generated parser table, a minified bundle, or embedded data — none of which
// yield symbols worth 200 MB of arena. Such files are still recorded, with
// StatusSkipped and a reason, so the gap is visible rather than silent.
//
// Raise it deliberately if you must index generated code, and expect the memory
// cost above.
const MaxSourceBytes = 256 << 10

// genericParser extracts symbols for any of the 206 registered grammars using
// tree-sitter tags queries.
//
// This is the Tier-1 path: it yields definitions, references and positions for
// every supported language with no per-language Go code. Tier-2 resolution
// (binding a reference to a definition) lives in the resolve layer.
type genericParser struct {
	entry     grammars.LangEntry
	lang      *ts.Language
	tagsQuery string

	// importQuery is compiled once and shared. gotreesitter documents Query as
	// safe for concurrent Execute (only cursors are per-goroutine), so unlike
	// Tagger it does not need pooling. Nil when the language has no query.
	importQuery *ts.Query

	// receiverQuery captures the object a method is called on, which the tags
	// query does not report. Nil when the language has none; the resolver then
	// loses its receiver tier for that language rather than any correctness.
	receiverQuery *ts.Query

	// varTypeQuery maps a declared variable to its type, so a receiver can be
	// reported as the TYPE it refers to rather than the identifier that names
	// it. Nil when the language has none or is not explicitly typed.
	varTypeQuery *ts.Query

	// paramCountQuery and argCountQuery carry A4's two halves: how many
	// parameters a definition declares, and how many arguments a call passes.
	// Nil when the language has no query — the resolver then has no arity
	// evidence for it and ranks exactly as it did before, which is the whole
	// point of making this additive.
	paramCountQuery *ts.Query
	argCountQuery   *ts.Query

	// arity says how to turn this language's list nodes into a count. Zero
	// value with hasArity false when the language has none; the two queries
	// above are then never run, because a captured list nobody knows how to
	// count is not evidence.
	arity    arityDialect
	hasArity bool

	// Taggers are pooled because gotreesitter's Parser is documented unsafe for
	// concurrent use and constructing one recompiles the tags query.
	//
	// A channel, not sync.Pool, because this pool must be DRAINABLE. Each
	// Tagger owns a Parser whose node arena grows to fit the largest input it
	// has seen and never shrinks, and profiling a 10,896-file run showed 3 GB
	// of live arenas held by pooled taggers alone. sync.Pool offers no way to
	// evict on demand — only the GC decides, and it will not, because the pool
	// holds strong references. Draining is the only way to give that memory
	// back mid-run.
	pool chan *ts.Tagger

	// initErr is recorded rather than returned at construction so a single
	// broken grammar degrades that language instead of failing the run.
	initErr error
}

// ParserOption configures a parser.
type ParserOption func(*parserConfig)

type parserConfig struct {
	timeout time.Duration
}

// WithTimeout overrides the per-file parse timeout.
func WithTimeout(d time.Duration) ParserOption {
	return func(c *parserConfig) { c.timeout = d }
}

var defaultParserConfig = parserConfig{timeout: DefaultParseTimeout}

// registry implements Registry over the embedded grammar set.
type registry struct {
	cfg parserConfig

	mu      sync.RWMutex
	parsers map[string]*genericParser
}

// NewRegistry builds a Registry covering every embedded grammar.
//
// Grammars are loaded lazily: decompressing 206 parse tables up front would
// cost far more memory and startup time than a typical repository needs, since
// most repos use a handful of languages.
func NewRegistry(opts ...ParserOption) Registry {
	cfg := defaultParserConfig
	for _, o := range opts {
		o(&cfg)
	}
	return &registry{cfg: cfg, parsers: make(map[string]*genericParser)}
}

func (r *registry) ForFile(path string) Parser {
	entry := grammars.DetectLanguage(path)
	if entry == nil {
		return nil
	}

	// A single-file component is an envelope around a script written in
	// another language; its own grammar leaves that script opaque. Route it to
	// the parser for what the script actually is — see sfc.go.
	if isSFC(entry.Name) {
		return &sfcParser{language: entry.Name, registry: r}
	}

	r.mu.RLock()
	p, ok := r.parsers[entry.Name]
	r.mu.RUnlock()
	if ok {
		return p
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.parsers[entry.Name]; ok {
		return p
	}
	p = newGenericParser(*entry)
	r.parsers[entry.Name] = p
	return p
}

// isSFC reports whether a language is a single-file-component envelope whose
// declarations live in an embedded script block.
func isSFC(language string) bool {
	return language == "vue" || language == "svelte"
}

func (r *registry) Languages() []string {
	all := grammars.AllLanguages()
	out := make([]string, 0, len(all))
	for _, e := range all {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func newGenericParser(entry grammars.LangEntry) *genericParser {
	p := &genericParser{entry: entry}

	lang := entry.Language()
	if lang == nil {
		p.initErr = fmt.Errorf("grammar %q failed to load", entry.Name)
		return p
	}
	p.lang = lang

	// Inferred query first, then RepoCortex's supplement. Additive by design:
	// a future release that improves inference cannot regress us, and a
	// duplicate capture is handled by de-duplication downstream.
	query := grammars.ResolveTagsQuery(entry)
	if sup := Supplement(entry.Name); sup != "" {
		query = query + "\n" + sup
	}
	if strings.TrimSpace(query) == "" {
		p.initErr = fmt.Errorf("no tags query available for %q", entry.Name)
		return p
	}

	// Supertype references ride along in the same tags query, because a Tagger
	// takes exactly one. They are appended LAST so they can be removed again:
	// a query naming a node type the grammar does not have is rejected
	// outright, and a rejected tags query costs the language every symbol, not
	// just the edges. Grammars get renamed nodes across releases, so the
	// inheritance patterns degrade on their own rather than taking a language
	// down with them.
	withoutInheritance := query
	if inh := InheritanceQuery(entry.Name); inh != "" {
		query = query + "\n" + inh
	}

	// Fail fast if the combined query is invalid, so a bad supplement surfaces
	// at construction rather than silently yielding zero symbols per file.
	if _, err := ts.NewTagger(lang, query); err != nil {
		if query == withoutInheritance {
			p.initErr = fmt.Errorf("tags query for %q rejected: %w", entry.Name, err)
			return p
		}
		// Retry without the supertype patterns: losing EXTENDS/IMPLEMENTS
		// edges for this language is a recall cost; losing its symbols is a
		// correctness cliff.
		if _, err2 := ts.NewTagger(lang, withoutInheritance); err2 != nil {
			p.initErr = fmt.Errorf("tags query for %q rejected: %w", entry.Name, err2)
			return p
		}
		query = withoutInheritance
	}
	p.tagsQuery = query

	// A broken import query degrades imports for this language; it must not
	// cost us symbols, so the error is swallowed deliberately and the parser
	// continues without import extraction.
	if iq := ImportQuery(entry.Name); iq != "" {
		if q, err := ts.NewQuery(iq, lang); err == nil {
			p.importQuery = q
		}
	}

	// Same policy as imports: a receiver query that a grammar rejects costs
	// the receiver tier for that language and nothing else.
	if rq := ReceiverQuery(entry.Name); rq != "" {
		if q, err := ts.NewQuery(rq, lang); err == nil {
			p.receiverQuery = q
		}
	}
	if vq := VarTypeQuery(entry.Name); vq != "" {
		if q, err := ts.NewQuery(vq, lang); err == nil {
			p.varTypeQuery = q
		}
	}

	// A4 — arity. Compiled only when the language ALSO has counting rules: a
	// query that captures a parameter list this package cannot count would
	// produce an arity of zero for every declaration, which is a confident
	// wrong answer rather than a missing one. Same degradation policy as the
	// queries above — a grammar that rejects one costs arity evidence for that
	// language and nothing else.
	if d, ok := arityDialectFor(entry.Name); ok {
		p.arity, p.hasArity = d, true
		if pq := ParamCountQuery(entry.Name); pq != "" {
			if q, err := ts.NewQuery(pq, lang); err == nil {
				p.paramCountQuery = q
			}
		}
		if aq := ArgCountQuery(entry.Name); aq != "" {
			if q, err := ts.NewQuery(aq, lang); err == nil {
				p.argCountQuery = q
			}
		}
	}

	// Capacity tracks parallelism: more taggers than parsing goroutines would
	// only park grown arenas.
	p.pool = make(chan *ts.Tagger, runtime.NumCPU())
	return p
}

// getTagger takes a pooled Tagger or builds one.
func (p *genericParser) getTagger() *ts.Tagger {
	select {
	case tg := <-p.pool:
		return tg
	default:
		tg, err := ts.NewTagger(p.lang, p.tagsQuery)
		if err != nil {
			return nil
		}
		return tg
	}
}

// putTagger returns a Tagger, unless it has grown too large to be worth keeping.
func (p *genericParser) putTagger(tg *ts.Tagger, parsedBytes int) {
	if tg == nil || parsedBytes > PoolReturnLimit {
		// Dropped rather than pooled: its arena is sized to a large file and
		// pooling it would pin that memory for the life of the process.
		return
	}
	select {
	case p.pool <- tg:
	default:
		// Pool full; let this one go.
	}
}

// DrainPool discards every pooled Tagger, releasing the arenas they own.
//
// Called periodically during indexing. Costs a query recompilation on the next
// parse per language, which is negligible against the memory it returns.
func (p *genericParser) DrainPool() {
	for {
		select {
		case <-p.pool:
		default:
			return
		}
	}
}

func (p *genericParser) Language() string { return p.entry.Name }

func (p *genericParser) Parse(ctx context.Context, path string, src []byte) (*Result, error) {
	res := &Result{Language: p.entry.Name, Status: StatusOK}

	if p.initErr != nil {
		// A grammar that failed to load degrades THAT LANGUAGE, not the run.
		// The failure is recorded on the result so the gap is visible in the
		// index; returning it would abort indexing over one bad grammar.
		res.Status = StatusSkipped
		res.Err = p.initErr
		return res, nil //nolint:nilerr // deliberate: see above
	}
	if len(src) == 0 {
		res.Status = StatusSkipped
		return res, nil
	}
	if len(src) > MaxSourceBytes {
		res.Status = StatusSkipped
		res.Err = fmt.Errorf("source is %d bytes, over the %d limit", len(src), MaxSourceBytes)
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tags, tree, err := p.tagWithGuards(ctx, src)
	if err != nil {
		// A failed parse is data about this file, not a run-level failure.
		res.Status = StatusFailed
		res.Err = err
		// Deliberate, and load-bearing: spec §13 requires a parser failure to
		// be isolated to the affected file. The error is recorded ON the
		// result so the gap stays visible; returning it here would abort the
		// whole indexing run over one bad file.
		return res, nil //nolint:nilerr // see above
	}
	res.Imports = p.extractImports(tree, src)
	// Before the tree is dropped below: this is the last consumer of it, and
	// running after the drop is how the first version of this silently
	// extracted nothing at all.
	receivers := p.extractReceivers(tree, src)
	// Rewrite each receiver from the variable that names it to the type it was
	// declared as, where the file says so. `b.build()` becomes Builder.build().
	if types := p.extractVarTypes(tree, src); len(types) > 0 {
		for k, v := range receivers {
			if t, ok := types[v]; ok {
				receivers[k] = t
			}
		}
	}

	// A4 — arity, read from the same tree, for the same reason imports and
	// receivers are: it is the last chance before the tree is dropped.
	argCounts := p.extractCounts(p.argCountQuery, "args", tree, src)
	paramCounts := p.extractCounts(p.paramCountQuery, "params", tree, src)

	// Drop the tree as soon as the last consumer is done with it.
	//
	// A tree-sitter tree is one to two orders of magnitude larger than its
	// source — a megabyte of generated code measured at roughly 200 MB. Holding
	// one for even a moment longer than needed multiplies across every parsing
	// goroutine, and the Tagger returning to the pool must not carry it along.
	tree = nil
	_ = tree

	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag.Kind, "definition"):
			if isAnonymousName(tag.Name) {
				continue
			}
			sym := p.toSymbol(tag)
			sym.ParamCount = paramCounts[callSite{line: sym.Range.Start.Line, name: sym.Name}]
			res.Symbols = append(res.Symbols, sym)
		case strings.HasPrefix(tag.Kind, "reference"):
			r := Reference{
				Name:  tag.Name,
				Kind:  tag.Kind,
				Range: toRange(tag.Range),
			}
			// Matched by position: the receiver query captures @name at the
			// same identifier the tags query reported, so the two agree on
			// where the call is without either having to know about the other.
			//
			// Supertype references are excluded because a receiver is a
			// property of a CALL — the object a method was invoked on — and a
			// supertype has none. The match is keyed by line and name, so
			// `class Foo extends Bar` sharing a line with a `Bar(...)` call
			// would otherwise attach a receiver the resolver would then use to
			// narrow candidates for a relation that has no receiver at all.
			if !isSupertypeRef(tag.Kind) {
				site := callSite{line: r.Range.Start.Line, name: r.Name}
				if recv, ok := receivers[site]; ok {
					r.Receiver = recv
				}
				// Matched the same way and excluded for the same reason: a
				// supertype clause is not a call and has no argument list, so
				// `class Foo extends Bar` sharing a line with a `Bar(...)` call
				// must not inherit that call's argument count.
				r.ArgCount = argCounts[site]
			}
			res.References = append(res.References, r)
		}
	}

	res.Symbols = dedupeSymbols(res.Symbols)

	if len(res.Symbols) == 0 && len(res.References) == 0 && !isBenignSymbolless(path) {
		// Parsed cleanly but produced nothing on an actionable source file.
		// Usually a grammar whose inferred query has no useful patterns.
		// Recording this as partial keeps the gap visible instead of looking
		// like an empty file.
		res.Status = StatusPartial
	}
	return res, nil
}

// isBenignSymbolless reports whether path represents a file that legitimately
// contains no code symbol declarations or references (such as Go package
// documentation files, database migrations, stylesheets, or simple utility scripts).
//
// Marking these as StatusPartial degrades the overall index status to DEGRADED on
// healthy repositories that simply document packages or include migration DDL.
// StatusPartial is reserved for source files where executable language constructs
// were present but grammar queries failed to extract them.
func isBenignSymbolless(path string) bool {
	lower := strings.ToLower(path)
	base := lower
	if idx := strings.LastIndex(lower, "/"); idx >= 0 {
		base = lower[idx+1:]
	}
	if base == "doc.go" {
		return true
	}
	if strings.Contains(base, ".example.") || strings.HasSuffix(base, ".example") ||
		strings.Contains(base, ".sample.") || strings.HasSuffix(base, ".sample") {
		return true
	}
	if strings.Contains(lower, "/fixtures/") || strings.Contains(lower, "/testdata/") ||
		strings.Contains(lower, "/e2e/support/") || strings.Contains(lower, "/test/support/") {
		return true
	}
	switch {
	case strings.HasSuffix(lower, ".sql"):
		return true
	case strings.HasSuffix(lower, ".css"), strings.HasSuffix(lower, ".scss"), strings.HasSuffix(lower, ".sass"), strings.HasSuffix(lower, ".less"):
		return true
	case strings.HasSuffix(lower, ".sh"), strings.HasSuffix(lower, ".bash"), strings.HasSuffix(lower, ".zsh"), strings.HasSuffix(lower, ".ps1"), strings.HasSuffix(lower, ".bat"), strings.HasSuffix(lower, ".cmd"):
		return true
	}
	return false
}

// tagWithGuards runs the tagger under a timeout and a panic barrier.
//
// Both guards are required by the threat model: RepoCortex indexes repositories
// it did not write, and a hostile or merely broken file must cost one file's
// result, never the indexing run.
func (p *genericParser) tagWithGuards(ctx context.Context, src []byte) (tags []ts.Tag, tree *ts.Tree, err error) {
	timeout := DefaultParseTimeout
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type outcome struct {
		tags []ts.Tag
		tree *ts.Tree
		err  error
	}
	// Buffered so the worker can always publish and exit even after a timeout,
	// rather than blocking forever on an abandoned channel.
	done := make(chan outcome, 1)

	go func() {
		var out outcome
		defer func() {
			if r := recover(); r != nil {
				out = outcome{err: fmt.Errorf("parser panicked: %v", r)}
			}
			done <- out
		}()

		tg := p.getTagger()
		if tg == nil {
			out.err = fmt.Errorf("tagger unavailable for %q", p.entry.Name)
			return
		}
		defer p.putTagger(tg, len(src))

		// TagIncremental with a nil prior tree parses from scratch and returns
		// the syntax tree alongside the tags. Reusing it for the import query
		// avoids parsing the file twice, and avoids reimplementing the
		// TokenSource plumbing that real grammars require.
		out.tags, out.tree = tg.TagIncremental(src, nil)
	}()

	select {
	case o := <-done:
		return o.tags, o.tree, o.err
	case <-ctx.Done():
		// The worker is left to finish on its own; gotreesitter exposes no
		// cancellation hook we can safely reach here. It is bounded by the
		// source-size cap, so it terminates. Returning now keeps the indexer
		// moving rather than stalling on one pathological file.
		return nil, nil, fmt.Errorf("parse exceeded %s", timeout)
	}
}

// extractImports runs the language's import query against an already-parsed
// tree. Returns nil when the language has no query or the tree is unusable —
// missing imports weaken resolution confidence but must never fail a file.
// callSite keys a method call by the line it is on and the method's name.
//
// NOT by column. The tags query reports every reference in `b.build()` at the
// start of the expression — both `b` and `build` come back at the same column —
// while a receiver query captures the method identifier at its own column. The
// two never agree, so position matching silently found nothing.
//
// Line plus name is what both agree on. Two calls to the SAME method on ONE
// line with different receivers would be ambiguous, so those are dropped rather
// than guessed: see extractReceivers.
type callSite struct {
	line int
	name string
}

// extractReceivers maps a called method's position to the object it is called
// on.
//
// Keyed by position rather than by name because a name repeats many times in
// one file and only the position identifies the call site. Returns nil when the
// language has no receiver query, which the caller treats as "no receivers"
// rather than as an error.
func (p *genericParser) extractReceivers(tree *ts.Tree, src []byte) map[callSite]string {
	if p.receiverQuery == nil || tree == nil {
		return nil
	}
	out := make(map[callSite]string, 64)
	conflict := make(map[callSite]bool, 8)
	defer func() {
		// Query execution over a malformed tree is an untrusted-input path;
		// contain it exactly as tagging and imports do.
		_ = recover()
	}()

	for _, m := range p.receiverQuery.Execute(tree) {
		var recv, name string
		var line int
		for _, c := range m.Captures {
			switch c.Name {
			case "receiver":
				recv = c.Text(src)
			case "name":
				name = c.Text(src)
				if n := c.Node; n != nil {
					line = int(n.StartPoint().Row) + 1
				}
			}
		}
		if recv == "" || name == "" || line == 0 {
			continue
		}
		k := callSite{line: line, name: name}
		if prev, seen := out[k]; seen && prev != recv {
			// The same method called on two different objects on one line.
			// Which reference belongs to which cannot be told apart from a
			// line number, and a wrong receiver is worse than none: it would
			// narrow the candidate set to the wrong type with high confidence.
			conflict[k] = true
			continue
		}
		out[k] = recv
	}
	for k := range conflict {
		delete(out, k)
	}
	return out
}

// extractCounts maps a call site or declaration to the size of its argument or
// parameter list.
//
// One function for both halves of A4, because they are the same operation over
// different captures: find the identifier the tags query already reported, find
// the list node beside it, count the list. listCapture names which capture
// holds the list — "args" or "params".
//
// Keyed by callSite for the reason extractReceivers is: a name repeats many
// times in a file and only the position identifies which occurrence this is,
// and the two queries agree on the LINE but never on the column.
//
// A conflict — the same name on the same line with two different counts, as in
// `f(1) + f(2, 3)` — yields NO count rather than a guess. That is the same
// choice extractReceivers makes about two receivers on one line, and it matters
// more here: a wrong arity is evidence pointing at the wrong candidate, while a
// missing one leaves the resolver exactly where it was.
func (p *genericParser) extractCounts(
	q *ts.Query,
	listCapture string,
	tree *ts.Tree,
	src []byte,
) map[callSite]model.Arity {
	if q == nil || tree == nil || !p.hasArity {
		return nil
	}
	out := make(map[callSite]model.Arity, 64)
	conflict := make(map[callSite]bool, 8)
	defer func() {
		// Query execution over a malformed tree is an untrusted-input path;
		// contain it exactly as tagging, imports and receivers do.
		_ = recover()
	}()

	for _, m := range q.Execute(tree) {
		var name string
		var line int
		var list *ts.Node
		for _, c := range m.Captures {
			switch c.Name {
			case "name":
				name = c.Text(src)
				if n := c.Node; n != nil {
					line = int(n.StartPoint().Row) + 1
				}
			case listCapture:
				list = c.Node
			}
		}
		if name == "" || line == 0 || list == nil {
			continue
		}
		n := p.arity.count(list, p.lang)
		k := callSite{line: line, name: name}
		if prev, seen := out[k]; seen && prev != n {
			conflict[k] = true
			continue
		}
		out[k] = n
	}
	for k := range conflict {
		delete(out, k)
	}
	return out
}

// count turns a parameter or argument list node into an arity.
//
// Returns model.ArityUnknown for anything it does not fully understand, which
// is the disposition the whole feature rests on. The alternatives are both
// worse: counting a node type nobody classified would report a number derived
// from a shape that was never checked, and refusing the language outright would
// discard the arities it does get right.
//
// Extra nodes — comments, and anything else the grammar attaches out of band —
// are skipped rather than counted. A comment between two arguments is not a
// third argument.
func (d arityDialect) count(list *ts.Node, lang *ts.Language) model.Arity {
	if list == nil {
		return model.ArityUnknown
	}
	total := 0
	for i := 0; i < list.ChildCount(); i++ {
		c := list.Child(i)
		if c == nil || !c.IsNamed() || c.IsExtra() {
			continue
		}
		typ := c.Type(lang)
		switch {
		case d.isVariadic(c, lang):
			// Open-ended on one side means the two sides cannot be compared at
			// all. Neither `func v(xs ...int)` nor `v(a, b, c)` is wrong about
			// the other, so no count is published for either.
			return model.ArityUnknown
		case d.group[typ]:
			total += namedFieldCount(c, lang)
		default:
			total++
		}
	}
	return model.KnownArity(total)
}

// isVariadic reports whether node or any of its immediate named children is a
// variadic parameter or argument (such as Go's variadic_parameter_declaration,
// Java's spread_parameter, or TypeScript's rest_pattern wrapped in required_parameter).
func (d arityDialect) isVariadic(n *ts.Node, lang *ts.Language) bool {
	if n == nil {
		return false
	}
	if d.variadic[n.Type(lang)] {
		return true
	}
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c != nil && c.IsNamed() && d.variadic[c.Type(lang)] {
			return true
		}
	}
	return false
}

// namedFieldCount counts how many parameters one group node declares.
//
// Go writes `a, b int` as ONE parameter_declaration carrying two children with
// the `name:` field, so counting nodes reports 1 for an arity of 2. A group
// that names NOTHING still declares one parameter — `func h(int, string)` is
// two parameter_declarations, each with a type and no name — so the floor is
// one rather than zero.
func namedFieldCount(group *ts.Node, lang *ts.Language) int {
	n := 0
	for i := 0; i < group.ChildCount(); i++ {
		c := group.Child(i)
		if c == nil || !c.IsNamed() || c.IsExtra() {
			continue
		}
		if group.FieldNameForChild(i, lang) == "name" {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

// extractVarTypes maps a variable name to its declared type, file-locally.
//
// Last declaration wins on a repeated name. That is a real limitation — a name
// reused for two types in one file resolves to whichever came last — and it is
// preferred to dropping both, because the common case by far is one name, one
// type, and the receiver tier only claims high confidence when the type
// narrows candidates to exactly one anyway.
func (p *genericParser) extractVarTypes(tree *ts.Tree, src []byte) map[string]string {
	if p.varTypeQuery == nil || tree == nil {
		return nil
	}
	out := make(map[string]string, 32)
	defer func() { _ = recover() }()

	for _, m := range p.varTypeQuery.Execute(tree) {
		var name, typ string
		for _, c := range m.Captures {
			switch c.Name {
			case "var":
				name = c.Text(src)
			case "type":
				typ = c.Text(src)
			}
		}
		if name != "" && typ != "" {
			out[name] = typ
		}
	}
	return out
}

func (p *genericParser) extractImports(tree *ts.Tree, src []byte) []Import {
	if p.importQuery == nil || tree == nil {
		return nil
	}

	var out []Import
	defer func() {
		// Query execution over a malformed tree is another untrusted-input
		// path; contain it the same way as tagging.
		_ = recover()
	}()

	for _, m := range p.importQuery.Execute(tree) {
		var imp Import
		for _, c := range m.Captures {
			text := unquote(c.Text(src))
			switch c.Name {
			case "path":
				imp.Path = text
				if n := c.Node; n != nil {
					imp.Range = model.Range{
						Start: model.Position{Line: int(n.StartPoint().Row) + 1, Col: int(n.StartPoint().Column)},
						End:   model.Position{Line: int(n.EndPoint().Row) + 1, Col: int(n.EndPoint().Column)},
					}
				}
			case "alias":
				imp.Alias = text
			}
		}
		if imp.Path != "" {
			out = append(out, imp)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return dedupeImports(out)
}

// unquote strips the quoting that string-literal captures carry, so
// `"github.com/pay/repo"` becomes github.com/pay/repo.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') ||
			(s[0] == '\'' && s[len(s)-1] == '\'') ||
			(s[0] == '`' && s[len(s)-1] == '`') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// dedupeImports collapses duplicates produced by overlapping patterns (e.g. the
// Go aliased and unaliased import_spec rules both matching one line), keeping
// whichever entry carries an alias.
func dedupeImports(in []Import) []Import {
	if len(in) < 2 {
		return in
	}
	out := in[:0]
	for i, imp := range in {
		if i > 0 && imp.Path == out[len(out)-1].Path {
			if out[len(out)-1].Alias == "" && imp.Alias != "" {
				out[len(out)-1] = imp
			}
			continue
		}
		out = append(out, imp)
	}
	return out
}

func (p *genericParser) toSymbol(tag ts.Tag) model.Symbol {
	return model.Symbol{
		Kind:  kindFromTag(tag.Kind),
		Name:  tag.Name,
		Range: toRange(tag.Range),
		// Tier-1 extraction is syntactic: the symbol genuinely exists at this
		// location, but its fully-qualified identity is not yet known, so it
		// is not exact. The resolve layer upgrades this.
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceSourceCode,
		Exported:   isExported(tag.Name),
	}
}

// kindSpecificity ranks entity kinds so deduplication keeps the most
// informative one when a declaration is tagged more than once.
//
// KindFunction is the fallback that kindFromTag returns for anything it does
// not recognise, so it must rank LOWEST: a symbol tagged both "function" and
// something specific was really the specific thing, with "function" arriving
// from a generic pattern that also matched.
//
// KindInterface and KindTypeAlias outrank KindClass for the same reason one
// rung up. A general "this is a type" pattern matches interfaces and aliases as
// well as structs — Go's (type_spec ...) does exactly that — so a declaration
// arrives tagged both ways and the more specific claim has to win. Ranking them
// equal would leave the outcome to tag emission order, which is to say to
// chance, and an interface labelled `class` half the time is worse than one
// labelled wrongly all the time: it is not reproducible.
func kindSpecificity(k model.EntityKind) int {
	switch k {
	case model.KindInterface, model.KindTypeAlias:
		return 5
	case model.KindMethod, model.KindClass:
		return 4
	case model.KindConstant, model.KindField:
		return 3
	case model.KindVariable, model.KindPackage:
		return 2
	case model.KindFunction:
		return 1 // the catch-all; never let it displace a specific kind
	default:
		return 0
	}
}

// kindFromTag maps a tree-sitter tag kind to a domain entity kind.
//
// THE MAPPING IS DICTATED BY THE COMPILER-BACKED PATH, not chosen here. SCIP
// ingestion (internal/resolve/scip/kinds.go) maps Struct, Enum, Union and Type
// to KindClass, and reserves KindTypeAlias for TypeAlias and AssociatedType.
// This path must agree, because a heuristic edge is UPGRADED IN PLACE to an
// exact one when a SCIP index arrives: if the two disagreed on kind, the
// upgrade would silently change what a symbol is, and the SCIP cross-check that
// measures heuristic precision would report differences that are really just
// two vocabularies.
//
// "type" therefore means class, not alias. It used to mean alias, which made
// every Go struct and interface a type_alias — no Go symbol was ever a class,
// so the context engine's containerKinds gate (class/interface) matched nothing
// in Go and no method was attributed to the type that owns it.
func kindFromTag(tagKind string) model.EntityKind {
	k := strings.TrimPrefix(tagKind, "definition.")
	switch k {
	case "class", "struct", "type", "typedef", "enum", "union":
		return model.KindClass
	case "interface", "trait", "protocol":
		return model.KindInterface
	case "method", "constructor":
		return model.KindMethod
	case "function":
		return model.KindFunction
	case "field", "property", "member":
		return model.KindField
	case "constant", "const":
		return model.KindConstant
	case "variable", "var":
		return model.KindVariable
	case "type_alias", "alias":
		// A genuine alias — Go's `type X = Y`, TypeScript's
		// `type X = ...` — which introduces no new type. Distinct from the
		// cases above, and the distinction is the whole point: a query that
		// cannot tell `type X = Y` from `type X struct{}` states something
		// false about the code either way round.
		return model.KindTypeAlias
	case "module", "package", "namespace":
		return model.KindPackage
	default:
		// Unknown kinds become functions rather than being dropped: a symbol
		// with an imprecise kind is far more useful to an agent than a
		// missing one.
		return model.KindFunction
	}
}

// isSupertypeRef reports whether a reference kind names a supertype relation
// rather than a use of a name in an expression.
//
// The substrings are the SAME ones internal/resolve/infer.edgeKindFor tests to
// choose an edge kind, deliberately: this predicate and that mapping have to
// agree about which references are inheritance, and stating the test twice in
// two vocabularies is how they would drift apart. See inheritanceQueries.
func isSupertypeRef(refKind string) bool {
	return strings.Contains(refKind, "extend") ||
		strings.Contains(refKind, "inherit") ||
		strings.Contains(refKind, "implementation")
}

// isExported approximates visibility without language-specific rules.
//
// A leading uppercase letter means exported in Go and is conventional for
// public types in Java, C# and TypeScript; a leading underscore means private
// by convention in Python and JavaScript. The resolve layer replaces this with
// real visibility for languages that have a Tier-2 resolver, which is why the
// symbol's Confidence stays below exact.
func isExported(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "_") {
		return false
	}
	r := []rune(name)[0]
	return r >= 'A' && r <= 'Z'
}

// isAnonymousName reports whether a captured name declares nothing findable.
//
// Go's blank identifier is the case that shows up: `var _ = x`, `_, err := f()`
// and `func (_ T) M()` are all definitions to a tags query, and a real service
// contributed 46 symbols named `_` to its index. Nobody searches for `_`, no
// reference can bind to it — the language guarantees that — and `cortex
// symbol _` answering with 46 results is noise in a table whose value is that
// every row means something.
//
// Deliberately narrow: only the blank identifier itself. A short name like `i`
// or `db` is a real declaration that a reader may well look up.
func isAnonymousName(name string) bool {
	return name == "_"
}

func toRange(r ts.Range) model.Range {
	return model.Range{
		Start: model.Position{Line: int(r.StartPoint.Row) + 1, Col: int(r.StartPoint.Column)},
		End:   model.Position{Line: int(r.EndPoint.Row) + 1, Col: int(r.EndPoint.Column)},
	}
}

// dedupeSymbols removes duplicates produced by overlapping inferred and
// supplemental patterns. Sorting first keeps output deterministic, which the
// registry digest depends on.
func dedupeSymbols(in []model.Symbol) []model.Symbol {
	if len(in) < 2 {
		return in
	}
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Range.Start.Line != b.Range.Start.Line {
			return a.Range.Start.Line < b.Range.Start.Line
		}
		if a.Range.Start.Col != b.Range.Start.Col {
			return a.Range.Start.Col < b.Range.Start.Col
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		// Specificity, not alphabetical order. Overlapping inferred and
		// supplemental patterns can tag one declaration twice — a Java
		// constructor arrives as both "function" and "method" — and sorting by
		// name would keep "function" purely because f < m. The differential
		// test against the C runtime caught exactly that, so ranking is
		// explicit here.
		return kindSpecificity(a.Kind) > kindSpecificity(b.Kind)
	})

	out := in[:0]
	var prev model.Symbol
	for i, s := range in {
		if i > 0 && s.Name == prev.Name &&
			s.Range.Start.Line == prev.Range.Start.Line &&
			s.Range.Start.Col == prev.Range.Start.Col {
			continue
		}
		out = append(out, s)
		prev = s
	}
	return out
}
