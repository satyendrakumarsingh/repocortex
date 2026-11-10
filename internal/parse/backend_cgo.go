//go:build cgo_parser

package parse

// The escape hatch: symbol extraction on the official C tree-sitter runtime.
//
// Built only with `-tags cgo_parser`, and requires a C toolchain. Everything in
// this file is absent from the default build, which is why the zero-CGO promise
// survives its existence. If you have arrived here because gotreesitter has let
// you down, read BACKENDS.md first — it says what this backend does and does
// not cover.
//
// GRAMMAR SCOPE
//
// Four languages: go, java, python, typescript. Not 206. Each additional
// grammar is a separate C dependency that must be compiled on every build, and
// the point of this backend is to be a credible fallback for the languages the
// differential test can actually hold it to, not to duplicate the pure-Go
// grammar set.
//
// LICENSING: github.com/tree-sitter/go-tree-sitter and the four grammar
// packages are MIT, published by the tree-sitter organisation itself. The tags
// queries below are derived from each grammar's queries/tags.scm in those same
// MIT repositories.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	cts "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tspython "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tstypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// ActiveBackendName is the runtime this binary parses with.
const ActiveBackendName = "cgo"

// ActiveBackend returns the backend this binary was built with.
//
// Under `-tags cgo_parser` that is the C runtime. It falls back to the pure-Go
// backend only if the C runtime cannot be initialised at all, because a build
// that asked for CGO and silently got something else would be worse than a
// clear failure — and a build that asked for CGO and got nothing would be
// worse still.
func ActiveBackend() Backend {
	b, err := CGOBackend()
	if err != nil {
		return PureGoBackend()
	}
	return b
}

// CGOBackend returns the official C tree-sitter runtime.
func CGOBackend() (Backend, error) { return newCGOBackend() }

// cgoLanguage pairs a compiled-in grammar with the tags query RepoCortex runs
// against it.
type cgoLanguage struct {
	name  string
	lang  *cts.Language
	query *cts.Query
}

type cgoBackend struct {
	langs map[string]*cgoLanguage

	// Query cursors are cheap but not concurrency-safe, and neither is a
	// Parser. Both are pooled per-goroutine rather than locked, because
	// indexing runs one parse per worker and a global lock would serialise the
	// whole pipeline behind the C runtime.
	pool sync.Pool
}

// cgoTagsQueries are the tags queries run against the official grammars.
//
// Each is derived from the grammar repository's own queries/tags.scm, with two
// deliberate departures. Both are documented here because they are exactly the
// kind of silent difference that would make a differential score meaningless.
//
//  1. The `(comment)* @doc ... (#strip! ...) (#set-adjacent! ...)` wrappers are
//     removed. RepoCortex does not extract docstrings, and those predicates are
//     tags-CLI conventions that the query engine has no reason to honour.
//     Keeping them would change nothing about the symbols and risk a query
//     compile error.
//
//  2. Where upstream tags.scm is thinner than RepoCortex's shipped coverage, the
//     missing categories are added against the official grammar's node names —
//     Java enums, records, constructors and fields; TypeScript classes and
//     methods that upstream leaves to the inherited JavaScript query; Python
//     decorated functions. This is the same coverage INTENT as
//     queries.go/supplements, expressed in a different grammar's vocabulary.
//
// Point 2 is a limitation of the differential test as much as a feature of this
// backend: matching the intent is what makes the comparison measure the two
// RUNTIMES rather than two unrelated query authors. It does not prove that
// gotreesitter's query INFERENCE agrees with upstream tags.scm — it does not,
// and BACKENDS.md says so.
var cgoTagsQueries = map[string]string{
	// Upstream Go tags.scm captures var/const specs as a bare @name with no
	// @definition capture, which yields a symbol with no kind. A kindless
	// symbol is not usable, so the kinds RepoCortex's own Go supplement assigns
	// are applied here.
	// The four type patterns must match queries.go exactly. They are what
	// distinguish a struct from an interface from a named type from a real
	// alias; collapsing them to one (type_spec ...) rule made every Go type an
	// alias on both backends at once, which is precisely the kind of shared
	// blind spot the differential test cannot see.
	"go": `
(function_declaration name: (identifier) @name) @definition.function
(method_declaration name: (field_identifier) @name) @definition.method
(type_spec name: (type_identifier) @name type: (struct_type)) @definition.class
(type_spec name: (type_identifier) @name type: (interface_type)) @definition.interface
(type_spec name: (type_identifier) @name) @definition.type
(type_alias name: (type_identifier) @name) @definition.type_alias
(const_spec name: (identifier) @name) @definition.constant
(var_spec name: (identifier) @name) @definition.variable
`,

	// Upstream Java tags.scm covers only classes, methods and interfaces.
	// Enums, records, constructors and fields are how most Java structure is
	// actually expressed, and RepoCortex's pure-Go supplement captures them.
	"java": `
(class_declaration name: (identifier) @name) @definition.class
(interface_declaration name: (identifier) @name) @definition.interface
(enum_declaration name: (identifier) @name) @definition.enum
(record_declaration name: (identifier) @name) @definition.class
(annotation_type_declaration name: (identifier) @name) @definition.interface
(method_declaration name: (identifier) @name) @definition.method
(constructor_declaration name: (identifier) @name) @definition.method
(field_declaration declarator: (variable_declarator name: (identifier) @name)) @definition.field
`,

	// The module-scope assignment pattern from upstream tags.scm is kept even
	// though gotreesitter's inference has no equivalent. Dropping it to
	// flatter the score would hide a real coverage difference; the differential
	// report lists it instead.
	"python": `
(class_definition name: (identifier) @name) @definition.class
(function_definition name: (identifier) @name) @definition.function
(decorated_definition definition: (function_definition name: (identifier) @name)) @definition.function
(module (expression_statement (assignment left: (identifier) @name) @definition.constant))
`,

	// tree-sitter-typescript's tags.scm holds only the TypeScript-specific
	// patterns; the tags CLI composes it with tree-sitter-javascript's, which
	// is where class_declaration, function_declaration and method_definition
	// live. That composition is not available through the Go bindings, so the
	// inherited patterns are written out here against the TypeScript grammar.
	"typescript": `
(class_declaration name: (type_identifier) @name) @definition.class
(abstract_class_declaration name: (type_identifier) @name) @definition.class
(interface_declaration name: (type_identifier) @name) @definition.interface
(type_alias_declaration name: (type_identifier) @name) @definition.type_alias
(enum_declaration name: (identifier) @name) @definition.enum
(function_declaration name: (identifier) @name) @definition.function
(function_signature name: (identifier) @name) @definition.function
(generator_function_declaration name: (identifier) @name) @definition.function
(method_definition name: (property_identifier) @name) @definition.method
(method_signature name: (property_identifier) @name) @definition.method
(abstract_method_signature name: (property_identifier) @name) @definition.method
(public_field_definition name: (property_identifier) @name) @definition.field
`,
}

