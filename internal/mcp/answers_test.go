package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ---------------------------------------------------------------- fixtures

// lowCoverageJava is a Java index in which most bindable references are
// ambiguous: the shape of gson, where "COVERAGE TOO LOW" and "verdict: strong"
// were printed on one answer.
func lowCoverageJava(b *indexBuilder) {
	b.file("svc/Service.java", "java", 80,
		typ("Service", model.KindClass, 1, 80),
		method("update", 10, 20, 1),
		method("helper", 30, 40, 0))
	b.file("api/Handler.java", "java", 60,
		typ("Handler", model.KindClass, 1, 60),
		method("handle", 5, 15, 0),
		method("retry", 20, 30, 0))
	update := symKey("svc/Service.java", "update", 10)
	b.call(symKey("api/Handler.java", "handle", 5), update, "update", "svc", "api/Handler.java", 7, model.ConfidenceHigh)
	b.call(symKey("api/Handler.java", "retry", 20), update, "update", "svc", "api/Handler.java", 22, model.ConfidenceHigh)
	b.call(update, symKey("svc/Service.java", "helper", 30), "helper", "", "svc/Service.java", 12, model.ConfidenceHigh)
	for i := 0; i < 12; i++ {
		b.ref("api/Handler.java", "process", "x", 40+i, registry.ResolutionAmbiguous, "")
	}
	// A reference spelled like the target that was bound to nothing: the
	// symbol's own accounting is NOT complete, so the language's rate stands.
	b.ref("api/Handler.java", "update", "svc", 55, registry.ResolutionAmbiguous, "")
}

// lowCoverageJavaTargetBound is the same index with every reference spelled
// like `update` bound to it: the language resolves badly and this symbol does
// not. Its own accounting is complete, which is the narrowest evidence there is.
func lowCoverageJavaTargetBound(b *indexBuilder) {
	b.file("svc/Service.java", "java", 80,
		typ("Service", model.KindClass, 1, 80),
		method("update", 10, 20, 1),
		method("helper", 30, 40, 0))
	b.file("api/Handler.java", "java", 60,
		typ("Handler", model.KindClass, 1, 60),
		method("handle", 5, 15, 0),
		method("retry", 20, 30, 0))
	update := symKey("svc/Service.java", "update", 10)
	b.call(symKey("api/Handler.java", "handle", 5), update, "update", "svc", "api/Handler.java", 7, model.ConfidenceHigh)
	b.call(symKey("api/Handler.java", "retry", 20), update, "update", "svc", "api/Handler.java", 22, model.ConfidenceHigh)
	b.call(update, symKey("svc/Service.java", "helper", 30), "helper", "", "svc/Service.java", 12, model.ConfidenceHigh)
	for i := 0; i < 12; i++ {
		b.ref("api/Handler.java", "process", "x", 40+i, registry.ResolutionAmbiguous, "")
	}
}

// kotlinWithoutReferences is okio's shape: thousands of Kotlin definitions,
// not one Kotlin reference, and a few well-resolved Java files beside them.
func kotlinWithoutReferences(b *indexBuilder) {
	b.file("okio/Sink.kt", "kotlin", 60,
		typ("Sink", model.KindInterface, 1, 20),
		fn("emit", 30, 40))
	b.file("okio/Buffer.kt", "kotlin", 40, fn("flushAll", 5, 15))
	// Enough Kotlin that "no reference from any of it" is a statement about the
	// language and not about one small file.
	var more []symDef
	for i := 0; i < 25; i++ {
		more = append(more, fn(fmt.Sprintf("op%02d", i), 10+10*i, 15+10*i))
	}
	b.file("okio/Ops.kt", "kotlin", 300, more...)
	b.file("samples/Sample.java", "java", 60,
		typ("Sample", model.KindClass, 1, 60),
		method("run", 5, 15, 0),
		method("step", 20, 30, 0))
	b.call(symKey("samples/Sample.java", "run", 5), symKey("samples/Sample.java", "step", 20),
		"step", "", "samples/Sample.java", 8, model.ConfidenceHigh)
	// The Java files resolve poorly. A Kotlin symbol must not be described by
	// them, and must not be described by them resolving well either.
	for i := 0; i < 6; i++ {
		b.ref("samples/Sample.java", "other", "", 40+i, registry.ResolutionAmbiguous, "")
	}
	// Two references spelled like the Java symbol that were bound to nothing,
	// so its own accounting is incomplete and the language's rate is its grade.
	b.ref("samples/Sample.java", "step", "", 50, registry.ResolutionAmbiguous, "")
	b.ref("samples/Sample.java", "step", "", 51, registry.ResolutionAmbiguous, "")
}

// wellCoveredGo is a Go index with nothing held against it.
func wellCoveredGo(b *indexBuilder) {
	b.file("app/app.go", "go", 80, fn("serve", 5, 15), fn("start", 20, 30), fn("stop", 40, 50))
	b.call(symKey("app/app.go", "start", 20), symKey("app/app.go", "serve", 5), "serve", "", "app/app.go", 22, model.ConfidenceHigh)
	b.call(symKey("app/app.go", "stop", 40), symKey("app/app.go", "serve", 5), "serve", "", "app/app.go", 42, model.ConfidenceHigh)
	// One declared type relationship, so the index is seen to record them.
	b.file("app/types.go", "go", 40, typ("Base", model.KindClass, 1, 10), typ("Derived", model.KindClass, 20, 30))
	b.edge(symKey("app/types.go", "Derived", 20), symKey("app/types.go", "Base", 1),
		model.EdgeExtends, model.ConfidenceHigh, "app/types.go", 21)
	b.wellBound("go", ".go")
}

func serverFor(t *testing.T, build func(*indexBuilder), opts ...fullOption) fullServer {
	t.Helper()
	b := &indexBuilder{}
	build(b)
	return newFullServer(t, b.seed, opts...)
}

// ---------------------------------------------------------------- item 1

// callsForEveryTool is one valid call per tool, against a symbol each fixture
// below defines under the name `target`.
func callsForEveryTool(target, path string) map[string]map[string]any {
	return map[string]map[string]any{
		"get_task_context":        {"task": "change how " + target + " works"},
		"get_project_overview":    {},
		"search_code":             {"query": target},
		"find_symbol":             {"name": target},
		"get_symbol":              {"name": target},
		"find_references":         {"name": target},
		"find_callers":            {"name": target},
		"explore_symbol":          {"name": target},
		"find_callees":            {"name": target},
		"find_implementations":    {"name": target},
		"trace_call_path":         {"from": target, "to": target},
		"find_tests":              {"symbol": target},
		"analyze_impact":          {"symbol": target},
		"analyze_change":          {"base": "HEAD"},
		"analyze_patch":           {"path": path, "content": "package p\nfunc F() {}\n"},
		"find_owners":             {"symbol": target},
		"get_resolution_coverage": {},
		"plan_rename":             {"name": target, "new_name": target + "Renamed"},
		"plan_change":             {"symbol": target},
		"list_api_endpoints":      {},
		"list_db_tables":          {},
		"get_config_key":          {"key": "DATABASE_URL"},
		"get_file_outline":        {"path": path},
		"minimize_diff":           {"diff": "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"},
		"list_blindspots":         {},
		"list_dead_code":          {"min_certainty": "possible"},
		"list_hotspots":           {},
		"list_knowledge_risks":    {},
		"list_communities":        {},
		"search_graph":            {"query": "SELECT count(*) FROM symbols"},
		"get_session_stats":       {},
	}
}

