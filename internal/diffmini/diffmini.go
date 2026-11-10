package diffmini

import (
	"bufio"
	"fmt"
	"strings"
)

// Hunk represents a single compressed change block within a file diff.
type Hunk struct {
	Header          string   `json:"header"`
	EnclosingSymbol string   `json:"enclosing_symbol,omitempty"`
	Lines           []string `json:"lines"`
}

// FileDiff represents compressed changes for one file.
type FileDiff struct {
	Path         string `json:"path"`
	OldPath      string `json:"old_path,omitempty"`
	AddedCount   int    `json:"added_count"`
	DeletedCount int    `json:"deleted_count"`
	Hunks        []Hunk `json:"hunks"`
}

// Report aggregates compressed diff metrics for token budgeting.
type Report struct {
	Files           []FileDiff `json:"files"`
	OriginalBytes   int        `json:"original_bytes"`
	CompressedBytes int        `json:"compressed_bytes"`
	SavedTokensPct  float64    `json:"saved_tokens_pct"`
	EstimatedTokens int        `json:"estimated_tokens"`
}

// Minimize compresses a unified diff into a token-efficient human/agent-readable string.
func Minimize(diffText string) (string, error) {
	rep, err := ParseAndCompress(diffText)
	if err != nil {
		return "", err
	}
	return rep.FormatText(), nil
}

// ParseAndCompress parses a unified diff and extracts compressed hunks and metrics.
func ParseAndCompress(diffText string) (*Report, error) {
	rep := &Report{
		OriginalBytes: len(diffText),
	}

	scanner := bufio.NewScanner(strings.NewReader(diffText))
	var currentFile *FileDiff
	var currentHunk *Hunk
	var contextRun []string

	flushContext := func() {
		if len(contextRun) == 0 || currentHunk == nil {
			contextRun = nil
			return
		}
		if len(contextRun) <= 2 {
			currentHunk.Lines = append(currentHunk.Lines, contextRun...)
		} else {
			// Compress unchanged context run
			currentHunk.Lines = append(currentHunk.Lines,
				contextRun[0],
				fmt.Sprintf("... (%d unchanged lines) ...", len(contextRun)-2),
				contextRun[len(contextRun)-1],
			)
		}
		contextRun = nil
	}

	flushHunk := func() {
		flushContext()
		if currentFile != nil && currentHunk != nil && len(currentHunk.Lines) > 0 {
			currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
			currentHunk = nil
		}
	}

	flushFile := func() {
		flushHunk()
		if currentFile != nil {
			rep.Files = append(rep.Files, *currentFile)
			currentFile = nil
		}
	}

	for scanner.Scan() {
		line := scanner.Text()

		// Diff file boundary: "diff --git a/path b/path"
		if strings.HasPrefix(line, "diff --git ") {
			flushFile()
			parts := strings.Fields(line)
			filePath := ""
			if len(parts) >= 4 {
				filePath = strings.TrimPrefix(parts[3], "b/")
			}
			currentFile = &FileDiff{Path: filePath}
			continue
		}

		// Metadata headers to ignore (index, ---, +++, old mode, new mode)
		if strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "old mode ") ||
			strings.HasPrefix(line, "new mode ") || strings.HasPrefix(line, "similarity index ") {
			continue
		}

		// Hunk boundary: "@@ -10,5 +10,6 @@ func Foo() {"
		if strings.HasPrefix(line, "@@ ") {
			flushHunk()
			header, symbol := parseHunkHeader(line)
			currentHunk = &Hunk{
				Header:          header,
				EnclosingSymbol: symbol,
			}
			continue
		}

		if currentHunk == nil {
			continue
		}

		// Added line
		if strings.HasPrefix(line, "+") {
			flushContext()
			currentHunk.Lines = append(currentHunk.Lines, line)
			if currentFile != nil {
				currentFile.AddedCount++
			}
			continue
		}

		// Deleted line
		if strings.HasPrefix(line, "-") {
			flushContext()
			currentHunk.Lines = append(currentHunk.Lines, line)
			if currentFile != nil {
				currentFile.DeletedCount++
			}
			continue
		}

		// Unchanged context line (starts with ' ' or empty)
		if strings.HasPrefix(line, " ") || line == "" {
			contextRun = append(contextRun, line)
		}
	}

	flushFile()

	formatted := rep.FormatText()
	rep.CompressedBytes = len(formatted)
	rep.EstimatedTokens = (rep.CompressedBytes + 3) / 4
	if rep.OriginalBytes > 0 {
		saved := float64(rep.OriginalBytes-rep.CompressedBytes) / float64(rep.OriginalBytes) * 100
		if saved < 0 {
			saved = 0
		}
		rep.SavedTokensPct = saved
	}

	return rep, nil
}

func parseHunkHeader(line string) (string, string) {
	// Example: "@@ -45,6 +45,7 @@ const (" -> header: "@@ -45,6 +45,7 @@", symbol: "const ("
	idx := strings.Index(line[3:], "@@")
	if idx < 0 {
		return line, ""
	}
	header := strings.TrimSpace(line[:idx+5])
	symbol := strings.TrimSpace(line[idx+5:])
	return header, symbol
}

// FormatText renders the compressed diff into a compact string suitable for agent prompts.
func (r *Report) FormatText() string {
	var sb strings.Builder
	for i, f := range r.Files {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "--- %s (+%d -%d) ---\n", f.Path, f.AddedCount, f.DeletedCount)
		for _, h := range f.Hunks {
			if h.EnclosingSymbol != "" {
				fmt.Fprintf(&sb, "@@ %s @@\n", h.EnclosingSymbol)
			} else {
				sb.WriteString(h.Header + "\n")
			}
			for _, l := range h.Lines {
				sb.WriteString(l + "\n")
			}
		}
	}
	return sb.String()
}
