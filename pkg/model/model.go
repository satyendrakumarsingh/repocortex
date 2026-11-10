// Package model defines RepoCortex's language-independent domain entities.
//
// Two invariants govern everything here:
//
//  1. Every fact carries provenance. A relationship discovered by a compiler is
//     not the same kind of fact as one guessed from a name, and the difference
//     must survive all the way to the agent that consumes it. See Confidence.
//
//  2. Nothing in this package parses, queries, or performs I/O. It is the shared
//     vocabulary between the acquisition, registry, graph and context layers.
package model

import "fmt"

// Confidence records how strongly a fact is believed, and why.
//
// This is not decoration. RepoCortex's core promise is that an AI agent can tell
// a proven relationship from a plausible one; an impact analysis that presents
// guesses as facts is worse than no impact analysis at all.
type Confidence string

const (
	// ConfidenceExact means a compiler proved it. Sourced from a SCIP index
	// produced by a real typechecker. Safe to act on without verification.
	ConfidenceExact Confidence = "exact"

	// ConfidenceHigh means resolved through an explicit import plus an
	// unambiguous receiver type. Wrong only in unusual cases (shadowing,
	// reflection, runtime dispatch).
	ConfidenceHigh Confidence = "high"

	// ConfidenceMedium means the target was resolved by type inference where
	// more than one candidate existed and one was clearly preferred.
	ConfidenceMedium Confidence = "medium"

	// ConfidenceLow means resolved by weak signal — matching name in an
	// imported package, no type confirmation. Report it, but never let it
	// drive a risk score on its own.
	ConfidenceLow Confidence = "low"
)

