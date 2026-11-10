package scip

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	scippb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ErrNoMetadata reports an index whose Metadata message never arrived, or
// arrived after the first document.
//
// This is fatal rather than tolerated because Metadata carries ToolInfo, and
// ToolInfo is what every emitted Evidence.Detail names. An index ingested
// without it would produce exact edges that cannot say which compiler proved
// them, which is provenance in name only. Real indexers write metadata first —
// it is field 1 — so hitting this means the file is truncated at the front or
// is not a SCIP index at all.
var ErrNoMetadata = errors.New("scip: index has no metadata before its first document")

// DefaultMaxBytes caps how much of an index will be read. It exists because a
// .scip file arrives with the repository and is therefore untrusted input; a
// bogus length prefix should end the ingest rather than stream forever.
const DefaultMaxBytes int64 = 8 << 30

// DefaultMaxMessageBytes caps one Document. Real documents are kilobytes;
// 256 MiB is far past anything a compiler emits for a single source file and
// still small enough that a hostile value fails as an error rather than as an
// out-of-memory kill.
const DefaultMaxMessageBytes int64 = 256 << 20

const defaultUnmatchedSamples = 20

// Options configures an Ingester.
type Options struct {
	// RepoRoot is RepoCortex's repository root, used to place the index's
	// project_root inside the repository. Optional: without it, path
	// reconciliation falls back to matching document paths directly.
	RepoRoot string

	// KnownPaths is every repo-relative path RepoCortex has indexed.
	//
	// It is what makes path reconciliation checkable. Leave it nil and the
	// ingester runs unverified: paths are computed but never confirmed, and the
	// mismatch count in Stats is reported as meaningless rather than zero.
	KnownPaths []string

	// PathPrefix forces the repo-relative directory that the index's documents
	// hang off, for the case where project_root is an absolute path from a
	// build container. When set, no fallback strategy is tried — see
	// StrategyExplicit.
	PathPrefix string

	// MaxBytes caps the index size. Zero means DefaultMaxBytes; negative means
	// no cap.
	MaxBytes int64

	// MaxMessageBytes caps a single Metadata, Document or external symbol
	// submessage. Zero means DefaultMaxMessageBytes.
	//
	// This is the real defence against a hostile index, not MaxBytes: the
	// parser allocates a buffer of the declared submessage length before
	// reading it, so a bogus prefix is an out-of-memory kill rather than a
	// long read. See framingReader.
	MaxMessageBytes int64

	// MaxUnmatchedSamples bounds how many unmatched document paths Stats
	// retains. Zero means 20. The sample is what turns "412 documents did not
	// match" into a diagnosable report, but keeping all of them on a
	// wholly-mismatched index would be its own memory problem.
	MaxUnmatchedSamples int
}

// Stats is the report of one ingest.
//
// Its job is to make silent failure impossible. An index whose paths do not
// line up produces zero edges and no error, which is indistinguishable from a
// repository that genuinely has no cross-references unless the numbers below
// are surfaced.
type Stats struct {
	Tool            Tool
	ProjectRoot     string
	ProtocolVersion string
	// PositionEncoding is the first matched document's encoding. It is reported
	// because emitted columns are in whatever unit the indexer used — see
	// toModelRange — so a consumer that needs byte columns can tell when a
	// conversion is owed.
	PositionEncoding string

	BytesRead int64

	Documents          int64
	DocumentsMatched   int64
	DocumentsUnmatched int64
	// UnmatchedPaths is a bounded sample of document paths that matched no
	// indexed file, in encounter order.
	UnmatchedPaths []string
	// PathStrategies counts how each matched document was reconciled.
	PathStrategies map[Strategy]int64
	// PathsVerified is false when Options.KnownPaths was nil, in which case
	// DocumentsUnmatched is not a meaningful number.
	PathsVerified bool

	Occurrences int64
	// SyntaxOnly counts occurrences carrying no symbol. Indexers emit these for
	// syntax highlighting; they are not resolution facts.
	SyntaxOnly int64
	// Rangeless counts occurrences dropped because they name a symbol but have
	// no source range. Such an occurrence cannot be evidence, and evidence is
	// the point, so it is dropped — but counted, because a large number here
	// means the index is malformed rather than that the code has no references.
	Rangeless        int64
	Definitions      int64
	References       int64
	ImportReferences int64
	ReadAccesses     int64
	WriteAccesses    int64

	Symbols   int64
	Relations int64
	// FileAnchored counts references that could not be attributed to an
	// enclosing definition and were anchored to the file symbol instead.
	FileAnchored int64
	// DuplicateDefinitions counts definition occurrences dropped because the
	// same symbol was already defined on the same line of the same file.
	DuplicateDefinitions int64
	// ExternalSymbols counts Index.external_symbols entries, which are
	// deliberately not emitted: they describe symbols in dependencies that have
	// no file in this repository to hang off.
	ExternalSymbols int64
	// ImplementsRelations counts edges derived from SymbolInformation
	// relationships rather than from occurrences.
	ImplementsRelations int64
}