// doubtWords are how an answer says it is not one. goAheadWords are how it
// says to act. No answer may carry one of each.
var (
	doubtWords   = []string{"COVERAGE TOO LOW", "COVERAGE UNKNOWN", "Do not present what follows"}
	goAheadWords = []string{"verdict: strong", "verdict: proven", "Safe to act on", "Act on these"}
)

func containsAny(text string, words []string) (string, bool) {
	for _, w := range words {
		if strings.Contains(text, w) {
			return w, true
		}
	}
	return "", false
}

// TestNoAnswerSaysBothDoNotUseAndActOnThese calls EVERY tool the server
// registers, on an index that resolves badly, one whose language yields no
// references, and one with nothing wrong, and holds each answer to the rule
// the audit found broken: an answer that says it is not reliable may not also
// tell the agent to act on it, and it must say so on its first line.
func TestNoAnswerSaysBothDoNotUseAndActOnThese(t *testing.T) {
	fixtures := []struct {
		name         string
		build        func(*indexBuilder)
		target, path string
		wantDoubt    bool
	}{
		{"low coverage", lowCoverageJava, "update", "svc/Service.java", true},
		{"language without references", kotlinWithoutReferences, "emit", "okio/Sink.kt", true},
		{"well covered", wellCoveredGo, "serve", "app/app.go", false},
		// The language resolves badly and the target does not. The tools that
		// hold the target's own accounting no longer doubt it; the ones that do
		// not (callees, hierarchy, routes) still do, on their first line.
		{"low language rate, target accounted for", lowCoverageJavaTargetBound, "update", "svc/Service.java", true},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			fs := serverFor(t, fx.build)
			calls := callsForEveryTool(fx.target, fx.path)
			sawDoubt := false
			for _, tool := range fs.srv.Tools() {
				args, ok := calls[tool]
				if !ok {
					t.Errorf("tool %q has no call in this test; add one so it is held to the rule too", tool)
					continue
				}
				text, _ := answer(t, fs.sess, tool, args)
				doubt, doubted := containsAny(text, doubtWords)
				goAhead, urged := containsAny(text, goAheadWords)
				if doubted && urged {
					t.Errorf("%s says %q and %q in one answer:\n%s", tool, doubt, goAhead, text)
				}
				// get_resolution_coverage is exempt from the first-line rule and
				// only from that: coverage is its SUBJECT, so its first line is the
				// rate itself, and the note is its finding about the other tools.
				if doubted && tool != "get_resolution_coverage" {
					sawDoubt = true
					first := firstLine(text)
					if !strings.HasPrefix(first, leadUnknown) && !strings.HasPrefix(first, leadUnreliable) {
						t.Errorf("%s doubts itself in a note (%q) and not on its first line: %q", tool, doubt, first)
					}
				}
				if !fx.wantDoubt && doubted {
					t.Errorf("%s doubted a well-covered index: %q\n%s", tool, doubt, text)
				}
			}
			if fx.wantDoubt && !sawDoubt {
				t.Error("no tool doubted this index; the fixture does not exercise the gate")
			}
		})
	}
}

// TestLowCoverageNeverGradesStrong is the audited case stated directly: rows
// that are individually high, on an index that bound a minority of what it
// could.
func TestLowCoverageNeverGradesStrong(t *testing.T) {
	fs := serverFor(t, lowCoverageJava)
	for _, tool := range []string{"find_callers", "find_references", "analyze_impact"} {
		arg := "name"
		if tool == "analyze_impact" {
			arg = "symbol"
		}
		text, res := answer(t, fs.sess, tool, map[string]any{arg: "update"})
		if res.IsError {
			t.Fatalf("%s failed:\n%s", tool, text)
		}
		if !strings.Contains(text, "verdict: unreliable") {
			t.Errorf("%s on a low-coverage index is not graded unreliable:\n%s", tool, text)
		}
		if !strings.HasPrefix(firstLine(text), leadUnreliable+" — ") {
			t.Errorf("%s: first line does not open with %s: %q", tool, leadUnreliable, firstLine(text))
		}
	}
}

// TestWellCoveredAnswerStaysStrong: the ceiling must not cost a sound answer
// its verdict, or every answer becomes a warning and none is read.
func TestWellCoveredAnswerStaysStrong(t *testing.T) {
	fs := serverFor(t, wellCoveredGo)
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "serve"})
	if !strings.Contains(text, "verdict: strong") {
		t.Errorf("a fully bound symbol on a well-covered index lost its verdict:\n%s", text)
	}
	first := firstLine(text)
	if !strings.HasPrefix(first, "2 caller(s) of serve (app/app.go:5)") {
		t.Errorf("first line = %q, want the plain count with the definition answered for", first)
	}
}

// ---------------------------------------------------------------- item 2

// unboundName is Gson.fromJson's shape: one definition, many references
// spelled like it, none bound.
func unboundName(b *indexBuilder) {
	b.file("lib/codec.go", "go", 60, fn("decode", 5, 15), fn("unused", 30, 40))
	for i := 0; i < 9; i++ {
		b.ref("lib/codec.go", "decode", "c", 45+i, registry.ResolutionAmbiguous, "")
	}
	b.wellBound("go", ".go")
}

// topLevelCallers is express's defineGetter: every reference bound, none
// inside an extracted function, so no edge and no caller row.
func topLevelCallers(b *indexBuilder) {
	b.file("lib/request.go", "go", 60, fn("defineGetter", 50, 58))
	for i := 0; i < 12; i++ {
		b.ref("lib/request.go", "defineGetter", "", 5+i, registry.ResolutionResolved,
			symKey("lib/request.go", "defineGetter", 50))
	}
	b.wellBound("go", ".go")
}

