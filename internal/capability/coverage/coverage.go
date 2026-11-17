package coverage

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RangeCoverage records a covered line range.
type RangeCoverage struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
	Count     int `json:"count"`
}

// FileCoverage represents coverage data for a single file.
type FileCoverage struct {
	Path         string          `json:"path"`
	TotalLines   int             `json:"total_lines"`
	CoveredLines int             `json:"covered_lines"`
	CoverageRate float64         `json:"coverage_rate"`
	Lines        map[int]int     `json:"-"`
	Ranges       []RangeCoverage `json:"ranges,omitempty"`
}

// Report holds the parsed test coverage report across all files.
type Report struct {
	Format       string                   `json:"format"`
	Files        map[string]*FileCoverage `json:"files"`
	TotalLines   int                      `json:"total_lines"`
	CoveredLines int                      `json:"covered_lines"`
	CoverageRate float64                  `json:"coverage_rate"`
}

// ParseCoverageFile reads and parses a coverage file, auto-detecting the format.
func ParseCoverageFile(filePath string) (*Report, error) {
	//nolint:gosec // filePath is the user-supplied coverage file to import
	f, err := os.Open(filepath.Clean(filePath))
	if err != nil {
		return nil, fmt.Errorf("open coverage file: %w", err)
	}
	defer func() { _ = f.Close() }()

	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".xml":
		return parseXMLReport(f)
	case ".info", ".lcov":
		return ParseLCOV(f)
	default:
		// Sniff first line
		r := bufio.NewReader(f)
		header, err := r.Peek(64)
		if err == nil && strings.HasPrefix(string(header), "mode:") {
			return ParseGoCover(r)
		}
		if err == nil && (strings.HasPrefix(string(header), "SF:") || strings.HasPrefix(string(header), "TN:")) {
			return ParseLCOV(r)
		}
		trimmed := strings.TrimSpace(string(header))
		if err == nil && (strings.HasPrefix(trimmed, "<?xml") || strings.HasPrefix(trimmed, "<coverage") ||
			strings.HasPrefix(trimmed, "<report") || strings.HasPrefix(trimmed, "<!DOCTYPE")) {
			return parseXMLReport(r)
		}
		// Fallback to Go coverprofile
		return ParseGoCover(r)
	}
}

// ParseGoCover parses Go's standard coverprofile format.
func ParseGoCover(r io.Reader) (*Report, error) {
	scanner := bufio.NewScanner(r)
	files := make(map[string]*FileCoverage)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// Format: file.go:startLine.startCol,endLine.endCol numStatements count
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		locParts := strings.Split(parts[0], ":")
		if len(locParts) < 2 {
			continue
		}
		filePath := normalizePath(locParts[0])
		spanParts := strings.Split(locParts[1], ",")
		if len(spanParts) < 2 {
			continue
		}

		startLine, err1 := strconv.Atoi(strings.Split(spanParts[0], ".")[0])
		endLine, err2 := strconv.Atoi(strings.Split(spanParts[1], ".")[0])
		count, err3 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}

		fc, ok := files[filePath]
		if !ok {
			fc = &FileCoverage{
				Path:  filePath,
				Lines: make(map[int]int),
			}
			files[filePath] = fc
		}

		for l := startLine; l <= endLine; l++ {
			fc.Lines[l] += count
		}
		fc.Ranges = append(fc.Ranges, RangeCoverage{
			StartLine: startLine,
			EndLine:   endLine,
			Count:     count,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read go coverage: %w", err)
	}
	return finalizeReport("go-cover", files), nil
}

// ParseLCOV parses LCOV format (.info / .lcov).
func ParseLCOV(r io.Reader) (*Report, error) {
	scanner := bufio.NewScanner(r)
	files := make(map[string]*FileCoverage)
	var currentFile *FileCoverage

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "SF:"):
			p := normalizePath(strings.TrimPrefix(line, "SF:"))
			currentFile = &FileCoverage{
				Path:  p,
				Lines: make(map[int]int),
			}
			files[p] = currentFile
		case strings.HasPrefix(line, "DA:") && currentFile != nil:
			// DA:<line>,<execution_count>
			daParts := strings.Split(strings.TrimPrefix(line, "DA:"), ",")
			if len(daParts) >= 2 {
				lineNum, _ := strconv.Atoi(daParts[0])
				count, _ := strconv.Atoi(daParts[1])
				if lineNum > 0 {
					currentFile.Lines[lineNum] += count
				}
			}
		case line == "end_of_record":
			currentFile = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read lcov: %w", err)
	}
	return finalizeReport("lcov", files), nil
}

type xmlCoverage struct {
	Packages []struct {
		Classes []struct {
			Filename string `xml:"filename,attr"`
			Lines    []struct {
				Number int `xml:"number,attr"`
				Hits   int `xml:"hits,attr"`
			} `xml:"lines>line"`
		} `xml:"classes>class"`
	} `xml:"packages>package"`
}

// ParseCobertura parses Cobertura XML format.
func ParseCobertura(r io.Reader) (*Report, error) {
	var xc xmlCoverage
	if err := xml.NewDecoder(r).Decode(&xc); err != nil {
		return nil, fmt.Errorf("decode cobertura xml: %w", err)
	}
	files := make(map[string]*FileCoverage)
	for _, pkg := range xc.Packages {
		for _, cls := range pkg.Classes {
			p := normalizePath(cls.Filename)
			fc, ok := files[p]
			if !ok {
				fc = &FileCoverage{
					Path:  p,
					Lines: make(map[int]int),
				}
				files[p] = fc
			}
			for _, l := range cls.Lines {
				if l.Number > 0 {
					fc.Lines[l.Number] += l.Hits
				}
			}
		}
	}
	return finalizeReport("cobertura", files), nil
}

