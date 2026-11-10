package docs

import "fmt"

// Limits bound every section of the document.
//
// # Why caps rather than a page budget
//
// The failure this prevents is specific: a 10,000-symbol repository producing a
// 10,000-node diagram. That is not a large document, it is an unusable one —
// Mermaid's layout is superlinear, code hosts stop rendering past a few hundred
// nodes, and no human reads a graph they cannot see the edges of. A single
// overall byte budget would fix the file size and leave one runaway section
// eating the document, so each section is capped on its own and each one reports
// what it dropped.
//
// # Where the numbers come from
//
// They are set at the point a diagram stops being readable, not at the point it
// stops being renderable, and those are far apart. A component diagram is
// legible to about thirty boxes; a repository declaring more than 40 components
// has a taxonomy problem no diagram will fix. Fifty endpoint flows is already
// fifty diagrams, which is a long document; past that, the reader wants a
// filtered regeneration, not more pages.
//
// # Zero means "use the default"
//
// Not "show nothing". A caller building a Limits literal and setting one field
// is the overwhelmingly common case, and reading the other seven as "suppress
// that section entirely" would turn a partial literal into a silently empty
// document. A caller who genuinely wants a section gone omits the section, not
// the number. Negative values are rejected by Validate rather than clamped,
// because -1 in a config is a typo and rendering something for it hides the
// typo.
type Limits struct {
	// MaxComponents caps the component table and the component diagram's nodes.
	MaxComponents int
	// MaxCrossings caps the component diagram's edges. Higher than the node cap
	// because a readable component diagram is dense in edges and sparse in
	// boxes, which is the opposite of the flow diagrams.
	MaxCrossings int
	// MaxEndpointFlows caps how many endpoints get a diagram.
	MaxEndpointFlows int
	// MaxCallDepth bounds the CALLS walk out of a handler. Three hops covers
	// handler -> service -> repository, which is the shape the flow diagram is
	// named after. Deeper walks reach the whole program and stop describing the
	// endpoint.
	MaxCallDepth int
	// MaxCallsPerFlow and MaxTablesPerFlow cap one endpoint's diagram.
	MaxCallsPerFlow  int
	MaxTablesPerFlow int
	// MaxInheritance caps the inheritance view's edges.
	MaxInheritance int
	// MaxCapabilities caps the capability model.
	MaxCapabilities int
	// MaxEndpointsPerCapability and MaxTablesPerCapability cap one capability's
	// listing.
	MaxEndpointsPerCapability int
	MaxTablesPerCapability    int
	// MaxDependenciesPerCapability caps one capability's "depends on" list.
	MaxDependenciesPerCapability int
	// MaxSharedTables caps the shared-data section.
	MaxSharedTables int
	// MaxUnclaimed caps EACH of the two unclaimed listings — endpoints and
	// tables — rather than their total. A repository with 400 unattributed
	// endpoints and 3 unattributed tables must still be told about the 3, and a
	// shared budget spent in declaration order would hide them.
	MaxUnclaimed int
}

// DefaultLimits is what a caller gets when it asks for nothing.
func DefaultLimits() Limits {
	return Limits{
		MaxComponents:             40,
		MaxCrossings:              120,
		MaxEndpointFlows:          50,
		MaxCallDepth:              3,
		MaxCallsPerFlow:           12,
		MaxTablesPerFlow:          12,
		MaxInheritance:            100,
		MaxCapabilities:           40,
		MaxEndpointsPerCapability: 25,
		MaxTablesPerCapability:    25,

		MaxDependenciesPerCapability: 15,
		MaxSharedTables:              25,
		MaxUnclaimed:                 25,
	}
}

// withDefaults fills zero fields from DefaultLimits.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxComponents == 0 {
		l.MaxComponents = d.MaxComponents
	}
	if l.MaxCrossings == 0 {
		l.MaxCrossings = d.MaxCrossings
	}
	if l.MaxEndpointFlows == 0 {
		l.MaxEndpointFlows = d.MaxEndpointFlows
	}
	if l.MaxCallDepth == 0 {
		l.MaxCallDepth = d.MaxCallDepth
	}
	if l.MaxCallsPerFlow == 0 {
		l.MaxCallsPerFlow = d.MaxCallsPerFlow
	}
	if l.MaxTablesPerFlow == 0 {
		l.MaxTablesPerFlow = d.MaxTablesPerFlow
	}
	if l.MaxInheritance == 0 {
		l.MaxInheritance = d.MaxInheritance
	}
	if l.MaxCapabilities == 0 {
		l.MaxCapabilities = d.MaxCapabilities
	}
	if l.MaxEndpointsPerCapability == 0 {
		l.MaxEndpointsPerCapability = d.MaxEndpointsPerCapability
	}
	if l.MaxTablesPerCapability == 0 {
		l.MaxTablesPerCapability = d.MaxTablesPerCapability
	}
	if l.MaxDependenciesPerCapability == 0 {
		l.MaxDependenciesPerCapability = d.MaxDependenciesPerCapability
	}
	if l.MaxSharedTables == 0 {
		l.MaxSharedTables = d.MaxSharedTables
	}
	if l.MaxUnclaimed == 0 {
		l.MaxUnclaimed = d.MaxUnclaimed
	}
	return l
}

// Validate rejects limits that would produce a document nobody asked for.
//
// Reports every problem at once, like arch.Model.Validate, because these arrive
// from a flag set or a config block a person is editing.
func (l Limits) Validate() error {
	var problems []string
	check := func(name string, v int) {
		if v < 0 {
			problems = append(problems, fmt.Sprintf("%s is %d; a negative cap is a typo, and rendering something for it would hide the typo", name, v))
		}
	}
	check("MaxComponents", l.MaxComponents)
	check("MaxCrossings", l.MaxCrossings)
	check("MaxEndpointFlows", l.MaxEndpointFlows)
	check("MaxCallDepth", l.MaxCallDepth)
	check("MaxCallsPerFlow", l.MaxCallsPerFlow)
	check("MaxTablesPerFlow", l.MaxTablesPerFlow)
	check("MaxInheritance", l.MaxInheritance)
	check("MaxCapabilities", l.MaxCapabilities)
	check("MaxEndpointsPerCapability", l.MaxEndpointsPerCapability)
	check("MaxTablesPerCapability", l.MaxTablesPerCapability)
	check("MaxDependenciesPerCapability", l.MaxDependenciesPerCapability)
	check("MaxSharedTables", l.MaxSharedTables)
	check("MaxUnclaimed", l.MaxUnclaimed)

	switch len(problems) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("docs: invalid limits: %s", problems[0])
	default:
		msg := fmt.Sprintf("docs: %d invalid limits:", len(problems))
		for _, p := range problems {
			msg += "\n  - " + p
		}
		return fmt.Errorf("%s", msg)
	}
}

// truncate cuts an already-ordered slice to a cap and returns the Omission the
// document must print, or the zero Omission when nothing was dropped.
//
// Generic over the element type so every section truncates the same way. The
// ordering is the caller's job and is named in `order`, which lands verbatim in
// the document: a reader who sees "showing 40 of 312" needs to know whether the
// 40 they got were the biggest, the first alphabetically, or arbitrary.
func truncate[T any](items []T, max int, subject, order string) ([]T, Omission) {
	if len(items) <= max {
		return items, Omission{}
	}
	return items[:max], Omission{Subject: subject, Shown: max, Total: len(items), Order: order}
}

// appendOmission drops zero-value Omissions so callers can append
// unconditionally.
func appendOmission(dst []Omission, o Omission) []Omission {
	if o.Subject == "" {
		return dst
	}
	return append(dst, o)
}