// cgoGrammars maps a RepoCortex language name to the compiled-in grammar.
//
// The names are RepoCortex's (matching gotreesitter's registry), not the C
// packages', so that a caller never has to know which backend it is talking to.
func cgoGrammars() map[string]func() *cts.Language {
	return map[string]func() *cts.Language{
		"go":         func() *cts.Language { return cts.NewLanguage(tsgo.Language()) },
		"java":       func() *cts.Language { return cts.NewLanguage(tsjava.Language()) },
		"python":     func() *cts.Language { return cts.NewLanguage(tspython.Language()) },
		"typescript": func() *cts.Language { return cts.NewLanguage(tstypescript.LanguageTypescript()) },
	}
}

func newCGOBackend() (Backend, error) {
	b := &cgoBackend{langs: make(map[string]*cgoLanguage)}
	b.pool = sync.Pool{New: func() any { return cts.NewParser() }}

	var failures []string
	for name, load := range cgoGrammars() {
		lang := load()
		if lang == nil {
			failures = append(failures, name+": grammar returned nil")
			continue
		}

		// SetLanguage is the ABI check. A grammar generated for an older
		// tree-sitter ABI than this runtime supports fails here rather than
		// producing a silently empty tree, so a mismatched grammar version
		// degrades that language instead of the whole backend.
		probe := cts.NewParser()
		err := probe.SetLanguage(lang)
		probe.Close()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		q, qerr := cts.NewQuery(lang, cgoTagsQueries[name])
		if qerr != nil {
			failures = append(failures, fmt.Sprintf("%s: tags query rejected: %v", name, qerr))
			continue
		}
		b.langs[name] = &cgoLanguage{name: name, lang: lang, query: q}
	}

	if len(b.langs) == 0 {
		return nil, fmt.Errorf("cgo backend loaded no grammars: %s", strings.Join(failures, "; "))
	}
	return b, nil
}

func (b *cgoBackend) Name() string { return "cgo" }

func (b *cgoBackend) Supports(lang string) bool {
	_, ok := b.langs[lang]
	return ok
}

