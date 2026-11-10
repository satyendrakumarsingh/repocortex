package adr

import (
	"strings"
	"testing"
	"time"
)

// nygard is the template adr-tools generates, verbatim in shape.
const nygard = `# 2. Use PostgreSQL for the primary datastore

Date: 2016-02-17

## Status

Accepted

## Context

We need a datastore that supports transactions across the billing and
ledger tables, because a partial write leaves an invoice without its
line items and support cannot tell the difference from a refund.

The team has operated Postgres for four years.

## Decision

We will use PostgreSQL 14 as the primary datastore for all services in
the billing domain.

## Consequences

Read replicas become the scaling story rather than sharding.
`

// madr is the MADR 3.x template: YAML front matter, then the title.
const madr = `---
status: "proposed"
date: 2023-05-01
deciders: platform-team
---

# Replace the in-process cache with Redis

## Context and Problem Statement

<!-- This is an optional element. Feel free to remove. -->
Each pod keeps its own cache, so a config change takes up to fifteen
minutes to converge across the fleet and support tickets arrive in the
gap.

## Decision Drivers

* convergence time
`

func TestIsADRPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
		why  string
	}{
		{"docs/adr/0001-use-postgres.md", true, "canonical location"},
		{"docs/adrs/0001-use-postgres.md", true, "plural directory"},
		{"docs/decisions/0001-use-postgres.md", true, "MADR default"},
		{"docs/decisions/x.md", true, "located, unnumbered: the directory is the evidence"},
		{"adr/0002-y.markdown", true, "root adr directory, .markdown"},
		{"architecture/decisions/z.md", true, "the long-form convention"},
		{"0003-title.md", true, "numbered name at the repository root"},
		{"doc/adr/0001-use-postgres.md", true, "singular doc"},
		{"adr/0001-use-postgres.md", true, "repository root"},
		{"architecture/decisions/use-postgres.md", true, "unnumbered but located"},
		{"docs/architecture/decisions/record.markdown", true, ".markdown extension"},
		{"services/payments/docs/adr/0007-queue.md", true, "monorepo, not root-anchored"},
		{"DOCS/ADR/0001-Use-Postgres.MD", true, "case-insensitive"},
		{"./docs/adr/0002-x.md", true, "leading ./"},
		{"docs\\adr\\0002-x.md", true, "windows separators"},

		{"docs/0004-use-postgres.md", true, "numbered name anywhere"},
		{"notes/adr-7-use-kafka.md", true, "adr- prefix, one digit"},

		{"docs/adr/README.md", false, "the index, not a decision"},
		{"docs/adr/readme.md", false, "index, lowercased"},
		{"docs/adr/template.md", false, "the shape of a decision, not one"},
		{"docs/adr/index.md", false, "index"},
		{"README.md", false, "root readme"},
		{"docs/guide.md", false, "arbitrary markdown"},
		{"docs/adr/0001-use-postgres.txt", false, "not markdown"},
		{"docs/adr", false, "a directory, not a file"},
		{"blog/2024-01-15-postgres-at-scale.md", false, "date-stamped post, not ADR 2024"},
		{"docs/1-intro.md", false, "one digit outside an ADR directory is a chapter"},
		{"docs/adrian/notes.md", false, "substring of adr is not a segment"},
		{"", false, "empty"},
	}
	for _, tt := range tests {
		if got := IsADRPath(tt.path); got != tt.want {
			t.Errorf("IsADRPath(%q) = %v, want %v (%s)", tt.path, got, tt.want, tt.why)
		}
	}
}

func TestParseNygard(t *testing.T) {
	rec, ok := Parse("docs/adr/0002-use-postgresql.md", []byte(nygard))
	if !ok {
		t.Fatal("the adr-tools template was not recognised")
	}
	if rec.Number != 2 {
		t.Errorf("Number = %d, want 2", rec.Number)
	}
	// The ordinal is carried by Number; repeating it in the title would make
	// two records with the same decision sort differently by string.
	if rec.Title != "Use PostgreSQL for the primary datastore" {
		t.Errorf("Title = %q", rec.Title)
	}
	if rec.Status != "accepted" {
		t.Errorf("Status = %q, want accepted", rec.Status)
	}
	if rec.Date != "2016-02-17" {
		t.Errorf("Date = %q, want 2016-02-17", rec.Date)
	}
	if rec.Path != "docs/adr/0002-use-postgresql.md" {
		t.Errorf("Path = %q", rec.Path)
	}
	// The Context section, not the Status section and not the Consequences.
	if !strings.HasPrefix(rec.Summary, "We need a datastore that supports transactions") {
		t.Errorf("Summary = %q", rec.Summary)
	}
	// The first PARAGRAPH only: the sentence about the team's experience is a
	// second paragraph and would double the cost of every record in a report.
	if strings.Contains(rec.Summary, "four years") {
		t.Errorf("summary ran past the first paragraph: %q", rec.Summary)
	}
}

