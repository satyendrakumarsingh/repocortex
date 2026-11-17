package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// markedAbove is how many lines above a definition are read for its marked
// comments: a doc comment or an annotation sits there.
const markedAbove = 4

// maxMarkedSpan bounds the lines read for one definition. A class can be
// thousands of lines; its warnings are in its doc comment and its methods,
// which have their own lookups.
const maxMarkedSpan = 600

// maxMarkedListed bounds how many marked comments one note names.
const maxMarkedListed = 5

// markedNote is one note naming the comments in and just above a definition
// that tell whoever changes it what not to break (capability.MarkedComments),
// or "" when there are none. An agent about to edit a function reads this in
// the same answer that shows it the function, which is where it is needed.
func (s *Server) markedNote(ctx context.Context, runID int64, sym registry.SymbolHit) string {
	start := max(1, sym.StartLine-markedAbove)
	n := min(sym.EndLine-start+1, maxMarkedSpan)
	if n < 1 {
		return ""
	}
	text, err := s.deps.Registry.FileExcerpt(ctx, runID, sym.FilePath, start, n)
	if err != nil || text == "" {
		return ""
	}
	// The lines above belong to the definition only as its doc comment: the
	// unbroken run of comment and annotation lines directly above it. Past
	// that is the end of whatever came before, whose warnings are its own.
	lines := strings.Split(text, "\n")
	docFrom := sym.StartLine
	for i := sym.StartLine - 1 - start; i >= 0 && i < len(lines); i-- {
		if !isDocLine(lines[i]) {
			break
		}
		docFrom = start + i
	}
	var marks []capability.Marked
	for _, m := range capability.MarkedComments(text, start) {
		if m.Line >= docFrom {
			marks = append(marks, m)
		}
	}
	if len(marks) == 0 {
		return ""
	}
	parts := make([]string, 0, min(len(marks), maxMarkedListed))
	for _, m := range marks[:min(len(marks), maxMarkedListed)] {
		parts = append(parts, fmt.Sprintf("%s:%d %s", sym.FilePath, m.Line, m.Text))
	}
	note := fmt.Sprintf("Read before changing %s: the source marks %s. %s", sym.Name,
		pluralCount(len(marks), "comment"), strings.Join(parts, "; "))
	if extra := len(marks) - len(parts); extra > 0 {
		note += fmt.Sprintf("; and %d more", extra)
	}
	return note
}

func pluralCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// isDocLine reports whether a line can belong to the doc block above a
// definition: a comment, or an annotation or decorator.
func isDocLine(line string) bool {
	t := strings.TrimSpace(line)
	for _, p := range []string{"//", "#", "/*", "*", "--", "@", ";"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}
