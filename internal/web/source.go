package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The code map's Code view: a file's INDEXED text, with its declarations.
//
// # Why it reads the index and never the disk
//
// The text comes from SourceReader.FileExcerpt, which the registry answers from
// the full-text copy taken at index time. Three properties follow, and each is
// the reason rather than a side effect:
//
//   - Only a path the index holds can be served. There is no filesystem read,
//     so there is no path to traverse, no symlink to follow and no file outside
//     the repository to reach — the question "is this path allowed?" has no
//     place to be answered wrongly.
//   - The text is the text every relationship on every screen was extracted
//     from. A line number from the graph points at the same line here; a file
//     edited since indexing cannot shift it.
//   - Staleness is the index's staleness, which Meta already states on every
//     response. Nothing new to explain.
//
// The window is bounded (sourceMaxLines) and the remainder is recorded as an
// omission with the offset that continues it, like every other shortened list.

// SourceReader is the one read the Code view needs. Declared here, at the
// consumer; the production registry already satisfies it, and so does PackReader.
type SourceReader interface {
	FileExcerpt(ctx context.Context, runID int64, path string, startLine, lines int) (string, error)
}

// SourceDecl is one declaration in the file, joined to the structural index so
// the view can say how connected it is without a second request.
type SourceDecl struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Line       int    `json:"line"`
	Exported   bool   `json:"exported"`
	Confidence string `json:"confidence"`

	// Callers and Callees count recorded relationships at or above the floor,
	// into and out of this declaration.
	Callers int `json:"callers"`
	Callees int `json:"callees"`
}

// SourceEvidence is a line in this file that a recorded relationship cites.
type SourceEvidence struct {
	Line  int `json:"line"`
	Count int `json:"count"`
}

// SourceResponse is the Code view's payload.
type SourceResponse struct {
	Meta     Meta   `json:"meta"`
	Path     string `json:"path"`
	Language string `json:"language"`

	// Container is the graph container the file's declarations belong to, so
	// the view can ask /api/graph for their relationships without guessing.
	Container string `json:"container"`

	// From and To are the 1-based lines Lines covers. NextFrom continues the
	// file when HasMore; it is 0 otherwise.
	From     int      `json:"from"`
	To       int      `json:"to"`
	Lines    []string `json:"lines"`
	HasMore  bool     `json:"has_more"`
	NextFrom int      `json:"next_from"`

	Declarations []SourceDecl     `json:"declarations"`
	Evidence     []SourceEvidence `json:"evidence_lines"`
	Floor        string           `json:"min_confidence"`

	Omissions Omissions `json:"omissions"`
	Help      *Guidance `json:"guidance,omitempty"`
}

const (
	// sourceMaxLines bounds one response. Large enough for nearly every source
	// file in one request, small enough that a generated 200,000-line file does
	// not arrive as one JSON string.
	sourceMaxLines = 4000
)

// WithSourceReader injects the Code view's read port explicitly, for a caller
// whose Registry does not satisfy it.
func WithSourceReader(sr SourceReader) Option {
	return func(s *Server) {
		if sr != nil {
			s.deps.Source = sr
		}
	}
}