func TestParseMADR(t *testing.T) {
	rec, ok := Parse("docs/decisions/0012-replace-cache.md", []byte(madr))
	if !ok {
		t.Fatal("the MADR template was not recognised")
	}
	if rec.Number != 12 {
		t.Errorf("Number = %d, want 12", rec.Number)
	}
	if rec.Title != "Replace the in-process cache with Redis" {
		t.Errorf("Title = %q", rec.Title)
	}
	if rec.Status != "proposed" {
		t.Errorf("Status = %q, want proposed", rec.Status)
	}
	if rec.Date != "2023-05-01" {
		t.Errorf("Date = %q, want 2023-05-01", rec.Date)
	}
	// "Context and Problem Statement" must satisfy the same lookup as "Context".
	if !strings.HasPrefix(rec.Summary, "Each pod keeps its own cache") {
		t.Errorf("Summary = %q", rec.Summary)
	}
	// The template's own HTML comment is invisible to a reader and must not
	// become the summary.
	if strings.Contains(rec.Summary, "optional element") {
		t.Errorf("template comment leaked into the summary: %q", rec.Summary)
	}
}

func TestStatusNormalisation(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"section body", "# T\n\n## Status\n\nAccepted\n\n## Context\n\nx\n", "accepted"},
		{"shouting", "# T\n\n## Status\n\nACCEPTED\n\n## Context\n\nx\n", "accepted"},
		{"trailing space", "# T\n\n## Status\n\naccepted   \n\n## Context\n\nx\n", "accepted"},
		{"surrounding space", "# T\n\n## Status\n\n   accepted   \n\n## Context\n\nx\n", "accepted"},
		{"field line", "# T\n\nStatus: Accepted\n\n## Context\n\nx\n", "accepted"},
		{"bold field", "# T\n\n**Status**: Rejected\n\n## Context\n\nx\n", "rejected"},
		{"front matter", "---\nstatus: Deprecated\n---\n# T\n\n## Context\n\nx\n", "deprecated"},
		{"quoted front matter", "---\nstatus: \"Proposed\"\n---\n# T\n\n## Context\n\nx\n", "proposed"},
		{"with a date", "# T\n\n## Status\n\nAccepted 2016-02-17\n\n## Context\n\nx\n", "accepted"},
		{"markdown emphasis", "# T\n\n## Status\n\n*Superseded*\n\n## Context\n\nx\n", "superseded"},
		// Earliest word wins: a superseded record that mentions the decision
		// that replaced it is still superseded.
		{"superseded by an accepted one", "# T\n\n## Status\n\nSuperseded by ADR-9, which was accepted\n\n## Context\n\nx\n", "superseded"},
		// An invented status is preserved rather than dropped: an empty field
		// reads as "nobody recorded a status", which is a different fact.
		{"unknown preserved", "# T\n\n## Status\n\nUnder Review\n\n## Context\n\nx\n", "under review"},
		{"absent", "# T\n\n## Context\n\nx\n", ""},
		// "unaccepted" is not "accepted"; the whole-word test is what stops it.
		{"substring is not a status", "# T\n\n## Status\n\nUnaccepted\n\n## Context\n\nx\n", "unaccepted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, ok := Parse("docs/adr/0001-t.md", []byte(tt.src))
			if !ok {
				t.Fatalf("not parsed: %q", tt.src)
			}
			if rec.Status != tt.want {
				t.Errorf("Status = %q, want %q", rec.Status, tt.want)
			}
		})
	}
}

