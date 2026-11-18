package parse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/langdetect"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// DefaultParseCeiling is the wall-clock time after which a single file's parse
// is stopped. It is a LAST-RESORT SAFETY NET, not the parse budget.
//
// The budget is work, not time: gotreesitter stops a parse on its own
// iteration, node, stack-depth and arena-memory limits, every one of which is
// derived from the source length and counted by the parser. Those give the same
// verdict for the same bytes on any machine at any load, which is what keeps
// the index a function of the repository.
//
// A clock cannot do that. The limit used to be five seconds of wall time, and
// the same file failed on a busy machine and parsed on a quiet one: four redis
// files, 19 of 43 Newtonsoft.Json files and a 2,945-byte kernel header were
// measured flipping between runs. So the clock is now set far above anything a
// real parse needs (the slowest file measured, 1.9 MB of C#, takes 4 s) and
// exists only so that an input the work limits do not catch cannot hold a
// worker forever. A file it stops is recorded as incomplete and its error text
// says the limit was the clock, because that verdict, unlike the others, can
// differ between machines.
//
// What the work limits do not count is time spent inside one parser step.
// gotreesitter v0.50.1 had such a case: after a file-scope macro call, error
// recovery in an array initialiser took time that grew sevenfold each time the
// array doubled, and a 1.6 KB C file took eight seconds while staying far
// inside every limit. v0.57.0 bounds that path; the ceiling is what remains if
// another is found.
const DefaultParseCeiling = 60 * time.Second

// PoolReturnLimit is the largest source a parser may have read and still be
// returned to the pool. See putParser for why.
const PoolReturnLimit = 128 << 10

// DefaultMaxSourceBytes is the largest file handed to a parser, and the largest
// whose text is indexed for search. It equals fs.DefaultMaxFileSize, so there
// is one size limit and not two.
//
// It was 256 KiB, chosen when a megabyte of source cost several hundred
// megabytes of parser arena, and that limit silently dropped the centre of real
// programs: in redis, server.c, server.h, module.c, redis-cli.c, t_stream.c,
// t_hash.c and cluster_legacy.c are all between 256 KiB and 700 KiB, and all
// hand-written.
//
// Measured with gotreesitter v0.57.0, one file per process, peak resident
// memory of the whole process:
//
//	  693 KB  C     redis src/module.c         0.3 s   ~130 MB allocated
//	1,647 KB  C     five redis sources joined  0.9 s    170 MB
//	2,080 KB  C     module.c three times       0.9 s    298 MB
//	1,897 KB  C#    a 316 KB test file, x6     4.2 s    269 MB
//
// So a file at the limit costs about 300 MB while it is parsed. The pipeline
// admits large files against a memory budget (see pipeline.admissionWeight), so
// that cost is paid for one or two files at a time, not for every worker.
//
// Raise or lower it per repository with index.max_file_bytes in .repocortex.yml.
// A file over the limit is recorded as skipped with reason too_large, counted,
// and named by `repocortex index` and `repocortex status`.
const DefaultMaxSourceBytes = 2 << 20

// MaxSourceBytes is the default limit under its old name, kept because the
// CGO backend and callers outside this package compile against it.
const MaxSourceBytes = DefaultMaxSourceBytes

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

	// tagsQ is the compiled definitions-and-references query. One per language,
	// shared: gotreesitter documents Query as safe for concurrent Execute.
	tagsQ *ts.Query

	// cfg carries the limits this parser enforces.
	cfg parserConfig

	// Parsers are pooled because gotreesitter's Parser is documented unsafe for
	// concurrent use and building one is not free.
	//
	// A channel, not sync.Pool, because this pool must be DRAINABLE. Each
	// Parser owns a node arena that grows to fit the largest input it has seen
	// and never shrinks, and profiling a 10,896-file run showed 3 GB of live
	// arenas held by pooled parsers alone. sync.Pool offers no way to evict on
	// demand: only the GC decides, and it will not, because the pool holds
	// strong references. Draining is the only way to give that memory back
	// mid-run.
	pool chan *ts.Parser

	// initErr is recorded rather than returned at construction so a single
	// broken grammar degrades that language instead of failing the run.
	// initReason says which kind of gap it is: a grammar with no definition
	// query (text only, by design) or one that failed to load or compile.
	initErr    error
	initReason model.ParseReason

	// alt is this language's parser on the other error-recovery route, built
	// on first need; see alternate. isAlternate marks that parser itself, so
	// it never builds one of its own.
	altOnce     sync.Once
	alt         atomic.Pointer[genericParser]
	isAlternate bool
}

// ParserOption configures a parser.
type ParserOption func(*parserConfig)

type parserConfig struct {
	// ceiling is the wall-clock safety net. See DefaultParseCeiling.
	ceiling time.Duration

	// maxSourceBytes is the largest source parsed or text-indexed.
	maxSourceBytes int

	// iterationLimit, when positive, replaces the parser's source-derived
	// iteration limit. Zero keeps the library default.
	iterationLimit int
}

// WithParseCeiling overrides the wall-clock safety net. A value <= 0 restores
// the default rather than removing the ceiling: an unbounded parse is the
// failure this option exists beside.
func WithParseCeiling(d time.Duration) ParserOption {
	return func(c *parserConfig) {
		if d <= 0 {
			d = DefaultParseCeiling
		}
		c.ceiling = d
	}
}

// WithMaxSourceBytes overrides the size limit. A value <= 0 restores the
// default rather than disabling the limit.
func WithMaxSourceBytes(n int) ParserOption {
	return func(c *parserConfig) {
		if n <= 0 {
			n = DefaultMaxSourceBytes
		}
		c.maxSourceBytes = n
	}
}

// WithIterationLimit replaces the parser's iteration limit, which is otherwise
// derived from the source length (30 iterations per byte, 10,000 at least).
//
// The limit counts parser steps, so the verdict it gives is the same on every
// machine. It exists for tests, which need a small file to exceed its budget on
// purpose, and for an operator who wants a tighter deterministic bound than the
// library's.
func WithIterationLimit(n int) ParserOption {
	return func(c *parserConfig) {
		if n < 0 {
			n = 0
		}
		c.iterationLimit = n
	}
}

var defaultParserConfig = parserConfig{
	ceiling:        DefaultParseCeiling,
	maxSourceBytes: DefaultMaxSourceBytes,
}

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
	return r.ForLanguage(langdetect.FromName(path))
}

