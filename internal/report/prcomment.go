package report

import (
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/architecture"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// PRMarker identifies RepoCortex's comment so CI updates it instead of adding a
// new one on every push.
const PRMarker = "<!-- repocortex:pr-report -->"

// LegacyPRMarker is preserved for backward-compatibility with older CI workflows.
const LegacyPRMarker = "<!-- repocortex:pr-report -->"

// DefaultPRRows bounds each table. GitHub truncates comments past 65,536
// characters, and a comment cut off mid-table hides exactly the rows at the end.
const DefaultPRRows = 20

// MaxPRCommentChars caps the whole comment body, below GitHub's 65,536 limit.
const MaxPRCommentChars = 60000

const prTruncatedNote = "\n\n_Report truncated to fit GitHub's comment limit; run `repocortex impact`, `repocortex arch` and `repocortex blindspots` for the full results._\n"

const prFooter = "\n---\n<sub>Reported by [RepoCortex](https://github.com/satyendrakumarsingh/repocortex) — Deterministic code intelligence for engineers & agents. Local-first, offline, and verifiable.</sub>\n"

// PRInput holds whichever reports CI produced. A nil report renders as
// "not run", never as zero findings.
type PRInput struct {
	Impact  *impact.DiffReport
	Missed  *missed.Report
	Arch    *architecture.Report
	MaxRows int
}

// RenderPR returns the Markdown body of the pull-request comment.
func RenderPR(in PRInput) string {
	rows := in.MaxRows
	if rows <= 0 {
		rows = DefaultPRRows
	}
	var b strings.Builder
	b.WriteString(PRMarker + "\n## RepoCortex change-safety report\n\n")
	if in.Impact != nil && !in.Impact.RevisionMatchesRange {
		fmt.Fprintf(&b, "> **Stale index.** The index was built at %s, which is neither end of %s..%s. "+
			"The results below describe that revision.\n\n",
			inline(orUnknown(in.Impact.IndexedRevision)), inline(in.Impact.BaseRev), inline(in.Impact.HeadRev))
	}
	b.WriteString("| Check | Result |\n|---|---|\n")
	fmt.Fprintf(&b, "| Blast radius | %s |\n", impactSummary(in.Impact))
	fmt.Fprintf(&b, "| Architecture | %s |\n", archSummary(in.Arch))
	fmt.Fprintf(&b, "| Blindspots | %s |\n", missedSummary(in.Missed))
	writeConfidence(&b, in.Impact)
	writeImpact(&b, in.Impact, rows)
	writeArch(&b, in.Arch, rows)
	writeMissed(&b, in.Missed, rows)
	b.WriteString(prFooter)
	out := b.String()
	if len(out) > MaxPRCommentChars {
		cut := out[:MaxPRCommentChars]
		if i := strings.LastIndex(cut, "\n"); i >= 0 {
			cut = cut[:i]
		}
		out = cut + prTruncatedNote
	}
	return out
}

func impactSummary(r *impact.DiffReport) string {
	if r == nil {
		return "not run"
	}
	s := fmt.Sprintf("**%s** risk (score %d): %d symbols reached from %d changed symbols",
		inline(r.Risk.Level), r.Risk.Score, r.TotalReached(), r.SeedsFound)
	if mix := blastConfidenceOf(r); mix.total > 0 {
		s += "; " + mix.summary()
	}
	if r.Truncated {
		s += " (truncated: " + inline(r.TruncatedReason) + ")"
	}
	return s
}

// blastConfidence counts the blast radius by how strongly each symbol's
// reachability is believed (impact.Impacted.Confidence, the minimum along the
// path). It answers "how much of this list is proven".
//
// Dispatch rows are not counted: they are a different claim (the runtime MAY
// execute this), DiffReport keeps them apart, and folding them in would let an
// unproven claim dilute a proven share.
//
// Only the four known tiers get their own bucket. Any other value (the
// confidence strings come from JSON input) lands in "unlabelled" and is never
// echoed, so untrusted text cannot reach the output through this path.
type blastConfidence struct {
	total  int
	counts map[model.Confidence]int
}

func blastConfidenceOf(r *impact.DiffReport) blastConfidence {
	m := blastConfidence{total: len(r.Impacted), counts: make(map[model.Confidence]int, 4)}
	for _, s := range r.Impacted {
		m.counts[s.Confidence]++
	}
	return m
}

// confidenceTiers is the render order, strongest first.
func confidenceTiers() []model.Confidence {
	return []model.Confidence{model.ConfidenceExact, model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow}
}

// share rounds n/total to a whole percent, half up, so 2 of 3 reads 67%.
func (m blastConfidence) share(n int) int {
	if m.total == 0 {
		return 0
	}
	return (n*100 + m.total/2) / m.total
}

// unlabelled counts rows whose confidence is not a recognised tier. Shown, not
// dropped, so shares that do not reach 100% say why.
func (m blastConfidence) unlabelled() int {
	n := m.total
	for _, c := range confidenceTiers() {
		n -= m.counts[c]
	}
	return n
}

// summary is the one-line form for the summary row. Low is named only when
// present: a walk whose floor excluded it must not show a measured-looking 0%.
func (m blastConfidence) summary() string {
	parts := []string{
		fmt.Sprintf("%d%% exact", m.share(m.counts[model.ConfidenceExact])),
		fmt.Sprintf("%d%% high", m.share(m.counts[model.ConfidenceHigh])),
		fmt.Sprintf("%d%% medium", m.share(m.counts[model.ConfidenceMedium])),
	}
	if n := m.counts[model.ConfidenceLow]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d%% low", m.share(n)))
	}
	return strings.Join(parts, ", ")
}