// Summary renders the numbers that matter when an ingest looks wrong.
func (s Stats) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d document(s), %d symbol(s), %d relation(s)",
		s.Tool, s.Documents, s.Symbols, s.Relations)
	if !s.PathsVerified {
		b.WriteString("; paths unverified (no file list supplied)")
		return b.String()
	}
	fmt.Fprintf(&b, "; %d matched, %d unmatched", s.DocumentsMatched, s.DocumentsUnmatched)
	if s.DocumentsUnmatched > 0 {
		fmt.Fprintf(&b, " (e.g. %s)", strings.Join(s.UnmatchedPaths, ", "))
	}
	return b.String()
}

// EmitFunc receives one document's facts. It is called once per matched
// document, from the goroutine that called Ingest, and must not retain f —
// the ingester reuses nothing, but the caller writing f to the registry and
// dropping it is what keeps peak memory proportional to one document.
//
// Returning an error aborts the ingest and is reported to the caller of Ingest.
type EmitFunc func(ctx context.Context, f *FileFacts) error

// Ingester converts SCIP indexes to RepoCortex facts. It is immutable after New
// and safe for concurrent use; each Ingest call carries its own state.
type Ingester struct {
	opts Options
}

// New returns an Ingester configured by opts.
func New(opts Options) *Ingester {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if opts.MaxUnmatchedSamples <= 0 {
		opts.MaxUnmatchedSamples = defaultUnmatchedSamples
	}
	return &Ingester{opts: opts}
}

// IngestFile streams the .scip file at path.
func (ing *Ingester) IngestFile(ctx context.Context, path string, emit EmitFunc) (Stats, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied index path; contents are treated as untrusted
	if err != nil {
		return Stats{}, fmt.Errorf("scip: open index: %w", err)
	}
	defer func() { _ = f.Close() }()

	st, err := ing.Ingest(ctx, f, emit)
	if err != nil {
		return st, fmt.Errorf("scip: %s: %w", path, err)
	}
	return st, nil
}

