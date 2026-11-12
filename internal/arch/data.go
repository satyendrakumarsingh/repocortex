package arch

import (
	"fmt"
	"sort"
	"strings"
)

// Data classification: which tables hold regulated data, and which components
// are permitted to reach them.
//
// # Why this lives in the rule model and not in a new subsystem
//
// "Which endpoints can reach a PII table?" is a question about the graph plus a
// declaration, which is exactly what the rest of this package already is. The
// tables, the read and write edges and the endpoint-to-handler edges are all
// facts the indexer already extracted; the only thing missing was a person
// saying which tables matter. So this file adds two declarations and two pure
// functions, and the traversal lives with the other graph walkers in
// internal/capability/architecture.
//
// # Classifications are declared, never inferred
//
// Nothing here guesses that a table called `customer_card` holds cardholder
// data. A guessed classification is worse than none: it is confident, it is
// wrong in both directions, and the first false positive is what makes a
// compliance team stop reading the report. A table is PCI because someone
// typed that it is.
//
// # The vocabulary is open
//
// ClassPII and ClassPCI are the two names the product spec uses, and they are
// constants only so the code that mentions them agrees on spelling. A bank will
// want `phi`, a European team `gdpr-special`, a platform team `secret`. Nothing
// rejects an unknown classification name. What IS checked is that a rule only
// references a classification that some `data` block declares — that typo is
// the one that would otherwise leave a rule silently enforcing nothing.

// Classifications named by the product spec. See the package note above: the
// vocabulary is open, and these exist for spelling agreement, not to close it.
const (
	ClassPII = "pii"
	ClassPCI = "pci"
)

// Bounds on the data model, for the same reason as the component bounds: this
// arrives in the repository's own .repocortex.yml and is untrusted.
//
// maxTableNameBytes is far above any real identifier — Postgres truncates at 63
// bytes, Oracle at 128 — and exists so a hostile symbol name cannot make glob
// matching cost anything interesting.
const (
	maxDataClasses    = 512
	maxTableNameBytes = 256
)

// DataClass labels a set of tables with a compliance classification.
//
// The shape deliberately mirrors Component — `name`, `include`, `exclude`, glob
// patterns, exclude wins — because a reader who has already understood the
// components block has then understood this one. The one difference is what the
// globs match: a TABLE NAME, which is a single token with no path structure. A
// pattern containing `/` is rejected rather than silently matching nothing.
//
// # Duplicate names are legal here, unlike components
//
// Two blocks both named `pci` union. This is the opposite of the rule for
// components, and the difference is real: a component is an owner, so a path
// belonging to two components of the same name is ambiguous, whereas a
// classification is a label, and a table labelled `pci` by two different blocks
// is simply `pci`. Splitting a large classification across blocks — one per
// team, one per regulation clause, each with its own comment — is how these
// files stay readable.
type DataClass struct {
	Name    string   `yaml:"name"    json:"name"`    // pii | pci | ... (case-insensitive)
	Include []string `yaml:"include" json:"include"` // globs over table names, e.g. "card_*"
	Exclude []string `yaml:"exclude" json:"exclude"`
}

// DataRule constrains which components may REACH a classified table.
//
// Reach, not call directly. That is the whole point: a controller that never
// mentions the cardholder table but calls a service that calls a repository
// that writes it has the same regulatory exposure as one that writes it
// itself, and only a rule that follows the path can say so.
//
// The fields mean what the same fields on Rule mean, read in the other
// direction. Rule constrains dependencies OUT of the components in `from`;
// DataRule constrains reachability INTO a classification, so the component
// lists describe the origin of the path:
//
//   - Classification: which labels this rule governs; empty means every
//     declared classification.
//   - AllowedFrom: an allow-list of origin components. Empty means there is no
//     allow-list and the rule only forbids — identical to Rule.AllowedTo, and
//     for the identical reason: a single `forbidden` entry must not silently
//     cut every other component off from the data.
//   - Forbidden: origin components that must never reach this classification.
//     Beats an allow-list anywhere in the file, as on Rule.
//
// The origin of a path is the component owning the ENDPOINT's file, which is
// what makes "only the payments API may reach cardholder data" expressible as
// one line.
type DataRule struct {
	ID             string   `yaml:"id"             json:"id"`
	Description    string   `yaml:"description"    json:"description"`
	Classification []string `yaml:"classification" json:"classification"` // classification names; empty = all
	AllowedFrom    []string `yaml:"allowed_from"   json:"allowed_from"`   // component names; empty = no allow-list
	Forbidden      []string `yaml:"forbidden"      json:"forbidden"`      // component names
	Severity       string   `yaml:"severity"       json:"severity"`       // high | medium | low
}

