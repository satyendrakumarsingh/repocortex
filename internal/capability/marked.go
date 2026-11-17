package capability

import (
	"regexp"
	"strings"
)

// Marked is a comment in the source that tells whoever changes the code what
// not to break: HACK, XXX, DO NOT REMOVE, NOTE(race), INVARIANT, SAFETY...
//
// WHY THE SOURCE AND NOT THE COMMIT LOG. The constraint an agent most needs is
// the one the author wrote beside the code it protects. A commit message says
// it once, in a body few tools keep, and keyword matching on messages is noise
// ("fix" is in most of them). A marked comment stays on the lines it is about
// for as long as they exist, and moves with them.
type Marked struct {
	// Line is the comment's 1-based line in its file.
	Line int `json:"line"`
	// Text is the comment from its marker on, trimmed.
	Text string `json:"text"`
}

// markedComment finds a marker inside a comment. The marker must be written in
// capitals, which is how these are written and what keeps "note that" in a
// sentence from counting; and it must follow a comment opener on the line, so
// a string or an identifier spelled DO_NOT_REMOVE does not count.
var markedComment = regexp.MustCompile(
	`(?://|#|/\*|^\s*\*|--|;|<!--)[^\n]*?\b(HACK\b|XXX\b|DO NOT [A-Z]+\b|DON'T [A-Z]+\b|NOTE\([^)\n]*\)|WARNING\b|SAFETY\b|INVARIANT\b|IMPORTANT\b|KEEP IN SYNC\b)(.*)$`)

// maxMarkedText bounds one comment's text: these are read in a note, not as a
// listing.
const maxMarkedText = 160

// MarkedComments returns the marked comments in text, whose first line is
// firstLine in its file.
func MarkedComments(text string, firstLine int) []Marked {
	var out []Marked
	for i, line := range strings.Split(text, "\n") {
		m := markedComment.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		body := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(m[1]+m[2]), "*/->"))
		if len(body) > maxMarkedText {
			body = body[:maxMarkedText-1] + "…"
		}
		out = append(out, Marked{Line: firstLine + i, Text: body})
	}
	return out
}
