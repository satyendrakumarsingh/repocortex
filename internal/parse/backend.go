package parse

// The parser-runtime seam.
//
// WHY THIS EXISTS
//
// RepoCortex's default parser runtime is github.com/odvcencio/gotreesitter: a
// pure-Go tree-sitter implementation. That choice buys something valuable —
// `go install repocortex` works on any machine with a Go toolchain, and five
// release targets cross-compile from a single runner with CGO_ENABLED=0. The
// project is not willing to give that up.
//
// It is also the largest single-point dependency risk in the codebase. The
// module was first published on 2026-02-20, and 3,426 of its roughly 3,500
// commits come from one author. Every one of the 206 languages RepoCortex claims
// to support degrades at once if that module stalls, and profiling showed its
// GLR parser allocating gigabytes of node arena on real corpora (the numbers
// are in the MaxSourceBytes comment in generic.go).
//
// The architecture's answer was always "put the parser behind an interface and
// keep the official C runtime available as a compile-time swap". This file is
// that interface. Until differential_test.go existed the escape hatch was
// theoretical: nobody had ever shown the two runtimes agree about what is in a
// file, and an untested fallback is not a fallback.
//
// WHY THE SEAM IS THIS NARROW
//
// A Backend takes a language name and source bytes and returns tagged symbols.
// It does not see paths, timeouts, pooling, confidence, provenance or status —
// those are policy that belongs to exactly one place, genericParser, and
// duplicating them per backend would mean two implementations of RepoCortex's
// invariants that can drift apart. Every field the seam does not carry is a
// field the two backends cannot disagree about.

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/odvcencio/gotreesitter/grammars"
)

// TaggedSymbol is one definition a backend found, before RepoCortex attaches
// confidence and provenance.
//
// Kind is the only kind field the two backends are compared on, because it is
// the only one RepoCortex stores. RawKind is diagnostic detail: a backend that
// still holds the tree-sitter capture name reports it so a failing differential
// run can say WHY two kinds diverged, but genericParser collapses tag kinds
// internally and can only reconstruct it, so it is not a comparable field.
type TaggedSymbol struct {
	Name    string
	Kind    model.EntityKind
	RawKind string
	Range   model.Range
}

// Backend is a parser runtime capable of extracting symbols.
//
// Implementations must be safe for concurrent use, and must never panic: both
// runtimes are handed source from repositories RepoCortex did not write, so a
// hostile file has to degrade to an error for that file alone.
type Backend interface {
	// Name identifies the runtime in diagnostics and test output, e.g.
	// "purego" or "cgo".
	Name() string

	// Supports reports whether this backend has a grammar for lang. The
	// pure-Go backend covers all 206 embedded grammars; the CGO backend
	// covers only the languages whose grammar packages are compiled in.
	Supports(lang string) bool

	// Languages lists what Supports would accept, sorted.
	Languages() []string

	// Symbols extracts definitions from src. An unsupported language is an
	// error, not an empty result — silently returning nothing would let a
	// missing grammar look like an empty file.
	Symbols(ctx context.Context, lang string, src []byte) ([]TaggedSymbol, error)
}

// pureGoBackend is the default runtime, expressed through the same seam as the
// alternative so the differential test compares like with like.
//
// It deliberately routes through genericParser rather than calling gotreesitter
// directly. The pure-Go side of the comparison must be the code that actually
// ships — including the query supplements and the de-duplication — or the test
// would be measuring something no user ever runs.
type pureGoBackend struct {
	mu      sync.Mutex
	parsers map[string]*genericParser
}

// PureGoBackend returns the gotreesitter-backed runtime. Always available: the
// module is a plain Go dependency, so there is no build tag guarding it and the
// differential test can reach it from either build.
func PureGoBackend() Backend {
	return &pureGoBackend{parsers: make(map[string]*genericParser)}
}

func (b *pureGoBackend) Name() string { return "purego" }

func (b *pureGoBackend) Supports(lang string) bool {
	return grammars.DetectLanguageByName(lang) != nil
}

func (b *pureGoBackend) Languages() []string {
	all := grammars.AllLanguages()
	out := make([]string, 0, len(all))
	for _, e := range all {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func (b *pureGoBackend) Symbols(ctx context.Context, lang string, src []byte) ([]TaggedSymbol, error) {
	p, err := b.parserFor(lang)
	if err != nil {
		return nil, err
	}

	// The path argument is only a label for diagnostics; genericParser selects
	// its grammar from the entry it was built with, not from the extension.
	res, err := p.Parse(ctx, lang, src)
	if err != nil {
		return nil, err
	}
	if res.Status == StatusFailed {
		return nil, fmt.Errorf("purego backend failed on %s: %w", lang, res.Err)
	}

	out := make([]TaggedSymbol, 0, len(res.Symbols))
	for _, s := range res.Symbols {
		out = append(out, TaggedSymbol{
			Name: s.Name,
			Kind: s.Kind,
			// genericParser has already collapsed the tree-sitter tag kind into
			// a model kind and does not keep the original, so the raw kind is
			// reconstructed from the mapping rather than invented.
			RawKind: "definition." + string(s.Kind),
			Range:   s.Range,
		})
	}
	return out, nil
}

func (b *pureGoBackend) parserFor(lang string) (*genericParser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if p, ok := b.parsers[lang]; ok {
		return p, nil
	}
	entry := grammars.DetectLanguageByName(lang)
	if entry == nil {
		return nil, fmt.Errorf("purego backend has no grammar for %q", lang)
	}
	p := newGenericParser(*entry)
	if p.initErr != nil {
		return nil, p.initErr
	}
	b.parsers[lang] = p
	return p, nil
}
