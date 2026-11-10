package context

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The ports this engine depends on.
//
// CLAUDE.md §3 names the Context Engine explicitly under Dependency Inversion:
// a high-level module must depend on abstractions, never on a concrete driver.
// It used to take *registry.Registry and *graph.Graph, which meant the ranking
// and budgeting logic — the part of this product with the most judgement in it
// and the least I/O — could only be exercised against a real SQLite database.
//
// internal/capability already declares its ports this way, so this is the
// project's own established pattern rather than a new one.
//
// The interfaces are deliberately tiny: five methods and one. Every method here
// is a method a fake has to implement, and a fake that has grown to mirror the
// real registry has stopped being a test aid.
//
// HONEST LIMIT. These signatures still name registry.Fingerprint,
// registry.FileMatch and graph.Query, so this package still IMPORTS those
// packages for their data types and `go list -deps` still reaches the SQLite
// driver transitively. What the interfaces buy is substitutability and
// testability — a caller can supply anything satisfying them. Severing the
// transitive import as well would mean relocating those DTOs into pkg/model,
// which is a larger change than the benefit currently justifies; it is recorded
// here so the remaining gap is visible rather than assumed closed.
type Registry interface {
	// CatalogueFingerprint is how the cache decides a run's content changed.
	CatalogueFingerprint(ctx context.Context, runID int64) (registry.Fingerprint, error)
	Files(ctx context.Context, runID int64) ([]model.File, error)
	Symbols(ctx context.Context, runID int64) ([]model.Symbol, error)
	SearchFiles(ctx context.Context, query string, limit int) ([]registry.FileMatch, error)
	FileExcerpt(ctx context.Context, runID int64, path string, startLine, lines int) (string, error)
}

// Graph is the traversal surface. One method: expansion is bounded and
// confidence-aware inside internal/graph, and this package uses that rather
// than reimplementing it.
type Graph interface {
	Traverse(ctx context.Context, q graph.Query) (graph.Result, error)
}

// Compile-time proof that the production types satisfy the ports. If a
// signature moves, this file fails to build rather than the wiring failing at
// run time.
var (
	_ Registry = (*registry.Registry)(nil)
	_ Graph    = (*graph.Graph)(nil)
)
