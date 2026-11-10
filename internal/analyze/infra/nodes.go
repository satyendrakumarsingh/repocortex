package infra

import (
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// How a deployment unit appears in the knowledge graph.
//
// The vocabulary lives here rather than beside the extraction because it is a
// statement about what a Unit IS, and this package is the one that knows. What
// it deliberately does NOT do is build the symbols: constructing a node means
// deciding how a unit relates to a configuration key node, which is a fact
// about two extractors at once and therefore belongs to the layer that holds
// both. See internal/pipeline/infra.go.

// KindUnit is the entity kind of a deployment unit — a compose service, a
// Kubernetes container or Service, a Dockerfile stage, a Terraform resource.
//
// Not one of pkg/model's constants, and that is worth being explicit about. A
// deployment unit is not code: it is not declared by any grammar, it has no
// enclosing package, and it is the only node in the graph whose existence is
// asserted by a YAML file rather than by a syntax tree. It is spelled as a
// model.EntityKind because `symbols.kind` is an open TEXT column by design —
// migration 0001 puts no CHECK on it, precisely so a new node type does not
// require a schema change — and every reader treats an unfamiliar kind as
// opaque rather than switching on a closed set.
const KindUnit model.EntityKind = "deployment_unit"

// unitFQNPrefix namespaces unit nodes.
//
// Without it a compose service called `api` and a Go package called `api`
// would compete for one FQN, and the registry's symbol lookups key on FQN.
const unitFQNPrefix = "unit:"

// UnitFQN is how a deployment unit node is named.
//
// Qualified by the manifest's path, not just by the unit's name. Two compose
// files in one monorepo routinely both declare `api`, and they are two
// different services; collapsing them onto one node would produce a graph in
// which changing one team's configuration appears to restart another team's
// deployment. The path is the only thing that distinguishes them, so the path
// is in the identity.
func UnitFQN(path, name string) string { return unitFQNPrefix + path + "#" + name }

// IsUnitFQN reports whether an FQN names a deployment unit node.
func IsUnitFQN(fqn string) bool { return strings.HasPrefix(fqn, unitFQNPrefix) }

// UnitLocation recovers the manifest path and unit name from a unit FQN.
//
// ok is false for an FQN this package did not write. Callers rendering a report
// need the two halves back, and re-deriving them by splitting on "#" at every
// call site is how the two ends of an identity rule drift apart — the failure
// model.DefaultFQN exists to prevent.
func UnitLocation(fqn string) (path, name string, ok bool) {
	if !IsUnitFQN(fqn) {
		return "", "", false
	}
	rest := strings.TrimPrefix(fqn, unitFQNPrefix)
	// LastIndex, not Index: a unit name cannot contain '#' (plausibleName
	// rejects it) but a repository path can, and cutting at the first one would
	// mis-split `weird#dir/compose.yml#api`.
	i := strings.LastIndex(rest, "#")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}
