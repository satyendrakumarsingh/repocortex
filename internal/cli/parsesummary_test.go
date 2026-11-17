package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/parse"
)

// oneLine collapses runs of whitespace, so an assertion about a sentence does
// not depend on where a tabwriter or a terminal wrapped it.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// seedMixedRepo writes a repository holding one file of each kind the parse
// summary distinguishes, with a 4 KiB size limit so three files are over it.
func seedMixedRepo(t *testing.T) string {
	t.Helper()
	root := isolate(t)
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	filler := func(n int) string {
		var b strings.Builder
		b.WriteString("package gen\n\n")
		for i := 0; b.Len() < n; i++ {
			fmt.Fprintf(&b, "func Generated%05d() int { return %d }\n", i, i)
		}
		return b.String()
	}

	write(".repocortex.yml", "index:\n  max_file_bytes: 4096\n")
	write("pkg/clean.go", "package pkg\n\nfunc Clean() {}\n")
	write("pkg/doc.go", "// Package pkg declares nothing in this file.\npackage pkg\n")
	write("pkg/broken.go", "package pkg\n\nfunc Broken() {\n\tx := \n}\n\nfunc After() {}\n")
	write("scripts/report.fth", ": compute_quarterly_total 1 ;\n")
	write("docs/guide.rst", "Guide\n=====\n\nSome prose.\n")
	write("gen/medium.go", filler(6000))
	write("gen/largest.go", filler(20000))
	write("gen/large.go", filler(12000))
	write("LICENSE", "permission is hereby granted\n")
	return root
}

