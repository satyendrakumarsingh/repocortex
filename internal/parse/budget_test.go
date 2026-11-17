package parse

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The reproductions below are the files recorded in
// docs/verification/2026-11-12 and docs/performance.md as taking quadratic time
// or worse with gotreesitter v0.50.1. Each is built here, not checked in, so
// the sizes can be varied and nobody else's source is copied into the tree.

// hexTable is n comma-separated byte literals: the body of a lookup table.
func hexTable(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "0x%02x", i%256)
	}
	return b.String()
}

// cMacroThenTable is a file-scope macro call followed by an array of n
// literals: a kernel driver in miniature. With v0.50.1: n=32 0.15 s, 64 0.33 s,
// 128 1.39 s, 256 over 5 s, while the array alone at n=256 took 0.14 s.
func cMacroThenTable(n int) string {
	return "MODULE_LICENSE(\"GPL\");\n\nconst unsigned char table[" + fmt.Sprint(n) + "] = {\n" +
		hexTable(n) + "\n};\n\nint after_table(int x) { return x + 1; }\n"
}

// cCastThenLocalTable is redis's deps/lua/src/lobject.c in miniature: a
// function whose return expression calls a macro inside a parenthesised
// operand, and later a function-local array initialiser of n elements. With
// v0.50.1: 32/64/128/256 elements took 0.20/0.36/1.04/over 5 s.
func cCastThenLocalTable(n int) string {
	return "int luaO_int2fb (unsigned int x) {\n  int e = 0;\n" +
		"  return ((e+1) << 3) | (cast_int(x) - 8);\n}\n\n" +
		"int luaO_ceillog2 (unsigned int x) {\n  static const unsigned char log_2[" + fmt.Sprint(n) + "] = {\n    " +
		hexTable(n) + "\n  };\n  int l = 0;\n  return l + log_2[x];\n}\n\n" +
		"int after_table(int x) { return x + 1; }\n"
}

// csPreprocessorInSwitch is the construct in Newtonsoft.Json's
// Schema/JsonSchemaGenerator.cs that sends the C# parser into error recovery:
// conditional-compilation directives between the case labels of a switch, in
// each of methods methods, after conditional using directives.
//
// The real 21.7 KB file took over 30 s with v0.50.1 and takes 0.3 s with
// v0.57.0 (measured on the file itself; see the redis and Newtonsoft.Json
// tables in docs/performance.md). This synthetic file has the same construct
// and does NOT reproduce that blow-up on v0.50.1, where it merely costs about
// 80 ms per kilobyte; what it pins is the behaviour that matters afterwards:
// the parser recovers at every directive and still reaches the end.
func csPreprocessorInSwitch(methods int) string {
	var b strings.Builder
	b.WriteString("using System;\nusing System.Collections.Generic;\n\n" +
		"#if !HAVE_LINQ\nusing Sample.Utilities.LinqBridge;\n#else\nusing System.Linq;\n\n#endif\n\n" +
		"#nullable disable\n\nnamespace Sample.Schema\n{\n    public class Generator\n    {\n")
	for i := 0; i < methods; i++ {
		fmt.Fprintf(&b, "        private Kind Describe%d(Code code, Kind kind, string name)\n        {\n", i)
		b.WriteString("            if (name == null)\n            {\n" +
			"                throw new ArgumentNullException(nameof(name));\n            }\n" +
			"#if HAVE_ADO_NET\n            Kind result = kind | Kind.Extra;\n#else\n            Kind result = kind;\n#endif\n" +
			"            switch (code)\n            {\n" +
			"                case Code.Empty:\n                case Code.Object:\n" +
			"                    result = result | Kind.String;\n                    break;\n" +
			"#if HAVE_BINARY_SERIALIZATION\n                case Code.Serializable:\n" +
			"                    result = Combine(result, code, name.Length);\n                    break;\n#endif\n" +
			"#if HAVE_DYNAMIC\n                case Code.Dynamic:\n#endif\n" +
			"                case Code.Linq:\n                    result = result | Kind.Any;\n                    break;\n" +
			"                default:\n                    throw new Exception(\"unexpected\");\n" +
			"            }\n            return result;\n        }\n\n")
	}
	b.WriteString("        public int AfterEverything(int x)\n        {\n            return x + 1;\n        }\n    }\n}\n")
	return b.String()
}

