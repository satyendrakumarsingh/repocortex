package diffgraph

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// IndexSource is the port this package needs from the index, declared here at
// the consumer rather than imported as a concrete type — CLAUDE.md §1 and §3,
// and the pattern already set by internal/context/ports.go.
//
// Six methods, and each one is load-bearing for a class of fact this package
// classifies:
//
//   - Run supplies status, accuracy mode and digest. Without it the report
//     could not state what it was unable to compare, which is the difference
//     between a diff and a guess.
//   - Files supplies the id->path map (symbols carry a file id, not a path)
//     AND per-file parse status, which is what marks a change uncertain.
//   - Symbols and Edges are the graph itself.
//   - EndpointReport and TableUsageReport are the two derived views the
//     product already exposes; recomputing them from edges here would
//     duplicate a join that internal/registry owns.
//
// HONEST LIMIT, in the same spirit as internal/context/ports.go: the last two
// signatures name registry.Endpoint and registry.TableUsage, so this package
// still imports internal/registry for those DTOs and `go list -deps` still
// reaches the SQLite driver transitively. What the port buys is
// substitutability and testability — every behavioural test in this package
// runs against a hand-built fake and never opens a database. Severing the
// transitive import would mean relocating those DTOs into pkg/model, which is
// a change to a package this work does not own.
type IndexSource interface {
	Run(ctx context.Context, id int64) (*model.IndexRun, error)
	Files(ctx context.Context, runID int64) ([]model.File, error)
	Symbols(ctx context.Context, runID int64) ([]model.Symbol, error)
	Edges(ctx context.Context, runID int64) ([]model.Edge, error)
	EndpointReport(ctx context.Context, runID int64) ([]registry.Endpoint, error)
	TableUsageReport(ctx context.Context, runID int64) ([]registry.TableUsage, error)
}

// Compile-time proof that the production type satisfies the port. If a registry
// signature moves, this file fails to build rather than the CLI wiring failing
// at run time.
var _ IndexSource = (*registry.Registry)(nil)

// Local names for the two DTOs the port borrows. Aliases, not conversions, so
// the interface above still speaks the registry's own types while the
// reduction code below reads in this package's vocabulary — and so the single
// place to change if those DTOs ever move into pkg/model is here.
type (
	endpointRow = registry.Endpoint
	tableRow    = registry.TableUsage
)
