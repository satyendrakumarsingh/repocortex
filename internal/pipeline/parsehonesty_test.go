package pipeline

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// hexBytes is n byte literals: the body of a lookup table.
func hexBytes(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("0x%02x", i%256)
	}
	return strings.Join(parts, ", ")
}

// determinismCorpus is a small repository holding one file of every kind the
// parse status distinguishes, including the constructs that used to make the
// outcome depend on how busy the machine was.
func determinismCorpus() map[string]string {
	var goFuncs strings.Builder
	goFuncs.WriteString("package big\n\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&goFuncs, "func Fn%03d() int { return %d }\n\n", i, i)
	}
	return map[string]string{
		// A file-scope macro call, then a table: quadratic in gotreesitter v0.50.1.
		"drivers/table.c": "MODULE_LICENSE(\"GPL\");\n\nconst unsigned char table[512] = {\n" + hexBytes(512) +
			"\n};\n\nint after_table(int x) { return x + 1; }\n",
		// redis deps/lua/src/lobject.c in miniature: the other quadratic case.
		"deps/lobject.c": "int luaO_int2fb (unsigned int x) {\n  int e = 0;\n" +
			"  return ((e+1) << 3) | (cast_int(x) - 8);\n}\n\n" +
			"int luaO_ceillog2 (unsigned int x) {\n  static const unsigned char log_2[512] = {\n    " + hexBytes(512) +
			"\n  };\n  return log_2[x];\n}\n",
		// The default error recovery abandons this one; the alternate reads it.
		"src/bitops.c": "static void normalise_range(long long *start, long long *end, uint64_t *bit_start,\n" +
			"                            unsigned char *mask, long long len, int isbit);\n\n" +
			"static const uint8_t bits_in_byte[256] = {\n    #define B2(n) n, n+1, n+1, n+2\n" +
			"    #define B4(n) B2(n), B2(n+1), B2(n+1), B2(n+2)\n    B4(0), B4(1), B4(1), B4(2)\n" +
			"    #undef B4\n    #undef B2\n};\n\nint after_bits(int x) { return x + 1; }\n",
		// Conditional compilation inside a switch: the Newtonsoft.Json construct.
		"Schema/Generator.cs": "namespace Sample\n{\n    public class Generator\n    {\n" +
			"        private int Kind(int code)\n        {\n            switch (code)\n            {\n" +
			"                case 0:\n                    return 1;\n#if HAVE_EXTRA\n                case 1:\n" +
			"                    return 2;\n#endif\n                default:\n                    return 0;\n" +
			"            }\n        }\n\n        public int After(int x) { return x + 1; }\n    }\n}\n",
		"pkg/clean.go":       "package pkg\n\ntype Service struct{}\n\nfunc (s *Service) Run() error { return nil }\n",
		"pkg/big.go":         goFuncs.String(),
		"pkg/doc.go":         "// Package pkg is documented and declares nothing here.\npackage pkg\n",
		"pkg/broken.go":      "package pkg\n\nfunc Before() {}\n\nfunc Broken() {\n\tx := \n}\n\nfunc After() {}\n",
		"scripts/report.fth": ": compute_total ( n -- n )\n    1 + ;\n",
		"lib/main.tal":       "|0100 @on-reset ( -> )\n    #01 #02 ADD POP\n    BRK\n",
		"Makefile":           "build:\n\tgo build ./...\n",
		"notes.unknownext":   "free text that no grammar claims\n",
		"web/app.ts":         "export class Widget {\n  render(): string { return 'x' }\n}\n",
		"web/Panel.vue":      "<template><div/></template>\n<script lang=\"ts\">\nexport function panel(): number { return 1 }\n</script>\n",
	}
}

// indexFingerprint is every per-file fact a parse contributes to the index, in
// a stable order. Two runs agree exactly when these strings are equal.
func indexFingerprint(facts []FileFacts) string {
	lines := make([]string, 0, len(facts)*4)
	for _, f := range facts {
		lines = append(lines, fmt.Sprintf("F %s | %s | %s | %s", f.File.Path, f.File.Language, f.File.ParseStatus, f.File.ParseError))
		for _, s := range f.Symbols {
			lines = append(lines, fmt.Sprintf("S %s | %s %s %d:%d", f.File.Path, s.Kind, s.Name, s.Range.Start.Line, s.Range.Start.Col))
		}
		for _, r := range f.References {
			lines = append(lines, fmt.Sprintf("R %s | %s %s %d:%d", f.File.Path, r.Kind, r.Name, r.Range.Start.Line, r.Range.Start.Col))
		}
		for _, i := range f.Imports {
			lines = append(lines, fmt.Sprintf("I %s | %s", f.File.Path, i.Path))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// burnCPU occupies n goroutines with arithmetic until stop is closed, and
// returns once they have all exited.
func burnCPU(n int, stop <-chan struct{}) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			x := seed | 1
			for {
				select {
				case <-stop:
					return
				default:
				}
				for k := 0; k < 1<<14; k++ {
					x ^= x << 13
					x ^= x >> 7
					x ^= x << 17
				}
			}
		}(uint64(i) + 1)
	}
	return &wg
}

