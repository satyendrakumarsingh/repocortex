package scip

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Tool identifies the indexer that produced an index.
//
// It is carried all the way down to every Evidence.Detail rather than recorded
// once per run because facts from several indexers routinely coexist in one
// repository — scip-java for the service, scip-typescript for its frontend —
// and "which indexer claimed this" is the first question asked when an exact
// edge turns out to be wrong.
type Tool struct {
	Name    string
	Version string
}

// String renders the tool for humans. An index with no ToolInfo is legal
// protobuf but useless provenance, so it is named as such rather than rendered
// as an empty string that reads like a formatting bug.
func (t Tool) String() string {
	switch {
	case t.Name == "":
		return "unknown indexer"
	case t.Version == "":
		return t.Name
	default:
		return t.Name + " " + t.Version
	}
}

// Detail is the value written to model.Evidence.Detail. The "scip:" prefix is
// what distinguishes compiler evidence from the heuristic resolver's details
// (e.g. "import + receiver type") when reading rows by eye.
func (t Tool) Detail() string { return "scip: " + t.String() }

// Key identifies a relationship independently of database row ids.
//
// It is the deduplication key for the upgrade path: the heuristic resolver and
// a SCIP index describe the same relationship with different confidences, and
// the caller needs to recognise the collision before either has been assigned
// a rowid. FQNs work for this precisely because SCIP symbol strings are stable
// across runs and repositories, which synthesised path:name FQNs are not.
type Key struct {
	SrcFQN string
	DstFQN string
	Kind   model.EdgeKind
}

// Relation is one compiler-proven relationship, addressed by FQN.
//
// Edges are not produced directly because a reference's destination usually
// lives in a file this document knows nothing about, and rowids for symbols in
// other files do not exist until those files have been written. The caller
// resolves FQNs to ids once the symbol table is complete; see FileFacts.Edges.
type Relation struct {
	SrcFQN string
	DstFQN string
	Kind   model.EdgeKind

	// Evidence locates the occurrence that proves the relationship. Source is
	// left unset here and stamped by Edge, so there is exactly one place in the
	// package that can get it wrong.
	Evidence model.Evidence
}

// Key returns r's identity for deduplication against other resolvers.
func (r Relation) Key() Key { return Key{SrcFQN: r.SrcFQN, DstFQN: r.DstFQN, Kind: r.Kind} }

// Edge materialises r as a storable model.Edge once both endpoints have rowids.
//
// This is the only place in the package that constructs a model.Edge. It sets
// Confidence=exact and Evidence.Source=compiler together, so the pair cannot
// drift apart, and then runs Validate so a mistake fails here rather than at
// the SQL CHECK three layers down where the error names a constraint instead of
// a cause.
func (r Relation) Edge(srcID, dstID int64) (model.Edge, error) {
	e := model.Edge{
		SrcID:      srcID,
		DstID:      dstID,
		Kind:       r.Kind,
		Confidence: model.ConfidenceExact,
		Evidence:   r.Evidence,
	}
	e.Evidence.Source = model.SourceCompiler
	if err := e.Validate(); err != nil {
		return model.Edge{}, fmt.Errorf("scip: relation %s -> %s: %w", r.SrcFQN, r.DstFQN, err)
	}
	return e, nil
}

// Supersedes reports whether a SCIP relation should replace an already-known
// edge that shares its Key.
//
// SCIP wins over every heuristic confidence, and — the direction that actually
// matters — never loses to one. An inferred edge overwriting compiler evidence
// would silently downgrade a proven fact to a guess while leaving the graph
// looking healthy, which is the worst failure mode this system has: no error,
// no dangling row, just an impact analysis that quietly stopped being true.
// Callers merging resolver output must gate replacement on this function rather
// than on "last writer wins".
func Supersedes(existing model.Confidence) bool {
	return model.ConfidenceExact.Rank() > existing.Rank()
}

// FileFacts is one SCIP document translated into RepoCortex's model: everything
// needed for a single registry.ReplaceFile call for Path.
//
// Symbol ids are negative placeholders, which is the contract ReplaceFile
// defines for symbols that do not exist yet. Placeholder -1 is always the
// file-scope anchor symbol; see Ingester.Ingest for why one exists.
type FileFacts struct {
	// Path is repo-relative with forward slashes, reconciled against the
	// caller's file list. See paths.go.
	Path string

	// Language is SCIP's language string for the document, verbatim. It is not
	// normalised against internal/parse's names because a mismatch should be
	// visible as a mismatch rather than silently mapped to the nearest guess.
	Language string

	// Tool is the indexer that produced these facts.
	Tool Tool

	// Symbols are the definitions in this document, plus the file anchor.
	Symbols []model.Symbol

	// Relations are the references out of this document. Their SrcFQN is always
	// a symbol in Symbols; their DstFQN may live anywhere in the repository, or
	// outside it entirely (a dependency's symbol).
	Relations []Relation
}

// LocalIDs maps the FQN of every symbol defined in this file to its current id,
// so a caller can resolve intra-file relations without a round trip.
//
// "Current" is the operative word: the ids start as negative placeholders, and
// both registry.ReplaceFile and registry.Writer.AddSymbol write the real rowid
// back into model.Symbol.ID. So a caller that hands Symbols to the registry
// before calling Edges gets real ids out of it, and one that has not yet gets
// placeholders — which is exactly what ReplaceFile expects to remap.
func (f *FileFacts) LocalIDs() map[string]int64 {
	ids := make(map[string]int64, len(f.Symbols))
	for i := range f.Symbols {
		ids[f.Symbols[i].FQN] = f.Symbols[i].ID
	}
	return ids
}

// Edges resolves Relations into model.Edge values ready for ReplaceFile.
//
// Symbols defined in this file resolve from LocalIDs; everything else is asked
// of lookup, which the caller backs with its repository-wide symbol table.
// lookup may be nil, in which case only intra-file relations resolve.
//
// Relations whose destination cannot be resolved are returned separately rather
// than dropped. Most of them are genuinely unresolvable — references into
// third-party packages that RepoCortex never indexed — but "SCIP produced fewer
// edges than expected" is otherwise indistinguishable from a bug, and the
// caller is the only layer that knows whether the missing target should have
// existed.
func (f *FileFacts) Edges(lookup func(fqn string) (int64, bool)) ([]model.Edge, []Relation, error) {
	local := f.LocalIDs()
	resolve := func(fqn string) (int64, bool) {
		if id, ok := local[fqn]; ok {
			return id, true
		}
		if lookup == nil {
			return 0, false
		}
		return lookup(fqn)
	}

	edges := make([]model.Edge, 0, len(f.Relations))
	var unresolved []Relation
	for _, r := range f.Relations {
		src, ok := resolve(r.SrcFQN)
		if !ok {
			// A relation's source is always a symbol this same FileFacts
			// declares, so this can only happen if the caller mutated Symbols.
			unresolved = append(unresolved, r)
			continue
		}
		dst, ok := resolve(r.DstFQN)
		if !ok {
			unresolved = append(unresolved, r)
			continue
		}
		e, err := r.Edge(src, dst)
		if err != nil {
			return nil, nil, err
		}
		edges = append(edges, e)
	}
	return edges, unresolved, nil
}