func TestRiskIsUnknownWhenCallersWereNeverBound(t *testing.T) {
	cases := []struct {
		name   string
		build  func(*indexBuilder)
		symbol string
		want   []string // every one must appear in the answer
	}{
		{"references ambiguous", unboundName, "decode",
			[]string{"Risk unknown", "9 of the 9 reference(s) spelled like this name", "9 ambiguous"}},
		{"references bound outside any function", topLevelCallers, "defineGetter",
			[]string{"Risk unknown", "12 bound to it from outside any indexed function"}},
		{"language yields no references", kotlinWithoutReferences, "emit",
			[]string{"Risk unknown.", "RepoCortex does not extract calls from Kotlin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := serverFor(t, tc.build)
			text, res := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": tc.symbol})
			if res.IsError {
				t.Fatalf("analyze_impact failed:\n%s", text)
			}
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("answer does not say %q:\n%s", w, text)
				}
			}
			first := firstLine(text)
			if strings.Contains(first, "Risk low") || strings.Contains(first, "score") {
				t.Errorf("an undetermined risk printed a low level or a score: %q", first)
			}
			if !strings.Contains(first, "Risk unknown") {
				t.Errorf("first line does not carry the unknown risk: %q", first)
			}

			// plan_change and find_tests are the same question asked another way.
			plan, _ := answer(t, fs.sess, "plan_change", map[string]any{"symbol": tc.symbol})
			if !strings.Contains(firstLine(plan), "Risk UNKNOWN") || strings.Contains(firstLine(plan), "Risk LOW") {
				t.Errorf("plan_change: first line = %q, want an unknown risk", firstLine(plan))
			}
			if _, urged := containsAny(plan, goAheadWords); urged {
				t.Errorf("plan_change told the agent to act on a plan with undetermined callers:\n%s", plan)
			}
			tests, _ := answer(t, fs.sess, "find_tests", map[string]any{"symbol": tc.symbol})
			if !strings.Contains(firstLine(tests), "UNKNOWN") || strings.Contains(firstLine(tests), "0 test(s)") {
				t.Errorf("find_tests: first line = %q, want it to say whether it is tested is unknown", firstLine(tests))
			}
			if strings.Contains(tests, "That is a positive finding") {
				t.Errorf("find_tests called an undetermined absence a positive finding:\n%s", tests)
			}
		})
	}
}

// TestRiskStaysLowWhenEveryReferenceIsAccountedFor: unknown is for missing
// evidence. A symbol whose every reference was bound and listed is determined,
// and reporting it unknown would make the word mean nothing.
func TestRiskStaysLowWhenEveryReferenceIsAccountedFor(t *testing.T) {
	fs := serverFor(t, wellCoveredGo)
	text, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "start"})
	if strings.Contains(text, "Risk unknown") {
		t.Errorf("a symbol with no unbound reference was reported undetermined:\n%s", text)
	}
	if !strings.Contains(firstLine(text), "Risk low (score") {
		t.Errorf("first line = %q, want a determined low risk with its score", firstLine(text))
	}
}

// ---------------------------------------------------------------- item 3

// overloads is Gson.fromJson's other property: several definitions in one
// file under one qualified name, beside a namesake in another file.
func overloads(b *indexBuilder) {
	b.file("src/Gson.java", "java", 120,
		typ("Gson", model.KindClass, 1, 120),
		method("fromJson", 10, 19, 2),
		method("fromJson", 20, 29, 2),
		method("fromJson", 30, 39, 3))
	b.file("src/Adapter.java", "java", 60,
		typ("Adapter", model.KindClass, 1, 60),
		method("fromJson", 10, 19, 1))
	b.file("src/Caller.java", "java", 60,
		typ("Caller", model.KindClass, 1, 60),
		method("load", 5, 15, 0))
	b.call(symKey("src/Caller.java", "load", 5), symKey("src/Gson.java", "fromJson", 20),
		"fromJson", "gson", "src/Caller.java", 8, model.ConfidenceHigh)
	b.wellBound("java", ".java")
}

// symbolTools is every tool that takes a symbol, with the argument that names
// it and the selectors that choose between namesakes.
var symbolTools = []struct {
	tool, param, pathParam, lineParam string
	extra                             map[string]any
}{
	{"find_callers", "name", "path", "line", nil},
	{"find_references", "name", "path", "line", nil},
	{"find_callees", "name", "path", "line", nil},
	{"plan_rename", "name", "path", "line", map[string]any{"new_name": "parse"}},
	{"analyze_impact", "symbol", "path", "line", nil},
	{"find_tests", "symbol", "path", "line", nil},
	{"plan_change", "symbol", "path", "line", nil},
	{"find_owners", "symbol", "path", "line", nil},
	{"trace_call_path", "from", "from_path", "from_line", map[string]any{"to": "load"}},
}

func withArgs(base map[string]any, more map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

// TestAnAmbiguousNameIsAnAnswerNotAnInternalError: every tool that takes a
// symbol, asked for a name four definitions share, returns the four to choose
// from. None answers for one of them, and none reports a malfunction.
func TestAnAmbiguousNameIsAnAnswerNotAnInternalError(t *testing.T) {
	fs := serverFor(t, overloads)
	for _, st := range symbolTools {
		t.Run(st.tool, func(t *testing.T) {
			text, res := answer(t, fs.sess, st.tool, withArgs(st.extra, map[string]any{st.param: "fromJson"}))
			if res.IsError {
				t.Fatalf("an ambiguous name is flagged isError:\n%s", text)
			}
			for _, bad := range []string{"error_code", "internal", "failed internally"} {
				if strings.Contains(text, bad) {
					t.Errorf("answer contains %q:\n%s", bad, text)
				}
			}
			if !strings.HasPrefix(firstLine(text), leadAmbiguous+" — ") {
				t.Errorf("first line does not open with %s: %q", leadAmbiguous, firstLine(text))
			}
			if !strings.Contains(text, "outcome: "+outcomeAmbiguous) {
				t.Errorf("no outcome line:\n%s", text)
			}
			// Each candidate once, told apart, with the arguments that select it.
			for _, want := range []string{
				fmt.Sprintf("select with %s=src/Gson.java %s=10", st.pathParam, st.lineParam),
				fmt.Sprintf("select with %s=src/Gson.java %s=20", st.pathParam, st.lineParam),
				fmt.Sprintf("select with %s=src/Gson.java %s=30", st.pathParam, st.lineParam),
				fmt.Sprintf("select with %s=src/Adapter.java %s=10", st.pathParam, st.lineParam),
				"in class Gson", "in class Adapter", "3 parameter(s)", "1 parameter(s)",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("answer does not contain %q:\n%s", want, text)
				}
			}
			if n := rowCount(text); n != 4 {
				t.Errorf("%d candidate rows, want 4", n)
			}
			if _, urged := containsAny(text, goAheadWords); urged {
				t.Errorf("a list of candidates was graded as if it were the answer:\n%s", text)
			}
		})
	}
}

