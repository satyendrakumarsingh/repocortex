package multirepo

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// MemberSource is the port this package needs from ONE member's index,
// declared here at the consumer rather than imported as a concrete type —
// CLAUDE.md §1 and §3, and the pattern already set by internal/diffgraph/ports.go
// and internal/context/ports.go.
//
// Five methods, each load-bearing for something the report would otherwise have
// to invent:
//
//   - Run supplies git_revision, producer_version, status and index_state.
//     Without it the report could not say that a member was indexed six weeks
//     ago by a different build, which is the difference between a system graph
//     and a picture of a system that no longer exists.
//   - ClientCallReport is the CONSUMES side. It reads the STAGED calls rather
//     than the edges deliberately, for the reason registry records on it: a
//     call that bound to nothing has no edge, and those are exactly the rows
//     that cross a repository boundary.
//   - EndpointReport is the EXPOSES side: the route, the file that declares it,
//     and the handler it exposes when the framework's syntax revealed one.
//   - OutgoingEdges supplies the recorded confidence of the EXPOSES edge, which
//     EndpointReport does not carry and which is a leg of the path. Called only
//     for endpoints that actually took part in a match, so it costs one query
//     per cross-repo edge rather than one per endpoint.
//   - TableUsageReport is the second join: two members naming one table.
//
// HONEST LIMIT, stated in the same spirit as internal/diffgraph/ports.go: three
// of these signatures name registry DTOs, so this package still imports
// internal/registry and `go list -deps` still reaches the SQLite driver
// transitively. What the port buys is substitutability and testability — every
// behavioural test of the federation logic runs against a hand-built fake and
// never opens a database. Severing the transitive import would mean relocating
// those DTOs into pkg/model, which is a change to packages this work does not
// own.
type MemberSource interface {
	Run(ctx context.Context, id int64) (*model.IndexRun, error)
	ClientCallReport(ctx context.Context, runID int64) ([]registry.ClientCallUsage, error)
	EndpointReport(ctx context.Context, runID int64) ([]registry.Endpoint, error)
	OutgoingEdges(ctx context.Context, runID, symbolID int64, limit int, filter registry.EdgeFilter) ([]registry.EdgeHit, error)
	TableUsageReport(ctx context.Context, runID int64) ([]registry.TableUsage, error)
}

// Compile-time proof that the production type satisfies the port. If a registry
// signature moves, this file fails to build rather than the CLI wiring failing
// at run time.
var _ MemberSource = (*registry.Registry)(nil)

// Local names for the DTOs the port borrows. Aliases, not conversions, so the
// interface above still speaks the registry's own types while the code below
// reads in this package's vocabulary — and so the single place to change if
// those DTOs ever move into pkg/model is here.
type (
	callRow     = registry.ClientCallUsage
	endpointRow = registry.Endpoint
	tableRow    = registry.TableUsage
)