// TestQuadraticReproductionsParseToTheEnd pins the three documented cases at
// four times the size that used to exceed the limit.
//
// The assertion is about the parse, not about a stopwatch: every file must be
// read to its end, and the definition AFTER the construct must be in the
// result. A regression to quadratic time would fail this without a timing
// threshold, because at these sizes the old behaviour runs into the wall-clock
// ceiling and comes back incomplete. Timings are logged, not asserted, so the
// test means the same thing on a loaded machine.
func TestQuadraticReproductionsParseToTheEnd(t *testing.T) {
	cases := []struct {
		name, path, src string
		want            []string
	}{
		{"c macro then table, n=256", "table.c", cMacroThenTable(256), []string{"after_table"}},
		{"c macro then table, n=1024", "table.c", cMacroThenTable(1024), []string{"after_table"}},
		{"c cast then local table, n=256", "lobject.c", cCastThenLocalTable(256),
			[]string{"luaO_int2fb", "luaO_ceillog2", "after_table"}},
		{"c cast then local table, n=1024", "lobject.c", cCastThenLocalTable(1024),
			[]string{"luaO_int2fb", "luaO_ceillog2", "after_table"}},
		{"c# preprocessor in switch, 12 methods", "Generator.cs", csPreprocessorInSwitch(12),
			[]string{"Describe0", "Describe11", "AfterEverything"}},
	}
	reg := NewRegistry()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := reg.ForFile(tc.path)
			if p == nil {
				t.Fatalf("no parser for %s", tc.path)
			}
			start := time.Now()
			res, err := p.Parse(context.Background(), tc.path, []byte(tc.src))
			if err != nil {
				t.Fatalf("run-level error: %v", err)
			}
			t.Logf("%d bytes in %s: status=%s reason=%q symbols=%d",
				len(tc.src), time.Since(start).Round(time.Millisecond), res.Status, res.Reason, len(res.Symbols))

			if res.Status == StatusFailed || res.Status == StatusSkipped {
				t.Fatalf("status = %s (%v), want the file parsed", res.Status, res.Err)
			}
			if res.Reason == model.ParseReasonBudgetExceeded || res.Reason == model.ParseReasonStoppedEarly {
				t.Fatalf("parse did not reach the end of the file: %v", res.Err)
			}
			for _, name := range tc.want {
				if !hasSymbol(res, name, "") {
					t.Errorf("definition %q is missing; got %v", name, symbolNames(res))
				}
			}
		})
	}
}

// fingerprint is everything about a result that reaches the index.
func fingerprint(res *Result) string {
	var b strings.Builder
	msg := ""
	if res.Err != nil {
		msg = res.Err.Error()
	}
	fmt.Fprintf(&b, "%s|%s|%s|%d|%d|%d|%d\n", res.Status, res.Reason, msg,
		res.ErrorNodes, res.MissingTokens, res.FirstErrorLine, res.ParsedBytes)
	for _, s := range res.Symbols {
		fmt.Fprintf(&b, "S %s %s %d:%d\n", s.Kind, s.Name, s.Range.Start.Line, s.Range.Start.Col)
	}
	for _, r := range res.References {
		fmt.Fprintf(&b, "R %s %s %d:%d\n", r.Kind, r.Name, r.Range.Start.Line, r.Range.Start.Col)
	}
	for _, i := range res.Imports {
		fmt.Fprintf(&b, "I %s\n", i.Path)
	}
	return b.String()
}

