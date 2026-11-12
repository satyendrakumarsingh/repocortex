package report

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
)

// FromBlindspots converts `repocortex blindspots` results.
func FromBlindspots(bs []missed.Blindspot) []Finding {
	out := make([]Finding, 0, len(bs))
	for _, b := range bs {
		msg := b.Description
		if b.Remediation != "" {
			msg += " Remediation: " + b.Remediation
		}
		out = append(out, Finding{
			RuleID:          "repocortex/blindspot/" + string(b.Kind),
			RuleDescription: "Code-intelligence blindspot: " + string(b.Kind),
			Severity:        b.Severity,
			Message:         fmt.Sprintf("%s: %s", b.Target, msg),
			Path:            b.Path,
			Line:            b.Line,
		})
	}
	return out
}

// FromViolations converts `repocortex arch` rule violations. The edge's
// confidence travels with the finding: a violation on a `medium` edge may not
// exist at all, and a reviewer must be able to see that.
func FromViolations(vs []architecture.Violation) []Finding {
	out := make([]Finding, 0, len(vs))
	for _, v := range vs {
		out = append(out, Finding{
			RuleID:          "repocortex/arch/" + ruleOrUnnamed(v.RuleID),
			RuleDescription: "Architecture rule " + ruleOrUnnamed(v.RuleID),
			Severity:        v.Severity,
			Message: fmt.Sprintf("%s → %s (%s): %s %s %s, %s confidence",
				v.From, v.To, v.Verdict, v.FromName, v.Kind, v.ToName, v.Confidence),
			Path:       v.FilePath,
			Line:       v.Line,
			Confidence: v.Confidence,
		})
	}
	return out
}

// FromDataViolations converts sensitive-data reachability violations. The
// location is the first hop that has one: the endpoint itself is often a route
// string rather than a file position.
func FromDataViolations(vs []architecture.DataViolation) []Finding {
	out := make([]Finding, 0, len(vs))
	for _, v := range vs {
		f := Finding{
			RuleID:          "repocortex/data/" + ruleOrUnnamed(v.RuleID),
			RuleDescription: "Sensitive-data rule " + ruleOrUnnamed(v.RuleID),
			Severity:        v.Severity,
			Message:         v.String(),
		}
		for _, s := range v.Path.Steps {
			if s.FilePath != "" {
				f.Path, f.Line = s.FilePath, s.Line
				break
			}
		}
		out = append(out, f)
	}
	return out
}

func ruleOrUnnamed(id string) string {
	if id == "" {
		return "unnamed"
	}
	return id
}