// TestPathAndLineSelectAnyCandidate: the arguments the disambiguation answer
// prints select exactly the definition they name, on every tool, including
// one overload among three that share a qualified name.
func TestPathAndLineSelectAnyCandidate(t *testing.T) {
	fs := serverFor(t, overloads)
	for _, st := range symbolTools {
		for _, line := range []int{10, 20, 30} {
			t.Run(fmt.Sprintf("%s line %d", st.tool, line), func(t *testing.T) {
				args := withArgs(st.extra, map[string]any{st.param: "fromJson", st.pathParam: "src/Gson.java", st.lineParam: line})
				text, res := answer(t, fs.sess, st.tool, args)
				if res.IsError {
					t.Fatalf("selecting by path and line failed:\n%s", text)
				}
				if strings.Contains(text, "outcome: "+outcomeAmbiguous) {
					t.Fatalf("path and line did not select one definition:\n%s", text)
				}
				want := fmt.Sprintf("(src/Gson.java:%d)", line)
				if st.tool == "find_owners" {
					// No history is wired in this fixture, so find_owners stops
					// after selecting; reaching that answer is the selection.
					return
				}
				if !strings.Contains(firstLine(text), want) {
					t.Errorf("first line does not name the definition answered for, %s: %q", want, firstLine(text))
				}
			})
		}
	}

	// Only the second overload has a caller. If selection fell back to the
	// first, this is where it would show.
	second, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "fromJson", "path": "src/Gson.java", "line": 20})
	if !strings.Contains(firstLine(second), "1 caller(s) of fromJson (src/Gson.java:20)") {
		t.Errorf("the overload at line 20 has one caller; got %q", firstLine(second))
	}
	first, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "fromJson", "path": "src/Gson.java", "line": 10})
	if !strings.Contains(firstLine(first), "0 caller(s) of fromJson (src/Gson.java:10)") {
		t.Errorf("the overload at line 10 has no caller; got %q", firstLine(first))
	}
}

// TestAQualifiedNameSharedByOverloadsIsStillAmbiguous: the old error told the
// caller to "re-ask with a fully-qualified name", and the re-ask silently
// answered for the first overload.
func TestAQualifiedNameSharedByOverloadsIsStillAmbiguous(t *testing.T) {
	fs := serverFor(t, overloads)
	text, res := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "src/Gson.java:fromJson"})
	if res.IsError {
		t.Fatalf("failed:\n%s", text)
	}
	if !strings.Contains(text, "outcome: "+outcomeAmbiguous) || rowCount(text) != 3 {
		t.Errorf("three overloads share this qualified name; want the three listed, got:\n%s", text)
	}

	// One that IS unique resolves.
	unique, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "src/Adapter.java:fromJson"})
	if !strings.Contains(firstLine(unique), "(src/Adapter.java:10)") {
		t.Errorf("a unique qualified name did not resolve: %q", firstLine(unique))
	}
}

// ---------------------------------------------------------------- item 4

// manyNamesakes defines New in seven packages, more than the five the old
// lookup read, with the one a test asks for sorting last.
func manyNamesakes(b *indexBuilder) {
	for i := 1; i <= 7; i++ {
		path := fmt.Sprintf("pkg/a%d/a.go", i)
		b.file(path, "go", 40, fn("New", 10, 20), fn("use", 25, 35))
		b.call(symKey(path, "use", 25), symKey(path, "New", 10), "New", "", path, 27, model.ConfidenceHigh)
	}
	b.file("vendor/pkg/a7/a.go", "go", 40, fn("New", 10, 20))
	b.wellBound("go", ".go")
}

func TestThePathHintIsHonouredHoweverManyDefinitionsShareTheName(t *testing.T) {
	fs := serverFor(t, manyNamesakes)

	// The seventh by path, which the first-five lookup never saw.
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "New", "path": "pkg/a7/a.go"})
	if !strings.Contains(firstLine(text), "1 caller(s) of New (pkg/a7/a.go:10)") {
		t.Errorf("path=pkg/a7/a.go did not select that file's definition: %q", firstLine(text))
	}
	// EXACTLY: vendor/pkg/a7/a.go contains the hint as a substring and must
	// not make it ambiguous, nor be chosen.
	if strings.Contains(text, "outcome: "+outcomeAmbiguous) {
		t.Errorf("an exact path was treated as ambiguous:\n%s", text)
	}
	if !strings.Contains(text, "8 definitions are named \"New\"") || !strings.Contains(text, "chosen by path") {
		t.Errorf("the answer does not say which of the 8 it answered for, or how:\n%s", text)
	}

	// A directory selects the one definition under it.
	dir, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "New", "path": "pkg/a3"})
	if !strings.Contains(firstLine(dir), "(pkg/a3/a.go:10)") {
		t.Errorf("path=pkg/a3 did not select the definition in that directory: %q", firstLine(dir))
	}

	// No hint: never silently one of them.
	bare, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "New"})
	if !strings.HasPrefix(firstLine(bare), leadAmbiguous) || rowCount(bare) != 8 {
		t.Errorf("a name shared by 8 definitions was answered for one of them:\n%s", bare)
	}

	// A hint that matches nothing is not a licence to fall back to the first.
	wrong, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "New", "path": "pkg/nowhere"})
	if !strings.HasPrefix(firstLine(wrong), leadAmbiguous) ||
		!strings.Contains(firstLine(wrong), "no definition of \"New\" is in a file matching path=\"pkg/nowhere\"") {
		t.Errorf("a path matching nothing did not say so: %q", firstLine(wrong))
	}
}

// typeAndMethods is gin's Render: one interface and methods of the same name.
func typeAndMethods(b *indexBuilder) {
	b.file("ctx.go", "go", 40, method("Render", 10, 20, 2))
	b.file("render/json.go", "go", 40, typ("JSON", model.KindClass, 1, 8), method("Render", 10, 20, 1))
	b.file("render/render.go", "go", 40, typ("Render", model.KindInterface, 10, 14))
	b.file("render/html.go", "go", 40, typ("HTML", model.KindClass, 1, 8))
	b.edge(symKey("render/html.go", "HTML", 1), symKey("render/render.go", "Render", 10),
		model.EdgeImplements, model.ConfidenceHigh, "render/html.go", 2)
	b.wellBound("go", ".go")
}

func TestFindImplementationsPrefersTheType(t *testing.T) {
	fs := serverFor(t, typeAndMethods)
	text, _ := answer(t, fs.sess, "find_implementations", map[string]any{"name": "Render"})
	first := firstLine(text)
	if !strings.Contains(first, "Render (render/render.go:10): 1 implementing or extending it") {
		t.Errorf("the interface was not the definition answered for: %q", first)
	}
	if !strings.Contains(text, "3 definitions are named \"Render\"") ||
		!strings.Contains(text, "because find_implementations is about types") {
		t.Errorf("the answer does not say it chose the type among 3 namesakes:\n%s", text)
	}

	// A path the caller gives outranks the preference.
	byPath, _ := answer(t, fs.sess, "find_implementations", map[string]any{"name": "Render", "path": "render/json.go"})
	if !strings.Contains(firstLine(byPath), "Render (render/json.go:10)") {
		t.Errorf("path did not override the kind preference: %q", firstLine(byPath))
	}

	// find_callers is not about types, and two methods remain: no silent pick.
	callers, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "Render"})
	if !strings.HasPrefix(firstLine(callers), leadAmbiguous) {
		t.Errorf("find_callers answered for one of two methods named Render: %q", firstLine(callers))
	}
}