// TestParseVerdictIsRepeatable parses each reproduction, a clean file, a file
// with syntax errors and a file that exceeds its budget several times over, on
// fresh and on reused parsers, and requires one answer.
func TestParseVerdictIsRepeatable(t *testing.T) {
	inputs := map[string]string{
		"table.c":      cMacroThenTable(256),
		"lobject.c":    cCastThenLocalTable(256),
		"Generator.cs": csPreprocessorInSwitch(6),
		"clean.go":     "package p\n\nfunc A() int { return 1 }\n\nfunc B() int { return A() }\n",
		"errors.go":    "package p\n\nfunc A() {\n\tx := \n}\n\nfunc B() {}\n",
	}
	for path, src := range inputs {
		var first string
		for run := 0; run < 4; run++ {
			reg := NewRegistry() // a fresh pool on every run
			for again := 0; again < 2; again++ {
				res, err := reg.ForFile(path).Parse(context.Background(), path, []byte(src))
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				got := fingerprint(res)
				if first == "" {
					first = got
					continue
				}
				if got != first {
					t.Fatalf("%s: run %d/%d differs from the first\nfirst:\n%s\ngot:\n%s", path, run, again, first, got)
				}
			}
		}
	}
}

// manyGoFuncs is n small functions, named so a test can tell which were kept.
func manyGoFuncs(n int) string {
	var b strings.Builder
	b.WriteString("package p\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "func Fn%04d() int {\n\treturn %d + 1\n}\n\n", i, i)
	}
	return b.String()
}

// TestBudgetExceededKeepsWhatWasParsed is the contract for a file the parser
// stops in: partial, with a reason that names the limit and how far it got, the
// definitions before the stop kept, none invented after it, and the same
// outcome every time.
func TestBudgetExceededKeepsWhatWasParsed(t *testing.T) {
	src := manyGoFuncs(400)
	parseOnce := func() *Result {
		t.Helper()
		reg := NewRegistry(WithIterationLimit(1500))
		res, err := reg.ForFile("big.go").Parse(context.Background(), "big.go", []byte(src))
		if err != nil {
			t.Fatalf("run-level error: %v", err)
		}
		return res
	}

	res := parseOnce()
	if res.Status != StatusPartial {
		t.Fatalf("status = %s, want partial", res.Status)
	}
	if res.Reason != model.ParseReasonBudgetExceeded {
		t.Fatalf("reason = %q, want %q", res.Reason, model.ParseReasonBudgetExceeded)
	}
	if res.Err == nil || !strings.HasPrefix(res.Err.Error(), "budget_exceeded: ") {
		t.Fatalf("error text must start with the reason token, got %v", res.Err)
	}
	for _, want := range []string{"iteration limit", "definitions after that point are missing"} {
		if !strings.Contains(res.Err.Error(), want) {
			t.Errorf("error text %q does not mention %q", res.Err, want)
		}
	}
	if res.ParsedBytes <= 0 || res.ParsedBytes >= len(src) {
		t.Fatalf("ParsedBytes = %d of %d, want strictly inside the file", res.ParsedBytes, len(src))
	}
	if len(res.Symbols) == 0 {
		t.Fatal("no definitions were kept from the part of the file that was parsed")
	}
	if !hasSymbol(res, "Fn0000", "") {
		t.Errorf("the first function is missing; got %d symbols", len(res.Symbols))
	}
	if hasSymbol(res, "Fn0399", "") {
		t.Error("the last function is present, so the parse was not stopped")
	}
	stopLine := 1 + strings.Count(src[:res.ParsedBytes], "\n")
	for _, s := range res.Symbols {
		if s.Range.Start.Line > stopLine {
			t.Errorf("symbol %s at line %d is past the stop at line %d", s.Name, s.Range.Start.Line, stopLine)
		}
	}

	want := fingerprint(res)
	for i := 0; i < 5; i++ {
		if got := fingerprint(parseOnce()); got != want {
			t.Fatalf("run %d of a budget-limited parse differs:\nfirst:\n%.400s\ngot:\n%.400s", i, want, got)
		}
	}

	// And the same file inside its budget is a clean parse with every function.
	full, err := NewRegistry().ForFile("big.go").Parse(context.Background(), "big.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if full.Status != StatusOK || len(full.Symbols) != 400 {
		t.Fatalf("unlimited parse: status=%s symbols=%d, want ok and 400", full.Status, len(full.Symbols))
	}
}