// TestIndexDoesNotDependOnMachineLoad is the determinism gate for the parse
// budget. The same corpus is indexed three ways:
//
//   - on one processor with one worker, the quietest a run can be;
//   - with eight workers while every processor is kept busy by other work,
//     several times over;
//   - with the default settings.
//
// Every run must record the same status, the same reason text, the same
// symbols, references and imports for every file. The limit this replaces was
// five seconds of wall-clock time, under which the second configuration lost
// files the first one indexed.
func TestIndexDoesNotDependOnMachineLoad(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, determinismCorpus())

	index := func(workers int) string {
		t.Helper()
		var c collector
		if _, err := runnerFor(t, root, Options{Workers: workers}).Run(context.Background(), &c); err != nil {
			t.Fatal(err)
		}
		return indexFingerprint(c.facts)
	}

	// Quiet: one processor, one worker.
	prev := runtime.GOMAXPROCS(1)
	quiet := index(1)
	runtime.GOMAXPROCS(prev)

	// Loaded: four busy goroutines per processor competing with eight workers.
	stop := make(chan struct{})
	burners := burnCPU(4*runtime.NumCPU(), stop)
	var loaded []string
	for i := 0; i < 3; i++ {
		loaded = append(loaded, index(8))
	}
	close(stop)
	burners.Wait()

	defaults := index(0)

	for i, got := range append(loaded, defaults) {
		if got != quiet {
			t.Fatalf("run %d differs from the quiet single-processor run:\n%s", i, firstDifference(quiet, got))
		}
	}

	// And the corpus exercised what it was built to: every status is present,
	// and nothing was abandoned.
	for _, want := range []string{
		"F pkg/clean.go | go | ok | ",
		"F pkg/doc.go | go | ok | ",
		"F pkg/broken.go | go | partial | syntax_errors: ",
		// A #define between the braces of an initialiser: the grammar reads it
		// as errors, the C mask overwrites it and the table parses, so the
		// file is read clean. It was partial before the mask learned the shape.
		"F src/bitops.c | c | ok | ",
		"F scripts/report.fth | forth | skipped | text_only: ",
		"F lib/main.tal | uxntal | skipped | text_only: ",
		"F notes.unknownext |  | skipped | no grammar",
		"S drivers/table.c | function after_table",
		"S deps/lobject.c | function luaO_ceillog2",
		"S src/bitops.c | function after_bits",
		"S pkg/big.go | function Fn299",
	} {
		if !strings.Contains(quiet, want) {
			t.Errorf("the index is missing a line starting %q", want)
		}
	}
	for _, bad := range []string{"budget_exceeded", "stopped_early", "| failed |"} {
		if strings.Contains(quiet, bad) {
			t.Errorf("a file was abandoned (%s):\n%s", bad, linesContaining(quiet, bad))
		}
	}
}

func firstDifference(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d\n  quiet: %s\n  other: %s", i+1, x, y)
		}
	}
	return "(no difference found)"
}