// TestAGoInterfaceSaysItsImplementationsAreNotListed: "0 implementing it" for
// a Go interface is almost never true, and was the first line.
func TestAGoInterfaceSaysItsImplementationsAreNotListed(t *testing.T) {
	fs := serverFor(t, typeAndMethods)
	text, _ := answer(t, fs.sess, "find_implementations", map[string]any{"name": "Render"})
	if !strings.HasPrefix(firstLine(text), leadIncomplete+" — Go types can satisfy this interface without declaring it") {
		t.Errorf("first line = %q", firstLine(text))
	}
	if _, urged := containsAny(text, goAheadWords); urged {
		t.Errorf("a structurally-typed interface's declared subtypes were graded as the answer:\n%s", text)
	}
}

func interfaceDispatchFixture(b *indexBuilder) {
	b.file("repo/repo.go", "go", 40, typ("UserRepo", model.KindInterface, 5, 20), method("Save", 10, 15, 1))
	b.file("repo/impl.go", "go", 40, typ("UserRepoImpl", model.KindClass, 5, 30), method("Save", 12, 25, 1))
	b.file("service/main.go", "go", 40, fn("Main", 5, 20))

	ifaceMethod := symKey("repo/repo.go", "Save", 10)
	implMethod := symKey("repo/impl.go", "Save", 12)
	caller := symKey("service/main.go", "Main", 5)

	// UserRepoImpl.Save implements UserRepo.Save
	b.edge(implMethod, ifaceMethod, model.EdgeImplements, model.ConfidenceHigh, "repo/impl.go", 12)
	// Main calls UserRepo.Save
	b.call(caller, ifaceMethod, "Save", "repo", "service/main.go", 8, model.ConfidenceHigh)
	b.wellBound("go", ".go")
}

func TestFindCallersInterfaceDispatchNotes(t *testing.T) {
	fs := serverFor(t, interfaceDispatchFixture)
	// Query callers for the concrete implementation
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "Save", "path": "repo/impl.go"})
	if !strings.Contains(text, "Interface dispatch: Save implements Save (repo/repo.go:10)") {
		t.Errorf("expected interface dispatch note, got:\n%s", text)
	}
	if !strings.Contains(text, "which has 1 recorded caller(s)") {
		t.Errorf("expected 1 recorded caller note, got:\n%s", text)
	}
}

// ---------------------------------------------------------------- item 5

// manyCallers gives one function 180 callers, and one interface 36 subtypes.
func manyCallers(b *indexBuilder) {
	b.file("ui/cn.go", "go", 20, fn("cn", 5, 10))
	target := symKey("ui/cn.go", "cn", 5)
	for i := 0; i < 180; i++ {
		path := fmt.Sprintf("ui/c%03d.go", i)
		name := fmt.Sprintf("Component%03d", i)
		b.file(path, "go", 20, fn(name, 3, 12))
		b.call(symKey(path, name, 3), target, "cn", "", path, 6, model.ConfidenceHigh)
	}
	b.file("zod/types.go", "go", 20, typ("ZodType", model.KindClass, 1, 10))
	for i := 0; i < 36; i++ {
		path := fmt.Sprintf("zod/t%02d.go", i)
		name := fmt.Sprintf("Zod%02d", i)
		b.file(path, "go", 20, typ(name, model.KindClass, 1, 10))
		b.edge(symKey(path, name, 1), symKey("zod/types.go", "ZodType", 1),
			model.EdgeExtends, model.ConfidenceHigh, path, 1)
	}
}

func TestListToolsSayHowManyRowsExistAndHowToReadOn(t *testing.T) {
	fs := serverFor(t, manyCallers)

	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "cn"})
	if n := rowCount(text); n != 50 {
		t.Errorf("default page carries %d rows, want 50", n)
	}
	first := firstLine(text)
	if !strings.HasPrefix(first, leadIncomplete+" — rows 1-50 of 180 are shown. ") ||
		!strings.Contains(first, "180 caller(s) of cn (ui/cn.go:5)") {
		t.Errorf("first line does not give the total and say rows were cut: %q", first)
	}
	for _, want := range []string{
		"showing rows 1-50 of 180 caller(s), so 130 are not shown here",
		"Pass offset=50 to read on", "limit accepts up to 200",
		"verdict: incomplete",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("answer does not say %q:\n%s", want, text)
		}
	}

	// limit is accepted, and bounded by the server maximum.
	hundred, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "cn", "limit": 100})
	if n := rowCount(hundred); n != 100 {
		t.Errorf("limit=100 carried %d rows", n)
	}
	huge, res := answer(t, fs.sess, "find_callers", map[string]any{"name": "cn", "limit": 100000})
	if res.IsError {
		t.Fatalf("a limit above the maximum is an error, want it clamped:\n%s", huge)
	}
	if n := rowCount(huge); n > DefaultMaxPageItems || n < 100 {
		t.Errorf("limit=100000 carried %d rows; want it clamped to the maximum of %d (or fewer if the size limit cut it)",
			n, DefaultMaxPageItems)
	}
	if len(huge) > DefaultLimits().MaxTotalBytes {
		t.Errorf("a large page is %d bytes, over the %d limit", len(huge), DefaultLimits().MaxTotalBytes)
	}

	// Paging reaches every row exactly once.
	seen := map[string]int{}
	for offset := 0; offset < 180; {
		page, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "cn", "limit": 70, "offset": offset})
		n := 0
		for _, line := range strings.Split(page, "\n") {
			if strings.HasPrefix(line, "- calls ") {
				seen[strings.Fields(line)[2]]++
				n++
			}
		}
		if n == 0 {
			t.Fatalf("offset=%d returned no rows:\n%s", offset, page)
		}
		offset += n
	}
	if len(seen) != 180 {
		t.Errorf("paging reached %d distinct callers, want 180", len(seen))
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s appeared %d times across pages", name, n)
		}
	}

	// The last page is complete and says nothing about cut rows.
	last, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "cn", "limit": 50, "offset": 150})
	if strings.Contains(last, "truncated:") || strings.HasPrefix(firstLine(last), leadIncomplete) {
		t.Errorf("the final page claims rows were cut:\n%s", last)
	}

	if _, res := answer(t, fs.sess, "find_callers", map[string]any{"name": "cn", "limit": -1}); !res.IsError {
		t.Error("a negative limit was accepted")
	}
}

func TestFindImplementationsTakesALimit(t *testing.T) {
	fs := serverFor(t, manyCallers)
	text, res := answer(t, fs.sess, "find_implementations", map[string]any{"name": "ZodType", "limit": 100})
	if res.IsError {
		t.Fatalf("limit=100 was rejected:\n%s", text)
	}
	if n := rowCount(text); n != 36 {
		t.Errorf("limit=100 carried %d of 36 subtypes", n)
	}
	if !strings.Contains(firstLine(text), "36 implementing or extending it") {
		t.Errorf("first line = %q", firstLine(text))
	}
	if strings.Contains(text, "truncated:") {
		t.Errorf("a complete list says it was cut:\n%s", text)
	}

	// Supertypes and subtypes are one list under one offset.
	page, _ := answer(t, fs.sess, "find_implementations", map[string]any{"name": "ZodType", "limit": 10, "offset": 30})
	if n := rowCount(page); n != 6 {
		t.Errorf("offset=30 limit=10 carried %d rows, want the last 6", n)
	}
}