// TestWallClockCeilingStopsTheParse sets the last-resort ceiling far below
// what the file needs. The parse must come back, promptly, as incomplete, and
// the reason must say the limit was the clock.
func TestWallClockCeilingStopsTheParse(t *testing.T) {
	src := manyGoFuncs(20000) // about 700 KB; hundreds of milliseconds to parse
	reg := NewRegistry(WithParseCeiling(time.Millisecond))

	start := time.Now()
	res, err := reg.ForFile("big.go").Parse(context.Background(), "big.go", []byte(src))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run-level error: %v", err)
	}
	if res.Status != StatusPartial || res.Reason != model.ParseReasonBudgetExceeded {
		t.Fatalf("status=%s reason=%q err=%v, want partial / budget_exceeded", res.Status, res.Reason, res.Err)
	}
	if !strings.Contains(res.Err.Error(), "wall-clock ceiling") {
		t.Errorf("error text %q does not say the limit was the clock", res.Err)
	}
	if hasSymbol(res, "Fn19999", "") {
		t.Error("the last function is present, so the parse was not stopped")
	}
	// Generous: the point is that it is nowhere near the unlimited parse plus
	// query time, and that it returned at all.
	if elapsed > 20*time.Second {
		t.Errorf("a parse with a 1 ms ceiling took %s to come back", elapsed)
	}
	t.Logf("stopped after %s at byte %d of %d", elapsed.Round(time.Millisecond), res.ParsedBytes, len(src))
}

// settledGoroutines waits for the goroutine count to come back to at most
// want, and reports the count it ended on.
//
// A short wait, not an instant read: a goroutine that has finished its work is
// still counted until the scheduler has retired it. And only a short one: the
// leak this guards against is a parse left running, which ends by itself once
// the file is parsed, so a long grace period would wait the evidence out.
func settledGoroutines(want int) int {
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		n := runtime.NumGoroutine()
		if n <= want || time.Now().After(deadline) {
			return n
		}
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}

// quietGoroutines reports the goroutine count once it has stopped moving.
func quietGoroutines() int {
	prev := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		time.Sleep(5 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
	}
	return prev
}

// TestNoGoroutineOutlivesParse is the regression test for the abandoned
// parse. The old guard started a goroutine, waited five seconds and returned,
// leaving the goroutine to finish on its own: a pool of four workers was
// measured using 6.8 cores. Every way a parse can end must leave nothing
// running.
func TestNoGoroutineOutlivesParse(t *testing.T) {
	// Large enough that a parse left running would still be running when the
	// count is taken: about 1.8 MB, most of a second of parsing.
	big := []byte(manyGoFuncs(45000))
	small := []byte(manyGoFuncs(50))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	scenarios := []struct {
		name string
		reg  Registry
		ctx  func() (context.Context, context.CancelFunc)
		src  []byte
	}{
		{"completes", NewRegistry(), nil, small},
		{"stopped by the wall-clock ceiling", NewRegistry(WithParseCeiling(time.Millisecond)), nil, big},
		{"stopped by the iteration limit", NewRegistry(WithIterationLimit(500)), nil, big},
		{"context already cancelled", NewRegistry(), func() (context.Context, context.CancelFunc) {
			return cancelled, func() {}
		}, small},
		{"cancelled while parsing", NewRegistry(), func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 2*time.Millisecond)
		}, big},
	}

	// Warm every code path once, so lazily started runtime goroutines and the
	// grammar's one-time initialisation are not charged to a scenario.
	if _, err := NewRegistry().ForFile("w.go").Parse(context.Background(), "w.go", small); err != nil {
		t.Fatal(err)
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			before := quietGoroutines()
			for i := 0; i < 5; i++ {
				ctx, done := context.Background(), func() {}
				if sc.ctx != nil {
					ctx, done = sc.ctx()
				}
				_, _ = sc.reg.ForFile("x.go").Parse(ctx, "x.go", sc.src)
				done()
			}
			if after := settledGoroutines(before); after > before {
				buf := make([]byte, 1<<16)
				buf = buf[:runtime.Stack(buf, true)]
				t.Fatalf("%d goroutines before, %d after five parses: something outlived the call\n%s",
					before, after, buf)
			}
		})
	}
}

