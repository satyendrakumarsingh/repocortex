package verify

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ArchitectureSource re-checks the declared architecture against the index as
// it stands AFTER the change.
//
// One method, declared here at the consumer per CLAUDE.md §1, and satisfied in
// production by a two-line closure over internal/capability/architecture.Check.
// That package already answers "does this code break a declared rule?", already
// attributes every violation to a file and a line, and already carries the
// confidence of the relationship that caused it. Re-deriving any of that here
// would produce a second implementation of the one judgement in this product
// that must not drift.
//
// HONEST LIMIT, in the spirit of internal/context/ports.go: this signature
// names architecture.Report, so this package imports internal/capability/
// architecture and `go list -deps` therefore reaches the SQLite driver
// transitively. Restating Violation as a local struct would sever that, at the
// cost of a mapping layer between two structs that must agree field for field —
// and a silent disagreement between them is exactly the failure this port
// exists to prevent. The trade is recorded rather than assumed away.
type ArchitectureSource interface {
	// Check evaluates the declared architecture over runID, dropping
	// relationships weaker than minConfidence.
	Check(ctx context.Context, runID int64, minConfidence model.Confidence) (architecture.Report, error)
}
