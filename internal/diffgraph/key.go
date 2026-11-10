package diffgraph

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// SymbolKey is a symbol's content identity.
//
// It is deliberately NOT the digest's symbol key. The digest hashes position
// because any content change must flip it; this key excludes position because a
// symbol that moved is still the same symbol. See the package doc.
//
// KNOWN LIMIT of Ordinal: it disambiguates several symbols sharing
// (path, fqn, kind) inside one file, assigned in position order. Migration
// 0001 makes (path, fqn, start_line) unique, so duplicates are rare; when they
// do occur and one of them is deleted, the survivor inherits the lower ordinal
// and is reported as moved rather than as "the other one was removed". That is
// a strictly quieter failure than the alternative, and it is recorded here
// rather than assumed away.
type SymbolKey struct {
	Path    string           `json:"path"`
	FQN     string           `json:"fqn"`
	Kind    model.EntityKind `json:"kind"`
	Ordinal int              `json:"ordinal,omitempty"`
}

// String renders the key for display. The path prefix is dropped when the FQN
// already carries it — model.DefaultFQN is path-qualified, so printing both
// gives "pkg/api.go:pkg/api.go:handleRefund". Identity is unaffected; this is
// presentation only, and it stays a pure function of the key so the rendered
// report remains deterministic.
func (k SymbolKey) String() string {
	label := k.Path + ":" + k.FQN
	if strings.HasPrefix(k.FQN, k.Path+":") || strings.HasPrefix(k.FQN, k.Path+"#") {
		label = k.FQN
	}
	if k.Ordinal > 0 {
		return fmt.Sprintf("%s (%s)#%d", label, k.Kind, k.Ordinal)
	}
	return fmt.Sprintf("%s (%s)", label, k.Kind)
}

func compareSymbolKey(a, b SymbolKey) int {
	if c := cmp.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	if c := cmp.Compare(a.FQN, b.FQN); c != 0 {
		return c
	}
	if c := cmp.Compare(string(a.Kind), string(b.Kind)); c != 0 {
		return c
	}
	return cmp.Compare(a.Ordinal, b.Ordinal)
}

// EdgeKey is a relationship's content identity: which two entities, and what
// kind of relationship. Confidence and evidence are attributes, so a compiler
// that stops proving a call reports as a transition on one fact rather than as
// a removal plus an addition of two unrelated ones.
//
// Several edges of the same kind between the same pair (two call sites in one
// function) collapse into ONE fact, because "A calls B" is one fact about the
// code. The sites are retained on EdgeFact and counted.
type EdgeKey struct {
	Src  SymbolKey      `json:"src"`
	Dst  SymbolKey      `json:"dst"`
	Kind model.EdgeKind `json:"kind"`
}

func (k EdgeKey) String() string {
	return fmt.Sprintf("%s -[%s]-> %s", k.Src, k.Kind, k.Dst)
}

func compareEdgeKey(a, b EdgeKey) int {
	if c := compareSymbolKey(a.Src, b.Src); c != 0 {
		return c
	}
	if c := compareSymbolKey(a.Dst, b.Dst); c != 0 {
		return c
	}
	return cmp.Compare(string(a.Kind), string(b.Kind))
}

// EndpointKey identifies an HTTP endpoint by the route as the framework spells
// it plus the file that declares it. The handler is an attribute, so
// re-pointing a route at a different function is a change to one endpoint
// rather than the disappearance of one and the arrival of another.
type EndpointKey struct {
	Path  string `json:"path"`
	Route string `json:"route"`
}

func (k EndpointKey) String() string { return k.Route + " (" + k.Path + ")" }

func compareEndpointKey(a, b EndpointKey) int {
	if c := cmp.Compare(a.Route, b.Route); c != 0 {
		return c
	}
	return cmp.Compare(a.Path, b.Path)
}