// writeConfidence renders the breakdown table. Nothing when impact did not run
// or reached nothing: the summary row already says which.
func writeConfidence(b *strings.Builder, r *impact.DiffReport) {
	if r == nil {
		return
	}
	m := blastConfidenceOf(r)
	if m.total == 0 {
		return
	}
	b.WriteString("\n### How much of the blast radius is proven\n\n| Confidence | Symbols | Share |\n|---|---|---|\n")
	for _, c := range confidenceTiers() {
		n := m.counts[c]
		if c == model.ConfidenceLow && n == 0 {
			continue
		}
		label := string(c)
		if c == model.ConfidenceExact {
			label = "exact (compiler-proven)"
		}
		fmt.Fprintf(b, "| %s | %d | %d%% |\n", label, n, m.share(n))
	}
	if n := m.unlabelled(); n > 0 {
		fmt.Fprintf(b, "| unlabelled | %d | %d%% |\n", n, m.share(n))
	}
	fmt.Fprintf(b, "\n_Shares are of the %d symbols reached through recorded references, rounded to whole percent", m.total)
	if n := len(r.Dispatch); n > 0 {
		fmt.Fprintf(b, "; %d reached only through interface dispatch are not counted", n)
	}
	b.WriteString("._\n")
	if m.counts[model.ConfidenceExact] == 0 {
		b.WriteString("\n_No part of this blast radius is compiler-proven. Set `REPOCORTEX_ACCURACY: auto` in the gate workflow, " +
			"or import a SCIP index with `repocortex scip import`, to prove it._\n")
	}
}

func archSummary(r *architecture.Report) string {
	if r == nil {
		return "not run"
	}
	if r.RulesEvaluated == 0 && len(r.DataViolations) == 0 && len(r.DataPaths) == 0 {
		return "no rules declared"
	}
	s := fmt.Sprintf("%d rule violations, %d data-rule violations", len(r.Violations), len(r.DataViolations))
	if worst := r.HighestSeverity(); worst != "" {
		s += ", worst " + inline(worst)
	}
	return s
}

func missedSummary(r *missed.Report) string {
	if r == nil {
		return "not run"
	}
	high := 0
	for _, bs := range r.Blindspots {
		if bs.Severity == "high" {
			high++
		}
	}
	return fmt.Sprintf("%d (%d high)", len(r.Blindspots), high)
}