// Ingest streams a SCIP index from r, calling emit once per matched document.
//
// Memory is bounded by the largest single document, not by the index: the
// underlying parser decodes one Document submessage at a time and this function
// keeps no cross-document state beyond counters and the path map the caller
// supplied.
//
// The SCIP -> model mapping, in one place:
//
//   - A Document becomes one FileFacts, plus a file-scope anchor symbol.
//   - An Occurrence with the Definition role becomes a model.Symbol whose FQN
//     is the SCIP symbol string verbatim. Preserving it matters: SCIP symbols
//     are stable across runs and repositories, so exact and heuristic facts
//     about the same entity converge on one graph node, which synthesised
//     path:name FQNs cannot do.
//   - Every other Occurrence becomes a Relation from the innermost definition
//     enclosing it to the symbol it names. Where no enclosing definition is
//     known the relation is anchored to the file symbol — the compiler still
//     proved that this file references that symbol, so nothing is invented.
//   - A SymbolInformation relationship with is_implementation becomes an
//     IMPLEMENTS relation.
//
// The ForwardDefinition role (a C/C++ forward declaration, an OCaml interface
// `val`) is treated as a reference, not a definition: it declares that a symbol
// exists elsewhere, and making it a second definition would give the symbol two
// homes and make "go to definition" ambiguous.
//
// Deliberately ignored: Index.external_symbols (no file to attach them to),
// Occurrence.syntax_kind (highlighting), Occurrence.diagnostics,
// Document.text, and is_type_definition/is_definition relationships. See
// edgeKind for why no CALLS, READS or WRITES edges are produced HERE — a call
// cannot be recognised from an occurrence's roles, and Ingest sees nothing but
// the index. CALLS edges are produced one step later by PromoteCalls, which
// joins these references against the parser's own record of where a call
// expression sits; calls.go carries that argument.
func (ing *Ingester) Ingest(ctx context.Context, r io.Reader, emit EmitFunc) (st Stats, err error) {
	if emit == nil {
		return Stats{}, errors.New("scip: Ingest requires an EmitFunc")
	}

	run := &ingestRun{
		opts:  ing.opts,
		paths: newPathMapper(ing.opts),
		emit:  emit,
	}
	run.st.PathStrategies = map[Strategy]int64{}
	run.st.PathsVerified = run.paths.verifying()

	// A .scip file is untrusted input and the framing parser lives in a third-
	// party package. A malformed length prefix that trips a slice bound there
	// must surface as an error, not as a crashed indexer. A panic raised by the
	// caller's own EmitFunc is re-raised untouched: swallowing it would turn
	// their bug into our error message.
	defer func() {
		if p := recover(); p != nil {
			if run.emitting {
				panic(p)
			}
			// Hand back the counters gathered so far: knowing it died on
			// document 4,112 is most of the diagnosis.
			st = run.st
			err = fmt.Errorf("scip: malformed index: recovered from %v", p)
		}
	}()

	counted := &framingReader{r: r, max: ing.opts.MaxBytes, maxMessage: ing.opts.MaxMessageBytes}
	buffered := bufio.NewReaderSize(counted, 64<<10)

	visitor := scippb.IndexVisitor{
		VisitMetadata:       run.visitMetadata,
		VisitDocument:       run.visitDocument,
		VisitExternalSymbol: run.visitExternalSymbol,
	}
	perr := visitor.ParseStreaming(ctx, buffered)
	run.st.BytesRead = counted.n
	if perr != nil {
		return run.st, fmt.Errorf("scip: parse index after %d byte(s): %w", counted.n, perr)
	}
	if !run.sawMetadata {
		return run.st, ErrNoMetadata
	}
	return run.st, nil
}

// Collect ingests the whole index into memory.
//
// It exists for tests and for indexes small enough that the convenience is
// worth it. It gives up the memory bound Ingest provides, so it must not be
// used on the indexes this package was written for.
func Collect(ctx context.Context, r io.Reader, opts Options) ([]FileFacts, Stats, error) {
	var out []FileFacts
	st, err := New(opts).Ingest(ctx, r, func(_ context.Context, f *FileFacts) error {
		out = append(out, *f)
		return nil
	})
	return out, st, err
}

// ingestRun is the mutable state of a single Ingest call.
type ingestRun struct {
	opts  Options
	paths *pathMapper
	emit  EmitFunc
	st    Stats

	sawMetadata bool
	sawDocument bool
	emitting    bool
}