// sourceReader resolves the port: Deps.Source when wired, otherwise whichever
// wired value already satisfies it. Nil means the view answers with guidance.
func (s *Server) sourceReader() SourceReader {
	if s.deps.Source != nil {
		return s.deps.Source
	}
	if pr := s.packReader(); pr != nil {
		return pr
	}
	if sr, ok := s.deps.Registry.(SourceReader); ok {
		return sr
	}
	return nil
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	floor := confidenceParam(r, "min_confidence", DefaultFloor)
	resp := SourceResponse{
		Meta: meta, Path: path, Floor: string(floor),
		Lines: []string{}, Declarations: []SourceDecl{}, Evidence: []SourceEvidence{},
	}
	if !ok {
		resp.Help = meta.Help
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if path == "" {
		resp.Help = &Guidance{
			Title:  "Name a file",
			Detail: "The Code view shows one indexed file at a time: ?path=<path as the index records it>.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	reader := s.sourceReader()
	if reader == nil {
		resp.Help = &Guidance{
			Title:  "This portal was started without a source reader",
			Detail: "The Code view reads a file's indexed text through FileExcerpt; nothing wired here provides it.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fileID, file, found := idx.fileByPath(path)
	if !found {
		resp.Help = &Guidance{
			Title: fmt.Sprintf("%s is not a file in this index", path),
			Detail: "Only files the index recorded can be shown, and only as they were when it was built. " +
				"The path may be spelled differently, ignored by the walker, or newer than the index.",
			Steps: []string{"repocortex index — if the file is newer than the index"},
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Language = file.Language

	from := intParam(r, "from", 1, 1, 1<<30)
	// Ask for everything from `from` on: FileExcerpt holds the whole text in
	// memory already, and the remainder is what makes the omission a count.
	text, err := reader.FileExcerpt(ctx, run.ID, path, from, 1<<30)
	if errors.Is(err, sql.ErrNoRows) {
		resp.Help = &Guidance{
			Title: "The index holds this file's symbols but not its text",
			Detail: "Full text is stored for files the indexer reads as text; this one was recorded without it, " +
				"so its relationships are known but its lines cannot be shown.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("source for %s: %w", path, err))
		return
	}

	lines := []string{}
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	var om Omissions
	if len(lines) > sourceMaxLines {
		rest := len(lines) - sourceMaxLines
		lines = lines[:sourceMaxLines]
		resp.HasMore = true
		resp.NextFrom = from + sourceMaxLines
		om.Hide(ReasonRowBudget, rest,
			fmt.Sprintf("This response carries lines %d–%d; %d more follow.", from, from+sourceMaxLines-1, rest),
			fmt.Sprintf("?from=%d", resp.NextFrom))
	}
	resp.From = from
	resp.To = from + len(lines) - 1
	if len(lines) == 0 {
		resp.To = 0
	}
	resp.Lines = lines
	resp.Declarations, resp.Evidence = idx.sourceFacts(fileID, path, floor)
	resp.Container = idx.containerOfFile(fileID)
	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

// fileByPath finds a file of this run by its recorded path.
func (i *structuralIndex) fileByPath(path string) (int64, fileInfo, bool) {
	for id, f := range i.files {
		if f.Path == path {
			return id, f, true
		}
	}
	return 0, fileInfo{}, false
}

// sourceFacts returns the file's declarations, ordered by line, with their
// relationship counts at the floor, and every line of the file that a
// relationship at the floor cites as evidence.
func (i *structuralIndex) sourceFacts(fileID int64, path string, floor model.Confidence) ([]SourceDecl, []SourceEvidence) {
	decls := []SourceDecl{}
	pos := map[int64]int{}
	for id, sym := range i.syms {
		if sym.FileID != fileID {
			continue
		}
		pos[id] = len(decls)
		decls = append(decls, SourceDecl{
			ID: id, Name: sym.Name, Kind: string(sym.Kind), Line: sym.Line,
			Exported: sym.Exported, Confidence: tierOf(sym.Confidence),
		})
	}
	byLine := map[int]int{}
	for _, e := range i.edges {
		if !e.Confidence.AtLeast(floor) {
			continue
		}
		if p, ok := pos[e.Dst]; ok {
			decls[p].Callers++
		}
		if p, ok := pos[e.Src]; ok {
			decls[p].Callees++
		}
		if e.File == path && e.Line > 0 {
			byLine[int(e.Line)]++
		}
	}
	sort.Slice(decls, func(a, b int) bool {
		if decls[a].Line != decls[b].Line {
			return decls[a].Line < decls[b].Line
		}
		return decls[a].ID < decls[b].ID
	})
	evidence := make([]SourceEvidence, 0, len(byLine))
	for line, n := range byLine {
		evidence = append(evidence, SourceEvidence{Line: line, Count: n})
	}
	sort.Slice(evidence, func(a, b int) bool { return evidence[a].Line < evidence[b].Line })
	return decls, evidence
}

// containerOfFile names the container the file's symbols were assigned to, or
// "" for a file with no symbols.
func (i *structuralIndex) containerOfFile(fileID int64) string {
	for _, sym := range i.syms {
		if sym.FileID == fileID && sym.Container != "" {
			return sym.Container
		}
	}
	return ""
}
