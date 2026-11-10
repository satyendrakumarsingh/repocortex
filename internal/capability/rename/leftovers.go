package rename

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/satyendrakumarsingh/repocortex/internal/edit"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TextSearcher finds the indexed files whose text mentions a term — the
// index's full-text search, the one `cortex search` uses — and lists the
// files that search cannot see because the index holds no text for them.
type TextSearcher interface {
	SearchFiles(ctx context.Context, query string, limit int) ([]registry.FileMatch, error)
	FilesWithoutText(ctx context.Context, runID int64) ([]model.File, error)
}

// FileReader returns a file's current bytes, by its path relative to the
// project root. The caller decides containment and size limits.
type FileReader func(path string) ([]byte, error)

// Default bounds of a leftover scan.
const (
	DefaultMaxLeftovers     = 50
	DefaultMaxLeftoverFiles = 500
	maxLeftoverText         = 160
)

// LeftoverLimits bounds a scan; zero fields take the defaults.
type LeftoverLimits struct {
	// MaxItems caps the lines returned; Total still counts every one found.
	MaxItems int
	// MaxFiles caps the candidate files the full-text search returns. Hitting
	// it makes Total a lower bound.
	MaxFiles int
}

// Leftover is one line that still names the old identifier.
type Leftover struct {
	Path  string `json:"path"`
	Line  int    `json:"line"`
	Count int    `json:"count"` // whole-identifier occurrences on the line
	Text  string `json:"text"`  // trimmed, bounded, escaped for a terminal
}

// LeftoverScan is what a scan found.
type LeftoverScan struct {
	Items []Leftover `json:"leftovers"`
	// Total is every line found, including those past MaxItems.
	Total int `json:"leftover_total"`
	// LowerBound: the candidate-file cap was reached, so more may exist.
	LowerBound bool `json:"leftover_total_is_lower_bound,omitempty"`
	// Unreadable counts candidate files that could not be read now, or that
	// the redaction policy marks sensitive and are therefore never read.
	Unreadable int `json:"leftover_unreadable,omitempty"`
	// Unsearchable: the name has no letter or digit ($, _), so no scan can
	// tell its uses from punctuation.
	Unsearchable bool `json:"leftover_unsearchable,omitempty"`
}

// Clear reports whether the scan proves no occurrence is left: none found,
// every candidate read, no cap reached, and the name searchable at all.
func (s LeftoverScan) Clear() bool {
	return s.Total == 0 && !s.LowerBound && s.Unreadable == 0 && !s.Unsearchable
}

// searchable reports whether name has a letter or digit — something a word
// index can hold.
func searchable(name string) bool {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// FindLeftovers lists the lines of the run's files that still contain old as
// a whole identifier — by internal/edit's identifier rules, so CreateOrderX is
// not a hit. These are references RepoCortex never bound, or comments and
// strings: either way, a person has to look.
//
// exclude holds sites still expected to hold old (a dry run's sites): each
// one discounts one occurrence on its line. After an apply, pass nil — the
// edited sites no longer hold old, and anything left on their lines is a real
// leftover.
//
// Candidates are the full-text hits plus every file the index holds no text
// for (skipped as too large or binary, or redacted), which the search cannot
// see. A file the redaction policy marks sensitive is never read; it counts
// as Unreadable.
func FindLeftovers(ctx context.Context, ts TextSearcher, runID int64, read FileReader, old string, exclude []Site, lim LeftoverLimits) (LeftoverScan, error) {
	if !searchable(old) {
		return LeftoverScan{Items: []Leftover{}, Unsearchable: true}, nil
	}
	if lim.MaxItems <= 0 {
		lim.MaxItems = DefaultMaxLeftovers
	}
	if lim.MaxFiles <= 0 {
		lim.MaxFiles = DefaultMaxLeftoverFiles
	}
	// A phrase query: old is an identifier, so it holds no quote to escape.
	matches, err := ts.SearchFiles(ctx, `"`+old+`"`, lim.MaxFiles)
	if err != nil {
		return LeftoverScan{}, fmt.Errorf("rename: search for leftover %q: %w", old, err)
	}
	type pos struct {
		path string
		line int
	}
	discount := make(map[pos]int, len(exclude))
	for _, s := range exclude {
		discount[pos{s.Path, s.Line}]++
	}

	noText, err := ts.FilesWithoutText(ctx, runID)
	if err != nil {
		return LeftoverScan{}, fmt.Errorf("rename: list files without text: %w", err)
	}
	seen := make(map[string]bool, len(matches)+len(noText))
	paths := make([]string, 0, len(matches)+len(noText))
	for _, p := range matchPaths(matches, noText) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	scan := LeftoverScan{Items: []Leftover{}, LowerBound: len(matches) >= lim.MaxFiles}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return LeftoverScan{}, fmt.Errorf("rename: leftover scan: %w", err)
		}
		if security.IsSensitivePath(path) {
			scan.Unreadable++
			continue
		}
		src, err := read(path)
		if err != nil {
			scan.Unreadable++
			continue
		}
		ln := 0
		for len(src) > 0 || ln == 0 {
			ln++
			line := src
			if i := bytes.IndexByte(src, '\n'); i >= 0 {
				line, src = src[:i], src[i+1:]
			} else {
				src = nil
			}
			line = bytes.TrimSuffix(line, []byte{'\r'})
			n := len(edit.Occurrences(line, old)) - discount[pos{path, ln}]
			if n <= 0 {
				continue
			}
			scan.Total++
			if len(scan.Items) < lim.MaxItems {
				scan.Items = append(scan.Items, Leftover{Path: path, Line: ln, Count: n, Text: terminalSafe(line)})
			}
		}
	}
	return scan, nil
}

func matchPaths(matches []registry.FileMatch, noText []model.File) []string {
	out := make([]string, 0, len(matches)+len(noText))
	for _, m := range matches {
		out = append(out, m.Path)
	}
	for _, f := range noText {
		out = append(out, f.Path)
	}
	return out
}

// TerminalSafe escapes anything in s a terminal would interpret: control
// characters (escape sequences included), bidirectional overrides, and
// invalid UTF-8. For paths and other text printed verbatim.
func TerminalSafe(s string) string {
	return escapeTerminal([]byte(s), -1)
}

// terminalSafe trims a source line, bounds it, and escapes it like
// TerminalSafe.
func terminalSafe(line []byte) string {
	return escapeTerminal(bytes.TrimSpace(line), maxLeftoverText)
}

// escapeTerminal escapes line, cutting it after limit runes (limit < 0: no
// cut).
func escapeTerminal(line []byte, limit int) string {
	var b strings.Builder
	for n := 0; len(line) > 0; n++ {
		if n == limit {
			b.WriteString("...")
			break
		}
		r, size := utf8.DecodeRune(line)
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, line[0])
		case unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		line = line[size:]
	}
	return b.String()
}
