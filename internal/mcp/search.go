package mcp

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type searchArgs struct {
	Query string `json:"query" jsonschema:"Search terms. Matches identifiers and code text; camelCase identifiers are searchable by their parts, so 'callback' finds handleCallback. Punctuation separates words: 'errors.New' finds those two side by side."`
	Limit int    `json:"limit,omitempty" jsonschema:"Most files to return. Default 20, maximum 200."`
}

// defaultSearchItems is how many files search_code returns unless asked.
const defaultSearchItems = 20

// ftsQuery turns what a caller typed into a full-text MATCH expression that
// cannot be a syntax error.
//
// THE DEFECT. The query went to the full-text engine verbatim, and that engine
// has a query language of its own in which `.`, `:`, `-`, `$`, `#` and `@` are
// operators or errors. So the three most natural things to search for in code
// — `errors.New`, `Utils::chooseHandler`, `$this->handler` — each failed, and
// failed as error_code: internal with the hint "fall back to search_code",
// which is the tool that had just failed.
//
// THE RULE. The index tokenises text on anything that is not a letter or a
// digit, so `errors.New` is stored as the two words `errors` and `New` next to
// each other. Each whitespace-separated term of the query is therefore turned
// into the same thing: the PHRASE of its letter-and-digit runs, quoted, which
// matches exactly where those words are adjacent. A term with no punctuation
// is a one-word phrase, which is what it always was.
//
// Three pieces of the engine's own syntax are kept, because a caller who knows
// them means them: AND, OR and NOT between two terms; a trailing `*` for a
// prefix; and a quoted phrase.
//
// rewritten lists the phrases that differ from what was typed, so the answer
// can say what was actually searched for. An empty match means the query held
// no letter or digit at all.
func ftsQuery(raw string) (match string, rewritten []string) {
	var parts []string
	lastWasTerm := false

	fields := queryFields(raw)
	for i, f := range fields {
		if !f.quoted && (f.text == "AND" || f.text == "OR" || f.text == "NOT") {
			// An operator only where the engine accepts one: after a term and
			// before another. Anywhere else it is a word like any other, and
			// passing it through would be the syntax error this function
			// exists to prevent.
			if lastWasTerm && anyTerm(fields[i+1:]) {
				parts = append(parts, f.text)
				lastWasTerm = false
				continue
			}
		}

		prefix := !f.quoted && strings.HasSuffix(f.text, "*")
		runs := wordRuns(f.text)
		if len(runs) == 0 {
			continue
		}
		phrase := `"` + strings.Join(runs, " ") + `"`
		if prefix {
			phrase += "*"
		}
		parts = append(parts, phrase)
		lastWasTerm = true

		plain := strings.TrimSuffix(f.text, "*")
		if (!f.quoted && len(runs) > 1) || (len(runs) == 1 && runs[0] != plain) {
			rewritten = append(rewritten, strings.Join(runs, " "))
		}
	}
	// A dangling operator would be a syntax error; it has no right-hand side.
	for len(parts) > 0 {
		last := parts[len(parts)-1]
		if last != "AND" && last != "OR" && last != "NOT" {
			break
		}
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, " "), rewritten
}

// FTSQuery is ftsQuery for callers outside this package, so the command line
// and the search_code tool turn what was typed into the same MATCH expression.
func FTSQuery(raw string) (match string, rewritten []string) { return ftsQuery(raw) }

type queryField struct {
	text   string
	quoted bool
}

// queryFields splits a query on whitespace, keeping a double-quoted run as one
// field.
func queryFields(raw string) []queryField {
	var out []queryField
	var cur strings.Builder
	inQuote := false
	flush := func(quoted bool) {
		if cur.Len() > 0 {
			out = append(out, queryField{text: cur.String(), quoted: quoted})
			cur.Reset()
		}
	}
	for _, r := range raw {
		switch {
		case r == '"':
			flush(inQuote)
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			flush(false)
		default:
			cur.WriteRune(r)
		}
	}
	flush(inQuote)
	return out
}

// anyTerm reports whether any field carries a letter or digit.
func anyTerm(fields []queryField) bool {
	for _, f := range fields {
		if len(wordRuns(f.text)) > 0 {
			return true
		}
	}
	return false
}