// xmlRootName returns the local name of the first element in data. Both
// Cobertura (<coverage>) and JaCoCo (<report>) are .xml files, so the root
// element is the only reliable discriminator. Leading declarations, comments
// and a DOCTYPE are skipped: encoding/xml surfaces the DOCTYPE as a Directive
// token and never fetches the DTD it names, so nothing is loaded from disk or
// the network.
func xmlRootName(data []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("find xml root element: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, nil
		}
	}
}

// parseXMLReport routes an XML coverage report to the Cobertura or JaCoCo
// parser by root element. Anything else is reported as unsupported rather than
// silently parsed as an empty Cobertura report.
func parseXMLReport(r io.Reader) (*Report, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read coverage xml: %w", err)
	}
	root, err := xmlRootName(data)
	if err != nil {
		return nil, err
	}
	switch root {
	case "report":
		return ParseJaCoCo(bytes.NewReader(data))
	case "coverage":
		return ParseCobertura(bytes.NewReader(data))
	default:
		return nil, fmt.Errorf("unsupported coverage xml root element <%s> (want <coverage> for Cobertura or <report> for JaCoCo)", root)
	}
}

// ParseJaCoCo parses the jacoco.xml written by the Maven and Gradle JaCoCo
// plugins. A file is keyed as "<package>/<sourcefile>", for example
// "com/acme/x/Foo.java": JaCoCo records no source root, so the key is relative
// to it. IsSymbolCovered already suffix-matches report paths against indexed
// paths, which resolves it to src/main/java/com/acme/x/Foo.java (or a Gradle
// module's src/main/kotlin) without a separate mapping.
//
// A line counts as covered when any of its instructions ran (ci > 0), and as
// uncovered when none did but some exist (mi > 0). Lines with neither carry no
// executable code and are skipped, so they cannot dilute the rate. A partially
// covered line (mi > 0 and ci > 0) is covered: the line ran, which is the
// signal a line-level test-coverage map needs.
func ParseJaCoCo(r io.Reader) (*Report, error) {
	// Read as a stream rather than into a fixed struct: a multi-module
	// aggregate report (Maven's report-aggregate) nests packages inside
	// <group> elements, to any depth, and a fixed report>package path read
	// such a report as empty.
	dec := xml.NewDecoder(r)
	files := make(map[string]*FileCoverage)
	var pkg string
	var fc *FileCoverage
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode jacoco xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "package":
				pkg = xmlAttr(t, "name")
			case "sourcefile":
				name := xmlAttr(t, "name")
				p := name
				if pkg != "" {
					p = pkg + "/" + name // the default package has no directory
				}
				p = normalizePath(p)
				if fc = files[p]; fc == nil {
					fc = &FileCoverage{Path: p, Lines: make(map[int]int)}
					files[p] = fc
				}
			case "line":
				if fc == nil {
					continue
				}
				nr, _ := strconv.Atoi(xmlAttr(t, "nr"))
				mi, _ := strconv.Atoi(xmlAttr(t, "mi"))
				ci, _ := strconv.Atoi(xmlAttr(t, "ci"))
				if nr <= 0 || (ci == 0 && mi == 0) {
					continue
				}
				fc.Lines[nr] += ci
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "package":
				pkg = ""
			case "sourcefile":
				fc = nil
			}
		}
	}
	return finalizeReport("jacoco", files), nil
}

func xmlAttr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func finalizeReport(format string, files map[string]*FileCoverage) *Report {
	var totalLines, coveredLines int
	for _, fc := range files {
		var fTotal, fCovered int
		for _, count := range fc.Lines {
			fTotal++
			if count > 0 {
				fCovered++
			}
		}
		fc.TotalLines = fTotal
		fc.CoveredLines = fCovered
		if fTotal > 0 {
			fc.CoverageRate = float64(fCovered) / float64(fTotal)
		}
		totalLines += fTotal
		coveredLines += fCovered
	}

	var rate float64
	if totalLines > 0 {
		rate = float64(coveredLines) / float64(totalLines)
	}

	return &Report{
		Format:       format,
		Files:        files,
		TotalLines:   totalLines,
		CoveredLines: coveredLines,
		CoverageRate: rate,
	}
}

// IsSymbolCovered checks if a symbol range has test coverage.
func (r *Report) IsSymbolCovered(filePath string, startLine, endLine int) (covered bool, coveredCount, totalCount int) {
	norm := normalizePath(filePath)
	fc, ok := r.Files[norm]
	if !ok {
		// Try suffix match
		for p, f := range r.Files {
			if strings.HasSuffix(p, norm) || strings.HasSuffix(norm, p) {
				fc = f
				break
			}
		}
	}
	if fc == nil || len(fc.Lines) == 0 {
		return false, 0, 0
	}

	for l := startLine; l <= endLine; l++ {
		if count, exists := fc.Lines[l]; exists {
			totalCount++
			if count > 0 {
				coveredCount++
			}
		}
	}
	return coveredCount > 0, coveredCount, totalCount
}

// Summary returns a human-readable summary.
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Coverage Report (%s):\n", r.Format)
	fmt.Fprintf(&b, "Total Coverage: %.1f%% (%d / %d lines covered across %d files)\n",
		r.CoverageRate*100, r.CoveredLines, r.TotalLines, len(r.Files))

	paths := make([]string, 0, len(r.Files))
	for p := range r.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	b.WriteString("\nPer-file breakdown:\n")
	for _, p := range paths {
		fc := r.Files[p]
		fmt.Fprintf(&b, "  - %-40s %5.1f%% (%d/%d lines)\n",
			p, fc.CoverageRate*100, fc.CoveredLines, fc.TotalLines)
	}
	return b.String()
}

func normalizePath(p string) string {
	p = filepath.Clean(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}
