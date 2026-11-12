package registry

import (
	"context"
	"fmt"
	"sort"
)

// The compiler adjudication: every call site whose correct target is known,
// collapsed to one verdict each.
//
// # Why this lives in the Repository adapter and not in a command
//
// It used to live in internal/cli, as raw SQL beside the renderer that printed
// it. That was defensible while exactly one caller existed. It stopped being
// defensible the moment the portal needed the same numbers: two copies of a
// measurement are two measurements, and the failure mode is not a compile error
// — it is `repocortex accuracy` and /api/accuracy publishing different F1 scores
// for one index, for a reason nobody can locate. CLAUDE.md §2 puts SQL here;
// this is that rule applied to the one query where drift would be silent.
//
// # What this returns, and what it deliberately does not
//
// Outcomes, coverage and miss causes. NEVER a rate. Precision, recall and F1
// are bench.AccuracyOf's job, and a repository method that returned them would
// make it possible for two callers to fold the same outcomes two ways. The
// registry counts rows; internal/bench does the arithmetic; the delivery
// adapters render it.
//
// # The restriction, restated because it is part of the number
//
// A call site is adjudicable only when the parser recorded a call on that line
// AND the compiler resolved the line to exactly one definition. SCIP records
// REFERENCES — a superset of calls — so a denominator of every compiler-resolved
// reference would score the resolver down for not emitting a CALLS edge where
// no call exists. internal/bench carries the argument in full; the SQL below is
// that argument executed.

// CallSiteOutcome is a group of adjudicated call sites sharing one verdict.
//
// One row per (language, tier, correctness) rather than one per call site: the
// aggregation happens in the query, and no consumer needs the individual site.
type CallSiteOutcome struct {
	// Language is the language of the file the call site is in.
	Language string
	// Tier is the STRONGEST confidence the resolver attached to any CALLS edge
	// at this call site. EMPTY means the resolver produced no edge at all,
	// which is the missed-caller case this whole measurement exists to expose.
	Tier string
	// Correct is true when some edge at the call site named the definition the
	// compiler named.
	Correct bool
	Count   int64
}

// AccuracyCoverage is how much of the repository was adjudicated at all.
//
// Without it a rate is unreadable: 95% over 40 of 10,000 call sites and 95%
// over 9,000 of them are the same number and a different product.
type AccuracyCoverage struct {
	// ParsedCallSites is every (file, line) the parser recorded a call on.
	ParsedCallSites int64
	// HeuristicEdges is every CALLS edge the RESOLVER claimed; CompilerFacts is
	// every fact imported from a compiler-backed index. CompilerFacts of zero is
	// the whole explanation for an empty measurement.
	HeuristicEdges int64
	CompilerFacts  int64
	// UnambiguousLines are lines the compiler resolved to exactly one target —
	// the only lines where a right answer is defined. AmbiguousLines are the
	// rest, excluded and counted rather than silently scored.
	UnambiguousLines int64
	AmbiguousLines   int64
}

// LanguageCoverage is one language's stake in the measurement.
//
// Reported for every language carrying call sites or edges, INCLUDING the ones
// no compiler index adjudicated. A language with call sites and no compiler
// facts is the "not measured" case, and it must be nameable — an unmeasured
// language that reads like a measured one is the single failure a trust report
// cannot survive, and it is not derivable from Outcomes, which by construction
// only holds languages something adjudicated.
type LanguageCoverage struct {
	Language string
	Files    int64
	// CallSites is every parsed call site in this language, adjudicated or not.
	CallSites      int64
	HeuristicEdges int64
	CompilerFacts  int64
}

// CallSiteMiss is one reason an adjudicable call site produced no edge at all.
//
// Reported because "missed" without a cause is unactionable and the causes have
// different owners: unresolved is a resolver problem, pending means `repocortex
// resolve` was never run, and resolved-with-no-edge is a call outside any
// indexed symbol — a modelling choice rather than a defect.
type CallSiteMiss struct {
	Reason      string
	Explanation string
	CallSites   int64
}