// TestCancellationStopsARunningParse cancels a parse that is under way. It
// must return the context's error, not a result built from half a tree, and it
// must return long before the parse would have finished.
func TestCancellationStopsARunningParse(t *testing.T) {
	src := []byte(manyGoFuncs(40000)) // about 1.4 MB
	reg := NewRegistry()

	// How long the whole parse takes here, to compare against.
	start := time.Now()
	if _, err := reg.ForFile("x.go").Parse(context.Background(), "x.go", src); err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)

	for attempt := 0; attempt < 5; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), full/20)
		start = time.Now()
		res, err := reg.ForFile("x.go").Parse(ctx, "x.go", src)
		took := time.Since(start)
		cancel()
		if err == nil {
			// The deadline can land after the parser has finished and before
			// the queries have: a real result, and not what is under test.
			if res == nil {
				t.Fatal("nil result with nil error")
			}
			continue
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want the context's", err)
		}
		if res != nil {
			t.Fatalf("a cancelled parse returned a result: %+v", res.Status)
		}
		t.Logf("full parse %s; cancelled parse returned after %s", full.Round(time.Millisecond), took.Round(time.Millisecond))
		return
	}
	t.Skip("the parse finished before the deadline on every attempt; nothing to cancel on this machine")
}

// TestStatusDescribesTheParse is the vocabulary: each status and reason, on
// the smallest file that produces it.
func TestStatusDescribesTheParse(t *testing.T) {
	t.Run("a clean file that declares nothing is ok", func(t *testing.T) {
		// It used to be partial: "no symbol or reference was recorded".
		for path, src := range map[string]string{
			"only_comment.c":  "/* nothing is declared here */\n",
			"only_comment.go": "// Package p is documented and otherwise empty.\npackage p\n",
			"only_data.c":     "#define LIMIT 10\n",
		} {
			res := parse(t, path, src)
			if res.Status != StatusOK || res.Reason != model.ParseReasonNone || res.Err != nil {
				t.Errorf("%s: status=%s reason=%q err=%v, want ok with no reason", path, res.Status, res.Reason, res.Err)
			}
		}
	})

	t.Run("syntax errors are partial, counted and located", func(t *testing.T) {
		// It used to be ok: it has symbols.
		res := parse(t, "broken.go", "package p\n\nfunc Before() {}\n\nfunc Broken() {\n\tx := \n}\n\nfunc After() {}\n")
		if res.Status != StatusPartial || res.Reason != model.ParseReasonSyntaxErrors {
			t.Fatalf("status=%s reason=%q, want partial / syntax_errors", res.Status, res.Reason)
		}
		if res.ErrorNodes+res.MissingTokens == 0 {
			t.Error("no error or missing node was counted")
		}
		if res.FirstErrorLine < 5 || res.FirstErrorLine > 7 {
			t.Errorf("first error at line %d, want it in the broken function (lines 5 to 7)", res.FirstErrorLine)
		}
		if res.Err == nil || !strings.HasPrefix(res.Err.Error(), "syntax_errors: ") {
			t.Fatalf("error text must start with the reason token, got %v", res.Err)
		}
		if !strings.Contains(res.Err.Error(), fmt.Sprintf("first at line %d", res.FirstErrorLine)) {
			t.Errorf("error text %q does not give the first error's line", res.Err)
		}
		for _, name := range []string{"Before", "After"} {
			if !hasSymbol(res, name, "") {
				t.Errorf("definition %q outside the broken region was lost", name)
			}
		}
	})

	t.Run("an empty file is skipped", func(t *testing.T) {
		res := parse(t, "empty.go", "")
		if res.Status != StatusSkipped || res.Reason != model.ParseReasonEmpty {
			t.Errorf("status=%s reason=%q, want skipped / empty", res.Status, res.Reason)
		}
	})

	t.Run("a file over the limit is skipped with its size", func(t *testing.T) {
		reg := NewRegistry(WithMaxSourceBytes(1024))
		res, err := reg.ForFile("x.go").Parse(context.Background(), "x.go", []byte(manyGoFuncs(100)))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != StatusSkipped || res.Reason != model.ParseReasonTooLarge {
			t.Fatalf("status=%s reason=%q, want skipped / too_large", res.Status, res.Reason)
		}
		if !strings.Contains(res.Err.Error(), "over the 1024 byte limit") {
			t.Errorf("error text %q does not give the limit", res.Err)
		}
		if res.TextIndexable() {
			t.Error("a file over the size limit must not be text-indexed either")
		}
	})

	t.Run("a grammar with no definition query is text only", func(t *testing.T) {
		for path, src := range map[string]string{
			"script.fth": ": compute_total ( n -- n )\n    1 + ;\n",
			"main.tal":   "|0100 @on-reset ( -> )\n    #01 #02 ADD POP\n    BRK\n",
			"more.csv":   "x,y\n3,4\n",
			"table.csv":  "a,b\n1,2\n",
		} {
			res := parse(t, path, src)
			if res.Status != StatusSkipped || res.Reason != model.ParseReasonTextOnly {
				t.Errorf("%s: status=%s reason=%q, want skipped / text_only", path, res.Status, res.Reason)
				continue
			}
			if !res.TextIndexable() {
				t.Errorf("%s: a text-only file must be text-indexed", path)
			}
			if len(res.Symbols) != 0 || len(res.References) != 0 {
				t.Errorf("%s: symbols were invented for a language with no definition query", path)
			}
			if res.Err == nil || !strings.HasPrefix(res.Err.Error(), "text_only: ") {
				t.Errorf("%s: error text must start with the reason token, got %v", path, res.Err)
			}
		}
	})

	t.Run("the size limit applies to a text-only file too", func(t *testing.T) {
		reg := NewRegistry(WithMaxSourceBytes(64))
		res, err := reg.ForFile("big.pl").Parse(context.Background(), "big.pl", []byte(strings.Repeat("print 1;\n", 100)))
		if err != nil {
			t.Fatal(err)
		}
		if res.Reason != model.ParseReasonTooLarge || res.TextIndexable() {
			t.Errorf("reason=%q indexable=%v, want too_large and not indexed", res.Reason, res.TextIndexable())
		}
	})
}