// ---------------------------------------------------------------- item 7

func exportFixture(b *indexBuilder) {
	b.file("src/Api.java", "java", 60, typ("Api", model.KindClass, 1, 60), method("parse", 10, 20, 1), method("caller", 30, 40, 0))
	b.call(symKey("src/Api.java", "caller", 30), symKey("src/Api.java", "parse", 10), "parse", "", "src/Api.java", 32, model.ConfidenceHigh)
	b.file("lib/lib.go", "go", 60, fn("helper", 10, 20), fn("uses", 30, 40))
	b.call(symKey("lib/lib.go", "uses", 30), symKey("lib/lib.go", "helper", 10), "helper", "", "lib/lib.go", 32, model.ConfidenceHigh)
	b.wellBound("java", ".java")
	b.wellBound("go", ".go")
}

// TestExportStatusIsNotStatedWhereTheIndexCannotKnowIt: `public T parse(...)`
// starts with a lower-case letter, the index records it unexported, and the
// report said its blast radius was bounded by the repository.
func TestExportStatusIsNotStatedWhereTheIndexCannotKnowIt(t *testing.T) {
	fs := serverFor(t, exportFixture)

	java, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "parse"})
	if strings.Contains(java, "is unexported") || strings.Contains(java, "is exported.") {
		t.Errorf("a Java method's export status was stated as fact:\n%s", java)
	}
	if !strings.Contains(java, "Whether the target is exported is UNKNOWN") || !strings.Contains(java, "Java") {
		t.Errorf("the answer does not say the export status is unknown for Java:\n%s", java)
	}

	// Go is the language whose rule the flag follows.
	goText, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "helper"})
	if !strings.Contains(goText, "The target is unexported") {
		t.Errorf("a Go function's export status, which the index does know, was withheld:\n%s", goText)
	}
}

// ---------------------------------------------------------------- item 8

func TestCoverageIsReportedForTheSymbolsOwnLanguage(t *testing.T) {
	fs := serverFor(t, kotlinWithoutReferences)

	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "emit"})
	first := firstLine(text)
	if want := "UNKNOWN — RepoCortex does not extract calls from Kotlin; nothing about callers can be concluded."; !strings.HasPrefix(first, want) {
		t.Errorf("first line = %q, want it to open %q", first, want)
	}
	for _, want := range []string{"COVERAGE UNKNOWN", "28 Kotlin definition", "Running 'repocortex resolve' again will not change this",
		// No reference by this name is not a finding where no reference by any
		// name was recorded.
		"none is recorded from any Kotlin code"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer does not say %q:\n%s", want, text)
		}
	}
	// The Java files' rate is not this symbol's coverage, and a command that
	// has already run is not the remedy.
	for _, bad := range []string{"Java", "Run 'repocortex resolve'", "to raise it"} {
		if strings.Contains(text, bad) {
			t.Errorf("a Kotlin symbol's answer mentions %q:\n%s", bad, text)
		}
	}

	// The Java symbol beside it is judged on the Java files.
	java, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "step"})
	if !strings.HasPrefix(firstLine(java), leadUnreliable+" — only 11% of the Java references") {
		t.Errorf("the Java symbol was not judged on the Java files: %q", firstLine(java))
	}
	if strings.Contains(java, "Kotlin") {
		t.Errorf("a Java symbol's answer mentions Kotlin:\n%s", java)
	}
	if !strings.Contains(java, "'repocortex resolve' has already run on every one of them") {
		t.Errorf("resolve was advised although nothing is pending:\n%s", java)
	}
}

// TestResolveIsAdvisedOnlyWhenSomethingIsPending: the one case where the
// command changes the answer.
func TestResolveIsAdvisedOnlyWhenSomethingIsPending(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		b.file("a/A.java", "java", 60, typ("A", model.KindClass, 1, 60), method("run", 5, 15, 0))
		for i := 0; i < 8; i++ {
			b.ref("a/A.java", "later", "", 20+i, registry.ResolutionPending, "")
		}
	})
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "run"})
	if !strings.HasPrefix(firstLine(text), leadUnknown) || !strings.Contains(text, "Run 'repocortex resolve'") {
		t.Errorf("an unresolved index did not say to resolve it:\n%s", text)
	}
}

// TestALanguageWithNoInheritanceSaysSo: find_implementations on a language
// whose calls resolve and whose types have no recorded relationship.
func TestALanguageWithNoInheritanceSaysSo(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		b.file("lib/base.rb", "ruby", 60, typ("Base", model.KindClass, 1, 20), fn("run", 30, 40), fn("go", 45, 55))
		b.call(symKey("lib/base.rb", "go", 45), symKey("lib/base.rb", "run", 30), "run", "", "lib/base.rb", 47, model.ConfidenceHigh)
		b.wellBound("ruby", ".rb")
	})
	text, _ := answer(t, fs.sess, "find_implementations", map[string]any{"name": "Base"})
	if want := "UNKNOWN — this index records no extends or implements relationship for any Ruby type"; !strings.HasPrefix(firstLine(text), want) {
		t.Errorf("first line = %q, want it to open %q", firstLine(text), want)
	}
	// The same language's callers are not doubted for it.
	callers, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "run"})
	if strings.HasPrefix(firstLine(callers), leadUnknown) {
		t.Errorf("a missing type hierarchy made a caller list unknown: %q", firstLine(callers))
	}
}

// ---------------------------------------------------------------- qualifiers

// aliasedCalls is a Go function called bare from inside its package and as
// devmcp.New and mcp.New from two files that import the package under
// different names. Both qualifiers name the package, so nothing is wrong.
func aliasedCalls(b *indexBuilder) {
	b.file("gin.go", "go", 80, fn("New", 10, 20), fn("Default", 30, 40), fn("cmd", 50, 60), fn("page", 65, 75))
	target := symKey("gin.go", "New", 10)
	b.callOn(symKey("gin.go", "Default", 30), target, "New", model.ReceiverBare, "", "gin.go", 32, model.ConfidenceHigh)
	b.callOn(symKey("gin.go", "cmd", 50), target, "New", model.ReceiverName, "devmcp", "gin.go", 52, model.ConfidenceHigh)
	b.callOn(symKey("gin.go", "page", 65), target, "New", model.ReceiverName, "mcp", "gin.go", 67, model.ConfidenceHigh)
	b.wellBound("go", ".go")
}