// Rank orders confidence from weakest to strongest so callers can threshold
// (e.g. "only expand the graph through edges >= high").
func (c Confidence) Rank() int {
	switch c {
	case ConfidenceExact:
		return 4
	case ConfidenceHigh:
		return 3
	case ConfidenceMedium:
		return 2
	case ConfidenceLow:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether c is at least as strong as min.
func (c Confidence) AtLeast(min Confidence) bool { return c.Rank() >= min.Rank() }

// Valid reports whether c is a recognised confidence level.
func (c Confidence) Valid() bool { return c.Rank() > 0 }

// SourceType records the kind of evidence a fact came from. Spec §9.
type SourceType string

const (
	SourceCompiler   SourceType = "compiler"    // SCIP index — authoritative
	SourceSourceCode SourceType = "source_code" // parsed from a syntax tree
	SourceGit        SourceType = "git"         // commit history
	SourceConfig     SourceType = "config"      // developer-declared config
	SourceRuntime    SourceType = "runtime"     // OpenTelemetry spans
	SourceExternal   SourceType = "external"    // dependency manifests, advisories
	SourceInferred   SourceType = "inferred"    // heuristic with no direct evidence
)

// EntityKind enumerates the node types in the knowledge graph. Spec §8.
type EntityKind string

const (
	KindRepository    EntityKind = "repository"
	KindModule        EntityKind = "module"
	KindDirectory     EntityKind = "directory"
	KindFile          EntityKind = "file"
	KindPackage       EntityKind = "package"
	KindClass         EntityKind = "class"
	KindInterface     EntityKind = "interface"
	KindFunction      EntityKind = "function"
	KindMethod        EntityKind = "method"
	KindField         EntityKind = "field"
	KindConstant      EntityKind = "constant"
	KindVariable      EntityKind = "variable"
	KindTypeAlias     EntityKind = "type_alias"
	KindEndpoint      EntityKind = "endpoint"
	KindTable         EntityKind = "table"
	KindColumn        EntityKind = "column"
	KindEvent         EntityKind = "event"
	KindTest          EntityKind = "test"
	KindConfiguration EntityKind = "configuration"
	KindDependency    EntityKind = "dependency"
	KindCommit        EntityKind = "commit"
)

// EdgeKind enumerates relationship types in the knowledge graph. Spec §8.
type EdgeKind string

const (
	EdgeContains   EdgeKind = "CONTAINS"
	EdgeImports    EdgeKind = "IMPORTS"
	EdgeReferences EdgeKind = "REFERENCES"
	EdgeCalls      EdgeKind = "CALLS"
	EdgeExtends    EdgeKind = "EXTENDS"
	EdgeImplements EdgeKind = "IMPLEMENTS"
	EdgeDependsOn  EdgeKind = "DEPENDS_ON"
	EdgeExposes    EdgeKind = "EXPOSES"
	EdgeReads      EdgeKind = "READS"
	EdgeWrites     EdgeKind = "WRITES"
	EdgePublishes  EdgeKind = "PUBLISHES"
	EdgeConsumes   EdgeKind = "CONSUMES"
	EdgeTestedBy   EdgeKind = "TESTED_BY"
	EdgeConfigures EdgeKind = "CONFIGURED_BY"
	EdgeTracesTo   EdgeKind = "TRACES_TO"
	EdgeViolates   EdgeKind = "VIOLATES"
	EdgeOwnedBy    EdgeKind = "OWNED_BY"
	EdgeChangedBy  EdgeKind = "CHANGED_BY"
)

// Position is a 1-indexed line and 0-indexed byte column in a file.
type Position struct {
	Line int `json:"line"`
	Col  int `json:"col"`
}

// Range is a half-open span within a single file.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Evidence points at the exact source location that justifies a fact. Without
// it a relationship is an assertion; with it the agent can verify.
type Evidence struct {
	FilePath string     `json:"file_path"`
	Range    Range      `json:"range"`
	Source   SourceType `json:"source"`
	// Detail optionally names the concrete mechanism, e.g. "scip:
	// scip-java 0.10.3" or "import + receiver type".
	Detail string `json:"detail,omitempty"`
}

// File is an indexed source file.
type File struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"` // repo-relative, always forward slashes
	Language    string `json:"language"`
	ContentHash string `json:"content_hash"` // sha256 of bytes
	SizeBytes   int64  `json:"size_bytes"`
	ModTimeUnix int64  `json:"mod_time_unix"`
	ParseStatus string `json:"parse_status"` // ok | partial | failed | skipped
	ParseError  string `json:"parse_error,omitempty"`
}

// Arity is a parameter or argument count that MAY BE UNKNOWN.
//
// A plain int cannot carry this fact. Its zero value is 0, which is a perfectly
// ordinary arity — `Close()` takes none — so a producer that never set the
// field would be indistinguishable from one reporting a niladic call. That
// distinction is the whole safety property here: a symbol imported from SCIP,
// a Symbol built by a test, or a language whose parser has no arity query must
// read as "no information", never as "takes nothing".
//
// So the encoding is offset by one: the zero value is ArityUnknown, and a known
// count of n is stored as n+1. Nothing outside this type needs to know that —
// build one with KnownArity and read it with Known and Count.
type Arity int

// ArityUnknown is the zero value, meaning no count was determined.
const ArityUnknown Arity = 0

// KnownArity returns the Arity for a determined count. A negative count is
// nonsense and yields ArityUnknown rather than a value that would compare equal
// to some real arity.
func KnownArity(n int) Arity {
	if n < 0 {
		return ArityUnknown
	}
	return Arity(n + 1)
}

// Known reports whether a count was determined.
func (a Arity) Known() bool { return a > 0 }

// Count is the determined count. Meaningless unless Known reports true.
func (a Arity) Count() int { return int(a) - 1 }

// Symbol is a named, addressable code entity.
type Symbol struct {
	ID   int64      `json:"id"`
	Kind EntityKind `json:"kind"`
	Name string     `json:"name"` // e.g. "updateStatus"

	// FQN is the fully-qualified name, unique within a repository. For SCIP
	// symbols this is the SCIP symbol string, which makes exact and heuristic
	// facts about the same entity converge on one node.
	FQN string `json:"fqn"`

	FileID    int64  `json:"file_id"`
	Range     Range  `json:"range"`
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	Exported  bool   `json:"exported"`

	// ParamCount is how many parameters this symbol declares, where the parser
	// could determine that. ArityUnknown everywhere else — for non-callables,
	// for variadic and defaulted signatures, for languages with no arity query,
	// and for symbols that came from SCIP rather than from a syntax tree.
	//
	// Read by internal/resolve/infer to PREFER a candidate whose parameter
	// count matches the call site's argument count. Only to prefer: an arity
	// mismatch is never grounds for removing a candidate. See A4 in
	// internal/resolve/infer/resolver.go for why that distinction is the whole
	// design.
	ParamCount Arity `json:"param_count,omitempty"`

	// Confidence describes how reliably this symbol was extracted. Symbols are
	// usually high or exact; a Tier-1 generic extraction may be medium.
	Confidence Confidence `json:"confidence"`
	Source     SourceType `json:"source"`
}

// Edge is a typed, provenance-carrying relationship between two entities.
//
// The Confidence and Evidence fields are the reason this type exists rather
// than a bare (src, dst, kind) triple.
type Edge struct {
	ID         int64      `json:"id"`
	SrcID      int64      `json:"src_id"`
	DstID      int64      `json:"dst_id"`
	Kind       EdgeKind   `json:"kind"`
	Confidence Confidence `json:"confidence"`
	Evidence   Evidence   `json:"evidence"`

	// IndexRunID ties the edge to the run that produced it, which is what
	// makes the two-phase atomic swap and stale-edge removal possible.
	IndexRunID int64 `json:"index_run_id"`
}

// Validate rejects edges that would corrupt the graph. The registry calls this
// before every write; the doctor gate re-checks it across the whole store.
func (e Edge) Validate() error {
	if e.SrcID == 0 || e.DstID == 0 {
		return fmt.Errorf("edge %s: src and dst must be set (got %d -> %d)", e.Kind, e.SrcID, e.DstID)
	}
	if e.Kind == "" {
		return fmt.Errorf("edge %d -> %d: kind must be set", e.SrcID, e.DstID)
	}
	if !e.Confidence.Valid() {
		return fmt.Errorf("edge %s %d -> %d: invalid confidence %q", e.Kind, e.SrcID, e.DstID, e.Confidence)
	}
	if e.Confidence == ConfidenceExact && e.Evidence.Source != SourceCompiler {
		return fmt.Errorf("edge %s %d -> %d: confidence=exact requires source=compiler, got %q",
			e.Kind, e.SrcID, e.DstID, e.Evidence.Source)
	}
	return nil
}

// IndexStatus is the lifecycle state of an index. Spec §37.
type IndexStatus string

const (
	StatusIndexing IndexStatus = "indexing"
	StatusReady    IndexStatus = "ready"
	StatusDegraded IndexStatus = "degraded" // usable, but some files failed to parse
	StatusFailed   IndexStatus = "failed"
)

// IndexState records how much of what a run DISCOVERED the run actually
// covered.
//
// It is orthogonal to IndexStatus, which is the run's lifecycle. A run can be
// ready and partial, or degraded and complete, and that pair is precisely why
// this is a second field rather than two more values of the first: an index
// that some files failed to parse in is a different fact from an index that
// silently lost files, and a reader must be able to tell them apart.
type IndexState string

const (
	// IndexStateComplete means every discovered file reached one of the three
	// terminal outcomes: indexed, skipped, or recorded as failed.
	IndexStateComplete IndexState = "complete"

	// IndexStatePartial means the run did not cover everything it discovered.
	// Still published — a partial index beats none — but never presented as a
	// full picture of the repository.
	IndexStatePartial IndexState = "partial"
)

// IndexRun records one indexing pass. A run is only visible to readers once it
// is committed, which is what prevents a crash from exposing a half-built index.
type IndexRun struct {
	ID             int64       `json:"id"`
	Status         IndexStatus `json:"status"`
	StartedUnix    int64       `json:"started_unix"`
	FinishedUnix   int64       `json:"finished_unix,omitempty"`
	GitRevision    string      `json:"git_revision,omitempty"`
	FilesIndexed   int64       `json:"files_indexed"`
	SymbolsFound   int64       `json:"symbols_found"`
	EdgesFound     int64       `json:"edges_found"`
	ParseFailures  int64       `json:"parse_failures"`
	AccuracyMode   string      `json:"accuracy_mode"` // fast | auto | exact
	RegistryDigest string      `json:"registry_digest,omitempty"`
	Error          string      `json:"error,omitempty"`

	// ProducerVersion is the build that wrote this run.
	//
	// An index is data PLUS the resolution rules that produced it, and those
	// rules change between builds. Empty means the run predates the stamp, which
	// is a different claim from a version that simply differs: the first is
	// unknown provenance, the second is known-stale.
	ProducerVersion string `json:"producer_version,omitempty"`

	// FilesDiscovered is what the walker handed the pipeline, which is the
	// denominator FilesIndexed alone cannot supply.
	FilesDiscovered int64 `json:"files_discovered"`

	// IndexState is the completeness verdict. Empty only on a run read from a
	// database written before this was recorded.
	IndexState IndexState `json:"index_state"`

	// FilesUnaccounted is how many discovered files reached none of the three
	// terminal outcomes. Zero on a healthy run; a negative value means a file
	// was counted twice, which is a different bug with the same consequence.
	FilesUnaccounted int64 `json:"files_unaccounted"`
}

// Complete reports whether the run covered everything it discovered.
//
// A run recorded before completeness was measured has an empty IndexState and
// reads as complete: it made no claim either way, and relabelling every
// existing index as suspect on upgrade would be a louder lie than the silence.
func (r IndexRun) Complete() bool { return r.IndexState != IndexStatePartial }

// DefaultFQN is the fully-qualified name given to a symbol whose extractor did
// not supply one.
//
// It exists so the rule has ONE definition. It is applied in two places that
// must agree — the indexing sink, which persists symbols, and the analyzer,
// which attributes SQL statements to the definition that runs them by FQN — and
// when they disagreed the analyzer attributed nothing: it looked up names the
// sink had not assigned yet, found none, and produced seven bound table
// accesses with zero edges between them.
//
// Path-qualified rather than bare, because a bare name is not unique in a
// repository and the registry digest depends on this being stable across runs.
func DefaultFQN(path, name string) string { return path + ":" + name }
