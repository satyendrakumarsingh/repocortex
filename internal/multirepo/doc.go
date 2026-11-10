// Package multirepo federates a READ-ONLY view across several existing index
// databases and reports the edges that cross between them.
//
// # The question this exists to answer
//
// RepoCortex indexes ONE repository. Real systems are many: a React frontend
// calls a Go API which calls a Java service which writes a table. Each of those
// is a separate index that knows nothing of the others, so the question that
// matters most in a microservice estate — "if I change this response type,
// which other repositories break?" — cannot be asked at all. Every fact needed
// to answer it is already on disk; only the edge between the databases is
// missing.
//
// # Federation, not a combined database
//
// Nothing here migrates a schema, and nothing here builds a merged index. Each
// member repository keeps its own index, its own runs and its own digest, and
// the system graph is a VIEW assembled at query time — exactly the shape
// internal/cli/diff.go already uses for its --from-index flag, and for the same
// three reasons:
//
//   - Each repository is indexed at its own commit on its own schedule. There
//     is no moment at which a combined index would be correct for all of them.
//   - "One committed run per database" is an invariant the two-phase swap
//     depends on (CommitRun retires the previous run). A database holding the
//     committed runs of eight repositories cannot express that.
//   - A merged database would have to be rebuilt whenever any member changed,
//     which is the one property that makes a tool stop being run.
//
// The cost of federation is that the view is only as fresh as its least fresh
// member, and the whole of [Provenance] exists to say so out loud.
//
// # What is joined
//
//   - CONSUMES to EXPOSES. An outbound HTTP client call staged in one member
//     (internal/registry.ClientCallReport) is matched, on method and route,
//     against an endpoint declared in ANOTHER member
//     (internal/registry.EndpointReport). This is the hop nothing else in the
//     product can make: an unbound call is, by construction, a call this
//     repository does not answer itself.
//   - Shared database tables. Two repositories naming the same table is a real
//     coupling and is usually invisible to everyone involved, most of all when
//     both of them WRITE it.
//
// # The route match is a heuristic and is labelled as one
//
// `/users/{id}`, `/users/:id` and `/users/%s` denote the same route in three
// syntaxes, and deciding that they meet is a guess no compiler proved. So:
//
//   - No fact this package produces is ever [model.ConfidenceExact]. `exact`
//     means a compiler proved it, it is already refused in two other places
//     (registry.stageConfidence and model.Edge.Validate), and capHeuristic
//     refuses it here too rather than relying on the ceiling falling out of
//     the arithmetic by accident.
//   - A cross-repo edge's confidence is the MINIMUM over every leg of the path
//     INCLUDING the match itself, and each leg is reported next to it, so the
//     minimum can be audited rather than believed.
//   - Ambiguity refuses. A call that could be answered by two members has no
//     right answer, and choosing one would produce the invented dependency that
//     the whole binding design exists to avoid.
//
// # Saying what could not be joined
//
// A federated view that silently omits a member is worse than no federated view
// at all, because it reads as completeness. Every report therefore carries:
// calls that matched no known endpoint, calls that matched more than one,
// endpoints nobody in the system calls, tables only one member touches, and —
// first, and loudest — every member whose index is missing, unreadable,
// uncommitted or stale. See [Report.Unjoined] and [Report.Members].
package multirepo
