// Package rename decides where a rename must edit and whether it may.
//
// It collects the same sites plan_rename shows an agent (the registry's
// incoming edges, every kind, no floor), adds the definition itself, and
// states every reason an unattended edit must not happen. It writes nothing;
// internal/edit does that, and only after Refusals is empty.
package rename

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// DefaultFloor is the weakest confidence an unattended edit rests on unless
// the caller opts into MinFloor: a compiler-proven binding.
const DefaultFloor = model.ConfidenceExact

// MinFloor is the weakest floor a caller may opt into. Nothing below it is
// ever edited unattended; Refusals raises any lower floor to it.
const MinFloor = model.ConfidenceHigh

// HighPrecision is the measured precision range of `high` edges against
// compiler indexes (docs/accuracy.md: five repositories, three languages). It
// is what a caller accepts per site when it opts into MinFloor.
const HighPrecision = "86.8-96.1%"

// KindDefinition marks the site that is the definition, not a reference.
const KindDefinition = "DEFINITION"

// DefaultMaxSites bounds the reference fetch. A rename with more references is
// refused as truncated, never applied in part.
const DefaultMaxSites = 2000

// maxCandidates bounds the definitions considered for one name.
const maxCandidates = 50

var (
	// ErrInvalidName: a name is not a single identifier, or old equals new.
	ErrInvalidName = errors.New("rename: invalid name")
	// ErrNotFound: nothing in the index is defined with that name.
	ErrNotFound = errors.New("rename: no definition with that name")
)

// AmbiguousError: several definitions share the name and --path did not
// narrow them to one. plan_rename picks the first; an edit must not.
type AmbiguousError struct {
	Old        string
	Candidates []Target
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("rename: %d definitions are named %q; pass --path to choose one", len(e.Candidates), e.Old)
}

// Request names the rename.
type Request struct {
	Old, New string
	// Path narrows the definition when several share Old, matched as a
	// substring of the defining file's path — plan_rename's rule.
	Path string
	// MaxSites bounds the reference fetch; zero means DefaultMaxSites.
	MaxSites int
}

// Target is the definition being renamed.
type Target struct {
	Name string `json:"name"`
	FQN  string `json:"fqn,omitempty"`
	Kind string `json:"kind"`
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Site is one place the identifier must change.
type Site struct {
	Path       string           `json:"path"`
	Line       int              `json:"line"`
	Col        int              `json:"col"` // evidence column hint; -1 when none
	Confidence model.Confidence `json:"confidence"`
	Kind       string           `json:"kind"` // edge kind, or KindDefinition
	Why        string           `json:"why,omitempty"`
}

// FileSites is one file's sites in the shape internal/edit takes.
type FileSites struct {
	Path  string
	Hash  string
	Lines map[int][]int
}

// Plan is everything an apply needs, and everything that may stop one.
type Plan struct {
	Old    string `json:"old"`
	New    string `json:"new"`
	Target Target `json:"target"`
	Sites  []Site `json:"sites"`

	// References is the exact count of incoming edges; Truncated says the
	// fetch returned fewer.
	References int  `json:"references"`
	Truncated  bool `json:"truncated"`

	// Collisions are existing definitions of New in a file this rename edits.
	Collisions []Target `json:"collisions,omitempty"`

	hashes  map[string]string
	fetched int
}

// Refusal is one reason the plan cannot be applied as it stands.
type Refusal struct {
	Site   *Site  `json:"site,omitempty"`
	Reason string `json:"reason"`
}

// ValidIdentifier reports whether s is one identifier in the languages
// RepoCortex parses: a letter, '_' or '$', then letters, digits, '_' or '$'.
func ValidIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '$' || unicode.IsLetter(r):
		case i > 0 && unicode.IsDigit(r):
		default:
			return false
		}
	}
	return true
}