// TestAnImportAliasIsNotAConflict: two package qualifiers on calls to one Go
// function used to mark the answer UNVERIFIED, on the reading that at most one
// of them could be this package. A call is bound through its qualifier now,
// and one package is imported under different names in different files.
func TestAnImportAliasIsNotAConflict(t *testing.T) {
	fs := serverFor(t, aliasedCalls)
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "New"})

	if strings.HasPrefix(firstLine(text), leadUnverified) || !strings.Contains(text, "verdict: strong") {
		t.Errorf("two import aliases of one package were read as a conflict:\n%s", text)
	}
	for _, want := range []string{"written as devmcp.New", "written as mcp.New",
		"How the calls bound to this definition were made: New ×1, devmcp.New ×1, mcp.New ×1",
		"different import aliases"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "matched the NAME") {
		t.Errorf("the note says a qualified call was bound by its name:\n%s", text)
	}
}

// valueCalls is a Go function that three call sites reach through a value, an
// instance and an expression. A function cannot be called that way, so each of
// those bindings is wrong.
func valueCalls(b *indexBuilder) {
	b.file("gin.go", "go", 80, fn("New", 10, 20), fn("Default", 30, 40), fn("fail", 50, 60), fn("page", 65, 75))
	target := symKey("gin.go", "New", 10)
	b.callOn(symKey("gin.go", "Default", 30), target, "New", model.ReceiverBare, "", "gin.go", 32, model.ConfidenceHigh)
	b.callOn(symKey("gin.go", "fail", 50), target, "New", model.ReceiverValue, "engine", "gin.go", 52, model.ConfidenceHigh)
	b.callOn(symKey("gin.go", "page", 65), target, "New", model.ReceiverExpr, model.OpaqueReceiver, "gin.go", 67, model.ConfidenceHigh)
	b.wellBound("go", ".go")
}

func TestAGoFunctionCalledOnAValueIsUnverified(t *testing.T) {
	fs := serverFor(t, valueCalls)
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "New"})

	for _, want := range []string{"written as engine.New", "written as a call on an expression",
		"2 call(s) bound to this Go function were made on a value, an instance or an expression",
		"How the calls bound to this definition were made: New ×1,", "a call on an expression ×1"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "?.New") {
		t.Errorf("the opaque receiver of an expression was printed as a qualifier:\n%s", text)
	}
	if !strings.HasPrefix(firstLine(text), leadUnverified+" — ") {
		t.Errorf("first line = %q, want it to open with %s", firstLine(text), leadUnverified)
	}
	if !strings.Contains(text, "verdict: mixed") {
		t.Errorf("rows labelled high, two of them provably wrong, were not graded mixed:\n%s", text)
	}
	if _, urged := containsAny(text, goAheadWords); urged {
		t.Errorf("the answer tells the agent to act on rows it has evidence against:\n%s", text)
	}

	// The blast radius, the tests and the plan are built on the same bindings.
	for _, tool := range []string{"analyze_impact", "find_tests", "plan_change"} {
		other, _ := answer(t, fs.sess, tool, map[string]any{"symbol": "New"})
		if !strings.HasPrefix(firstLine(other), leadUnverified+" — ") {
			t.Errorf("%s presents callers find_callers has evidence against: %q", tool, firstLine(other))
		}
		if _, urged := containsAny(other, goAheadWords); urged {
			t.Errorf("%s tells the agent to act on them:\n%s", tool, other)
		}
	}
}

// TestMethodReceiversAreNotAConflict: a method is called on a different
// variable at every correct call site. Reading that as a conflict would mark
// every method in every object-oriented language unverified.
func TestMethodReceiversAreNotAConflict(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		b.file("r.go", "go", 80, method("Read", 10, 20, 1), fn("a", 30, 40), fn("b", 50, 60))
		target := symKey("r.go", "Read", 10)
		b.callOn(symKey("r.go", "a", 30), target, "Read", model.ReceiverValue, "reader", "r.go", 32, model.ConfidenceHigh)
		b.callOn(symKey("r.go", "b", 50), target, "Read", model.ReceiverValue, "in", "r.go", 52, model.ConfidenceHigh)
		b.wellBound("go", ".go")
	})
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "Read"})
	if strings.HasPrefix(firstLine(text), leadUnverified) || !strings.Contains(text, "verdict: strong") {
		t.Errorf("two receivers on a method were read as a conflict:\n%s", text)
	}
	if !strings.Contains(text, "written as reader.Read") {
		t.Errorf("the rows still say how each call is written:\n%s", text)
	}
}

// TestCallsOnAValueAreSaidToRestOnTheName: a call on a value whose type the
// source does not state is bound on the member name alone, and the answer must
// not describe it as bound through its qualifier.
func TestCallsOnAValueAreSaidToRestOnTheName(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		b.file("r.go", "go", 80, method("Read", 10, 20, 1), fn("a", 30, 40), fn("b", 50, 60))
		target := symKey("r.go", "Read", 10)
		b.callOn(symKey("r.go", "a", 30), target, "Read", model.ReceiverValue, "reader", "r.go", 32, model.ConfidenceHigh)
		b.callOn(symKey("r.go", "b", 50), target, "Read", model.ReceiverType, "buf", "r.go", 52, model.ConfidenceHigh)
		b.wellBound("go", ".go")
	})
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "Read"})
	want := "1 of them were made on a value or an expression whose type the source does not state. " +
		"Those were bound on the member NAME alone"
	if !strings.Contains(text, want) {
		t.Errorf("answer does not say %q:\n%s", want, text)
	}
	if strings.Contains(text, "different import aliases") {
		t.Errorf("the alias sentence was printed for a name-only binding:\n%s", text)
	}
}

// ---------------------------------------------------------------- constructors

// TestAClassOnlyEverConstructedIsNotReportedAsUnused: a constructor call is an
// INSTANTIATES edge to the type, and find_callers reads CALLS only. Its first
// line used to say "0 caller(s)" and nothing else, which reads as "nothing
// uses this type" for a class that is constructed in two places.
func TestAClassOnlyEverConstructedIsNotReportedAsUnused(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		b.file("lib/widget.py", "python", 30, typ("Widget", model.KindClass, 1, 10))
		b.file("lib/factory.py", "python", 30, fn("make", 3, 6), fn("make_big", 8, 12))
		target := symKey("lib/widget.py", "Widget", 1)
		b.ref("lib/factory.py", "Widget", "", 5, registry.ResolutionResolved, target)
		b.edge(symKey("lib/factory.py", "make", 3), target, model.EdgeInstantiates, model.ConfidenceHigh, "lib/factory.py", 5)
		b.ref("lib/factory.py", "Widget", "", 10, registry.ResolutionResolved, target)
		b.edge(symKey("lib/factory.py", "make_big", 8), target, model.EdgeInstantiates, model.ConfidenceHigh, "lib/factory.py", 10)
		b.wellBound("python", ".py")
	})
	callers, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "Widget"})
	if want := "2 construct it, which is not a call"; !strings.Contains(firstLine(callers), want) {
		t.Errorf("first line = %q, want it to say %q", firstLine(callers), want)
	}
	if want := "all 2 construct it (INSTANTIATES)"; !strings.Contains(callers, want) {
		t.Errorf("answer does not say %q:\n%s", want, callers)
	}

	// The tool that follows both kinds lists them under their own label, and the
	// blast radius says what each row does.
	refs, _ := answer(t, fs.sess, "find_references", map[string]any{"name": "Widget"})
	if !strings.Contains(refs, "- instantiates make ") {
		t.Errorf("find_references does not list the constructions as instantiates:\n%s", refs)
	}
	impact, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "Widget"})
	if !strings.Contains(impact, "constructs the target directly") || strings.Contains(impact, "calls the target directly") {
		t.Errorf("analyze_impact does not say the callers construct the type:\n%s", impact)
	}
}