func (r *registry) ForLanguage(language string) Parser {
	if language == "" {
		return nil
	}
	entry := grammars.DetectLanguageByName(language)
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
	p = newGenericParser(*entry, r.cfg)
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

func newGenericParser(entry grammars.LangEntry, cfg parserConfig) *genericParser {
	return buildGenericParser(entry, cfg, entry.Language())
}

// buildGenericParser builds a parser over a given Language value. The registry
// passes the grammar's shared one; alternate passes a private copy.
func buildGenericParser(entry grammars.LangEntry, cfg parserConfig, lang *ts.Language) *genericParser {
	p := &genericParser{entry: entry, cfg: cfg}

	if lang == nil {
		p.initReason = model.ParseReasonGrammarUnavailable
		p.initErr = fmt.Errorf("grammar %q failed to load; text is indexed for search, no symbols are extracted", entry.Name)
		return p
	}
	p.lang = lang

	// Inferred query first, then RepoCortex's supplement. Additive by design:
	// a future release that improves inference cannot regress us, and a
	// duplicate capture is handled by de-duplication downstream.
	query := definitionQueryText(entry)
	if !strings.Contains(query, definitionCapturePrefix) {
		// Not a fault. More than half of the embedded grammars ship no
		// definition query (or, for Angular templates, one that can only
		// record a call), so their files have a syntax tree nobody can ask
		// anything of. The file is still worth finding by its text.
		p.initReason = model.ParseReasonTextOnly
		p.initErr = fmt.Errorf("no definition query for %q; text is indexed for search, no symbols are extracted", entry.Name)
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
	tagsQ, err := ts.NewQuery(query, lang)
	if err != nil {
		if query == withoutInheritance {
			p.initReason = model.ParseReasonGrammarUnavailable
			p.initErr = fmt.Errorf("tags query for %q rejected: %w", entry.Name, err)
			return p
		}
		// Retry without the supertype patterns: losing EXTENDS/IMPLEMENTS
		// edges for this language is a recall cost; losing its symbols is a
		// correctness cliff.
		var err2 error
		if tagsQ, err2 = ts.NewQuery(withoutInheritance, lang); err2 != nil {
			p.initReason = model.ParseReasonGrammarUnavailable
			p.initErr = fmt.Errorf("tags query for %q rejected: %w", entry.Name, err2)
			return p
		}
		query = withoutInheritance
	}
	p.tagsQuery = query
	p.tagsQ = tagsQ

	// A broken import query degrades imports for this language; it must not
	// cost us symbols, so the error is swallowed deliberately and the parser
	// continues without import extraction.
	if iq := ImportQuery(entry.Name); iq != "" {
		if q, err := ts.NewQuery(iq, lang); err == nil {
			p.importQuery = q
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

	// Capacity tracks parallelism: more parsers than parsing goroutines would
	// only park grown arenas.
	p.pool = make(chan *ts.Parser, runtime.NumCPU())
	return p
}

// getParser takes a pooled Parser or builds one.
func (p *genericParser) getParser() *ts.Parser {
	select {
	case ps := <-p.pool:
		return ps
	default:
		return ts.NewParser(p.lang)
	}
}

// putParser returns a Parser, unless it has grown too large to be worth keeping.
func (p *genericParser) putParser(ps *ts.Parser, parsedBytes int) {
	if ps == nil || parsedBytes > PoolReturnLimit {
		// Dropped rather than pooled: its arena is sized to a large file and
		// pooling it would pin that memory for the life of the process.
		return
	}
	select {
	case p.pool <- ps:
	default:
		// Pool full; let this one go.
	}
}

// DrainPool discards every pooled Parser, releasing the arenas they own.
//
// Called periodically during indexing. Costs a parser construction on the next
// parse per language, which is negligible against the memory it returns.
func (p *genericParser) DrainPool() {
	if alt := p.alt.Load(); alt != nil {
		alt.DrainPool()
	}
	if p.pool == nil {
		return
	}
	for {
		select {
		case <-p.pool:
		default:
			return
		}
	}
}

func (p *genericParser) Language() string { return p.entry.Name }

// normalizeLoneCR turns a carriage return that is not part of "\r\n" into a
// line feed, byte for byte, so a classic-Mac file reads as lines to a grammar
// that only knows "\n" and "\r\n". Same length, so every offset and range
// stays valid. The input is copied only when there is something to change.
func normalizeLoneCR(src []byte) []byte {
	var out []byte
	for i, c := range src {
		if c != '\r' || (i+1 < len(src) && src[i+1] == '\n') {
			continue
		}
		if out == nil {
			out = append([]byte(nil), src...)
		}
		out[i] = '\n'
	}
	if out == nil {
		return src
	}
	return out
}

func (p *genericParser) Parse(ctx context.Context, path string, src []byte) (*Result, error) {
	res := &Result{Language: p.entry.Name, Status: StatusOK}

	if len(src) == 0 {
		skip(res, model.ParseReasonEmpty, "")
		return res, nil
	}
	// Size before grammar: a file whose text is all that can be indexed obeys
	// the same limit as one that would be parsed.
	if limit := p.cfg.sourceLimit(); len(src) > limit {
		skip(res, model.ParseReasonTooLarge, tooLargeDetail(int64(len(src)), int64(limit)))
		return res, nil
	}
	src = normalizeLoneCR(src)
	if norm := sourceNormalizers[p.entry.Name]; norm != nil {
		// A same-length rewrite of text the grammar cannot read, made before
		// the first parse; see mask_fsharp.go.
		src = norm(src)
	}
	if p.initErr != nil {
		// A grammar that yields no definitions degrades THAT LANGUAGE, not the
		// run. The gap is recorded on the result so it is visible in the
		// index; returning it would abort indexing over one grammar.
		skip(res, p.initReason, p.initErr.Error())
		return res, nil //nolint:nilerr // deliberate: the gap is recorded on the result, see above
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// From here on "by" is the parser whose tree this is: p, or p's alternate
	// when the second recovery route was the one that read the file. Its
	// queries are the ones compiled against the tree's own Language.
	// readTree is the plain parse and its tags, under the given wall-clock
	// allowance. done is true when res is final (a failure) or ctx was
	// cancelled, and cerr then says which.
	readTree := func(micros uint64) (by *genericParser, tree *ts.Tree, stop stopInfo, tags []ts.Tag, implementers map[uint32]string, done bool, cerr error) {
		by, tree, stop, err := p.parseTreeWithin(ctx, src, micros)
		if cerr := ctx.Err(); cerr != nil {
			// Cancellation stops the parser mid-file and leaves a tree of
			// whatever it had read. That tree is the caller's cancellation,
			// not a fact about the file, so nothing from it is reported.
			return nil, nil, stop, nil, nil, true, cerr
		}
		if err != nil {
			// A failed parse is data about this file, not a run-level failure.
			// Spec §13 requires a parser failure to be isolated to the
			// affected file. The error is recorded ON the result so the gap
			// stays visible; returning it here would abort the whole indexing
			// run over one file.
			res.Status = StatusFailed
			res.Reason = model.ParseReasonUnspecified
			res.Err = err
			return nil, nil, stop, nil, nil, true, nil //nolint:nilerr // deliberate: recorded on the result, see above
		}
		tags, implementers, err = by.tagTree(tree, src)
		if err != nil {
			res.Status = StatusFailed
			res.Reason = model.ParseReasonUnspecified
			res.Err = err
			return nil, nil, stop, nil, nil, true, nil //nolint:nilerr // deliberate: a failed query is this file's result, not the run's
		}
		return by, tree, stop, tags, implementers, false, nil
	}

	// The first parse of a file in a language the masks apply to gets a
	// shorter wall-clock allowance than the ceiling; see probeMicros.
	probe := p.cfg.probeMicros(p.entry.Name, len(src))
	// From here on "by" is the parser whose tree this is: p, or p's alternate
	// when the second recovery route was the one that read the file. Its
	// queries are the ones compiled against the tree's own Language.
	by, tree, stop, tags, implementers, done, cerr := readTree(probe)
	if done {
		return doneResult(res, cerr)
	}
	// C, C++, Objective-C and C#: a declaration written behind a macro the
	// grammar does not know, or cut apart by a conditional-compilation group,
	// is retried once with the macros masked and the groups resolved, and the
	// retry is kept only if the guard in cmacro.go says it is better. A nil
	// answer is every file with nothing to mask and every file that parsed
	// cleanly. The two masks share that one retry.
	mp := by.retryMaskedWithin(ctx, tree, stop, src, tags, probe)
	outran := probe < p.cfg.ceilingMicros() && stop.early && stop.reason == ts.ParseStopTimeout
	if mp == nil && outran && ctx.Err() == nil {
		// The first parse ran out of its shorter allowance and no mask
		// rescued it: it gets the whole ceiling, as every file does.
		if by, tree, stop, tags, implementers, done, cerr = readTree(p.cfg.ceilingMicros()); done {
			return doneResult(res, cerr)
		}
	}
	if mp != nil && outran && mp.unclean() && ctx.Err() == nil {
		// The mask was taken because the plain parse ran out of a wall-clock
		// allowance, and what it bought still has errors. On a slow or loaded
		// machine that is a file the plain parse would have read cleanly a
		// moment later (seen under the race detector: a C# file recorded ok on
		// one run and partial with four error nodes on the next). So the plain
		// parse gets a second, longer allowance, and if it finishes inside it
		// the decision is made as it is on a fast machine: against a complete
		// plain parse, by the same guard. A parse that does not finish inside
		// it is one the parser does not finish, and the masked reading stands.
		second := min(p.cfg.ceilingMicros(), probe*probeSecondLook)
		by2, tree2, stop2, tags2, implementers2, done2, cerr2 := readTree(second)
		if done2 {
			return doneResult(res, cerr2)
		}
		if !stop2.early || stop2.reason != ts.ParseStopTimeout {
			by, tree, stop, tags, implementers = by2, tree2, stop2, tags2, implementers2
			mp = by.retryMaskedWithin(ctx, tree, stop, src, tags, p.cfg.ceilingMicros())
		}
	}
	if mp != nil {
		tree, stop, src, tags, implementers = mp.tree, mp.stop, mp.src, mp.tags, mp.implementers
		res.MacrosMasked = mp.count
		res.BranchesBlanked = mp.branches
		res.ConstructsMasked = mp.constructs
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if adjust := tagAdjusters[by.entry.Name]; adjust != nil {
		tags = adjust(src, tags)
	}
	hosted := unmarkHostedTags(tags)
	res.Imports = fixImports(by.entry.Name, path, by.extractImports(tree, src))

	// Python's scope-dependent facts: what each call was made on, each nested
	// definition's qualified name, and the names its imports bring in. Nil for
	// every other language, and for a Python tree the walk could not finish.
	// See pyscope.go.
	py := by.analyzeScopes(tree, src)
	if py != nil {
		res.Imports = mergeMemberImports(res.Imports, py.members)
	}

	// Every other language: what each call was made on and what each nested
	// definition belongs to, read from the tag ranges and one walk of the
	// tree. Before the tree is dropped below — this is the last consumer of
	// it, and running after the drop is how the first receiver extraction
	// silently extracted nothing at all. See shape.go.
	var shapes *shapeFacts
	if py == nil {
		shapes = by.analyzeShapes(tree, src, tags)
		// The names an import binds are written with the path the statement wrote,
		// so a language that rewrites its paths (langreg.go) rewrites these too.
		// No language that has bindings and a rewrite had both before Solidity.
		res.Imports = mergeMemberImports(res.Imports, fixBindings(by.entry.Name, path, by.importedBindings(tree, src)))
	}

	// A language that needs a fact about a definition that no query can state
	// (what an assignment is the definition OF, which object a prototype
	// method belongs to) reads it here, from the same tree, for the same
	// reason. Nil for every language without a refiner. See lang_javascript.go.
	fixes := by.refineDefinitions(tree, src, path, tags)

	// A4 — arity, read from the same tree, for the same reason imports and
	// receivers are: it is the last chance before the tree is dropped.
	argCounts := by.extractCounts(by.argCountQuery, "args", tree, src)
	paramCounts := by.extractCounts(by.paramCountQuery, "params", tree, src)

	// The type each function declares it returns, read from the same tree for
	// the same reason. See returns.go.
	returnTypes := by.definitionReturnTypes(tree, src, tags)

	// Constructor delegation (`this(...)`, `super(...)`, `: base(...)`) has no
	// name to tag and is built from the tree. See delegation.go.
	delegations := by.constructorDelegations(tree, src)

	// Each clause head's arity, for the languages that write a function as
	// several clauses. See clausemerge.go.
	clauseKeys := by.clauseArities(tree, src, tags)

	// The verdict on the parse itself, also read from the tree.
	p.describeParse(res, tree, stop, src)
	if res.Status == StatusOK && (res.BranchesBlanked > 0 || res.ConstructsMasked > 0) {
		// A clean masked parse has no error to carry the note, and a reader
		// asking why a definition is missing must be able to see that a
		// branch was blanked or a construct masked. The status stays ok: the file was read.
		res.Err = errors.New(strings.TrimPrefix(res.maskNote(), "; "))
	}

	// Definitions in what is still ERROR after any mask, read by line shape;
	// see fallback.go. The candidates are checked against the symbols below.
	var shapeCands []shapeCandidate
	if res.Reason == model.ParseReasonSyntaxErrors {
		shapeCands = by.lineShapeCandidates(tree, src)
	}

	// Drop the tree as soon as the last consumer is done with it.
	//
	// A tree-sitter tree is one to two orders of magnitude larger than its
	// source. Holding one for even a moment longer than needed multiplies
	// across every parsing goroutine.
	tree = nil
	_ = tree

	// callStarts maps the start byte of a call's name to where its reference
	// is recorded, for the calls a receiver's value came from (shape.go).
	var callStarts map[uint32]model.Position
	if shapes != nil && shapes.producers {
		callStarts = make(map[uint32]model.Position, len(tags))
		for _, tag := range tags {
			if isCallTag(tag.Kind) {
				callStarts[tag.NameRange.StartByte] = toRange(tag.Range).Start
			}
		}
	}

	// objectDropped remembers where a call's object has already been dropped;
	// see the reference case below.
	var objectDropped map[pySite]bool

	// Groovy's grammar reads a method written with modifiers and no return type
	// (`private helper(x) { x }`) and a constructor as a CALL, and the definition
	// query takes the same identifier as the method's name. A method does not call
	// itself from the line that declares it, so a call tag on a name that a
	// definition tag names is the declaration read twice.
	var declaredAt map[uint32]struct{}
	if p.entry.Name == "groovy" {
		declaredAt = make(map[uint32]struct{}, 16)
		for _, tag := range tags {
			if strings.HasPrefix(tag.Kind, "definition") {
				declaredAt[tag.NameRange.StartByte] = struct{}{}
			}
		}
	}

	for i, tag := range tags {
		if declaredAt != nil && isCallTag(tag.Kind) {
			if _, declared := declaredAt[tag.NameRange.StartByte]; declared {
				continue
			}
		}
		switch {
		case strings.HasPrefix(tag.Kind, "definition"):
			// A tag from a host's own patterns (a component file's script) is
			// judged by that host's rules, not by the script language's
			// refiner, which would drop what a plain script does not define.
			fix, fixed := fixes[tag.NameRange.StartByte]
			if fixed && !hosted[i] {
				if fix.drop {
					continue
				}
				if fix.name != "" {
					tag.Name = fix.name
				}
				if fix.kind != "" {
					tag.Kind = fix.kind
				}
			}
			if isAnonymousName(tag.Name) || isKeywordName(p.entry.Name, tag.Name) {
				continue
			}
			sym := p.toSymbol(tag)
			// Keyed by the line of the NAME, not of the range: an annotation or an
			// attribute above the declaration starts the range a line or more earlier
			// than the name the arity query captured.
			sym.ParamCount = paramCounts[callSite{line: int(tag.NameRange.StartPoint.Row) + 1, name: sym.Name}]
			sym.ReturnType = returnTypes[tag.NameRange.StartByte]
			if py != nil {
				// A nested definition is qualified by what encloses it, so that
				// two classes' `save` are two names. Module-level definitions
				// are absent from the map and keep the default.
				if q, ok := py.qualNames[callSite{line: sym.Range.Start.Line, name: sym.Name}]; ok {
					sym.FQN = model.DefaultFQN(path, q)
				}
			} else if shapes != nil {
				// The same for every other language: `Context.Set`,
				// `outer.<locals>.inner`.
				if q, ok := shapes.quals[tag.NameRange.StartByte]; ok {
					sym.FQN = model.DefaultFQN(path, q)
				}
			}
			if fixed && fix.owner != "" {
				sym.FQN = model.DefaultFQN(path, fix.owner+"."+sym.Name)
			}
			if fixed && !hosted[i] && fix.endLine > 0 {
				sym.Range.End = model.Position{Line: fix.endLine, Col: fix.endCol}
			}
			if tag.Kind == macroDefinitionTag {
				// A macro_rules! is a function by kind and a macro by name:
				// `src/error.rs:print_error!`. See macroFQNMarker.
				if sym.FQN == "" {
					sym.FQN = model.DefaultFQN(path, sym.Name)
				}
				sym.FQN += macroFQNMarker
			}
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
				if py != nil {
					// The scope walk saw every call in the file and keyed each
					// by the position the tags query reports, so its answer
					// replaces the line-keyed one outright: an empty receiver
					// here means the source wrote a bare name.
					at := pySite{line: r.Range.Start.Line, col: r.Range.Start.Col, name: r.Name}
					if py.notCalls[at] {
						// The object of a method call, tagged as a call, where
						// the object is a local value. Not a call and not a
						// use of anything the index holds.
						//
						// `run.run()` is the one shape where the object and
						// the method share a name, so TWO references arrive
						// at this position with this name and only one of
						// them is the object. Exactly one is dropped; the
						// other is the call, and keeps its receiver below.
						_, alsoCalled := py.receivers[at]
						if !alsoCalled || !objectDropped[at] {
							if objectDropped == nil {
								objectDropped = make(map[pySite]bool, 4)
							}
							objectDropped[at] = true
							res.Qualifiers++
							continue
						}
					}
					r.Receiver = py.receivers[at]
					r.ReceiverKind = py.receiverKind(at)
					if py.indirect[at] {
						r.Kind += IndirectCallSuffix
					}
				} else if shapes != nil && isCallTag(tag.Kind) {
					shape, ok := shapes.calls[tag.NameRange.StartByte]
					switch {
					case !ok:
						// The walk did not reach it; the shape is unknown and
						// is recorded as that.
						r.Receiver, r.ReceiverKind = model.OpaqueReceiver, model.ReceiverExpr
					case shape.notCall:
						// The receiver or qualifier of a call, tagged as a
						// call: `reader` in `reader.peek()`. Not a call.
						res.Qualifiers++
						continue
					default:
						r.Receiver, r.ReceiverKind = shape.receiver, shape.kind
						if shape.indirect {
							r.Kind += IndirectCallSuffix
						}
						if shape.from != 0 {
							// The call the receiver's value came from, by the
							// position its own reference is recorded at.
							r.Value = callStarts[shape.from]
						}
					}
				}
				// Matched the same way and excluded for the same reason: a
				// supertype clause is not a call and has no argument list, so
				// `class Foo extends Bar` sharing a line with a `Bar(...)` call
				// must not inherit that call's argument count.
				r.ArgCount = argCounts[site]
			}
			if t, ok := implementers[tag.NameRange.StartByte]; ok && isSupertypeRef(tag.Kind) {
				r.Receiver, r.ReceiverKind = t, model.ReceiverImplementer
			} else if isSupertypeRef(tag.Kind) {
				// The qualifier the supertype is written with, if any. See
				// supertypeQualifier.
				r.Receiver = supertypeQualifier(p.entry.Name, src, tag.NameRange.StartByte)
			}
			res.References = append(res.References, r)
		}
	}

	res.References = append(res.References, delegations...)
	res.Symbols = mergeClauses(dedupeSymbols(res.Symbols), clauseKeys)
	if len(shapeCands) > 0 {
		res.Symbols, res.RecoveredByShape = addRecovered(res.Symbols, shapeCands, src)
		if res.RecoveredByShape > 0 && res.Err != nil {
			res.Err = errors.New(res.Err.Error() + recoveredNote(res.RecoveredByShape))
		}
	}

	// The status was decided by describeParse and is NOT revisited here.
	// "Parsed cleanly and declared nothing" is a complete, correct reading of
	// a file of constants, a package comment or a migration; it used to be
	// recorded as partial, which made a healthy repository degraded and left
	// the word meaning two unrelated things.
	return res, nil
}

// doneResult is what Parse returns when the first parse ended it: the
// context's error and no result when it was cancelled, else the result that
// records the failure.
func doneResult(res *Result, cerr error) (*Result, error) {
	if cerr != nil {
		return nil, cerr
	}
	return res, nil
}

// skip records that no tree was attempted, and why.
func skip(res *Result, reason model.ParseReason, detail string) {
	res.Status = StatusSkipped
	res.Reason = reason
	res.Err = errors.New(model.FormatParseError(reason, detail))
}

// tooLargeDetail is the text stored for a file over the size limit. Shared
// with the pipeline, which writes the same reason for a file discovery refused
// to read, so the two cannot describe one limit in two ways.
func tooLargeDetail(size, limit int64) string {
	return fmt.Sprintf("%d bytes, over the %d byte limit (index.max_file_bytes)", size, limit)
}

// TooLargeError is the files.parse_error text for a file of size bytes that is
// over limit.
func TooLargeError(size, limit int64) string {
	return model.FormatParseError(model.ParseReasonTooLarge, tooLargeDetail(size, limit))
}

func (c parserConfig) sourceLimit() int {
	if c.maxSourceBytes <= 0 {
		return DefaultMaxSourceBytes
	}
	return c.maxSourceBytes
}

func (c parserConfig) wallClockCeiling() time.Duration {
	if c.ceiling <= 0 {
		return DefaultParseCeiling
	}
	return c.ceiling
}

// The first parse of a file in a language the masks apply to is given
// probeBase plus probePerKiB for each KiB of source, when that is less than
// the ceiling.
const (
	probeBase   = 2 * time.Second
	probePerKiB = 10 * time.Millisecond
)

// probeSecondLook is how many times the first allowance the plain parse is
// given when it ran out of the first one and the masked reading that replaced
// it still has errors. It bounds what a file the parser never finishes costs
// (the 60 s ceiling is not paid again: a five-line C++ file that stalls costs
// 2 s for the first parse and 4 s for this one) and is what keeps a verdict
// the same on a machine up to three times slower than the one the allowance
// was measured on.
const probeSecondLook = 2

// probeMicros is the wall-clock allowance of the first parse of a file, in
// microseconds.
//
// WHY LESS THAN THE CEILING. In google/benchmark, test/benchmark_test.cc held
// one worker for the whole 60 s ceiling and was recorded as incomplete. The
// time was not in the work limits' reach: it was one dispatch of the library's
// third full-parse retry pass, entered at iteration 2,299 of a limit of
// 295,950, which neither returns nor counts. The first two passes die with no
// live stack at a macro between a return type and a name
// (`int BENCHMARK_NOINLINE Factorial(int n) {`), the third recovers past it
// and stalls at `std::complex<double>>` 8,000 bytes later. The first pass takes
// 80 ms; with the macro blanked the whole file parses in 107 ms. A cut of the
// file to five lines (see TestAStalledParseIsRescuedByTheMask) stalls the same
// way. So a file in a masked language that outruns this allowance is offered
// to the masks, whose retry takes milliseconds, before it is given the rest of
// the ceiling.
//
// WHAT IT DOES NOT CHANGE. A file that finishes inside the allowance gets the
// result it got before. A file that does not finish and is not rescued is
// parsed again with the whole ceiling, so no file is stopped sooner than it
// was; what a file near the allowance pays is a second parse. The allowance
// is generous against what was measured here (redis src/module.c, 693 KB of
// C, takes 0.3 s in the table above and is allowed 9 s; the first pass of
// benchmark_test.cc takes 80 ms and is allowed 2 s), but it is a wall-clock
// figure, and a file that ran out of it on a slow machine and not on a fast
// one would get the masked reading on the first and the plain one on the
// second. One was seen, under the race detector with every package's tests
// running at once, and the second look in Parse (probeSecondLook) is the
// answer to it: a masked reading that still has errors is not kept on the
// strength of the short allowance alone.
func (c parserConfig) probeMicros(language string, srcLen int) uint64 {
	full := c.ceilingMicros()
	if !maskedLanguages[language] && !condLanguages[language] || language == "fsharp" {
		// F# reads conditional compilation through the same mask but keeps
		// the whole ceiling for its first parse: with a short allowance the
		// parser returned a finished tree with no definitions in it for a
		// file it reads in about 2.5 s (Fable's tests/Rust DateTimeTests.fs,
		// 15 definitions with the ceiling, none with 2.27 s), without
		// reporting a stop.
		return full
	}
	us := (probeBase + time.Duration(srcLen>>10)*probePerKiB).Microseconds() * probeScale
	if us < 1 {
		us = 1
	}
	return min(full, uint64(us))
}

// ceilingMicros is the ceiling in the unit the parser takes it in, never zero:
// zero means "no timeout" to the parser, so a ceiling below a microsecond must
// round up, not away.
func (c parserConfig) ceilingMicros() uint64 {
	us := c.wallClockCeiling().Microseconds()
	if us < 1 {
		return 1
	}
	return uint64(us)
}

// stopInfo is what the parser reported about a parse that ended before the end
// of the input. The zero value means it ran to completion.
type stopInfo struct {
	early       bool
	reason      ts.ParseStopReason
	memSource   string // which memory guard, when reason is the memory budget
	parsedBytes int
	iterations  int
	iterLimit   int
	nodes       int
	nodeLimit   int
}

// parseTree parses src, and when the parser stops short of the end, tries once
// more by its other recovery route and keeps whichever attempt read further.
// It returns the parser whose queries match the tree, which is p or p's
// alternate.
//
// WHY A SECOND ATTEMPT. gotreesitter has two ways of recovering from input its
// grammar does not accept: a port of the C runtime's cost-based recovery, on by
// default for the grammars certified for it, and the older resynchronising
// recovery it replaced. They fail on different files. With v0.57.0 the first
// one gives up outright ("invariant_violation") on eleven of redis's 1,025 C
// files, at a `#define` inside an array initialiser, 1,870 bytes into the
// 115 KB of src/bitops.c; the second reads every one of them to the end. And it
// was the first that took quadratic time in v0.50.1 where the second did not.
// So a parse the default route abandons is retried on the other, and a file is
// recorded as incomplete only when both stop early.
//
// A parse stopped by the wall-clock ceiling is NOT retried. The ceiling exists
// to bound the time one file can take, and a second attempt would double it.
func (p *genericParser) parseTree(ctx context.Context, src []byte) (*genericParser, *ts.Tree, stopInfo, error) {
	return p.parseTreeWithin(ctx, src, p.cfg.ceilingMicros())
}

// parseTreeWithin is parseTree with the wall-clock allowance given in
// microseconds. See probeMicros for why a first parse may be given less than
// the ceiling.
func (p *genericParser) parseTreeWithin(ctx context.Context, src []byte, micros uint64) (*genericParser, *ts.Tree, stopInfo, error) {
	by, tree, stop, err := p.parseTreeRoutes(ctx, src, micros)
	return by, tree, stop.settled(len(src)), err
}

// settled turns a stop that did not leave anything unread into no stop.
//
// The parser reports `accepted_prefix` when it accepted a tree before the end
// of the input and left the rest outside the root. If it had read every token
// to the end of the file, no budget ran out and nothing is unread: it is a file
// the grammar could not make sense of at its tail, and that is a syntax error
// (the tree carries the error flag and error nodes), not an incomplete parse.
// Two 17-byte files of rack, `### TestFile ###`, were recorded as incomplete
// this way, with 2 of 10,000 iterations used, and that alone made the index
// degraded. It is applied after the other recovery route has had its chance:
// a route that reads the file as a tree is better than one that gives up.
func (s stopInfo) settled(srcLen int) stopInfo {
	if s.early && s.reason == ts.ParseStopAcceptedPrefix && s.parsedBytes >= srcLen {
		return stopInfo{}
	}
	return s
}

// parseTreeRoutes is parseTree's two attempts, before the verdict is settled.
func (p *genericParser) parseTreeRoutes(ctx context.Context, src []byte, micros uint64) (*genericParser, *ts.Tree, stopInfo, error) {
	tree, stop, err := p.parseOnceWithin(ctx, src, micros)
	if err != nil || !stop.early || !stop.retryable() || ctx.Err() != nil {
		return p, tree, stop, err
	}
	alt := p.alternate()
	if alt == nil {
		return p, tree, stop, nil
	}

	tree2, stop2, err2 := alt.parseOnceWithin(ctx, src, micros)
	if err2 != nil || tree2 == nil {
		// The second route failed outright. The first attempt's partial tree
		// is still the best reading there is.
		return p, tree, stop, nil //nolint:nilerr // deliberate: the first attempt's result stands
	}
	if !stop2.early || stop2.parsedBytes > stop.parsedBytes {
		return alt, tree2, stop2, nil
	}
	return p, tree, stop, nil
}

// retryable reports whether a stopped parse is worth a second attempt on the
// other recovery route.
func (s stopInfo) retryable() bool {
	switch s.reason {
	case ts.ParseStopTimeout, ts.ParseStopCancelled:
		return false
	}
	return true
}

// alternate returns the parser for this language's other recovery route, or
// nil when the language has only one.
//
// HOW THE ROUTE IS SELECTED. gotreesitter chooses the recovery route per
// Language value, from two exported fields: the cost-based port runs when the
// grammar is CRecoveryCostCompetitionCapable and
// CRecoveryCostCompetitionEnabledByDefault. So the alternate is a SECOND
// Language value for the same grammar, loaded from the same embedded blob, with
// the second field cleared before anything parses with it. Nothing global is
// touched: the registry's own Language, and every parse on the default route,
// are unaffected. (The library also reads a GOT_C_RECOVERY environment
// variable, but it caches what it derives from it per Language on first use, so
// flipping it at run time changes nothing reliably. This does not use it.)
//
// The alternate has its own queries, compiled against its own Language, and its
// own parser pool. It is built on first need, so a repository none of whose
// files stop early never pays for it.
func (p *genericParser) alternate() *genericParser {
	if p.isAlternate || p.lang == nil || !p.lang.CRecoveryCostCompetitionCapable ||
		!p.lang.CRecoveryCostCompetitionEnabledByDefault {
		return nil
	}
	p.altOnce.Do(func() {
		blob := grammars.BlobByName(p.entry.Name)
		if len(blob) == 0 {
			return
		}
		lang, err := grammars.LoadLanguage(p.entry.Name, blob)
		if err != nil || lang == nil {
			return
		}
		lang.CRecoveryCostCompetitionEnabledByDefault = false
		alt := buildGenericParser(p.entry, p.cfg, lang)
		if alt.initErr != nil {
			return
		}
		alt.isAlternate = true
		p.alt.Store(alt)
	})
	return p.alt.Load()
}

// parseOnce runs one parse, in the calling goroutine, under three guards.
//
// IN THE CALLING GOROUTINE is the point. This used to start a goroutine, wait
// five seconds and return, leaving the parse running: gotreesitter was thought
// to have no cancellation hook. It has two, and both are used here, so a parse
// that is stopped actually stops and nothing outlives this call. Abandoned
// parses were why a pool of four workers was measured using 6.8 cores and tens
// of gigabytes on the Linux kernel: each kept its core and its arena while the
// worker that started it moved on.
//
//  1. Work limits. The parser counts its own iterations, nodes, stack depth and
//     arena bytes against limits derived from the source length, and stops
//     itself when one is reached. These are deterministic.
//  2. A cancellation flag, set when ctx is cancelled. The parser polls it.
//  3. A wall-clock ceiling (Parser.SetTimeoutMicros), polled the same way. A
//     last resort; see DefaultParseCeiling.
//
// All three stop the parser from inside its own loop, between steps. The
// parser returns the tree it had built so far, and the caller extracts what it
// can from it.
//
// WHAT IS NOT BOUNDED: the time of a single parser step, and the queries that
// run over the finished tree. The flag and the clock are polled about once per
// thousand steps, so a step that itself runs long delays the stop; and query
// execution has its own per-pattern work budget in the library but no clock.
// Both are bounded in practice by the size limit on the source.
func (p *genericParser) parseOnce(ctx context.Context, src []byte) (tree *ts.Tree, stop stopInfo, err error) {
	return p.parseOnceWithin(ctx, src, p.cfg.ceilingMicros())
}

// parseOnceCalls counts every tree-sitter parse started by parseOnceWithin.
// Tests read it to assert how many parses a file costs (one when nothing needs
// masking, two at most when the masks apply); nothing else does.
var parseOnceCalls atomic.Uint64

// parseOnceWithin is parseOnce with the wall-clock allowance given in
// microseconds (never zero, which the parser reads as no limit).
func (p *genericParser) parseOnceWithin(ctx context.Context, src []byte, micros uint64) (tree *ts.Tree, stop stopInfo, err error) {
	parseOnceCalls.Add(1)
	ps := p.getParser()
	if ps == nil {
		return nil, stop, fmt.Errorf("parser unavailable for %q", p.entry.Name)
	}
	if micros == 0 {
		micros = p.cfg.ceilingMicros()
	}

	// A fresh flag per parse, never one stored on the pooled parser: the
	// cancellation callback below may run a moment after this call decides to
	// return, and it must not be able to cancel whatever parse the parser is
	// handed next.
	var cancelFlag uint32
	ps.SetCancellationFlag(&cancelFlag)
	ps.SetTimeoutMicros(micros)
	if p.cfg.iterationLimit > 0 {
		ps.SetParseWorkLimits(ts.ParseWorkLimits{IterationLimit: p.cfg.iterationLimit})
	}

	fired := make(chan struct{})
	unregister := context.AfterFunc(ctx, func() {
		atomic.StoreUint32(&cancelFlag, 1)
		close(fired)
	})

	reusable := false
	defer func() {
		if !unregister() {
			// The callback has started. Wait for it, so that no goroutine
			// this call caused is still running when it returns.
			<-fired
		}
		if r := recover(); r != nil {
			// The parser's internal state is unknown after a panic, so it is
			// dropped and not returned to the pool.
			tree, stop, err = nil, stopInfo{}, fmt.Errorf("parser panicked: %v", r)
			return
		}
		if reusable {
			ps.SetCancellationFlag(nil)
			p.putParser(ps, len(src))
		}
	}()

	tree, err = ps.Parse(src)
	if err != nil {
		return nil, stop, fmt.Errorf("parser error: %w", err)
	}
	if tree == nil || tree.RootNode() == nil {
		reason := ts.ParseStopNone
		if tree != nil {
			reason = tree.ParseStopReason()
		}
		return nil, stop, fmt.Errorf("parser produced no tree (stop reason %q)", reason)
	}

	if tree.ParseStoppedEarly() {
		rt := tree.ParseRuntime()
		stop = stopInfo{
			early:       true,
			reason:      rt.StopReason,
			memSource:   rt.MemoryBudgetStopSource,
			parsedBytes: int(rt.LastTokenEndByte),
			iterations:  rt.Iterations,
			iterLimit:   rt.IterationLimit,
			nodes:       rt.NodesAllocated,
			nodeLimit:   rt.NodeLimit,
		}
		if stop.parsedBytes > len(src) {
			stop.parsedBytes = len(src)
		}
		// A parser that was stopped mid-file is not pooled. It is cheap to
		// rebuild and its arena is sized to a file that was by definition
		// unusual.
		return tree, stop, nil
	}

	reusable = true
	return tree, stop, nil
}

// tagTree runs the definitions-and-references query over a parsed tree.
//
// Behind a panic barrier for the same reason parsing is: the tree was built
// from a file this project did not write, and a query walking a malformed tree
// must cost that file's result, never the indexing run.
//
// implementers maps the start byte of a supertype reference's name to the type
// that declares it, for the one construct whose declaring type is not a symbol
// on the reference's line: Rust's `impl Trait for Type`. See implementer.go.
func (p *genericParser) tagTree(tree *ts.Tree, src []byte) (tags []ts.Tag, implementers map[uint32]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			tags, implementers, err = nil, nil, fmt.Errorf("tags query panicked: %v", r)
		}
	}()

	matches := p.tagsQ.Execute(tree)
	tags = make([]ts.Tag, 0, len(matches))
	for _, m := range matches {
		var tag ts.Tag
		var implementer string
		var impl *ts.Node
		for _, c := range m.Captures {
			switch {
			case c.Name == "implementer":
				implementer = c.Node.Text(src)
			case c.Name == "impl":
				impl = c.Node
			case c.Name == "name":
				tag.Name = cleanName(p.entry.Name, c.Node.Text(src))
				tag.NameRange = c.Node.Range()
			case strings.HasPrefix(c.Name, "definition.") || strings.HasPrefix(c.Name, "reference."):
				tag.Kind = c.Name
				tag.Range = c.Node.Range()
			}
		}
		if tag.Kind == "" {
			continue
		}
		if tag.Name == "" {
			// A pattern with no @name names the whole tagged node, which is
			// what gotreesitter's own Tagger does.
			if int(tag.Range.EndByte) <= len(src) && tag.Range.StartByte <= tag.Range.EndByte {
				tag.Name = string(src[tag.Range.StartByte:tag.Range.EndByte])
			}
			tag.NameRange = tag.Range
		}
		if implementer != "" {
			if isImplTypeParameter(p.lang, impl, implementer, src) {
				// `impl<T: Bound> Trait for T` names no single type.
				continue
			}
			if implementers == nil {
				implementers = map[uint32]string{}
			}
			implementers[tag.NameRange.StartByte] = implementer
		}
		tags = append(tags, tag)
	}
	return tags, implementers, nil
}

// describeParse sets the result's status from what the parser reported.
//
// Three outcomes, each a statement about the parse and none about what was
// extracted:
//
//   - the parser stopped before the end of the file: partial, with how far it
//     got and which limit stopped it;
//   - it reached the end and reported error or missing nodes: partial, with
//     how many and where the first one is;
//   - it reached the end with neither: ok.
func (p *genericParser) describeParse(res *Result, tree *ts.Tree, stop stopInfo, src []byte) {
	if stop.early {
		res.Status = StatusPartial
		res.ParsedBytes = stop.parsedBytes
		res.Reason = model.ParseReasonBudgetExceeded
		if !stop.isBudget() {
			res.Reason = model.ParseReasonStoppedEarly
		}
		line := 1 + bytes.Count(src[:stop.parsedBytes], []byte{'\n'})
		res.Err = errors.New(model.FormatParseError(res.Reason, fmt.Sprintf(
			"parser stopped at byte %d of %d (line %d) on %s; definitions after that point are missing",
			stop.parsedBytes, len(src), line, stop.describe(p.cfg.wallClockCeiling()))))
		return
	}

	root := tree.RootNode()
	if root == nil || !root.HasError() {
		return
	}
	errNodes, missing, firstLine := countSyntaxErrors(root)
	if errNodes == 0 && missing == 0 {
		// HasError without a node to show for it. Reported as it is and not
		// rounded down to "clean": the parser said the tree has an error.
		firstLine = int(root.StartPoint().Row) + 1
	}
	res.Status = StatusPartial
	res.Reason = model.ParseReasonSyntaxErrors
	res.ErrorNodes, res.MissingTokens, res.FirstErrorLine = errNodes, missing, firstLine
	res.Err = errors.New(model.FormatParseError(res.Reason, fmt.Sprintf(
		"%s and %s, first at line %d; definitions inside the regions the parser could not read may be missing%s",
		plural(errNodes, "error node"), plural(missing, "missing token"), firstLine, res.maskNote())))
}

// isBudget reports whether the stop was one of the limits that bound a parse,
// as opposed to the parser giving up for a reason of its own.
func (s stopInfo) isBudget() bool {
	switch s.reason {
	case ts.ParseStopIterationLimit, ts.ParseStopNodeLimit, ts.ParseStopStackDepthLimit,
		ts.ParseStopMemoryBudget, ts.ParseStopTimeout:
		return true
	}
	return false
}

// describe names the limit that stopped the parse, in words a person can act
// on, and says so when the limit is one that depends on the machine.
func (s stopInfo) describe(ceiling time.Duration) string {
	switch s.reason {
	case ts.ParseStopIterationLimit:
		return fmt.Sprintf("its iteration limit (%d of %d)", s.iterations, s.iterLimit)
	case ts.ParseStopNodeLimit:
		return fmt.Sprintf("its node limit (%d of %d)", s.nodes, s.nodeLimit)
	case ts.ParseStopStackDepthLimit:
		return "its stack depth limit"
	case ts.ParseStopMemoryBudget:
		if s.memSource == "hard_ceiling" || strings.HasPrefix(s.memSource, "runtime") {
			return "the process memory ceiling (a safety net that depends on what else the process held; the result can differ between runs)"
		}
		return "its memory budget"
	case ts.ParseStopTimeout:
		return fmt.Sprintf("the %s wall-clock ceiling (a safety net that depends on machine speed; the result can differ between runs)", ceiling)
	default:
		return fmt.Sprintf("parser stop reason %q", s.reason)
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// countSyntaxErrors counts the ERROR and MISSING nodes under root and finds the
// line of the earliest.
//
// Only subtrees the parser flagged are entered, so a clean tree costs one
// check and a tree with one error costs a walk down to it. An explicit stack,
// not recursion: a file of fifty thousand unbalanced braces produces a tree
// that deep.
func countSyntaxErrors(root *ts.Node) (errNodes, missing, firstLine int) {
	stack := []*ts.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == nil {
			continue
		}
		switch {
		case n.IsMissing():
			missing++
		case n.IsError():
			errNodes++
		}
		if n.IsMissing() || n.IsError() {
			if line := int(n.StartPoint().Row) + 1; firstLine == 0 || line < firstLine {
				firstLine = line
			}
		}
		if n.IsMissing() || !n.HasError() {
			continue
		}
		for i := n.ChildCount() - 1; i >= 0; i-- {
			if c := n.Child(i); c != nil && (c.HasError() || c.IsError() || c.IsMissing()) {
				stack = append(stack, c)
			}
		}
	}
	return errNodes, missing, firstLine
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
// line with different counts would be ambiguous, so those are dropped rather
// than guessed: see extractCounts.
//
// Receivers are not matched this way any more. shape.go reads them from the
// tag's own two ranges, which identify the call exactly.
type callSite struct {
	line int
	name string
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
		n := p.arity.count(list, p.lang, src)
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
func (d arityDialect) count(list *ts.Node, lang *ts.Language, src []byte) model.Arity {
	if list == nil {
		return model.ArityUnknown
	}
	total, ok := d.countItems(list, lang, src, 0)
	if !ok {
		return model.ArityUnknown
	}
	return model.KnownArity(total)
}

// countItems sums the items of a list. ok is false when the list holds
// anything the dialect cannot count exactly: that makes the whole count
// unknown, never a guess.
func (d arityDialect) countItems(list *ts.Node, lang *ts.Language, src []byte, depth int) (total int, ok bool) {
	if depth > 3 {
		return 0, false
	}
	if d.voidList != nil && d.voidList(list, lang, src) {
		return 0, true
	}
	for i := 0; i < list.ChildCount(); i++ {
		c := list.Child(i)
		if c == nil || c.IsExtra() {
			continue
		}
		typ := c.Type(lang)
		if !c.IsNamed() {
			// An anonymous token can still make the list open-ended: C's `...`.
			if d.variadic[typ] {
				return 0, false
			}
			continue
		}
		closed := d.items != nil && (len(d.closedIn) == 0 || d.closedIn[list.Type(lang)])
		switch {
		case d.opaque != nil && d.opaque(c, lang, src):
			return 0, false
		case d.skip[typ]:
			continue
		case d.flatten[typ]:
			n, ok := d.countItems(c, lang, src, depth+1)
			if !ok {
				return 0, false
			}
			total += n
		case d.isVariadic(c, lang):
			// Open-ended on one side means the two sides cannot be compared at
			// all. Neither `func v(xs ...int)` nor `v(a, b, c)` is wrong about
			// the other, so no count is published for either.
			return 0, false
		case d.group[typ]:
			total += namedFieldCount(c, lang)
		case closed && !d.items[typ]:
			// A closed dialect: a node it never classified (a default value
			// written beside its parameter, a `params` array the grammar
			// leaves bare) makes the list unknown.
			return 0, false
		default:
			total++
		}
	}
	return total, true
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
			case "directive":
				imp.Directive = text
			}
		}
		if imp.Path != "" {
			out = append(out, imp)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if keepsEveryDirective[p.entry.Name] {
		return dedupeDirectives(out)
	}
	return dedupeImports(out)
}

// mergeMemberImports appends the members a file imported by name to the
// modules it imported, dropping exact repeats.
//
// The two lists do not compete: `from shop.models import Account` yields the
// module row the import query already produced AND a member row, and both are
// kept. The module row is what the import tier measures a candidate's file
// against; the member row is what says WHICH name that module was imported for.
func mergeMemberImports(imports, members []Import) []Import {
	if len(members) == 0 {
		return imports
	}
	type key struct{ path, alias string }
	seen := make(map[key]struct{}, len(imports)+len(members))
	for _, imp := range imports {
		seen[key{imp.Path, imp.Alias}] = struct{}{}
	}
	for _, m := range members {
		k := key{m.Path, m.Alias}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		imports = append(imports, m)
	}
	sort.SliceStable(imports, func(i, j int) bool {
		if imports[i].Path != imports[j].Path {
			return imports[i].Path < imports[j].Path
		}
		return imports[i].Alias < imports[j].Alias
	})
	return imports
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
	case "function", "function.macro":
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
// reference can bind to it — the language guarantees that — and `repocortex
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