func (run *ingestRun) visitMetadata(_ context.Context, m *scippb.Metadata) error {
	if run.sawDocument {
		// Documents were already emitted with no tool recorded. Their evidence
		// would name "unknown indexer", which is worse than refusing.
		return ErrNoMetadata
	}
	run.sawMetadata = true
	run.st.Tool = Tool{Name: m.GetToolInfo().GetName(), Version: m.GetToolInfo().GetVersion()}
	run.st.ProjectRoot = m.GetProjectRoot()
	run.st.ProtocolVersion = m.GetVersion().String()
	run.paths.setProjectRoot(m.GetProjectRoot())
	return nil
}

func (run *ingestRun) visitExternalSymbol(_ context.Context, _ *scippb.SymbolInformation) error {
	run.st.ExternalSymbols++
	return nil
}

func (run *ingestRun) visitDocument(ctx context.Context, d *scippb.Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !run.sawMetadata {
		return ErrNoMetadata
	}
	run.sawDocument = true
	run.st.Documents++

	repoPath, strategy, ok := run.paths.resolve(d.GetRelativePath())
	if !ok {
		run.st.DocumentsUnmatched++
		if len(run.st.UnmatchedPaths) < run.opts.MaxUnmatchedSamples {
			run.st.UnmatchedPaths = append(run.st.UnmatchedPaths, d.GetRelativePath())
		}
		return nil
	}
	run.st.DocumentsMatched++
	run.st.PathStrategies[strategy]++
	if run.st.PositionEncoding == "" {
		run.st.PositionEncoding = d.GetPositionEncoding().String()
	}

	facts := run.convert(d, repoPath)

	run.emitting = true
	err := run.emit(ctx, facts)
	run.emitting = false
	return err
}

// definition is a symbol defined in the document being converted.
type definition struct {
	fqn         string
	placeholder int64
	name        scippb.Range // the identifier itself
	enclosing   scippb.Range // the body, when the indexer recorded one
	hasBody     bool
}