// TestIndexAndStatusNameWhatWasLeftOut is defects 8 and 10 as a user meets
// them: `index` and `status` say how many files were skipped for size and name
// the largest, say how many parsed with syntax errors, and do not call a file
// that declares nothing a problem.
func TestIndexAndStatusNameWhatWasLeftOut(t *testing.T) {
	root := seedMixedRepo(t)
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	indexOut, err := run(t, root, "index")
	if err != nil {
		t.Fatalf("index: %v\n%s", err, indexOut)
	}
	statusOut, err := run(t, root, "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOut)
	}

	for name, out := range map[string]string{"index": indexOut, "status": statusOut} {
		flat := oneLine(out)
		for _, want := range []string{
			"3 clean, 1 with syntax errors", // the fourth parsed file is .repocortex.yml
			"Too large: 3 files over the size limit",
			"index.max_file_bytes",
			"gen/largest.go",
			"gen/large.go",
			"gen/medium.go",
			"Text only: 2 files searchable by text, no definitions",
			"forth 1",
			"rst 1",
			"no_grammar",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("%s output does not contain %q:\n%s", name, want, out)
			}
		}
		// Largest first.
		if i, j, k := strings.Index(flat, "gen/largest.go"), strings.Index(flat, "gen/large.go"), strings.Index(flat, "gen/medium.go"); i >= j || j >= k {
			t.Errorf("%s names the oversized files out of size order (positions %d, %d, %d):\n%s", name, i, j, k, out)
		}
		for _, bad := range []string{"Parse failures", "Incomplete"} {
			if strings.Contains(flat, bad) {
				t.Errorf("%s reports %q for a repository with nothing abandoned:\n%s", name, bad, out)
			}
		}
	}
	if !strings.Contains(statusOut, "READY") {
		t.Errorf("status is not READY: a file with syntax errors, a file with no definitions and files "+
			"skipped for size are not abandoned work:\n%s", statusOut)
	}

	// The same facts, machine-readable.
	jsonOut, err := run(t, root, "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v\n%s", err, jsonOut)
	}
	var st struct {
		IndexStatus   string `json:"index_status"`
		ParseFailures int64  `json:"parse_failures"`
		PartialParses int64  `json:"partial_parses"`
		Parse         struct {
			Files           int64 `json:"files"`
			Clean           int64 `json:"clean"`
			SyntaxErrors    int64 `json:"syntax_errors"`
			Incomplete      int64 `json:"incomplete"`
			Failed          int64 `json:"failed"`
			TextOnly        int64 `json:"text_only"`
			TooLarge        int64 `json:"too_large"`
			LargestTooLarge []struct {
				Path      string `json:"path"`
				SizeBytes int64  `json:"size_bytes"`
			} `json:"largest_too_large"`
		} `json:"parse"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &st); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, jsonOut)
	}
	if st.IndexStatus != "ready" || st.ParseFailures != 0 || st.PartialParses != 1 {
		t.Errorf("index_status=%q parse_failures=%d partial_parses=%d, want ready, 0, 1", st.IndexStatus, st.ParseFailures, st.PartialParses)
	}
	if st.Parse.Clean != 3 || st.Parse.SyntaxErrors != 1 || st.Parse.TextOnly != 2 || st.Parse.TooLarge != 3 ||
		st.Parse.Incomplete != 0 || st.Parse.Failed != 0 {
		t.Errorf("parse summary = %+v", st.Parse)
	}
	if len(st.Parse.LargestTooLarge) != 3 || st.Parse.LargestTooLarge[0].Path != "gen/largest.go" ||
		st.Parse.LargestTooLarge[0].SizeBytes < 20000 {
		t.Errorf("largest_too_large = %+v, want gen/largest.go first with its size", st.Parse.LargestTooLarge)
	}

	// The text-only file is found by search; the oversized one is not.
	searchOut, err := run(t, root, "search", "compute_quarterly_total")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, searchOut)
	}
	if !strings.Contains(searchOut, "scripts/report.fth") {
		t.Errorf("search did not find a name that appears only in a Forth file:\n%s", searchOut)
	}

	// A second `index` changes nothing and must still name what is left out.
	again, err := run(t, root, "index")
	if err != nil {
		t.Fatalf("second index: %v\n%s", err, again)
	}
	if flat := oneLine(again); !strings.Contains(flat, "up to date") || !strings.Contains(flat, "gen/largest.go") {
		t.Errorf("an up-to-date index run does not name the files left out for size:\n%s", again)
	}

	// doctor reports the same gap as a finding.
	doctorOut, err := run(t, root, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, doctorOut)
	}
	for _, want := range []string{"parse status", "size limit", "gen/largest.go"} {
		if !strings.Contains(oneLine(doctorOut), want) {
			t.Errorf("doctor output does not contain %q:\n%s", want, doctorOut)
		}
	}
}

// TestRaisingTheSizeLimitIndexesTheFiles: the limit is the repository's to
// set, and raising it brings the named files into the index.
func TestRaisingTheSizeLimitIndexesTheFiles(t *testing.T) {
	root := seedMixedRepo(t)
	if out, err := run(t, root, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if out, err := run(t, root, "index"); err != nil {
		t.Fatalf("index: %v\n%s", err, out)
	}
	if out, _ := run(t, root, "symbol", "Generated00003"); strings.Contains(out, "gen/largest.go") {
		t.Fatalf("a symbol from an over-limit file is in the index:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(root, ".repocortex.yml"), []byte("index:\n  max_file_bytes: 65536\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "index", "--full")
	if err != nil {
		t.Fatalf("index --full: %v\n%s", err, out)
	}
	if strings.Contains(oneLine(out), "Too large") {
		t.Errorf("files are still reported too large under a 64 KiB limit:\n%s", out)
	}
	out, err = run(t, root, "symbol", "Generated00003")
	if err != nil {
		t.Fatalf("symbol: %v\n%s", err, out)
	}
	if !strings.Contains(out, "gen/largest.go") {
		t.Errorf("a symbol from a file now under the limit was not found:\n%s", out)
	}
}

// TestAnOutOfRangeSizeLimitIsRefused: a repository cannot ask for an unbounded
// limit, and the refusal names the setting.
func TestAnOutOfRangeSizeLimitIsRefused(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, ".repocortex.yml"), []byte("index:\n  max_file_bytes: 1099511627776\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "init")
	if err == nil {
		t.Fatalf("a 1 TiB size limit was accepted:\n%s", out)
	}
	if !strings.Contains(err.Error()+out, "index.max_file_bytes") {
		t.Errorf("the refusal does not name the setting: %v\n%s", err, out)
	}
}

// TestLanguagesSaysWhichAreTextOnly: the parsed tier's description promises
// definitions, and for the grammars with no definition query that is false.
// The command must list those apart from the parsed tier and name them.
func TestLanguagesSaysWhichAreTextOnly(t *testing.T) {
	root := isolate(t)
	out, err := run(t, root, "languages", "--json")
	if err != nil {
		t.Fatalf("languages --json: %v\n%s", err, out)
	}
	var rep struct {
		Parsed   []string `json:"parsed"`
		TextOnly []string `json:"text_only"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(rep.TextOnly) == 0 || len(rep.Parsed) == 0 {
		t.Fatalf("text_only has %d languages and parsed %d; want some in each", len(rep.TextOnly), len(rep.Parsed))
	}
	in := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	for _, lang := range []string{"forth", "uxntal", "dockerfile", "xml"} {
		if !in(rep.TextOnly, lang) {
			t.Errorf("%s is not listed as text only", lang)
		}
		// A language that yields no definitions is not in a tier that
		// promises them.
		if in(rep.Parsed, lang) {
			t.Errorf("%s is listed as text only and under parsed", lang)
		}
	}
	for _, lang := range []string{"go", "c", "python", "vue"} {
		if in(rep.TextOnly, lang) {
			t.Errorf("%s is listed as text only but yields definitions", lang)
		}
	}
	// The list and the parsers must agree, language by language.
	reg := parse.NewRegistry()
	for _, probe := range []struct{ lang, file string }{{"forth", "x.fth"}, {"uxntal", "x.tal"}, {"go", "x.go"}, {"c", "x.c"}} {
		res, err := reg.ForFile(probe.file).Parse(t.Context(), probe.file, []byte("x\n"))
		if err != nil {
			t.Fatal(err)
		}
		textOnly := string(res.Reason) == "text_only"
		if textOnly != in(rep.TextOnly, probe.lang) {
			t.Errorf("%s: the parser says text_only=%v, `languages` says %v", probe.lang, textOnly, in(rep.TextOnly, probe.lang))
		}
	}

	human, err := run(t, root, "languages")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(oneLine(human), "no definition query: full-text search only, no symbols") {
		t.Errorf("the human output does not say that text-only languages yield no symbols:\n%s", human)
	}
}