// Collect resolves the target and gathers every site.
func Collect(ctx context.Context, ix Index, runID int64, req Request) (Plan, error) {
	switch {
	case !ValidIdentifier(req.Old):
		return Plan{}, fmt.Errorf("%w: %q is not an identifier", ErrInvalidName, req.Old)
	case !ValidIdentifier(req.New):
		return Plan{}, fmt.Errorf("%w: %q is not an identifier", ErrInvalidName, req.New)
	case req.Old == req.New:
		return Plan{}, fmt.Errorf("%w: old and new are both %q", ErrInvalidName, req.Old)
	}
	limit := req.MaxSites
	if limit <= 0 {
		limit = DefaultMaxSites
	}

	hits, err := ix.FindSymbolsByName(ctx, runID, req.Old, "", maxCandidates)
	if err != nil {
		return Plan{}, fmt.Errorf("rename: find %q: %w", req.Old, err)
	}
	if req.Path != "" {
		var kept []registry.SymbolHit
		for _, h := range hits {
			if strings.Contains(h.FilePath, req.Path) {
				kept = append(kept, h)
			}
		}
		hits = kept
	}
	switch {
	case len(hits) == 0:
		return Plan{}, fmt.Errorf("%w: %q", ErrNotFound, req.Old)
	case len(hits) > 1:
		amb := &AmbiguousError{Old: req.Old}
		for _, h := range hits {
			amb.Candidates = append(amb.Candidates, targetOf(h))
		}
		return Plan{}, amb
	}
	t := hits[0]

	edges, err := ix.IncomingEdges(ctx, runID, t.ID, limit, registry.EdgeFilter{})
	if err != nil {
		return Plan{}, fmt.Errorf("rename: references to %q: %w", req.Old, err)
	}
	total, err := ix.IncomingEdgeCount(ctx, runID, t.ID, registry.EdgeFilter{}, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("rename: count references to %q: %w", req.Old, err)
	}
	files, err := ix.Files(ctx, runID)
	if err != nil {
		return Plan{}, fmt.Errorf("rename: list indexed files: %w", err)
	}

	p := Plan{
		Old: req.Old, New: req.New, Target: targetOf(t),
		References: total, Truncated: total > len(edges), fetched: len(edges),
		hashes: make(map[string]string),
	}
	sites := make([]Site, 0, len(edges)+1)
	sites = append(sites, Site{Path: t.FilePath, Line: t.StartLine, Col: -1,
		Confidence: model.Confidence(t.Confidence), Kind: KindDefinition, Why: "the definition itself"})
	for _, e := range edges {
		sites = append(sites, Site{Path: e.EvidenceFile, Line: e.EvidenceLine, Col: e.EvidenceCol,
			Confidence: model.Confidence(e.Confidence), Kind: e.Kind, Why: e.Detail})
	}
	p.Sites = dedupe(sites)

	touched := make(map[string]bool, len(p.Sites))
	for _, s := range p.Sites {
		touched[s.Path] = true
	}
	for _, f := range files {
		if touched[f.Path] {
			p.hashes[f.Path] = f.ContentHash
		}
	}

	clash, err := ix.FindSymbolsByName(ctx, runID, req.New, "", maxCandidates)
	if err != nil {
		return Plan{}, fmt.Errorf("rename: find %q: %w", req.New, err)
	}
	for _, h := range clash {
		if touched[h.FilePath] {
			p.Collisions = append(p.Collisions, targetOf(h))
		}
	}
	return p, nil
}

// Refusals lists every reason, known from the index alone, that the plan must
// not be applied with sites at floor or better. A floor below MinFloor is
// raised to it. internal/edit adds what only the files can say.
func (p Plan) Refusals(floor model.Confidence) []Refusal {
	if !floor.AtLeast(MinFloor) {
		floor = MinFloor
	}
	var out []Refusal
	if p.Truncated {
		out = append(out, Refusal{Reason: fmt.Sprintf(
			"%d references exist but only %d were fetched; refusing to rename part of them (raise --max-sites)",
			p.References, p.fetched)})
	}
	for _, c := range p.Collisions {
		out = append(out, Refusal{Reason: fmt.Sprintf(
			"%s is already defined at %s:%d, in a file this rename edits", p.New, c.Path, c.Line)})
	}
	for i := range p.Sites {
		s := p.Sites[i]
		if !s.Confidence.AtLeast(floor) {
			c := string(s.Confidence)
			if c == "" {
				c = "none"
			}
			out = append(out, Refusal{Site: &s, Reason: fmt.Sprintf("confidence %s is below %s", c, floor)})
			continue
		}
		if _, ok := p.hashes[s.Path]; !ok {
			out = append(out, Refusal{Site: &s,
				Reason: "file is not in the index, so it cannot be checked against what was indexed"})
		}
	}
	return out
}

// ByFile groups the sites per file, sorted by path. A file with no recorded
// hash is omitted; Refusals already names it.
func (p Plan) ByFile() []FileSites {
	byPath := make(map[string]*FileSites)
	var order []string
	for _, s := range p.Sites {
		h, ok := p.hashes[s.Path]
		if !ok {
			continue
		}
		fs := byPath[s.Path]
		if fs == nil {
			fs = &FileSites{Path: s.Path, Hash: h, Lines: make(map[int][]int)}
			byPath[s.Path] = fs
			order = append(order, s.Path)
		}
		fs.Lines[s.Line] = append(fs.Lines[s.Line], s.Col)
	}
	sort.Strings(order)
	out := make([]FileSites, 0, len(order))
	for _, path := range order {
		out = append(out, *byPath[path])
	}
	return out
}

func targetOf(h registry.SymbolHit) Target {
	return Target{Name: h.Name, FQN: h.FQN, Kind: h.Kind, Path: h.FilePath, Line: h.StartLine}
}

// dedupe keeps one site per (path, line, col), the strongest claim, then sorts
// by path, line, column. Two edges at one position name one token; the
// stronger claim about it stands.
func dedupe(sites []Site) []Site {
	type key struct {
		path      string
		line, col int
	}
	best := make(map[key]Site, len(sites))
	var keys []key
	for _, s := range sites {
		k := key{s.Path, s.Line, s.Col}
		cur, ok := best[k]
		if !ok {
			keys = append(keys, k)
			best[k] = s
			continue
		}
		if s.Confidence.Rank() > cur.Confidence.Rank() {
			best[k] = s
		}
	}
	out := make([]Site, 0, len(keys))
	for _, k := range keys {
		out = append(out, best[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	return out
}