// convert turns one document into FileFacts. Everything it allocates is scoped
// to this call, which is what bounds ingest memory by document.
func (run *ingestRun) convert(d *scippb.Document, repoPath string) *FileFacts {
	facts := &FileFacts{
		Path:     repoPath,
		Language: d.GetLanguage(),
		Tool:     run.st.Tool,
	}

	// Placeholder -1 is the file-scope anchor. Every document gets one because
	// model edges join symbol to symbol: a reference the indexer did not place
	// inside any definition still has to start somewhere, and "this file
	// references that symbol" is a fact the compiler actually established.
	// Inventing a plausible enclosing function instead would be the kind of
	// guess that must never carry confidence=exact.
	anchor := model.Symbol{
		ID:         -1,
		Kind:       model.KindFile,
		Name:       repoPath[strings.LastIndex(repoPath, "/")+1:],
		FQN:        repoPath,
		Confidence: model.ConfidenceExact,
		Source:     model.SourceCompiler,
		Exported:   true,
	}
	facts.Symbols = append(facts.Symbols, anchor)
	run.st.Symbols++

	symtab := d.SymbolTable()
	defs := make(map[string]*definition, len(d.GetSymbols()))
	seen := make(map[definitionKey]struct{}, len(d.GetSymbols()))
	next := int64(-2)

	// Pass 1: definitions. They must all exist before references are attributed,
	// because a reference may precede the definition it sits inside only in
	// pathological output, but may freely precede a *sibling* definition it
	// points at.
	for _, occ := range d.GetOccurrences() {
		run.st.Occurrences++
		sym := occ.GetSymbol()
		if sym == "" {
			run.st.SyntaxOnly++
			continue
		}
		roles := occ.GetSymbolRoles()
		if roles&int32(scippb.SymbolRole_ReadAccess) != 0 {
			run.st.ReadAccesses++
		}
		if roles&int32(scippb.SymbolRole_WriteAccess) != 0 {
			run.st.WriteAccesses++
		}
		if roles&int32(scippb.SymbolRole_Definition) == 0 {
			continue
		}
		rng, hasRange := occ.SourceRange()
		if !hasRange {
			run.st.Rangeless++
			continue
		}
		run.st.Definitions++

		fqn := qualifyFQN(repoPath, sym)
		key := definitionKey{fqn: fqn, line: rng.Start.Line}
		if _, dup := seen[key]; dup {
			// The registry's UNIQUE(run, fqn, file_id, start_line) would reject
			// the second row; failing the whole file over a duplicate the
			// indexer emitted is not worth it.
			run.st.DuplicateDefinitions++
			continue
		}
		seen[key] = struct{}{}

		def := &definition{fqn: fqn, placeholder: next, name: rng}
		if enc, ok := occ.EnclosingSourceRange(); ok {
			def.enclosing, def.hasBody = enc, true
		}
		defs[sym] = def
		next--

		facts.Symbols = append(facts.Symbols, symbolFor(sym, fqn, def.placeholder, rng, symtab[sym]))
		run.st.Symbols++
	}

	scopes := buildScopes(defs)

	// Pass 2: references.
	for _, occ := range d.GetOccurrences() {
		sym := occ.GetSymbol()
		if sym == "" || occ.GetSymbolRoles()&int32(scippb.SymbolRole_Definition) != 0 {
			continue
		}
		rng, ok := occ.SourceRange()
		if !ok {
			run.st.Rangeless++
			continue
		}
		run.st.References++
		kind := edgeKind(occ.GetSymbolRoles())
		if kind == model.EdgeImports {
			run.st.ImportReferences++
		}

		src := scopes.enclosing(rng.Start)
		if src == "" {
			src = repoPath
			run.st.FileAnchored++
		}
		facts.Relations = append(facts.Relations, Relation{
			SrcFQN: src,
			DstFQN: qualifyFQN(repoPath, sym),
			Kind:   kind,
			Evidence: model.Evidence{
				FilePath: repoPath,
				Range:    toModelRange(rng),
				Detail:   run.st.Tool.Detail(),
			},
		})
		run.st.Relations++
	}

	// Pass 3: implementation relationships. These are not occurrences — the
	// indexer states them about a symbol — so they are the one place SCIP
	// gives an interface/implementation link directly, and it is worth having:
	// "who implements this interface" is otherwise unanswerable without a
	// typechecker.
	for _, si := range d.GetSymbols() {
		def, defined := defs[si.GetSymbol()]
		if !defined {
			continue
		}
		for _, rel := range si.GetRelationships() {
			if !rel.GetIsImplementation() || rel.GetSymbol() == "" {
				continue
			}
			facts.Relations = append(facts.Relations, Relation{
				SrcFQN: def.fqn,
				DstFQN: qualifyFQN(repoPath, rel.GetSymbol()),
				Kind:   model.EdgeImplements,
				Evidence: model.Evidence{
					FilePath: repoPath,
					// The declaring identifier, not the body: a relationship
					// stated about a symbol is evidenced by where that symbol
					// is declared, and the body range is often absent anyway.
					Range:  toModelRange(def.name),
					Detail: run.st.Tool.Detail(),
				},
			})
			run.st.Relations++
			run.st.ImplementsRelations++
		}
	}

	return facts
}

type definitionKey struct {
	fqn  string
	line int32
}

func symbolFor(sym, fqn string, placeholder int64, rng scippb.Range, si *scippb.SymbolInformation) model.Symbol {
	return model.Symbol{
		ID:        placeholder,
		Kind:      entityKind(si.GetKind(), sym),
		Name:      displayName(sym, si),
		FQN:       fqn,
		Range:     toModelRange(rng),
		Signature: si.GetSignatureDocumentation().GetText(),
		Doc:       strings.TrimSpace(strings.Join(si.GetDocumentation(), "\n\n")),
		Exported:  exported(sym),
		// A symbol the typechecker resolved is as exact as facts get; recording
		// it as anything weaker would make every edge into it look better
		// evidenced than the node it points at.
		Confidence: model.ConfidenceExact,
		Source:     model.SourceCompiler,
	}
}

