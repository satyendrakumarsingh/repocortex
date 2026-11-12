package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Symbol lookup, for the blast-radius picker.
//
// A plain substring scan over the cached symbol table, ordered by a MECHANICAL,
// STATED rule: exact name first, then a name prefix, then anything containing
// the query; exported before unexported; shorter names before longer; symbol id
// last as the tie-break. It is not a relevance ranking and must not become one —
// relevance ranking lives in internal/context, where it is measured, and a
// second scoring function here would be a second thing to keep calibrated.
//
// The rule is echoed in the response so a reader who wonders why a symbol sits
// where it does can read the answer rather than infer it.

// SymbolMatch is one candidate for the picker.
type SymbolMatch struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	FQN       string `json:"fqn"`
	Kind      string `json:"kind"`
	Path      string `json:"file_path"`
	Language  string `json:"language"`
	Container string `json:"container"`
	Exported  bool   `json:"exported"`
	Degree    int    `json:"degree"`

	// Confidence is how reliably the SYMBOL was extracted — a report built on a
	// shaky definition should say so before it says anything about callers.
	Confidence string `json:"confidence"`

	// Match is exact | prefix | contains, so the ordering above is legible.
	Match string `json:"match"`
}

// SymbolsResponse is the picker's payload.
type SymbolsResponse struct {
	Meta  Meta   `json:"meta"`
	Query string `json:"query"`

	// Ordering states the rule. See the file comment for why it is not a score.
	Ordering string `json:"ordering"`

	Matches   []SymbolMatch `json:"matches"`
	Total     int           `json:"total_matched"`
	Omissions Omissions     `json:"omissions"`

	Help *Guidance `json:"guidance,omitempty"`
}

// symbolLimit bounds a picker response. A typeahead that returns two thousand
// rows has answered a different question from the one asked.
const symbolLimit = 40

func (s *Server) handleSymbols(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	resp := SymbolsResponse{
		Meta: meta, Query: query, Matches: []SymbolMatch{},
		Ordering: "exact name, then prefix, then substring; exported first; shorter name first; symbol id as tie-break. " +
			"A mechanical rule, not a relevance score — ranking belongs to the context engine, where it is measured.",
	}
	if !ok {
		resp.Help = meta.Help
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if query == "" {
		resp.Help = &Guidance{
			Title:  "Type a symbol name",
			Detail: "Matching is on the symbol's name and its fully-qualified name.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	needle := strings.ToLower(query)
	var matches []SymbolMatch
	for id, sym := range idx.syms {
		lowerName := strings.ToLower(sym.Name)
		kind := ""
		switch {
		case lowerName == needle:
			kind = "exact"
		case strings.HasPrefix(lowerName, needle):
			kind = "prefix"
		case strings.Contains(lowerName, needle) || strings.Contains(strings.ToLower(sym.FQN), needle):
			kind = "contains"
		default:
			continue
		}
		matches = append(matches, SymbolMatch{
			ID: id, Name: sym.Name, FQN: sym.FQN, Kind: string(sym.Kind),
			Path: idx.symbolPath(id), Language: idx.symbolLanguage(id),
			Container: sym.Container, Exported: sym.Exported, Degree: sym.Degree,
			Confidence: tierOf(sym.Confidence), Match: kind,
		})
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if ra, rb := matchRank(a.Match), matchRank(b.Match); ra != rb {
			return ra < rb
		}
		if a.Exported != b.Exported {
			return a.Exported
		}
		if len(a.Name) != len(b.Name) {
			return len(a.Name) < len(b.Name)
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})

	var om Omissions
	resp.Total = len(matches)
	resp.Matches = capped(matches, symbolLimit, &om, ReasonRowBudget,
		fmt.Sprintf("%d symbol(s) matched; the first %d are shown under the ordering above.",
			len(matches), symbolLimit),
		"type more of the name")
	if resp.Matches == nil {
		resp.Matches = []SymbolMatch{}
	}
	if len(matches) == 0 {
		resp.Help = &Guidance{
			Title:  fmt.Sprintf("Nothing in this index matches %q", query),
			Detail: "The index holds only what its parsers could extract. A language with no extracted symbols will never match.",
			Steps: []string{
				"Check the overview's blind spots for the language you expected.",
				"repocortex index — if the file is newer than the index",
			},
		}
	}
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

func matchRank(m string) int {
	switch m {
	case "exact":
		return 0
	case "prefix":
		return 1
	default:
		return 2
	}
}
