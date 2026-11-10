// Package adr finds and reads Architecture Decision Records: the markdown files
// a team keeps to record why a structural choice was made.
//
// # Why this exists
//
// The spec's get_adr capability asks for one thing — the ability to tell an
// agent "this was decided deliberately, and here is the reasoning" before it
// proposes undoing it. An agent that cannot see ADR-0004 will cheerfully suggest
// swapping the message queue back out, and the reviewer pays for that in a
// re-litigated argument the team already had.
//
// # What this deliberately does not do
//
// It does not verify that an ADR is still TRUE. An accepted record saying "we
// use Postgres" is evidence of an intent expressed on a date, not evidence about
// the running system. Checking one against the code would mean deciding what
// "we use Postgres" means in terms of imports and config keys, and a wrong
// answer here is worse than no answer: reporting a live decision as "stale"
// invites exactly the re-litigation the feature exists to prevent. Freshness is
// a human judgement and stays one.
//
// It does not link ADRs to code. There is no path from "Use Postgres" to the
// packages that talk to it that does not rest on guessing, and a fabricated edge
// in a dependency graph is indistinguishable from a real one downstream.
//
// It does not resolve supersession into a graph. [Record.Supersedes] reports the
// numbers a record CLAIMS to replace, exactly as written. Whether those records
// exist, whether the claim is reciprocated, and which record is current are
// questions for a caller holding the whole set — this package reads one file.
//
// It does not render markdown, follow links between records, or parse dates.
// [Record.Date] is the string the author wrote, because ADRs in the wild carry
// "2023-05-01", "May 2023" and "2023-05-01 (revised 2024-02)", and a parser that
// rejects two of those loses information a reader would have understood.
package adr

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Record is one architecture decision record.
type Record struct {
	// Number is the ADR's sequence number when its filename carries one
	// (0001-use-postgres.md -> 1). Zero when unnumbered.
	Number int    `json:"number"`
	Title  string `json:"title"`
	// Status is normalised lowercase: proposed | accepted | rejected |
	// deprecated | superseded | "" when absent.
	Status string `json:"status"`
	Date   string `json:"date"` // as written; not parsed into time.Time
	Path   string `json:"path"` // repo-relative, forward slashes
	// Supersedes lists ADR numbers this record replaces, from a
	// "Supersedes ADR-0003" / "Supersedes: 3" line.
	Supersedes []int `json:"supersedes,omitempty"`
	// Summary is the first paragraph of the Context/Decision section, bounded.
	Summary string `json:"summary,omitempty"`
}

// Bounds on everything this package retains.
//
// These parse files from repositories RepoCortex did not write, so every one of
// them is a safety bound and not a matter of taste. maxADRBytes is the important
// one: a decision record is prose a human wrote and read, which puts a real one
// in the low kilobytes. A megabyte of "markdown" is generated output, a
// vendored dump, or a file that is only nominally text, and reading it costs
// more than the answer is worth.
//
// The rest bound what reaches a caller's context window. Summary is the only
// field carrying free prose, and 500 characters is roughly the first paragraph
// of a well-written Context section — enough to convey the reasoning, small
// enough that fifty records still fit in a response. Title, Status and Date are
// short by nature; the caps exist so a file that puts a paragraph after
// "Status:" cannot smuggle one through.
const (
	maxADRBytes     = 1 << 20
	maxSummaryChars = 500
	maxTitleChars   = 200
	maxStatusChars  = 64
	maxDateChars    = 40
	// maxSupersedes caps a claim list. A record replacing more than a handful
	// of others is consolidating a whole directory, and the tail carries no
	// information a reader acts on.
	maxSupersedes = 32
	// maxSections and maxSectionLines bound what the section scan retains. A
	// hostile file of a million "#" lines would otherwise materialise a million
	// sections, and only the first paragraph of any section is ever read.
	maxSections     = 256
	maxSectionLines = 40
	// maxFrontMatterLines bounds YAML front matter. MADR's own template uses
	// under a dozen keys; a document whose front matter runs longer is using it
	// as a data file, and the keys past this point are not decision metadata.
	maxFrontMatterLines = 100
)

