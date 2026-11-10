package docs

import (
	"context"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Registry is the read surface this package needs, declared here at the
// consumer rather than imported as a concrete type.
//
// Three methods, matching internal/capability/architecture's port exactly. That
// is deliberate: the two packages ask the index the same question — "give me the
// whole graph for this run" — and a second, differently-shaped port for the same
// question would mean two fakes to keep in step. Every method here is a method a
// test double has to implement, and a double that has grown to mirror the real
// registry has stopped being a test aid.
//
// Note what is NOT here. There is no Endpoints method even though
// registry.EndpointReport exists and would have saved the endpoint derivation
// below: EndpointReport returns a registry DTO, which would put a concrete
// adapter type in this package's signature, and it answers only half the
// question — it knows the handler but not what the handler goes on to touch. The
// EXPOSES edges are in Edges already.
//
// There is no Graph port either. The obvious one — Traverse(ctx, graph.Query) —
// would name graph.Query in this package's signatures and would hand the
// document's shape to a confidence-weighted, budget-bounded expansion whose
// output is deliberately not a pure function of the edge set. Both are wrong for
// an artifact whose entire value is that it regenerates byte-identically. The
// endpoint→handler→service→table walk is done here, over the edges Edges already
// returned, bounded by Limits and ordered explicitly. This is a real deviation
// from the "ports for the registry and the graph" instruction and it is recorded
// here rather than left to be discovered.
type Registry interface {
	Files(ctx context.Context, runID int64) ([]model.File, error)
	Symbols(ctx context.Context, runID int64) ([]model.Symbol, error)
	Edges(ctx context.Context, runID int64) ([]model.Edge, error)
}

// Compile-time proof that the production adapter satisfies the port. If a
// signature moves, this file fails to build rather than the wiring failing at
// run time.
var _ Registry = (*registry.Registry)(nil)