// Adjudication is one run's compiler-adjudicated call sites, before any rate is
// computed from them.
type Adjudication struct {
	Outcomes  []CallSiteOutcome
	Coverage  AccuracyCoverage
	Languages []LanguageCoverage
	Misses    []CallSiteMiss
}

// resolverResolution reads what the RESOLVER did with a reference, which stops
// being what the row says the moment a compiler import settles it.
//
// The same correction resolverSource/resolverConfidence make one table over,
// for the same reason. A SCIP import writes symbol_references.resolution
// (migration 0013), so reading the live column would report the resolver as
// having succeeded wherever the compiler did — turning the miss breakdown into
// the same lie migration 0012 removed from the tier table.
const resolverResolution = `(CASE WHEN r.prior_resolution <> '' THEN r.prior_resolution ` +
	`ELSE r.resolution END)`

// accuracyCTE is the shared vocabulary of the whole measurement.
//
//   - unambiguous: lines the compiler resolved to EXACTLY ONE target. Same
//     predicate as ResolutionPrecision, including the reason — the two tools do
//     not share a column convention, so judgement is per line, and a line with
//     several targets has no defined right answer.
//   - callsite: every (file, line) the parser saw a call on, whether or not the
//     resolver could then bind it. This is what makes recall well defined; an
//     unbound call site is still a call site, and it is exactly the miss.
//     `kind LIKE '%call%'` mirrors infer.edgeKindFor, which maps any reference
//     kind containing "call" onto a CALLS edge — the two must agree or the
//     denominator would hold sites that could never have produced an edge.
//
// SHAPE MATTERS AS MUCH AS THE INDEXES HERE. This used to build a `callsite`
// CTE of every parsed call site — 305,237 rows on a real repository — and join
// it to `unambiguous` (5,587 rows). SQLite cannot index a CTE, so the plan was
// `SCAN c` then `SCAN u`: a nested loop over the product, 160 seconds for one
// query and roughly twenty minutes for the report.
//
// Inverted, the SMALL side drives. Each of the 5,587 compiler-adjudicated lines
// looks up its file by path and asks whether a call was parsed there, both
// indexed. 160s -> 0.16s, identical rows. The general rule: when one side of a
// join is two orders of magnitude smaller, it belongs on the outside, and a CTE
// that cannot be indexed belongs nowhere near the inside of a loop.
//
//   - truth: the intersection. Each row is a call whose correct target is known.
//   - heur: the resolver's CALLS edges, same predicate as the precision query.
//     READ THROUGH resolverSource/resolverConfidence, never off the live
//     columns — reading `source = 'source_code'` here is what made this report
//     invert after an import, from 51.2% recall to 0.5%.
//   - scored: one row per adjudicated call site, holding whether any edge there
//     was right, the strongest tier that answered, and the strongest tier that
//     was right. Ranked numerically because MIN over the confidence strings
//     would order 'high' < 'low' < 'medium'. The store's CHECK constraint fixes
//     the vocabulary to exact|high|medium|low, so the mapping is total.
const accuracyCTE = `
WITH unambiguous AS (
    SELECT evidence_file AS f, evidence_line AS l, MIN(dst_id) AS dst
      FROM edges
     WHERE index_run_id = ? AND source = 'compiler'
     GROUP BY evidence_file, evidence_line
    HAVING COUNT(DISTINCT dst_id) = 1),
truth AS (
    SELECT u.f, u.l, COALESCE(NULLIF(fl.language, ''), 'unknown') AS lang, u.dst
      FROM unambiguous u
      JOIN files fl ON fl.path = u.f AND fl.index_run_id = ?
     WHERE EXISTS (SELECT 1 FROM symbol_references r
                    WHERE r.file_id = fl.id AND r.start_line = u.l
                      AND r.index_run_id = ? AND r.kind LIKE '%call%'
                      AND r.start_line > 0)),
heur AS (
    SELECT e.evidence_file AS f, e.evidence_line AS l, e.dst_id,
           ` + resolverConfidence + ` AS confidence
      FROM edges e
     WHERE e.index_run_id = ? AND e.kind = 'CALLS'
       AND ` + resolverSource + ` = 'source_code'),
scored AS (
    SELECT t.f AS f, t.l AS l, t.lang AS lang,
           MAX(CASE WHEN h.dst_id = t.dst THEN 1 ELSE 0 END) AS correct,
           MIN(CASE WHEN h.confidence IS NULL THEN NULL ELSE
               (CASE h.confidence WHEN 'exact' THEN 0 WHEN 'high' THEN 1
                                  WHEN 'medium' THEN 2 WHEN 'low' THEN 3
                                  ELSE 4 END) END) AS any_rank,
           MIN(CASE WHEN h.dst_id = t.dst THEN
               (CASE h.confidence WHEN 'exact' THEN 0 WHEN 'high' THEN 1
                                  WHEN 'medium' THEN 2 WHEN 'low' THEN 3
                                  ELSE 4 END) ELSE NULL END) AS hit_rank
      FROM truth t
      LEFT JOIN heur h ON h.f = t.f AND h.l = t.l
     GROUP BY t.f, t.l, t.lang)
`