func TestSupersedes(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []int
	}{
		{"labelled", "# T\n\n## Status\n\nAccepted\n\nSupersedes ADR-0003\n\n## Context\n\nx\n", []int{3}},
		{"bare with colon", "# T\n\nSupersedes: 3\n\n## Context\n\nx\n", []int{3}},
		{"two records", "# T\n\nSupersedes ADR-0001 and ADR-0002\n\n## Context\n\nx\n", []int{1, 2}},
		{"bold", "# T\n\n**Supersedes:** ADR-4\n\n## Context\n\nx\n", []int{4}},
		{"list item", "# T\n\n- Supersedes ADR-0005\n\n## Context\n\nx\n", []int{5}},
		{"front matter list", "---\nstatus: accepted\nsupersedes:\n  - 6\n  - 7\n---\n# T\n\n## Context\n\nx\n", []int{6, 7}},
		// A parenthesised date on the line contributes 2019, 05 and 04 to a
		// naive integer scan.
		{"date in parentheses", "# T\n\nSupersedes ADR-0008 (2019-05-04)\n\n## Context\n\nx\n", []int{8}},
		// The arrow points the other way. Reading this as a claim to replace
		// ADR-9 inverts the supersession chain for both records.
		{"superseded by is not supersedes", "# T\n\n## Status\n\nSuperseded by ADR-9\n\n## Context\n\nx\n", nil},
		{"absent", "# T\n\n## Context\n\nx\n", nil},
		{"duplicates collapse", "# T\n\nSupersedes ADR-3\n\nSupersedes ADR-0003\n\n## Context\n\nx\n", []int{3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, ok := Parse("docs/adr/0010-t.md", []byte(tt.src))
			if !ok {
				t.Fatalf("not parsed: %q", tt.src)
			}
			if len(rec.Supersedes) != len(tt.want) {
				t.Fatalf("Supersedes = %v, want %v", rec.Supersedes, tt.want)
			}
			for i, n := range tt.want {
				if rec.Supersedes[i] != n {
					t.Fatalf("Supersedes = %v, want %v", rec.Supersedes, tt.want)
				}
			}
		})
	}
}

func TestNumberAndTitle(t *testing.T) {
	tests := []struct {
		path      string
		src       string
		wantNum   int
		wantTitle string
	}{
		{"docs/adr/0001-use-postgres.md", "# 1. Use Postgres\n\n## Context\n\nx\n", 1, "Use Postgres"},
		{"docs/adr/adr-0007-use-kafka.md", "# Use Kafka\n\n## Context\n\nx\n", 7, "Use Kafka"},
		// Unnumbered filename: the heading ordinal is the only source left.
		{"docs/adr/use-redis.md", "# 14. Use Redis\n\n## Context\n\nx\n", 14, "Use Redis"},
		{"docs/adr/use-redis.md", "# ADR-15: Use Redis\n\n## Context\n\nx\n", 15, "Use Redis"},
		{"docs/adr/use-redis.md", "# Use Redis\n\n## Context\n\nx\n", 0, "Use Redis"},
		// The separator is required, so a title that opens with a year keeps
		// its first word.
		{"docs/adr/postmortem.md", "# 2023 was a bad year for the mailer\n\n## Context\n\nx\n", 0, "2023 was a bad year for the mailer"},
		// Front matter wins over the heading: newer MADR carries the title there.
		{"docs/adr/0003-x.md", "---\ntitle: Adopt gRPC\nstatus: accepted\n---\n# Something Else\n", 3, "Adopt gRPC"},
		// No h1 at all — records pasted under a page title open at h2.
		{"docs/adr/0004-x.md", "## 4. Use SQS\n\n## Context\n\nx\n", 4, "Use SQS"},
	}
	for _, tt := range tests {
		rec, ok := Parse(tt.path, []byte(tt.src))
		if !ok {
			t.Errorf("%s: not parsed", tt.path)
			continue
		}
		if rec.Number != tt.wantNum {
			t.Errorf("%s: Number = %d, want %d", tt.path, rec.Number, tt.wantNum)
		}
		if rec.Title != tt.wantTitle {
			t.Errorf("%s: Title = %q, want %q", tt.path, rec.Title, tt.wantTitle)
		}
	}
}

