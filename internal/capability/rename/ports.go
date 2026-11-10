package rename

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The ports a rename reads through, declared here at the consumer (CLAUDE.md
// §1). *registry.Registry satisfies all three; a test fake satisfies them in a
// few lines each.

// SymbolFinder resolves a name to its definitions.
type SymbolFinder interface {
	FindSymbolsByName(ctx context.Context, runID int64, name, kind string, limit int) ([]registry.SymbolHit, error)
}

// ReferenceReader fetches what points at a symbol, and counts it exactly so a
// capped fetch can never pass for a complete one.
type ReferenceReader interface {
	IncomingEdges(ctx context.Context, runID, symbolID int64, limit int, filter registry.EdgeFilter) ([]registry.EdgeHit, error)
	IncomingEdgeCount(ctx context.Context, runID, symbolID int64, filter registry.EdgeFilter, excludeKinds []string) (int, error)
}

// FileLister reports the content hash the index recorded for each file.
type FileLister interface {
	Files(ctx context.Context, runID int64) ([]model.File, error)
}

// Index is the three together.
type Index interface {
	SymbolFinder
	ReferenceReader
	FileLister
}