// tierByRank inverts the rank expression in accuracyCTE. The index is the rank;
// a rank of -1 (no edge at all) is mapped to the empty tier by the caller.
var tierByRank = []string{"exact", "high", "medium", "low", "unclassified"}

// missReasons names the causes in the order they explain a miss: a reference
// that bound to nothing explains the absent edge better than one that bound
// fine, so MIN over the rank picks the explanation.
var missReasons = []CallSiteMiss{
	{Reason: "unresolved", Explanation: "the reference was bound to nothing"},
	{Reason: "ambiguous", Explanation: "several candidates and no way to choose, so no edge was claimed"},
	{Reason: "pending", Explanation: "never resolved — run 'repocortex resolve'"},
	{Reason: "ignored", Explanation: "deliberately skipped when references were bound"},
	{Reason: "resolved", Explanation: "resolved, but the call sits outside any indexed symbol, so there was no caller to attribute the edge to"},
	{Reason: "unknown", Explanation: "no call reference recorded for the line"},
}

// AccuracyAdjudication measures the heuristic resolver against the compiler
// facts imported into the same run, over the call sites both can see.
//
// It is the single gatherer behind `repocortex accuracy` and /api/accuracy. Both
// fold the result with bench.AccuracyOf, which is what makes it impossible for
// them to publish two different F1 scores for one index.
func (r *Registry) AccuracyAdjudication(ctx context.Context, runID int64) (Adjudication, error) {
	var adj Adjudication

	outcomes, err := r.accuracyOutcomes(ctx, runID)
	if err != nil {
		return adj, err
	}
	cov, err := r.accuracyCoverage(ctx, runID)
	if err != nil {
		return adj, err
	}
	langs, err := r.accuracyLanguages(ctx, runID)
	if err != nil {
		return adj, err
	}
	misses, err := r.accuracyMisses(ctx, runID)
	if err != nil {
		return adj, err
	}

	adj.Outcomes, adj.Coverage, adj.Languages, adj.Misses = outcomes, cov, langs, misses
	return adj, nil
}