func TestReadmeInADRDirectoryIsNotADecision(t *testing.T) {
	const readme = `# Architecture Decision Records

This directory holds the decision records for the billing platform. See
[0001-use-postgres.md](0001-use-postgres.md) to start.

| Number | Title |
| ------ | ----- |
| 0001   | Use Postgres |
`
	if IsADRPath("docs/adr/README.md") {
		t.Error("the directory index was classified as a decision record")
	}
	// And the content check must agree, because a caller that walks a
	// directory rather than filtering by path would otherwise report the index
	// as a decision the team made.
	if rec, ok := Parse("docs/adr/README.md", []byte(readme)); ok {
		t.Errorf("Parse accepted the directory index: %+v", rec)
	}
}

func TestParseRejectsNonRecords(t *testing.T) {
	tests := []struct {
		name string
		path string
		src  string
	}{
		{"empty", "docs/adr/0001-x.md", ""},
		{"prose elsewhere", "docs/guide.md", "# Getting Started\n\nInstall the CLI.\n"},
		{"no heading", "docs/notes.md", "just some text with no structure at all\n"},
		{"binary", "docs/adr/0001-x.md", "# T\x00\x01\x02 binary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if rec, ok := Parse(tt.path, []byte(tt.src)); ok {
				t.Errorf("accepted a non-record: %+v", rec)
			}
		})
	}

	// Over the size bound. A decision record is prose a human wrote and read.
	huge := append([]byte("# T\n\n## Context\n\n"), make([]byte, maxADRBytes)...)
	for i := range huge[17:] {
		huge[17+i] = 'a'
	}
	if _, ok := Parse("docs/adr/0001-x.md", huge); ok {
		t.Error("a file over the size bound was parsed")
	}
}

func TestSummaryIsBounded(t *testing.T) {
	long := strings.Repeat("the reasoning behind this decision is long and detailed ", 200)
	rec, ok := Parse("docs/adr/0001-x.md", []byte("# T\n\n## Context\n\n"+long+"\n"))
	if !ok {
		t.Fatal("not parsed")
	}
	if len(rec.Summary) > maxSummaryChars {
		t.Errorf("summary = %d bytes, bound is %d", len(rec.Summary), maxSummaryChars)
	}
	if !strings.HasSuffix(rec.Summary, "…") {
		t.Errorf("a truncated summary must say so: %q", rec.Summary)
	}
	// Cut on a word boundary: the last word must be whole.
	trimmed := strings.TrimSuffix(rec.Summary, "…")
	if !strings.HasSuffix(trimmed, "detailed") && !strings.HasSuffix(trimmed, "long") &&
		!strings.HasSuffix(trimmed, "and") && !strings.HasSuffix(trimmed, "is") &&
		!strings.HasSuffix(trimmed, "decision") && !strings.HasSuffix(trimmed, "this") &&
		!strings.HasSuffix(trimmed, "behind") && !strings.HasSuffix(trimmed, "reasoning") &&
		!strings.HasSuffix(trimmed, "the") {
		t.Errorf("summary cut mid-word: %q", rec.Summary)
	}

	// Every other retained field is bounded too, or a record could smuggle a
	// paragraph into a caller's context window through the status field.
	wide := "# " + strings.Repeat("t", 5000) + "\n\nStatus: " + strings.Repeat("s", 5000) +
		"\n\nDate: " + strings.Repeat("d", 5000) + "\n\n## Context\n\nx\n"
	rec, ok = Parse("docs/adr/0002-x.md", []byte(wide))
	if !ok {
		t.Fatal("not parsed")
	}
	if len(rec.Title) > maxTitleChars {
		t.Errorf("title = %d bytes", len(rec.Title))
	}
	if len(rec.Status) > maxStatusChars {
		t.Errorf("status = %d bytes", len(rec.Status))
	}
	if len(rec.Date) > maxDateChars {
		t.Errorf("date = %d bytes", len(rec.Date))
	}
}

func TestSummarySkipsMetadataSections(t *testing.T) {
	// No Context and no Decision. The fallback must not report "accepted" as
	// the reasoning behind the decision.
	rec, ok := Parse("docs/adr/0001-x.md", []byte(
		"# Use Postgres\n\n## Status\n\nAccepted\n\n## Consequences\n\nReplicas scale reads.\n"))
	if !ok {
		t.Fatal("not parsed")
	}
	if rec.Summary == "accepted" || rec.Summary == "Accepted" {
		t.Errorf("the status became the summary: %q", rec.Summary)
	}
}

