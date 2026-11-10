// Package search provides retrieval over the registry.
package search

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SplitIdentifier expands a code identifier into its constituent words.
//
// Why this exists: SQLite FTS5's unicode61 tokenizer treats an identifier as
// one opaque token, so "handleCallback" indexes as a single term and a search
// for "callback" finds nothing. For a code search engine that is a correctness
// problem, not a nicety — a bare word that appears only as a fragment of a
// camelCase or snake_case identifier is the single most common query shape.
//
// Verified empirically against modernc.org/sqlite; see
// tests/integration/fts5_guarantee_test.go.
//
//	handleCallback  -> [handle callback]
//	HTTPServer      -> [http server]
//	handle_callback -> [handle callback]
//
// Single-character fragments are dropped: they add index weight and match
// almost everything.
func SplitIdentifier(s string) []string {
	var out []string
	var cur strings.Builder

	flush := func() {
		// Lowercase FIRST, then measure in runes. Two traps here, both found
		// by FuzzSplitIdentifier: byte length overstates short non-ASCII
		// fragments, and case folding can shorten a string (U+0130 'İ' is two
		// bytes but folds to the single character "i").
		lower := strings.ToLower(cur.String())
		cur.Reset()
		if utf8.RuneCountInString(lower) > 1 {
			out = append(out, lower)
		}
	}

	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || r == '/' || r == '\\' || unicode.IsSpace(r):
			flush()
		case unicode.IsUpper(r):
			// Break on lower->Upper (handleCallback), and on the final capital
			// of an acronym run (HTTPServer -> HTTP, Server).
			prevLower := i > 0 && unicode.IsLower(runes[i-1])
			acronymEnd := i > 0 && unicode.IsUpper(runes[i-1]) &&
				i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if prevLower || acronymEnd {
				flush()
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// ExpandIdentifiers builds the text stored in the FTS `expanded` column: every
// identifier in src, split into its parts and de-duplicated.
//
// The original text is indexed separately so snippet() still returns real
// source rather than this derived form.
func ExpandIdentifiers(src string) string {
	seen := make(map[string]struct{})
	var parts []string

	fields := strings.FieldsFunc(src, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	for _, f := range fields {
		for _, w := range SplitIdentifier(f) {
			if _, dup := seen[w]; dup {
				continue
			}
			seen[w] = struct{}{}
			parts = append(parts, w)
		}
	}
	return strings.Join(parts, " ")
}