// accuracyOutcomes collapses each adjudicated call site to one verdict.
func (r *Registry) accuracyOutcomes(ctx context.Context, runID int64) ([]CallSiteOutcome, error) {
	rows, err := r.db.QueryContext(ctx, accuracyCTE+`
		SELECT lang, correct,
		       COALESCE(CASE WHEN correct = 1 THEN hit_rank ELSE any_rank END, -1) AS rank,
		       COUNT(*)
		  FROM scored
		 GROUP BY lang, correct, rank
		 ORDER BY lang, correct, rank`, runID, runID, runID, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: score call sites for run %d: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []CallSiteOutcome
	for rows.Next() {
		var lang string
		var correct, rank, n int64
		if err := rows.Scan(&lang, &correct, &rank, &n); err != nil {
			return nil, fmt.Errorf("registry: score call sites for run %d: %w", runID, err)
		}
		tier := ""
		if rank >= 0 && rank < int64(len(tierByRank)) {
			tier = tierByRank[rank]
		}
		out = append(out, CallSiteOutcome{
			Language: lang, Tier: tier, Correct: correct == 1, Count: n,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: score call sites for run %d: %w", runID, err)
	}
	return out, nil
}

// accuracyCoverage counts what the rates will have been computed over.
//
// The four edge-side counts are the same predicates ResolutionPrecision uses,
// deliberately: doctor and this measurement must not disagree about how many
// CALLS edges the resolver produced.
func (r *Registry) accuracyCoverage(ctx context.Context, runID int64) (AccuracyCoverage, error) {
	var cov AccuracyCoverage
	counts := []struct {
		dst   *int64
		query string
	}{
		{&cov.HeuristicEdges, `SELECT COUNT(*) FROM edges e
		   WHERE e.index_run_id = ? AND e.kind = 'CALLS'
		     AND ` + resolverSource + ` = 'source_code'`},
		{&cov.CompilerFacts, `SELECT COUNT(*) FROM edges
		   WHERE index_run_id = ? AND source = 'compiler'`},
		{&cov.UnambiguousLines, `SELECT COUNT(*) FROM (
		     SELECT 1 FROM edges WHERE index_run_id = ? AND source = 'compiler'
		      GROUP BY evidence_file, evidence_line HAVING COUNT(DISTINCT dst_id) = 1)`},
		{&cov.AmbiguousLines, `SELECT COUNT(*) FROM (
		     SELECT 1 FROM edges WHERE index_run_id = ? AND source = 'compiler'
		      GROUP BY evidence_file, evidence_line HAVING COUNT(DISTINCT dst_id) > 1)`},
		{&cov.ParsedCallSites, `SELECT COUNT(*) FROM (
		     SELECT DISTINCT file_id, start_line FROM symbol_references
		      WHERE index_run_id = ? AND kind LIKE '%call%' AND start_line > 0)`},
	}
	for _, c := range counts {
		if err := r.db.QueryRowContext(ctx, c.query, runID).Scan(c.dst); err != nil {
			return cov, fmt.Errorf("registry: accuracy coverage for run %d: %w", runID, err)
		}
	}
	return cov, nil
}

// accuracyLanguages counts, per language, what could be measured and what was
// available to measure it with.
//
// Edges are attributed by the file the edge's SOURCE symbol lives in, exactly
// as doctor's language coverage does. A second attribution rule would let the
// two reports name different languages for the same edge.
func (r *Registry) accuracyLanguages(ctx context.Context, runID int64) ([]LanguageCoverage, error) {
	byLang := map[string]*LanguageCoverage{}
	get := func(lang string) *LanguageCoverage {
		if l, ok := byLang[lang]; ok {
			return l
		}
		l := &LanguageCoverage{Language: lang}
		byLang[lang] = l
		return l
	}

	queries := []struct {
		query  string
		assign func(l *LanguageCoverage, a, b int64)
	}{
		{`SELECT COALESCE(NULLIF(language, ''), 'unknown') AS lang, COUNT(*), 0
		    FROM files WHERE index_run_id = ? GROUP BY lang`,
			func(l *LanguageCoverage, a, _ int64) { l.Files = a }},
		{`SELECT COALESCE(NULLIF(fl.language, ''), 'unknown') AS lang,
		         COUNT(DISTINCT r.file_id || ':' || r.start_line), 0
		    FROM symbol_references r JOIN files fl ON fl.id = r.file_id
		   WHERE r.index_run_id = ? AND r.kind LIKE '%call%' AND r.start_line > 0
		   GROUP BY lang`,
			func(l *LanguageCoverage, a, _ int64) { l.CallSites = a }},
		{`SELECT COALESCE(NULLIF(f.language, ''), 'unknown') AS lang,
		         SUM(CASE WHEN ` + resolverSource + ` = 'source_code'
		                   AND e.kind = 'CALLS' THEN 1 ELSE 0 END),
		         SUM(CASE WHEN e.source = 'compiler' THEN 1 ELSE 0 END)
		    FROM edges e
		    JOIN symbols s ON s.id = e.src_id
		    JOIN files f ON f.id = s.file_id
		   WHERE e.index_run_id = ?
		   GROUP BY lang`,
			func(l *LanguageCoverage, a, b int64) { l.HeuristicEdges, l.CompilerFacts = a, b }},
	}
	for _, q := range queries {
		if err := r.eachLanguageRow(ctx, q.query, runID, func(lang string, a, b int64) {
			q.assign(get(lang), a, b)
		}); err != nil {
			return nil, err
		}
	}

	// Only languages with something at stake. A repository's Markdown carries
	// no call sites, so listing it as unmeasured would bury the language that
	// genuinely is.
	out := make([]LanguageCoverage, 0, len(byLang))
	for _, l := range byLang {
		if l.CallSites == 0 && l.HeuristicEdges == 0 && l.CompilerFacts == 0 {
			continue
		}
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CallSites != out[j].CallSites {
			return out[i].CallSites > out[j].CallSites
		}
		return out[i].Language < out[j].Language
	})
	return out, nil
}

// eachLanguageRow runs one (lang, a, b) query and hands each row to fn.
func (r *Registry) eachLanguageRow(ctx context.Context, query string, runID int64,
	fn func(lang string, a, b int64)) error {
	rows, err := r.db.QueryContext(ctx, query, runID)
	if err != nil {
		return fmt.Errorf("registry: accuracy language coverage for run %d: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var lang string
		var a, b int64
		if err := rows.Scan(&lang, &a, &b); err != nil {
			return fmt.Errorf("registry: accuracy language coverage for run %d: %w", runID, err)
		}
		fn(lang, a, b)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("registry: accuracy language coverage for run %d: %w", runID, err)
	}
	return nil
}

// accuracyMisses breaks the no-edge call sites down by cause.
func (r *Registry) accuracyMisses(ctx context.Context, runID int64) ([]CallSiteMiss, error) {
	rows, err := r.db.QueryContext(ctx, accuracyCTE+`
		SELECT reason, COUNT(*) FROM (
		    SELECT COALESCE(MIN(CASE `+resolverResolution+`
		                          WHEN 'unresolved' THEN 0 WHEN 'ambiguous' THEN 1
		                          WHEN 'pending'    THEN 2 WHEN 'ignored'   THEN 3
		                          WHEN 'resolved'   THEN 4 ELSE 5 END), 5) AS reason
		      FROM scored s
		      JOIN files fl ON fl.index_run_id = ? AND fl.path = s.f
		      LEFT JOIN symbol_references r
		             ON r.file_id = fl.id AND r.start_line = s.l AND r.kind LIKE '%call%'
		     WHERE s.any_rank IS NULL
		     GROUP BY s.f, s.l)
		 GROUP BY reason ORDER BY reason`, runID, runID, runID, runID, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: accuracy miss causes for run %d: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	out := []CallSiteMiss{}
	for rows.Next() {
		var rank, n int64
		if err := rows.Scan(&rank, &n); err != nil {
			return nil, fmt.Errorf("registry: accuracy miss causes for run %d: %w", runID, err)
		}
		if rank < 0 || rank >= int64(len(missReasons)) {
			rank = int64(len(missReasons)) - 1
		}
		m := missReasons[rank]
		m.CallSites = n
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: accuracy miss causes for run %d: %w", runID, err)
	}
	return out, nil
}