// scope is one definition's body extent.
type scope struct {
	rng scippb.Range
	fqn string
}

// scopeIndex answers "which definition lexically encloses this position".
type scopeIndex []scope

// buildScopes indexes the definitions that carry an enclosing (body) range.
//
// Containment here is not an inference: the enclosing range is what the indexer
// recorded as the definition's extent. Lexical scopes nest, so scanning
// backwards from the last scope starting at or before the position and taking
// the first that contains it yields the innermost enclosing definition.
func buildScopes(defs map[string]*definition) scopeIndex {
	idx := make(scopeIndex, 0, len(defs))
	for _, d := range defs {
		if d.hasBody {
			idx = append(idx, scope{rng: d.enclosing, fqn: d.fqn})
		}
	}
	// Ties broken by FQN so the result does not depend on Go's map iteration
	// order; two definitions sharing a start position is pathological, but a
	// non-deterministic index would be worse than a wrong one.
	sort.Slice(idx, func(a, b int) bool {
		if c := idx[a].rng.Start.Compare(idx[b].rng.Start); c != 0 {
			return c < 0
		}
		return idx[a].fqn < idx[b].fqn
	})
	return idx
}

func (idx scopeIndex) enclosing(pos scippb.Position) string {
	i := sort.Search(len(idx), func(i int) bool { return idx[i].rng.Start.Compare(pos) > 0 })
	for i--; i >= 0; i-- {
		if idx[i].rng.Contains(pos) {
			return idx[i].fqn
		}
	}
	return ""
}

// qualifyFQN turns a SCIP symbol into a repository-unique FQN.
//
// Global symbols pass through untouched — that stability is the reason to use
// them at all. Local symbols ("local 0") are document-scoped by definition and
// collide across every file in the index, so they are prefixed with the
// document path. Without this, `local 0` in two files would be merged into one
// graph node and every reference to either would appear to reach both.
func qualifyFQN(repoPath, symbol string) string {
	if scippb.IsLocalSymbol(symbol) {
		return repoPath + "#" + symbol
	}
	return symbol
}

func displayName(sym string, si *scippb.SymbolInformation) string {
	if n := si.GetDisplayName(); n != "" {
		return n
	}
	if scippb.IsLocalSymbol(sym) {
		return strings.TrimPrefix(sym, "local ")
	}
	// Only definitions reach here — a few thousand per index rather than the
	// hundreds of thousands of occurrences — so the full symbol parser is
	// affordable, and it is the only correct way to read a name that may be
	// backtick-escaped.
	if parsed, err := scippb.ParseSymbol(sym); err == nil {
		if ds := parsed.GetDescriptors(); len(ds) > 0 {
			if n := ds[len(ds)-1].GetName(); n != "" {
				return n
			}
		}
	}
	return sym
}

// toModelRange converts SCIP's 0-indexed lines to model's 1-indexed ones.
//
// Columns are passed through unchanged, and that is a known fidelity gap:
// SCIP characters are counted in the document's PositionEncoding (UTF-16 units
// for JVM and TypeScript indexers, UTF-8 bytes for Go and Rust ones), while
// model.Position.Col is documented as a byte column. Converting would require
// reading every source file, which would undo the streaming property this
// package exists for. The encoding is reported in Stats so a consumer that
// needs byte columns knows when a conversion is owed.
func toModelRange(r scippb.Range) model.Range {
	return model.Range{
		Start: model.Position{Line: int(r.Start.Line) + 1, Col: int(r.Start.Character)},
		End:   model.Position{Line: int(r.End.Line) + 1, Col: int(r.End.Character)},
	}
}