func linesContaining(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// TestTextOnlyFilesAreSearchableAndHaveNoSymbols is defect 6 end to end: a
// function name that appears only in a Forth or a Uxntal file must be found by
// search, the file must say why it has no symbols, and none may be invented.
func TestTextOnlyFilesAreSearchableAndHaveNoSymbols(t *testing.T) {
	h := newHarness(t, map[string]string{
		"scripts/report.fth": ": compute_quarterly_total ( n -- n )\n    1 + ;\n",
		"lib/main.tal":       "|0100 @summariseLedgerEntries ( -> )\n    #01 #02 ADD POP\n    BRK\n",
		"pkg/a.go":           "package pkg\n\nfunc InGo() {}\n",
		"notes.unknownext":   "wordThatNoGrammarClaims\n",
	})
	ctx := context.Background()

	for query, wantPath := range map[string]string{
		"compute_quarterly_total": "scripts/report.fth",
		"summariseLedgerEntries":  "lib/main.tal",
		"quarterly":               "scripts/report.fth", // identifier parts are searchable too
	} {
		hits, err := h.reg.SearchFiles(ctx, query, 10)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		found := false
		for _, hit := range hits {
			if hit.Path == wantPath {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q did not find %s; hits: %+v", query, wantPath, hits)
		}
	}

	// A file no grammar claims is still not text-indexed: nothing has said
	// what kind of file it is, and that set includes .env files and keys.
	if hits, err := h.reg.SearchFiles(ctx, "wordThatNoGrammarClaims", 10); err != nil {
		t.Fatal(err)
	} else if len(hits) != 0 {
		t.Errorf("a file with no grammar was text-indexed: %+v", hits)
	}

	files, err := h.reg.Files(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]model.File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	for _, p := range []string{"scripts/report.fth", "lib/main.tal"} {
		f := byPath[p]
		if f.ParseStatus != model.ParseStatusSkipped || f.ParseReason() != model.ParseReasonTextOnly {
			t.Errorf("%s: status=%q reason=%q, want skipped / text_only", p, f.ParseStatus, f.ParseReason())
		}
		if !strings.Contains(f.ParseError, "no symbols are extracted") {
			t.Errorf("%s: the stored reason %q does not say why nothing is here", p, f.ParseError)
		}
	}
	for path := range h.symbolNames() {
		if strings.HasSuffix(path, ".fth") || strings.HasSuffix(path, ".tal") {
			t.Errorf("a symbol was recorded for text-only file key %q", path)
		}
	}

	// Not a failure, and not a reason for the run to be degraded.
	run, err := h.reg.Run(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.StatusReady || run.ParseFailures != 0 {
		t.Errorf("run status=%s parse_failures=%d, want ready and 0: text-only files are not failures",
			run.Status, run.ParseFailures)
	}

	// An edit to a text-only file keeps it searchable on the incremental path.
	h.write("scripts/report.fth", ": renamed_after_edit 2 ;\n")
	h.sync()
	hits, err := h.reg.SearchFiles(ctx, "renamed_after_edit", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("an edited text-only file is no longer searchable after an incremental update")
	}
	if hits, _ := h.reg.SearchFiles(ctx, "compute_quarterly_total", 10); len(hits) != 0 {
		t.Errorf("the old text of an edited file is still in the search index: %+v", hits)
	}
}

// TestRunIsNotDegradedByFilesThatDeclareNothing is defect 10's consequence: a
// cleanly parsed file with no definitions is not an error, and neither is a
// file the parser read to the end with syntax errors. Only abandoned work
// degrades a run.
func TestRunIsNotDegradedByFilesThatDeclareNothing(t *testing.T) {
	h := newHarness(t, map[string]string{
		"pkg/doc.go":      "// Package pkg is documented and declares nothing here.\npackage pkg\n",
		"include/limit.h": "#define LIMIT 10\n",
		"empty.c":         "/* nothing */\n",
		"pkg/broken.go":   "package pkg\n\nfunc Broken() {\n\tx := \n}\n",
	})
	ctx := context.Background()
	run, err := h.reg.Run(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.StatusReady {
		t.Errorf("run status = %s, want ready", run.Status)
	}
	if run.ParseFailures != 0 {
		t.Errorf("parse_failures = %d, want 0", run.ParseFailures)
	}

	sum, err := h.reg.ParseSummary(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Clean != 3 || sum.SyntaxErrors != 1 || sum.Abandoned() != 0 {
		t.Errorf("summary = %+v, want 3 clean, 1 with syntax errors, nothing abandoned", sum)
	}
}

// TestFilesOverTheSizeLimitAreNamed is defect 8: a file left out for its size
// is recorded with its size and the limit, counted, and named, largest first;
// and the limit is the repository's to set.
func TestFilesOverTheSizeLimitAreNamed(t *testing.T) {
	big := func(funcs int) string {
		var b strings.Builder
		b.WriteString("package gen\n\n")
		for i := 0; i < funcs; i++ {
			fmt.Fprintf(&b, "func Generated%05d() int { return %d }\n", i, i)
		}
		return b.String()
	}
	files := map[string]string{
		"pkg/small.go":   "package pkg\n\nfunc Small() {}\n",
		"gen/medium.go":  big(200),  // about 8 KB
		"gen/large.go":   big(600),  // about 25 KB
		"gen/largest.go": big(1200), // about 50 KB
		"docs/long.rst":  strings.Repeat("a line of prose in a text-only language\n", 600),
		"data/table.csv": strings.Repeat("1,", 4000) + "1\n",
	}

	indexWith := func(limit int64) (map[string]model.File, map[string]FileFacts) {
		t.Helper()
		root := t.TempDir()
		writeTree(t, root, files)
		cfg := config.Defaults()
		cfg.MaxFileBytes = limit
		if err := cfg.Validate(); err != nil {
			t.Fatalf("config with limit %d: %v", limit, err)
		}
		w, err := fs.NewWalker(root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		var c collector
		if _, err := NewRunner(w, Options{}).Run(context.Background(), &c); err != nil {
			t.Fatal(err)
		}
		rows, facts := map[string]model.File{}, map[string]FileFacts{}
		for _, f := range c.facts {
			rows[f.File.Path], facts[f.File.Path] = f.File, f
		}
		return rows, facts
	}

	// A 16 KiB limit: three files are over it.
	rows, facts := indexWith(16 << 10)
	for _, p := range []string{"gen/large.go", "gen/largest.go", "docs/long.rst"} {
		f := rows[p]
		if f.ParseStatus != model.ParseStatusSkipped || f.ParseReason() != model.ParseReasonTooLarge {
			t.Errorf("%s: status=%q reason=%q, want skipped / too_large", p, f.ParseStatus, f.ParseReason())
			continue
		}
		want := fmt.Sprintf("too_large: %d bytes, over the 16384 byte limit", len(files[p]))
		if !strings.HasPrefix(f.ParseError, want) {
			t.Errorf("%s: stored reason %q, want it to start %q", p, f.ParseError, want)
		}
		if f.SizeBytes != int64(len(files[p])) {
			t.Errorf("%s: size recorded as %d, want %d", p, f.SizeBytes, len(files[p]))
		}
		if len(facts[p].Symbols) != 0 || len(facts[p].Content) != 0 {
			t.Errorf("%s: an over-limit file contributed symbols or text", p)
		}
	}
	for _, p := range []string{"pkg/small.go", "gen/medium.go"} {
		if rows[p].ParseStatus != model.ParseStatusOK {
			t.Errorf("%s: status=%q (%s), want ok", p, rows[p].ParseStatus, rows[p].ParseError)
		}
	}
	// The limit applies to a text-only file exactly as to a parsed one.
	if rows["data/table.csv"].ParseReason() != model.ParseReasonTextOnly {
		t.Errorf("data/table.csv (under the limit): reason=%q, want text_only", rows["data/table.csv"].ParseReason())
	}

	// Raise the limit and the same files are indexed.
	rows, facts = indexWith(64 << 10)
	for _, p := range []string{"gen/large.go", "gen/largest.go"} {
		if rows[p].ParseStatus != model.ParseStatusOK || len(facts[p].Symbols) == 0 {
			t.Errorf("%s under a 64 KiB limit: status=%q symbols=%d, want ok with symbols",
				p, rows[p].ParseStatus, len(facts[p].Symbols))
		}
	}
	if rows["docs/long.rst"].ParseReason() != model.ParseReasonTextOnly {
		t.Errorf("docs/long.rst under a 64 KiB limit: reason=%q, want text_only", rows["docs/long.rst"].ParseReason())
	}
}

// TestParseSummaryNamesTheLargestSkippedFiles checks the report `index` and
// `status` print: the count is complete, the list is largest first.
func TestParseSummaryNamesTheLargestSkippedFiles(t *testing.T) {
	pad := func(n int) string { return "package gen\n\n// " + strings.Repeat("x", n) + "\n" }
	files := map[string]string{"pkg/small.go": "package pkg\n\nfunc Small() {}\n"}
	sizes := map[string]int{}
	for i := 0; i < 14; i++ {
		name := fmt.Sprintf("gen/file%02d.go", i)
		files[name] = pad(3000 + i*500)
		sizes[name] = len(files[name])
	}

	ctx := context.Background()
	h := newHarnessWithConfig(t, files, func(c *config.Config) { c.MaxFileBytes = 2 << 10 })
	sum, err := h.reg.ParseSummary(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.TooLarge != 14 {
		t.Fatalf("too_large = %d, want 14; summary %+v", sum.TooLarge, sum)
	}
	if sum.Clean != 1 || sum.Files != 15 {
		t.Errorf("files=%d clean=%d, want 15 and 1", sum.Files, sum.Clean)
	}
	if len(sum.LargestTooLarge) != 10 {
		t.Fatalf("named %d files, want the 10 largest", len(sum.LargestTooLarge))
	}
	for i, n := range sum.LargestTooLarge {
		want := fmt.Sprintf("gen/file%02d.go", 13-i)
		if n.Path != want || n.SizeBytes != int64(sizes[want]) {
			t.Errorf("entry %d = %s (%d bytes), want %s (%d bytes)", i, n.Path, n.SizeBytes, want, sizes[want])
		}
	}

	// Skipping for size is a decision, not a failure: the run is ready.
	run, err := h.reg.Run(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.StatusReady {
		t.Errorf("run status = %s, want ready", run.Status)
	}

	// The incremental path applies the same limit without reading the file.
	h.write("gen/new_big.go", pad(9000))
	h.write("pkg/new_small.go", "package pkg\n\nfunc NewSmall() {}\n")
	h.sync()
	rows, err := h.reg.Files(ctx, h.runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rows {
		switch f.Path {
		case "gen/new_big.go":
			if f.ParseReason() != model.ParseReasonTooLarge || !strings.Contains(f.ParseError, "over the 2048 byte limit") {
				t.Errorf("incremental: %s recorded as %q / %q, want too_large with the limit", f.Path, f.ParseStatus, f.ParseError)
			}
		case "pkg/new_small.go":
			if f.ParseStatus != model.ParseStatusOK {
				t.Errorf("incremental: %s status=%q, want ok", f.Path, f.ParseStatus)
			}
		}
	}
}

// TestThereIsOneSizeLimit holds the three spellings of the default together:
// the configuration's, discovery's and the parser's. They were two different
// numbers once, and a file between them was read, hashed and then not parsed.
func TestThereIsOneSizeLimit(t *testing.T) {
	if fs.DefaultMaxFileSize != config.DefaultMaxFileBytes {
		t.Errorf("discovery reads up to %d bytes, the configured default is %d", fs.DefaultMaxFileSize, config.DefaultMaxFileBytes)
	}
	if int64(parse.DefaultMaxSourceBytes) != config.DefaultMaxFileBytes {
		t.Errorf("the parser accepts up to %d bytes, the configured default is %d", parse.DefaultMaxSourceBytes, config.DefaultMaxFileBytes)
	}
	if got := config.Defaults().MaxFileBytes; got != config.DefaultMaxFileBytes {
		t.Errorf("Defaults().MaxFileBytes = %d, want %d", got, config.DefaultMaxFileBytes)
	}
}

// TestTextOnlyFilesAreRedactedOnBothPaths: making configuration formats
// searchable must not make the credentials in them searchable. The full path
// always redacted; the incremental path did not redact anything, which this
// test would have caught for source files too.
func TestTextOnlyFilesAreRedactedOnBothPaths(t *testing.T) {
	const first, second = "hunter2firstsentinel9f8e7d6c", "hunter2secondsentinel1a2b3c4d"
	h := newHarness(t, map[string]string{
		"config/app.yml": "service:\n  name: billing\n  db_password: " + first + "\n",
		"pkg/a.go":       "package pkg\n\nconst dbPassword = \"" + first + "\"\n",
	})
	ctx := context.Background()

	mustNotFind := func(when, secret string) {
		t.Helper()
		hits, err := h.reg.SearchFiles(ctx, secret, 10)
		if err != nil {
			t.Fatalf("%s: search: %v", when, err)
		}
		if len(hits) != 0 {
			t.Errorf("%s: a credential is in the search index: %+v", when, hits)
		}
		for _, path := range []string{"config/app.yml", "pkg/a.go"} {
			text, err := h.reg.FileExcerpt(ctx, h.runID, path, 1, 50)
			if err != nil {
				t.Fatalf("%s: excerpt of %s: %v", when, path, err)
			}
			if strings.Contains(text, secret) {
				t.Errorf("%s: %s is stored with its credential:\n%s", when, path, text)
			}
		}
	}
	mustFind := func(when, word, path string) {
		t.Helper()
		hits, err := h.reg.SearchFiles(ctx, word, 10)
		if err != nil {
			t.Fatalf("%s: search: %v", when, err)
		}
		for _, hit := range hits {
			if hit.Path == path {
				return
			}
		}
		t.Errorf("%s: %q does not find %s; the file must stay searchable around the redaction: %+v", when, word, path, hits)
	}

	mustNotFind("after a full index", first)
	mustFind("after a full index", "billing", "config/app.yml")

	h.write("config/app.yml", "service:\n  name: invoicing\n  db_password: "+second+"\n")
	h.write("pkg/a.go", "package pkg\n\nconst dbPassword = \""+second+"\"\n")
	h.sync()

	mustNotFind("after an incremental update", second)
	mustNotFind("after an incremental update", first)
	mustFind("after an incremental update", "invoicing", "config/app.yml")
}