func TestFencedCodeIsNotStructure(t *testing.T) {
	// A shell comment inside a fence is not a heading, and a "Status:" line
	// inside an example is not this record's status.
	src := "# Use Postgres\n\n## Context\n\nRun the migration:\n\n```sh\n# Status\nStatus: Rejected\n```\n\n## Status\n\nAccepted\n"
	rec, ok := Parse("docs/adr/0001-x.md", []byte(src))
	if !ok {
		t.Fatal("not parsed")
	}
	if rec.Status != "accepted" {
		t.Errorf("Status = %q, want accepted — a code example won the lookup", rec.Status)
	}
	if rec.Title != "Use Postgres" {
		t.Errorf("Title = %q", rec.Title)
	}
}

// TestSafeOnAdversarialInput drives both entry points with input no ADR author
// would write. These files come from repositories RepoCortex did not write, and
// this package runs inside an MCP server: a panic takes down the response, and
// a hang takes down the session.
func TestSafeOnAdversarialInput(t *testing.T) {
	srcs := map[string]string{
		"empty":             "",
		"no headings":       strings.Repeat("plain prose with no structure whatsoever. ", 20000),
		"all headings":      strings.Repeat("# h\n", 200000),
		"deep nesting":      strings.Repeat("#", 5000) + " title\n" + strings.Repeat("> ", 50000) + "x",
		"binary":            "\x00\x01\x02\xff\xfe\x00",
		"invalid utf8":      "# \xff\xfe\xfd title\n\n## Context\n\n\xc3\x28 body\n",
		"unclosed fence":    "# T\n\n```\n" + strings.Repeat("code\n", 50000),
		"unclosed front":    "---\n" + strings.Repeat("k: v\n", 50000),
		"front matter only": "---\nstatus: accepted\ndate: 2020-01-01\n---\n",
		"lone delimiters":   strings.Repeat("---\n", 100000),
		"only hashes":       strings.Repeat("#", 200000),
		"over the limit":    "# T\n\n## Context\n\n" + strings.Repeat("a", 2*maxADRBytes),
		"supersedes storm":  strings.Repeat("Supersedes ADR-1 ADR-2 ADR-3\n", 20000),
		"status storm":      strings.Repeat("Status: accepted\n", 50000),
		"one long line":     "# " + strings.Repeat("t", 500000),
		"nul late":          "# T\n\n## Context\n\n" + strings.Repeat("a", 8000) + "\x00\x00",
		"crlf":              "---\r\nstatus: accepted\r\n---\r\n# T\r\n\r\n## Context\r\n\r\nx\r\n",
		"bom":               "\uFEFF---\nstatus: accepted\n---\n# T\n\n## Context\n\nx\n",
		"multibyte cut":     "# T\n\n## Context\n\n" + strings.Repeat("日本語のテキスト", 500),
	}
	paths := []string{
		"docs/adr/0001-x.md", "README.md", "", "docs/adr/", strings.Repeat("a/", 5000) + "b.md",
		"docs/adr/" + strings.Repeat("n", 5000) + ".md", "\x00\x01.md",
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic: %v", r)
			}
		}()
		for _, p := range paths {
			_ = IsADRPath(p)
			for name, src := range srcs {
				rec, ok := Parse(p, []byte(src))
				if !ok {
					continue
				}
				// Whatever it decided to accept must still respect every bound,
				// because these are the values that reach a context window.
				if len(rec.Summary) > maxSummaryChars || len(rec.Title) > maxTitleChars ||
					len(rec.Status) > maxStatusChars || len(rec.Date) > maxDateChars ||
					len(rec.Supersedes) > maxSupersedes {
					t.Errorf("%s @ %q: bound exceeded: %+v", name, p, rec)
				}
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("adversarial input did not finish in 30s: a scan is not linear")
	}
}

func TestParseIsIdempotentOnRealisticInput(t *testing.T) {
	// Parsing twice must give the same record. A stateful parser that mutated
	// a package-level buffer would pass every test above and fail this one.
	for _, src := range []string{nygard, madr} {
		a, ok1 := Parse("docs/adr/0002-x.md", []byte(src))
		b, ok2 := Parse("docs/adr/0002-x.md", []byte(src))
		if ok1 != ok2 || a.Title != b.Title || a.Status != b.Status ||
			a.Summary != b.Summary || a.Date != b.Date || a.Number != b.Number {
			t.Errorf("Parse is not deterministic:\n%+v\n%+v", a, b)
		}
	}
}