// TestVueComponentCarriesItsScriptVerdict checks that a single-file component
// reports what happened to the script it wraps, in the component's own lines.
func TestVueComponentCarriesItsScriptVerdict(t *testing.T) {
	src := "<template>\n  <div/>\n</template>\n\n<script lang=\"ts\">\nexport function ok(): number { return 1 }\n\n" +
		"export function broken(): number {\n  const x = \n}\n\nexport function after(): number { return 2 }\n</script>\n"
	res := parse(t, "Widget.vue", src)
	if res.Status != StatusPartial || res.Reason != model.ParseReasonSyntaxErrors {
		t.Fatalf("status=%s reason=%q err=%v, want partial / syntax_errors", res.Status, res.Reason, res.Err)
	}
	if res.FirstErrorLine < 8 || res.FirstErrorLine > 10 {
		t.Errorf("first error at line %d, want it inside the broken function (lines 8 to 10 of the .vue file)", res.FirstErrorLine)
	}
	if !hasSymbol(res, "ok", "") || !hasSymbol(res, "after", "") {
		t.Errorf("definitions outside the broken region were lost: %v", symbolNames(res))
	}

	clean := parse(t, "Clean.vue", "<template><div/></template>\n<script lang=\"ts\">\nexport const n = 1\n</script>\n")
	if clean.Status != StatusOK {
		t.Errorf("a clean component: status=%s err=%v, want ok", clean.Status, clean.Err)
	}
}