// SeverityOrDefault is DataRule's copy of Rule.SeverityOrDefault, and defaults
// the same way for the same reason: an unannotated rule is worth reporting, and
// defaulting to high would fail a `--fail-on high` build for a reason its
// author never chose.
//
// Copied rather than shared through an interface or an embedded struct: two
// four-line methods are cheaper to read than the indirection that would remove
// one of them, and these two types are free to diverge.
func (r DataRule) SeverityOrDefault() string {
	switch strings.ToLower(strings.TrimSpace(r.Severity)) {
	case SeverityHigh:
		return SeverityHigh
	case SeverityLow:
		return SeverityLow
	}
	return SeverityMedium
}

// ClassesOf returns every classification labelling a table, sorted and
// deduplicated, or nil when the table is unclassified.
//
// # Several classifications, not most-specific-wins
//
// ComponentOf picks one component because ownership is exclusive. Labelling is
// not: a `customer_cards` table genuinely holds both personal data and
// cardholder data, and reporting only the more specific pattern's label would
// drop half of a compliance finding. Every matching class is returned.
//
// # Case
//
// Table names arrive from the extractor already lower-cased, but a config
// author writes `Card_Tokens` as readily as `card_tokens`. Both sides are
// folded to lower case, so the config reads how its author wants and matches
// either way.
func (m *Model) ClassesOf(table string) []string {
	name := strings.ToLower(strings.TrimSpace(table))
	if name == "" || len(name) > maxTableNameBytes {
		return nil
	}

	var out []string
	for i := range m.Data {
		d := &m.Data[i]
		n := normClass(d.Name)
		if n == "" || !d.matches(name) {
			continue
		}
		if !contains(out, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// matches reports whether a class labels an already-lower-cased table name.
//
// Exclude is checked first and wins outright, exactly as it does for a
// component: an exclude carves a hole — `card_*` minus `card_bin_reference`,
// which is a public lookup table and not cardholder data — and a hole an
// include can win back is not a hole.
func (d *DataClass) matches(name string) bool {
	for _, ex := range d.Exclude {
		if matchTable(ex, name) {
			return false
		}
	}
	for _, in := range d.Include {
		if matchTable(in, name) {
			return true
		}
	}
	return false
}

// matchTable matches one glob against one lower-cased table name.
//
// matchSegment, not matchSegs: a table name is a single token, so there are no
// segments to span and `**` has nothing to mean. Going through the segment
// matcher would also make `**` in a table pattern quietly behave like `*`,
// which is the kind of near-miss that is worse than an error. Patterns
// containing `/` are rejected here and reported by Validate.
func matchTable(pattern, name string) bool {
	if pattern == "" || len(pattern) > maxPatternBytes || strings.Contains(pattern, "/") {
		return false
	}
	return matchSegment(strings.ToLower(pattern), name)
}

// EvaluateData decides whether a path from an origin component into a
// classification is permitted, and returns the rule that decided it.
//
// The precedence is Evaluate's, deliberately: forbidden beats everything, an
// explicit allow beats a bare not-on-the-list, and every matching rule is
// examined before any allow verdict is returned so a forbidding rule declared
// last still wins. Two rule kinds that resolved conflicts differently would be
// two rule kinds nobody could reason about together.
//
// An empty origin component returns allowed with no rule. That is not a pass:
// it means the endpoint's file belongs to no declared component, so there is no
// component for a rule to name, and judging it would invent a boundary the
// model never declared. The caller counts those separately and the report says
// how many there were — see Report.UnassignedEndpoints.
func (m *Model) EvaluateData(class, from string) (Verdict, *DataRule) {
	class = normClass(class)
	if class == "" || from == "" {
		return VerdictAllowed, nil
	}

	var allowedBy, gatedBy *DataRule
	for i := range m.DataRules {
		r := &m.DataRules[i]
		if len(r.Classification) > 0 && !containsClass(r.Classification, class) {
			continue
		}
		if contains(r.Forbidden, from) {
			return VerdictForbidden, r
		}
		if len(r.AllowedFrom) == 0 {
			continue
		}
		if contains(r.AllowedFrom, from) {
			if allowedBy == nil {
				allowedBy = r
			}
			continue
		}
		if gatedBy == nil {
			// The FIRST allow-list that excludes the origin, so the reported
			// rule is stable when several do.
			gatedBy = r
		}
	}

	switch {
	case allowedBy != nil:
		return VerdictAllowed, allowedBy
	case gatedBy != nil:
		return VerdictNotAllowed, gatedBy
	}
	return VerdictAllowed, nil
}

// DeclaresData reports whether the model classifies anything.
//
// The traversal that answers "what reaches this table" is the most expensive
// thing this capability does, and a repository that classified no data has
// asked for none of it. Callers check this before building a graph.
func (m *Model) DeclaresData() bool { return len(m.Data) > 0 }

func normClass(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func containsClass(names []string, want string) bool {
	for _, n := range names {
		if normClass(n) == want {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------- validation

// appendDataClassProblems validates the `data` block and records the declared
// classification names, so a rule referencing one that does not exist can be
// caught below.
func appendDataClassProblems(problems []string, classes map[string]bool, data []DataClass) []string {
	if len(data) > maxDataClasses {
		problems = append(problems, fmt.Sprintf(
			"%d data classes declared, over the %d limit; every one is matched against every "+
				"table in the repository", len(data), maxDataClasses))
	}
	for i, d := range data {
		label := fmt.Sprintf("data class #%d", i+1)
		if normClass(d.Name) == "" {
			problems = append(problems, label+": name is empty; a classification nothing can name cannot appear in a rule")
		} else {
			// No duplicate check: see the note on DataClass. Two blocks with
			// one name union, which is a documented and useful way to write
			// this, not a mistake.
			label = fmt.Sprintf("data class %q", d.Name)
			classes[normClass(d.Name)] = true
		}

		if len(d.Include) == 0 {
			problems = append(problems, label+": no include patterns; it labels no table, so every rule naming it is inert")
		}
		problems = appendTablePatternProblems(problems, label, "include", d.Include)
		problems = appendTablePatternProblems(problems, label, "exclude", d.Exclude)
	}
	return problems
}

// appendTablePatternProblems is appendPatternProblems for table globs.
//
// Separate because the failure it exists to catch is different. A component
// pattern is a path and may contain `/`; a table pattern is a single
// identifier, so a `/` in one is a config author who copied a component pattern
// by mistake — it would match no table, forever, silently, and every rule
// naming that class would pass.
func appendTablePatternProblems(problems []string, label, field string, patterns []string) []string {
	if len(patterns) > maxPatternsPerComponent {
		return append(problems, fmt.Sprintf(
			"%s: %d %s patterns, over the %d limit; every one is matched against every table",
			label, len(patterns), field, maxPatternsPerComponent))
	}
	for j, p := range patterns {
		switch {
		case strings.TrimSpace(p) == "":
			problems = append(problems, fmt.Sprintf("%s: %s pattern #%d is empty", label, field, j+1))
		case len(p) > maxPatternBytes:
			problems = append(problems, fmt.Sprintf("%s: %s pattern #%d is %d bytes, over the %d-byte limit, and would never match", label, field, j+1, len(p), maxPatternBytes))
		case strings.Contains(p, "/"):
			problems = append(problems, fmt.Sprintf("%s: %s pattern #%d %q contains %q; these match table names, which have no path separator, so it would never match", label, field, j+1, p, "/"))
		}
	}
	return problems
}

// appendDataRuleProblems validates the `data_rules` block.
//
// seenID is shared with the ordinary rules: both kinds print an ID in the same
// report and are suppressed by the same ID, so an ID meaning one thing in one
// block and something else in the other is a report that cannot say where a
// finding came from.
func appendDataRuleProblems(problems []string, comps, classes, seenID map[string]bool, rules []DataRule) []string {
	for i, r := range rules {
		label := fmt.Sprintf("data rule #%d", i+1)
		if r.ID != "" {
			label = fmt.Sprintf("data rule %q", r.ID)
			if seenID[r.ID] {
				problems = append(problems, fmt.Sprintf("%s: duplicate rule id; a violation report could not say which rule it came from", label))
			}
			seenID[r.ID] = true
		}

		if len(r.AllowedFrom) == 0 && len(r.Forbidden) == 0 {
			problems = append(problems, label+": neither `allowed_from` nor `forbidden`; a rule that permits everything reports nothing, and the reachability inventory already lists every path without it")
		}
		for j, c := range r.Classification {
			if !classes[normClass(c)] {
				problems = append(problems, fmt.Sprintf("%s: classification #%d references undeclared classification %q; add a `data` block for it or the rule governs nothing", label, j+1, c))
			}
		}
		problems = appendUnknownProblems(problems, comps, label, "allowed_from", r.AllowedFrom)
		problems = appendUnknownProblems(problems, comps, label, "forbidden", r.Forbidden)

		switch r.Severity {
		case "", SeverityHigh, SeverityMedium, SeverityLow:
		default:
			problems = append(problems, fmt.Sprintf("%s: severity %q: want high, medium or low", label, r.Severity))
		}
	}
	return problems
}
