package context

import (
	"context"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// Definition-level text evidence.
//
// searchLexical ranks FILES, and bm25 normalises by length, so a long file
// holding the one function a task is about ranks below hundreds of short files
// that say the task's words once. On a 25,000-file repository the 12,000-line
// task_service.py ranked 2,315th for "audit" and 2,454th for "status", far
// outside the fifty files read, though its _persist_audit_metadata is what
// "add an audit log entry when a task status changes" is about. Every
// definition in a matching file also carried the file's evidence alike, so a
// long file's 300 functions could not be told apart.
//
// The registry keeps each function's and method's own text (defs_fts), and
// this searches it. A definition's evidence lifts the unit that owns it: a
// method's class, a free function itself.

// definitionSearcher is the registry's definition search. It is a separate,
// optional port: a registry without it (a test double, an index from before
// the table existed) gives no definition evidence, and the pack is assembled
// from file-level text as it always was.
type definitionSearcher interface {
	SearchDefinitions(ctx context.Context, query string, limit int) ([]registry.DefinitionMatch, error)
	DefinitionsMatching(ctx context.Context, query string, ids []int64) ([]int64, error)
}

// defHit is one definition's text evidence.
type defHit struct {
	symbolID, fileID int64
	// terms are the distinct task terms the definition's own text contains.
	terms  []string
	weight float64
	// score is the definition's bm25 for the whole task: negative, more
	// negative is better.
	score float64
}

// searchDefinitions ranks definitions by the whole task at once, then learns
// which task terms each of the best contains.
//
// ONE OR QUERY FIRST, not one query per term as for files. Per term, the
// definitions that rank are the short ones that say the word, which is the
// length problem again at a smaller grain. bm25 summed over the terms a
// definition matches puts the one that uses several of the task's words, and
// its rarer ones, first. The per-term pass after it only asks which of those
// few hundred rows contain each word.
func searchDefinitions(ctx context.Context, reg Registry, in Intent, th Thresholds) map[int64]*defHit {
	ds, ok := reg.(definitionSearcher)
	if !ok || th.DefinitionLimit <= 0 {
		return nil
	}
	terms := in.Terms
	if len(terms) > th.MaxQueryTerms {
		terms = terms[:th.MaxQueryTerms]
	}
	var forms [][]string
	var all []string
	for _, t := range terms {
		f := queryForms(t)
		forms = append(forms, f)
		all = append(all, f...)
	}
	if len(all) == 0 {
		return nil
	}
	matches, err := ds.SearchDefinitions(ctx, strings.Join(all, " OR "), th.DefinitionLimit)
	if err != nil || len(matches) == 0 {
		// As for files: a failed text query degrades the pack, it does not
		// abort it.
		return nil
	}
	hits := make(map[int64]*defHit, len(matches))
	ids := make([]int64, 0, len(matches))
	for _, m := range matches {
		hits[m.SymbolID] = &defHit{symbolID: m.SymbolID, fileID: m.FileID, score: m.Score}
		ids = append(ids, m.SymbolID)
	}
	for i, t := range terms {
		if len(forms[i]) == 0 {
			continue
		}
		got, err := ds.DefinitionsMatching(ctx, strings.Join(forms[i], " OR "), ids)
		if err != nil {
			continue
		}
		for _, id := range got {
			h := hits[id]
			if h != nil && !contains(h.terms, t.Text) {
				h.terms = append(h.terms, t.Text)
				h.weight += t.Weight
			}
		}
	}
	for id, h := range hits {
		if len(h.terms) == 0 {
			delete(hits, id) // matched only through a form no term owns
			continue
		}
		sort.Strings(h.terms)
	}
	return hits
}

// unitEvidence is the best definition evidence of one unit.
type unitEvidence struct {
	hit   *defHit
	value float64
}

// definitionEvidence scores each definition hit like a file hit (coverage of
// the task's intent, scaled by bm25 against the best definition) and keeps the
// strongest per unit.
func definitionEvidence(cat *catalogue, in Intent, hits map[int64]*defHit) map[int64]unitEvidence {
	if len(hits) == 0 {
		return nil
	}
	var best float64
	for _, h := range hits {
		if h.score < best {
			best = h.score
		}
	}
	total := in.TotalWeight()
	ids := make([]int64, 0, len(hits))
	for id := range hits {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := map[int64]unitEvidence{}
	for _, id := range ids {
		h := hits[id]
		if _, known := cat.symbols[id]; !known {
			continue
		}
		coverage := 0.0
		if total > 0 {
			coverage = h.weight / total
		}
		strength := 1.0
		if best < 0 {
			strength = h.score / best
		}
		v := coverage * (0.5 + 0.5*strength)
		unit := cat.unit(id)
		if cur, ok := out[unit]; !ok || v > cur.value {
			out[unit] = unitEvidence{hit: h, value: v}
		}
	}
	return out
}