func (b *cgoBackend) Languages() []string {
	out := make([]string, 0, len(b.langs))
	for k := range b.langs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (b *cgoBackend) Symbols(ctx context.Context, lang string, src []byte) (syms []TaggedSymbol, err error) {
	cl, ok := b.langs[lang]
	if !ok {
		return nil, fmt.Errorf("cgo backend has no grammar for %q", lang)
	}
	if len(src) == 0 {
		return nil, nil
	}
	if len(src) > MaxSourceBytes {
		return nil, fmt.Errorf("source is %d bytes, over the %d limit", len(src), MaxSourceBytes)
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}

	// Same threat model as the pure-Go path: RepoCortex indexes repositories it
	// did not write, and a hostile file must cost one file's result. A panic
	// crossing the CGO boundary is if anything more likely than in pure Go, so
	// the barrier is not optional.
	defer func() {
		if r := recover(); r != nil {
			syms, err = nil, fmt.Errorf("cgo parser panicked: %v", r)
		}
	}()

	p, _ := b.pool.Get().(*cts.Parser)
	if p == nil {
		p = cts.NewParser()
	}
	if serr := p.SetLanguage(cl.lang); serr != nil {
		p.Close()
		return nil, fmt.Errorf("cgo backend: %w", serr)
	}
	// Returned to the pool unconditionally: unlike gotreesitter's Tagger, a
	// tree-sitter Parser does not retain the node arena of the largest input it
	// has seen, so there is no equivalent of the PoolReturnLimit rule here.
	defer b.pool.Put(p)

	// ParseCtx honours cancellation inside the C runtime, which the pure-Go
	// path has to emulate with a goroutine and a timeout.
	tree := p.ParseCtx(ctx, src, nil)
	if tree == nil {
		return nil, fmt.Errorf("cgo backend produced no tree for %s", lang)
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil {
		return nil, fmt.Errorf("cgo backend produced no root node for %s", lang)
	}

	cursor := cts.NewQueryCursor()
	defer cursor.Close()

	names := cl.query.CaptureNames()
	out := make([]TaggedSymbol, 0, 32)

	matches := cursor.Matches(cl.query, root, src)
	for m := matches.Next(); m != nil; m = matches.Next() {
		var (
			name    string
			rawKind string
			span    model.Range
			haveDef bool
		)
		for i := range m.Captures {
			c := &m.Captures[i]
			if int(c.Index) >= len(names) {
				continue
			}
			capture := names[c.Index]
			switch {
			case capture == "name":
				name = c.Node.Utf8Text(src)
			case strings.HasPrefix(capture, "definition."):
				rawKind = capture
				span = ctsRange(&c.Node)
				haveDef = true
			}
		}
		if name == "" {
			continue
		}
		if !haveDef {
			// A @name with no @definition capture carries no kind. The pure-Go
			// path drops these too (its switch only accepts definition.* and
			// reference.*), so dropping them keeps the two sides comparable.
			continue
		}
		out = append(out, TaggedSymbol{
			Name:    name,
			Kind:    kindFromTag(rawKind),
			RawKind: rawKind,
			Range:   span,
		})
	}

	sortSymbols(out)
	return dedupeTagged(out), nil
}

// ctsRange converts a C-runtime node span to RepoCortex's 1-based line, 0-based
// column convention — the same conversion toRange applies on the pure-Go side.
// The span is the whole definition node, not the name, matching gotreesitter's
// Tag.Range so that positions are compared like with like.
func ctsRange(n *cts.Node) model.Range {
	s, e := n.StartPosition(), n.EndPosition()
	return model.Range{
		Start: model.Position{Line: int(s.Row) + 1, Col: int(s.Column)},
		End:   model.Position{Line: int(e.Row) + 1, Col: int(e.Column)},
	}
}

// dedupeTagged collapses symbols captured by more than one pattern, mirroring
// dedupeSymbols. Input must already be sorted.
// sortSymbols orders tags so that dedupeTagged, which compares neighbours and
// keeps the first of a run, keeps the RIGHT one.
//
// It mirrors dedupeSymbols on the pure-Go side and must keep mirroring it. The
// ordering is position, then name, then kind SPECIFICITY DESCENDING — never
// alphabetical. Overlapping patterns tag one declaration twice: a Go interface
// matches both the specific (type_spec ... type: (interface_type)) rule and the
// general (type_spec ...) rule, arriving as both `interface` and `class`. Sort
// them the wrong way round and this backend reports `class` while the pure-Go
// backend reports `interface`, which is either a differential failure with no
// real cause or, worse, a genuine divergence in what the two runtimes claim
// about the same code.
//
// This function was missing entirely: the cgo_parser build did not compile, so
// the differential test — the gate that exists specifically to catch a
// gotreesitter regression — had never run. It is restored here because the
// backends cannot be verified in sync without it.
func sortSymbols(in []TaggedSymbol) {
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
		return kindSpecificity(a.Kind) > kindSpecificity(b.Kind)
	})
}

func dedupeTagged(in []TaggedSymbol) []TaggedSymbol {
	if len(in) < 2 {
		return in
	}
	out := in[:0]
	var prev TaggedSymbol
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