// framingReader counts the bytes consumed and vets the index's top-level
// framing as they flow past.
//
// The vetting exists because of a specific denial of service. An Index is a
// sequence of (tag, length, submessage) frames, and the streaming parser
// allocates a buffer of the declared length before reading a single byte of the
// body. A hostile .scip — and a .scip arrives with the repository, so hostile
// is in the threat model — can declare a terabyte and take the indexer down
// with it. There is no way to stop that from outside the parser after the
// length has been read.
//
// So the length is read here first. Every byte the parser will see passes
// through this reader in order, which means the same varint can be decoded here
// one read earlier and the whole read refused before the byte that would
// complete it is ever delivered. The framing being mirrored is the protobuf
// wire format for a repeated message field, which is fixed by the format rather
// than by the library, so this does not go stale when the library changes.
type framingReader struct {
	r          io.Reader
	n          int64
	max        int64
	maxMessage int64

	state     framingState
	varint    []byte
	remaining int64
	err       error
}

type framingState int

const (
	frameTag framingState = iota
	frameLength
	frameBody
)

// maxVarintBytes is the largest number of bytes a protobuf varint may occupy.
const maxVarintBytes = 10

func (c *framingReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.max > 0 {
		if c.n >= c.max {
			c.err = fmt.Errorf("index exceeds the %d byte limit", c.max)
			return 0, c.err
		}
		if remaining := c.max - c.n; int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := c.r.Read(p)
	if n > 0 {
		if verr := c.vet(p[:n]); verr != nil {
			c.err = verr
			return 0, verr
		}
		c.n += int64(n)
	}
	return n, err
}

func (c *framingReader) vet(b []byte) error {
	for i := 0; i < len(b); {
		switch c.state {
		case frameTag:
			// Index has three fields, all with numbers below 16, so a legal tag
			// is a single byte. Anything else is not a SCIP index.
			num, typ, taken := protowire.ConsumeTag(b[i : i+1])
			if taken < 0 {
				return fmt.Errorf("bad field tag at byte %d: %w", c.n+int64(i), protowire.ParseError(taken))
			}
			if typ != protowire.BytesType {
				return fmt.Errorf("field %d at byte %d has wire type %d, want length-delimited",
					num, c.n+int64(i), typ)
			}
			if num < 1 || num > 3 {
				return fmt.Errorf("unknown Index field %d at byte %d", num, c.n+int64(i))
			}
			i++
			c.state, c.varint = frameLength, c.varint[:0]

		case frameLength:
			b0 := b[i]
			c.varint = append(c.varint, b0)
			i++
			if len(c.varint) > maxVarintBytes {
				return fmt.Errorf("overlong length prefix at byte %d", c.n+int64(i))
			}
			if b0 > 127 {
				continue // continuation bit set; more bytes to come
			}
			v, taken := protowire.ConsumeVarint(c.varint)
			if taken < 0 {
				return fmt.Errorf("bad length prefix at byte %d: %w", c.n+int64(i), protowire.ParseError(taken))
			}
			// Compare in uint64 only when maxMessage is non-negative; a
			// negative limit converted to uint64 becomes enormous and would
			// disable the bound entirely, which is exactly the OOM this
			// framing reader exists to prevent.
			if c.maxMessage < 0 || v > uint64(c.maxMessage) {
				return fmt.Errorf("submessage at byte %d declares %d bytes, over the %d byte limit",
					c.n+int64(i), v, c.maxMessage)
			}
			// v <= maxMessage (an int64) was just established, so this cannot
			// overflow. Stated as an assertion rather than a comment alone so a
			// future change to the bound cannot silently reintroduce a wrap.
			if v > uint64(math.MaxInt64) {
				return fmt.Errorf("submessage length %d exceeds int64", v)
			}
			c.remaining = int64(v)
			c.state = frameBody
			if c.remaining == 0 {
				c.state = frameTag
			}

		case frameBody:
			skip := int64(len(b) - i)
			if skip > c.remaining {
				skip = c.remaining
			}
			i += int(skip)
			c.remaining -= skip
			if c.remaining == 0 {
				c.state = frameTag
			}
		}
	}
	return nil
}