func writeImpact(b *strings.Builder, r *impact.DiffReport, rows int) {
	if r == nil || (len(r.Impacted) == 0 && len(r.Unknown) == 0 && len(r.Dispatch) == 0) {
		return
	}
	if len(r.Impacted) > 0 {
		b.WriteString("\n### Blast radius\n\n| Symbol | File | Depth | Confidence |\n|---|---|---|---|\n")
		for i, s := range r.Impacted {
			if i == rows {
				fmt.Fprintf(b, "\n_…and %d more._\n", len(r.Impacted)-rows)
				break
			}
			fmt.Fprintf(b, "| %s | %s | %d | %s |\n", inline(s.Name), inline(s.FilePath), s.Depth, inline(fmt.Sprint(s.Confidence)))
		}
	}
	if n := len(r.Dispatch); n > 0 {
		if len(r.Impacted) == 0 {
			b.WriteString("\n### Blast radius\n")
		}
		fmt.Fprintf(b, "\n_%d more reached only through interface dispatch (not listed)._\n", n)
	}
	if len(r.Unknown) > 0 {
		b.WriteString("\n#### Could not determine\n\n")
		for i, u := range r.Unknown {
			if i == rows {
				fmt.Fprintf(b, "- _…and %d more._\n", len(r.Unknown)-rows)
				break
			}
			fmt.Fprintf(b, "- **%s**: %s\n", inline(fmt.Sprint(u.Kind)), inline(u.Detail))
		}
	}
}

func writeArch(b *strings.Builder, r *architecture.Report, rows int) {
	if r == nil || (len(r.Violations) == 0 && len(r.DataViolations) == 0) {
		return
	}
	b.WriteString("\n### Architecture\n\n")
	if len(r.Violations) > 0 {
		b.WriteString("| Rule | Severity | From → To | Location | Confidence |\n|---|---|---|---|---|\n")
		for i, v := range r.Violations {
			if i == rows {
				fmt.Fprintf(b, "\n_…and %d more._\n", len(r.Violations)-rows)
				break
			}
			fmt.Fprintf(b, "| %s | %s | %s → %s | %s | %s |\n", inline(ruleOrUnnamed(v.RuleID)), inline(v.Severity),
				inline(v.From), inline(v.To), loc(v.FilePath, v.Line), inline(v.Confidence))
		}
	}
	if len(r.DataViolations) > 0 {
		b.WriteString("\n#### Data-rule violations\n\n")
	}
	for i, v := range r.DataViolations {
		if i == rows {
			fmt.Fprintf(b, "- _…and %d more data-rule violations._\n", len(r.DataViolations)-rows)
			break
		}
		fmt.Fprintf(b, "- %s\n", inline(v.String()))
	}
}

func writeMissed(b *strings.Builder, r *missed.Report, rows int) {
	if r == nil || len(r.Blindspots) == 0 {
		return
	}
	b.WriteString("\n### Blindspots\n\n| Severity | Kind | Target | Location |\n|---|---|---|---|\n")
	for i, bs := range r.Blindspots {
		if i == rows {
			fmt.Fprintf(b, "\n_…and %d more._\n", len(r.Blindspots)-rows)
			break
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", inline(bs.Severity), inline(string(bs.Kind)), inline(bs.Target), loc(bs.Path, bs.Line))
	}
}

// maxInlineRunes caps one untrusted field so it cannot crowd out other sections.
const maxInlineRunes = 500

// inline makes untrusted text inert in Markdown: line breaks become spaces,
// HTML and mentions are entity-escaped, and Markdown syntax is backslash-escaped.
func inline(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
	if rs := []rune(s); len(rs) > maxInlineRunes {
		s = string(rs[:maxInlineRunes]) + "…"
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '@':
			b.WriteString("&#64;\u200b")
		case '\\', '|', '`', '*', '_', '[', ']', '(', ')', '!', '#', '~':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	// Break bare-URL autolinking.
	return strings.NewReplacer("://", ":&#47;&#47;", "www.", "www&#46;").Replace(b.String())
}

func loc(path string, line int) string {
	switch {
	case path == "":
		return ""
	case line > 0:
		return fmt.Sprintf("%s:%d", inline(path), line)
	default:
		return inline(path)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