// adrDirectories are the conventional locations, matched as a consecutive run
// of path segments ANYWHERE in the path.
//
// Anchoring at the repository root would miss every monorepo, where the records
// live at services/payments/docs/adr/. The segment run is what carries the
// evidence: a directory literally named "decisions" under one named
// "architecture" is not an accident, whereas a substring match would claim
// "docs/adrian-notes/".
var adrDirectories = [][]string{
	{"docs", "adr"},
	{"docs", "adrs"},
	{"docs", "decisions"},
	{"doc", "adr"},
	{"architecture", "decisions"},
	{"adr"},
	{"adrs"},
}

var (
	// fileNumber reads the sequence number off a filename. Leading zeros are
	// stripped by 0* so 0001 and 1 are the same record number.
	fileNumber = regexp.MustCompile(`(?i)^(?:adr[-_])?0*(\d{1,6})(?:[-_.]|$)`)

	// datePrefixed rejects 2024-01-15-post.md from the numbered-filename rule.
	// A date-stamped filename is the blog and changelog convention, and its
	// leading four digits are a year, not ADR 2024. A file named this way
	// INSIDE an ADR directory still qualifies — there the directory is the
	// evidence and the filename does not have to carry it.
	datePrefixed = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[-_.]`)

	// bareNumberedFile is the unprefixed convention. Three digits minimum —
	// see numberedFilename for why one is not enough outside an ADR directory.
	bareNumberedFile = regexp.MustCompile(`^\d{3,6}[-_]`)

	// titleOrdinal splits "1. Use Postgres" and "ADR-0001: Use Postgres".
	//
	// The separator is REQUIRED. With it optional, the title "2023 was a bad
	// year for the mailer" parses as record 2023 and loses its first word.
	titleOrdinal = regexp.MustCompile(`^(?:(?i:adr)[-_ ]?)?0*(\d{1,6})\s*[.):\-\x{2013}\x{2014}]\s*(.+)$`)

	// statusInline matches "Status: Accepted" and "**Status**: Accepted" —
	// the form used by templates that carry status as a field rather than a
	// section heading.
	statusInline = regexp.MustCompile(`(?i)^[\s>*\-+#]*(?:\*\*)?status(?:\*\*)?\s*:\s*(.+)$`)

	dateInline = regexp.MustCompile(`(?i)^[\s>*\-+#]*(?:\*\*)?date(?:\*\*)?\s*:\s*(.+)$`)

	// supersedesLine requires the word boundary so that "Superseded by ADR-5"
	// — a statement about THIS record, pointing forward — is not read as a
	// claim to replace ADR-5, which points backward. Conflating the two
	// reverses the arrow.
	supersedesLine = regexp.MustCompile(`(?i)^[\s>*\-+#]*(?:\*\*)?supersedes\b(?:\*\*)?\s*:?\s*(.*)$`)

	// adrReference prefers an explicitly labelled record number over any bare
	// integer on the line. "Supersedes ADR-0001 and ADR-0002 (2019-05-04)"
	// contains five integers and two record numbers.
	adrReference = regexp.MustCompile(`(?i)adr[-_ ]?0*(\d{1,6})`)
	bareNumber   = regexp.MustCompile(`\b0*(\d{1,4})\b`)

	isoDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

// knownStatuses are the five states the ADR conventions actually define.
//
// An unrecognised status is preserved lowercased rather than dropped: teams
// invent "superseded-partially" and "accepted (trial)", and a reader is better
// served by the odd string than by an empty field that reads as "no status was
// recorded".
var knownStatuses = []string{"proposed", "accepted", "rejected", "deprecated", "superseded"}

// nonDecisionNames are files that live in an ADR directory without being a
// decision.
//
// README.md is the one that matters — every adr-tools directory has one, it
// indexes the records, and treating it as record zero puts a table of contents
// where a decision should be. The rest follow the same logic: a template is the
// shape of a decision, not one.
var nonDecisionNames = map[string]bool{
	"readme":       true,
	"index":        true,
	"template":     true,
	"adr-template": true,
	"_template":    true,
	"contributing": true,
	"license":      true,
	"changelog":    true,
}

// IsADRPath reports whether a repo-relative path looks like an ADR.
func IsADRPath(path string) bool {
	segs, base, ok := splitPath(path)
	if !ok {
		return false
	}
	stem := base
	if i := strings.LastIndex(stem, "."); i > 0 {
		stem = stem[:i]
	}
	if nonDecisionNames[stem] {
		return false
	}
	if numberedFilename(base) {
		return true
	}
	return inADRDirectory(segs)
}

// splitPath normalises separators and rejects anything that is not markdown.
// The extension test comes first because it is the cheapest way to discard the
// overwhelming majority of a repository.
func splitPath(path string) (segs []string, base string, ok bool) {
	p := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if !strings.HasSuffix(p, ".md") && !strings.HasSuffix(p, ".markdown") {
		return nil, "", false
	}
	segs = strings.Split(p, "/")
	base = segs[len(segs)-1]
	if base == "" {
		return nil, "", false
	}
	return segs[:len(segs)-1], base, true
}

// numberedFilename recognises the naming convention on its own, wherever the
// file sits.
//
// The two forms are treated differently on purpose. An "adr-" prefix is an
// explicit declaration and one digit is enough. A bare numeric prefix is not:
// "1-intro.md" is as likely to be a tutorial chapter as a decision, so outside
// a recognised directory the convention has to be spelled out with the three
// or four digits that adr-tools and MADR both generate.
func numberedFilename(base string) bool {
	if datePrefixed.MatchString(base) {
		return false
	}
	if strings.HasPrefix(base, "adr-") || strings.HasPrefix(base, "adr_") {
		return fileNumber.MatchString(base)
	}
	return bareNumberedFile.MatchString(base)
}

func inADRDirectory(segs []string) bool {
	for _, want := range adrDirectories {
		if len(want) > len(segs) {
			continue
		}
		for i := 0; i+len(want) <= len(segs); i++ {
			match := true
			for j, w := range want {
				if segs[i+j] != w {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// Parse reads one ADR's markdown. ok is false when the content does not look
// like an ADR at all.
func Parse(path string, src []byte) (Record, bool) {
	if len(src) == 0 || len(src) > maxADRBytes {
		return Record{}, false
	}
	// A NUL byte means this is not the text file its extension claims. Running
	// the line scan over a binary blob would not crash, but every field it
	// produced would be noise dressed as a decision. The first 4 KB is the
	// usual sniffing window and keeps the check O(1) on a large file.
	head := src
	if len(head) > 4096 {
		head = head[:4096]
	}
	if indexNUL(head) >= 0 {
		return Record{}, false
	}

	rec := Record{Path: normalisePath(path)}

	front, body := splitFrontMatter(string(src))
	lines := strings.Split(body, "\n")
	secs := splitSections(lines)

	rec.Title = title(front, secs)
	if rec.Title == "" {
		// The title is the record's identity. Without one there is nothing to
		// report that a caller could name, and prose that has no heading is not
		// a decision record whatever directory it sits in.
		return Record{}, false
	}

	// Number comes from the filename first because that is what the convention
	// guarantees to be unique and sortable within a directory. The heading
	// ordinal is a fallback for records whose filename lost it — a real case
	// when files are renamed to slugs — and the two agree in every generated
	// record.
	rec.Number = numberFromPath(rec.Path)
	if n, stripped, ok := splitTitleOrdinal(rec.Title); ok {
		rec.Title = stripped
		if rec.Number == 0 {
			rec.Number = n
		}
	}

	rec.Status = status(front, secs, lines)
	rec.Date = date(front, secs, lines)
	rec.Supersedes = supersedes(front, lines)
	rec.Summary = summary(secs)

	// A file with a heading and nothing else is a note. Requiring either a
	// recognised location or one of the structural markers every ADR template
	// emits is what keeps an ordinary README from being reported as a decision
	// the team made.
	if !IsADRPath(rec.Path) && rec.Status == "" &&
		findSection(secs, "decision") == nil && findSection(secs, "context") == nil {
		return Record{}, false
	}
	return rec, true
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

func normalisePath(path string) string {
	p := strings.ReplaceAll(path, "\\", "/")
	return strings.TrimPrefix(p, "./")
}

func numberFromPath(path string) int {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	if datePrefixed.MatchString(base) {
		return 0
	}
	m := fileNumber.FindStringSubmatch(base)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

func splitTitleOrdinal(t string) (int, string, bool) {
	m := titleOrdinal.FindStringSubmatch(t)
	if m == nil {
		return 0, t, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, t, false
	}
	rest := clean(m[2], maxTitleChars)
	if rest == "" {
		return 0, t, false
	}
	return n, rest, true
}

// ---------------------------------------------------------------- front matter

// splitFrontMatter separates MADR's YAML header from the body.
//
// Only top-level `key: value` pairs are read. A real YAML parser would handle
// anchors and nested maps, but nothing below needs them, and front matter is
// the one part of an untrusted document that invites a parser with a large
// attack surface.
func splitFrontMatter(src string) (map[string]string, string) {
	// A UTF-8 BOM ahead of the "---" is what a Windows editor leaves behind,
	// and it is enough to make the whole header invisible to a naive prefix
	// test.
	rest := strings.TrimLeft(src, "\uFEFF \t\r\n")
	if !strings.HasPrefix(rest, "---") {
		return nil, src
	}
	lines := strings.Split(rest, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return nil, src
	}

	front := make(map[string]string)
	lastKey := ""
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" || trimmed == "..." {
			return front, strings.Join(lines[i+1:], "\n")
		}
		if i > maxFrontMatterLines {
			// Unterminated or oversized: treat the whole document as body
			// rather than swallow it as metadata.
			return nil, src
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A block sequence continues the previous key. `supersedes:` followed
		// by "- 0003" is as common as the inline `[0003]` form.
		if strings.HasPrefix(trimmed, "- ") && lastKey != "" {
			front[lastKey] = strings.TrimSpace(front[lastKey] + " " + strings.TrimPrefix(trimmed, "- "))
			continue
		}
		colon := strings.Index(trimmed, ":")
		if colon <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(trimmed[:colon]))
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		front[key] = strings.TrimSpace(trimmed[colon+1:])
		lastKey = key
	}
	// No closing delimiter was found. The document is not front-mattered; it
	// starts with a horizontal rule.
	return nil, src
}

func frontValue(front map[string]string, key string) string {
	v, ok := front[key]
	if !ok {
		return ""
	}
	return strings.Trim(strings.TrimSpace(v), `"'`)
}

// ---------------------------------------------------------------- sections

type section struct {
	heading string
	body    []string
}

// splitSections walks headings, tracking fenced code so that a "# comment"
// inside a shell example is not read as a section of the document.
func splitSections(lines []string) []section {
	secs := []section{{}}
	fence := ""
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			appendBody(&secs[len(secs)-1], line)
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			fence = "```"
			appendBody(&secs[len(secs)-1], line)
			continue
		}
		if strings.HasPrefix(trimmed, "~~~") {
			fence = "~~~"
			appendBody(&secs[len(secs)-1], line)
			continue
		}

		if h, ok := headingText(trimmed); ok {
			if len(secs) >= maxSections {
				// Past the cap the document is not a decision record; keep
				// appending to the last section so nothing panics and stop
				// growing.
				appendBody(&secs[len(secs)-1], line)
				continue
			}
			secs = append(secs, section{heading: h})
			continue
		}
		appendBody(&secs[len(secs)-1], line)
	}
	return secs
}

func appendBody(s *section, line string) {
	if len(s.body) < maxSectionLines {
		s.body = append(s.body, line)
	}
}

func headingText(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "#") {
		return "", false
	}
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level > 6 || level == len(trimmed) {
		return "", false
	}
	if trimmed[level] != ' ' && trimmed[level] != '\t' {
		return "", false
	}
	text := strings.TrimSpace(strings.Trim(trimmed[level:], "#"))
	return strings.TrimSpace(text), text != ""
}

// findSection matches a heading by lowercase prefix, which is what makes one
// lookup serve both templates: Nygard writes "## Context" and MADR writes
// "## Context and Problem Statement".
func findSection(secs []section, prefix string) *section {
	for i := range secs {
		if strings.HasPrefix(strings.ToLower(secs[i].heading), prefix) {
			return &secs[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- fields

func title(front map[string]string, secs []section) string {
	if v := frontValue(front, "title"); v != "" {
		return clean(v, maxTitleChars)
	}
	// The first heading of ANY level. Requiring h1 loses the records that open
	// with "## 1. Use Postgres", which is common in documents where the ADRs
	// were pasted under a page title.
	for _, s := range secs {
		if s.heading != "" {
			return clean(s.heading, maxTitleChars)
		}
	}
	return ""
}

func status(front map[string]string, secs []section, lines []string) string {
	if v := frontValue(front, "status"); v != "" {
		return normaliseStatus(v)
	}
	if s := findSection(secs, "status"); s != nil {
		if p := firstParagraph(s.body); p != "" {
			return normaliseStatus(p)
		}
	}
	// A "Status: Accepted" field line, used by templates that carry metadata in
	// the body. Bounded to the head of the document so a status quoted inside a
	// long Consequences section does not override the record's own.
	for i, line := range lines {
		if i > 60 {
			break
		}
		if m := statusInline.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			return normaliseStatus(m[1])
		}
	}
	return ""
}

// normaliseStatus folds the written form onto the five conventional states.
//
// Matching the EARLIEST known word rather than the first one found in list
// order matters for "Superseded by the decision that was accepted in ADR-9",
// where scanning the list first would return "accepted" for a record that is
// superseded.
func normaliseStatus(raw string) string {
	s := strings.ToLower(clean(raw, maxStatusChars*4))
	s = strings.Trim(s, "*_`~ \t")
	s = strings.TrimPrefix(s, "status")
	s = strings.TrimLeft(s, ":*_ \t")
	if s == "" {
		return ""
	}

	best, bestAt := "", -1
	for _, want := range knownStatuses {
		if at := wordIndex(s, want); at >= 0 && (bestAt < 0 || at < bestAt) {
			best, bestAt = want, at
		}
	}
	if best != "" {
		return best
	}
	return clean(s, maxStatusChars)
}

// wordIndex finds needle as a whole word, so "unaccepted" does not report
// "accepted".
func wordIndex(hay, needle string) int {
	from := 0
	for {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return -1
		}
		i += from
		beforeOK := i == 0 || !isWordByte(hay[i-1])
		end := i + len(needle)
		afterOK := end >= len(hay) || !isWordByte(hay[end])
		if beforeOK && afterOK {
			return i
		}
		from = i + 1
		if from >= len(hay) {
			return -1
		}
	}
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func date(front map[string]string, secs []section, lines []string) string {
	if v := frontValue(front, "date"); v != "" {
		return clean(v, maxDateChars)
	}
	for i, line := range lines {
		if i > 60 {
			break
		}
		if m := dateInline.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			return clean(strings.Trim(m[1], `"'`), maxDateChars)
		}
	}
	// adr-tools writes the date into the Status section ("Accepted 2016-02-17").
	// Only an ISO date is taken from there: anything looser would pull version
	// numbers and ticket references out of prose.
	if s := findSection(secs, "status"); s != nil {
		if d := isoDate.FindString(strings.Join(s.body, " ")); d != "" {
			return d
		}
	}
	return ""
}

func supersedes(front map[string]string, lines []string) []int {
	var out []int
	seen := make(map[int]bool)
	add := func(text string) {
		for _, n := range recordNumbers(text) {
			if len(out) >= maxSupersedes || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	if v := frontValue(front, "supersedes"); v != "" {
		add(v)
	}
	for _, line := range lines {
		if len(out) >= maxSupersedes {
			break
		}
		if m := supersedesLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			add(m[1])
		}
	}
	return out
}

// recordNumbers pulls ADR numbers out of a claim.
//
// Labelled references win outright: if the line says ADR-0003 anywhere, every
// other integer on it is a year, a date part or a link fragment. Only when
// nothing is labelled does the bare form apply, and there it is capped at four
// digits so "Supersedes: 3" works without "(2019-05-04)" contributing 2019.
func recordNumbers(text string) []int {
	if i := strings.Index(text, "("); i >= 0 {
		text = text[:i]
	}
	var out []int
	for _, m := range adrReference.FindAllStringSubmatch(text, maxSupersedes) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, m := range bareNumber.FindAllStringSubmatch(text, maxSupersedes) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// summaryExcluded are sections whose first paragraph is metadata rather than
// reasoning. Without this the fallback reports "accepted" as the summary of
// every record that has no Context section.
var summaryExcluded = []string{"status", "date", "deciders", "tags", "metadata",
	"table of contents", "contents", "index"}

func summary(secs []section) string {
	for _, prefix := range []string{"context", "decision", "problem", "background"} {
		if s := findSection(secs, prefix); s != nil {
			if p := firstParagraph(s.body); p != "" {
				return truncateWords(p, maxSummaryChars)
			}
		}
	}
	// No recognised section: the paragraph under the title is what a human
	// reads first, and for the many hand-written records that skip the template
	// it is the only prose there is.
	for _, s := range secs {
		if excludedSection(s.heading) {
			continue
		}
		if p := firstParagraph(s.body); p != "" {
			return truncateWords(p, maxSummaryChars)
		}
	}
	return ""
}

func excludedSection(heading string) bool {
	h := strings.ToLower(heading)
	for _, ex := range summaryExcluded {
		if strings.HasPrefix(h, ex) {
			return true
		}
	}
	return false
}

// firstParagraph reads until the first blank line, skipping the HTML comments
// MADR's template leaves in place ("<!-- optional element -->"), which are
// invisible to a reader and would otherwise become the summary verbatim.
func firstParagraph(body []string) string {
	var parts []string
	inComment := false
	for _, raw := range body {
		line := strings.TrimSpace(raw)
		if inComment {
			if strings.Contains(line, "-->") {
				inComment = false
			}
			continue
		}
		if strings.HasPrefix(line, "<!--") {
			if !strings.Contains(line, "-->") {
				inComment = true
			}
			continue
		}
		if line == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "```") {
			break
		}
		parts = append(parts, line)
		// A paragraph running longer than this is not going to be summarised by
		// its opening 500 characters anyway, and the join below is what bounds
		// the work.
		if len(parts) >= maxSectionLines {
			break
		}
	}
	return clean(strings.Join(parts, " "), maxSummaryChars*4)
}

// ---------------------------------------------------------------- text

// clean makes a field safe to hand to a JSON encoder and to a terminal.
//
// Invalid UTF-8 is dropped rather than replaced: these bytes came from a file
// RepoCortex did not write, and a field of replacement characters is worse than a
// short one. Control characters become spaces so a record cannot move a
// reader's cursor or inject an escape sequence into a report.
func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = truncateWords(s, max)
	}
	return s
}

// truncateWords cuts on a word boundary and marks the cut.
//
// The ellipsis is included in the budget, so the result never exceeds max
// bytes; a caller sizing a context window gets the number it was promised.
func truncateWords(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := s[:max-len(ellipsis)]
	if i := strings.LastIndexAny(cut, " \t"); i > max/2 {
		cut = cut[:i]
	}
	// The byte cut may have landed inside a rune. At most three bytes are
	// dropped putting it back on a boundary.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	cut = strings.TrimRight(cut, " \t,;:-")
	return cut + ellipsis
}