// ---------------------------------------------------------------- bounds

// TestADisambiguationListStaysInsideTheSizeLimit: a name with hundreds of
// definitions must still produce an answer the host shows inline.
func TestADisambiguationListStaysInsideTheSizeLimit(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		for i := 0; i < 300; i++ {
			path := fmt.Sprintf("generated/some/long/package/path/number%03d/implementation_file.go", i)
			b.file(path, "go", 30, fn("Handle", 10, 20))
		}
	})
	for _, tool := range []string{"find_callers", "analyze_impact"} {
		arg := "name"
		if tool == "analyze_impact" {
			arg = "symbol"
		}
		text, res := answer(t, fs.sess, tool, map[string]any{arg: "Handle"})
		if res.IsError {
			t.Fatalf("%s failed:\n%s", tool, text)
		}
		if limit := DefaultLimits().MaxTotalBytes; len(text) > limit {
			t.Errorf("%s: a 300-candidate answer is %d bytes, over the %d limit", tool, len(text), limit)
		}
		if !strings.Contains(firstLine(text), "\"Handle\" names 300 definitions") {
			t.Errorf("%s: first line does not give the true count: %q", tool, firstLine(text))
		}
		if !strings.Contains(text, "are listed of 300 that match") {
			t.Errorf("%s: the answer does not say the list is partial:\n%s", tool, text[:min(len(text), 1200)])
		}
		// The path narrows it in the store, past the rows that were listed.
		one, _ := answer(t, fs.sess, tool, map[string]any{arg: "Handle", "path": "number299"})
		if !strings.Contains(firstLine(one), "number299/implementation_file.go:10") {
			t.Errorf("%s: a path naming the last of 300 did not select it: %q", tool, firstLine(one))
		}
	}
}

// TestSelectionReadsEveryDefinitionOfACommonName covers the store-side path
// filter: more definitions than selection reads in one go.
func TestSelectionReadsEveryDefinitionOfACommonName(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		for i := 0; i < maxSelectRows+20; i++ {
			b.file(fmt.Sprintf("m/p%04d/f.go", i), "go", 20, fn("get", 5, 10))
		}
	})
	last := fmt.Sprintf("m/p%04d/f.go", maxSelectRows+19)
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "get", "path": last})
	if !strings.Contains(firstLine(text), "("+last+":5)") {
		t.Errorf("a path beyond the first %d definitions did not select its definition: %q", maxSelectRows, firstLine(text))
	}
	// The count in the first line is the count, not the number of rows read.
	bare, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "get"})
	if want := fmt.Sprintf("\"get\" names %d definitions", maxSelectRows+20); !strings.Contains(firstLine(bare), want) {
		t.Errorf("first line = %q, want it to say %s", firstLine(bare), want)
	}
}

// TestAFewDefinitionsWithNoReferencesDoNotIndictTheLanguage: two small C files
// with six functions and no call between them are not evidence that RepoCortex
// cannot read C. The answer says what the index holds, concludes nothing about
// callers, and does not say the language is unsupported.
func TestAFewDefinitionsWithNoReferencesDoNotIndictTheLanguage(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		b.file("jni/glue.c", "c", 40, fn("attach", 5, 10), fn("detach", 15, 20))
		b.wellBound("go", ".go")
	})
	text, _ := answer(t, fs.sess, "find_callers", map[string]any{"name": "attach"})
	first := firstLine(text)
	if !strings.HasPrefix(first, leadUnknown+" — RepoCortex found no reference in any C file of this index; nothing about callers can be concluded.") {
		t.Errorf("first line = %q", first)
	}
	if strings.Contains(text, "does not extract calls from C") {
		t.Errorf("two definitions were taken as proof about the language:\n%s", text)
	}
	coverage, _ := answer(t, fs.sess, "get_resolution_coverage", nil)
	if strings.Contains(coverage, "does not extract calls from C") ||
		!strings.Contains(coverage, "this index holds no reference from any of its 2 C definition(s)") {
		t.Errorf("the coverage row for C overstates or omits what the index holds:\n%s", coverage)
	}
}

// TestDeadCodeSaysWhenNothingCouldBeJudged: on a PHP repository every
// definition is unjudged, and "0 definition(s) have no caller" was the first
// line.
func TestDeadCodeSaysWhenNothingCouldBeJudged(t *testing.T) {
	fs := serverFor(t, func(b *indexBuilder) {
		var syms []symDef
		for i := 0; i < 30; i++ {
			syms = append(syms, fn(fmt.Sprintf("handler%02d", i), 10+10*i, 15+10*i))
		}
		b.file("src/Handlers.php", "php", 400, syms...)
	})
	text, _ := answer(t, fs.sess, "list_dead_code", nil)
	first := firstLine(text)
	if !strings.HasPrefix(first, leadUnknown+" — 30 definition(s) in PHP were not judged") {
		t.Errorf("first line = %q; it must not read as 'nothing is unused'", first)
	}
}

// TestSelectorsThatLeaveSeveralSaySo: a path that narrows four namesakes to
// three overloads in one file is progress, and the answer says what was
// applied and what is still needed.
func TestSelectorsThatLeaveSeveralSaySo(t *testing.T) {
	fs := serverFor(t, overloads)
	text, _ := answer(t, fs.sess, "analyze_impact", map[string]any{"symbol": "fromJson", "path": "src/Gson.java"})
	first := firstLine(text)
	if !strings.HasPrefix(first, leadAmbiguous+" — 3 definitions of \"fromJson\" match path=src/Gson.java") {
		t.Errorf("first line = %q", first)
	}
	if rowCount(text) != 3 || strings.Contains(text, "src/Adapter.java") {
		t.Errorf("the list is not the three definitions the path left:\n%s", text)
	}
}

// TestTheFirstLineGivesItsReasonOnce: when the lead already says nothing can
// be concluded and why, the sentence after it does not say it again.
func TestTheFirstLineGivesItsReasonOnce(t *testing.T) {
	fs := serverFor(t, kotlinWithoutReferences)
	for _, tool := range []string{"analyze_impact", "find_tests", "plan_change"} {
		text, _ := answer(t, fs.sess, tool, map[string]any{"symbol": "emit"})
		first := firstLine(text)
		if n := strings.Count(first, "does not extract calls from Kotlin"); n != 1 {
			t.Errorf("%s: the first line gives its reason %d times: %q", tool, n, first)
		}
		if !strings.Contains(strings.ToUpper(first), "UNKNOWN") {
			t.Errorf("%s: first line = %q", tool, first)
		}
	}
}