// wordRuns returns the maximal runs of letters and digits in s, which is how
// the index's tokenizer sees it.
func wordRuns(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func (s *Server) searchCode(ctx context.Context, req *sdk.CallToolRequest, args searchArgs) (*sdk.CallToolResult, *Response, error) {
	if strings.TrimSpace(args.Query) == "" {
		return failWith("invalid_argument",
			"Pass a non-empty query, then call search_code again.",
			"query must not be empty")
	}
	if args.Limit < 0 {
		return failWith("invalid_argument", "Pass limit as a positive number, or omit it.",
			"limit must not be negative")
	}
	runID, status, err := s.currentRun(ctx)
	if err != nil {
		return s.noRun(ctx, req, err)
	}

	limit := args.Limit
	if limit == 0 {
		limit = defaultSearchItems
	}
	// Clamped to the maximum, not reset to the default: limit=500 used to
	// return twenty files with nothing to say the other four hundred and
	// eighty had been asked for.
	limit = min(limit, s.maxPage())

	match, rewritten := ftsQuery(args.Query)
	if match == "" {
		return s.guide(ctx, req, runID, status,
			fmt.Sprintf("Nothing to search for in %q: it holds no letter or digit.", args.Query),
			"The index stores words, and punctuation only separates them. Search for an identifier "+
				"or a word from the code you are looking for.")
	}

	// One more than asked for, so "there are more" is known and not guessed
	// from a full page.
	matches, err := s.deps.Registry.SearchFiles(ctx, match, limit+1)
	if err != nil {
		// ftsQuery cannot produce a syntax error by construction. If the
		// engine reports one anyway, that is a query this tool cannot run,
		// which is the caller's to change — not an internal failure.
		if strings.Contains(err.Error(), "fts5") || strings.Contains(err.Error(), "syntax error") {
			return failWith("invalid_argument",
				"Search for plain words or identifiers. AND, OR and NOT are accepted between two terms, "+
					"and a trailing * matches a prefix.",
				"the query %q could not be searched: %v", args.Query, err)
		}
		return failFrom(err, "search failed: %v", err)
	}
	more := len(matches) > limit
	if more {
		matches = matches[:limit]
	}

	resp := &Response{
		IndexRevision: runID,
		IndexStatus:   status,
		Summary:       fmt.Sprintf("%d file(s) matching %q.", len(matches), args.Query),
		itemCap:       limit,
	}
	if more {
		resp.Summary = fmt.Sprintf("The best %d file(s) matching %q; more match.", len(matches), args.Query)
		resp.markTruncated(fmt.Sprintf("showing the %d best-matching files and more match; "+
			"add a term to narrow it, or raise limit (maximum %d)", len(matches), s.maxPage()))
	}
	for _, m := range matches {
		resp.Items = append(resp.Items, Item{
			Kind: "file",
			Path: m.Path,
			Why:  "full-text match",
		})
	}
	if len(rewritten) > 0 {
		resp.Notes = append(resp.Notes, fmt.Sprintf(
			"Searched for %s as adjacent words. The index splits text on punctuation, so a.b, a::b, "+
				"a->b and a_b are all stored, and matched, as the words a b next to each other.",
			quotedList(rewritten)))
	}
	if len(matches) == 0 {
		resp.Notes = append(resp.Notes,
			"No matches. Search covers indexed files only; generated and oversized files are skipped, "+
				"and so is every file in a language the index has no parser for. "+
				"Try a shorter term — identifiers are indexed by their parts.")
	}
	return s.reply(ctx, req, resp)
}

// quotedList renders up to three phrases as "a b", "c d" and 2 more.
func quotedList(items []string) string {
	shown := items
	if len(shown) > 3 {
		shown = shown[:3]
	}
	quoted := make([]string, len(shown))
	for i, it := range shown {
		quoted[i] = fmt.Sprintf("%q", it)
	}
	out := strings.Join(quoted, ", ")
	if rest := len(items) - len(shown); rest > 0 {
		out += fmt.Sprintf(" and %d more", rest)
	}
	return out
}
