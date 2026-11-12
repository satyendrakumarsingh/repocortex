package registry

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// structureFixture is the smallest graph that separates every signal the
// unreferenced-symbol query reports:
//
//	live        called by caller              -> not returned
//	caller      nothing calls it              -> returned, clean
//	recursive   calls only itself             -> returned (a self-edge is not a use)
//	impl        implements iface, uncalled    -> returned, Overrides
//	ghost       uncalled, but an unresolved
//	            reference is spelled "ghost"  -> returned, NameUnbound
//	Total       a constant                    -> never returned (kind excluded)
func structureFixture(t *testing.T) (*Registry, int64, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	r, db := newTestRegistry(t)

	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	fn := func(path, name string, line int) symSpec {
		return symSpec{fqn: model.DefaultFQN(path, name), name: name, line: line, kind: model.KindFunction}
	}
	files := []fileSpec{
		{path: "a/a.go", lang: "go", hash: "h1", content: "0123456789", syms: []symSpec{
			fn("a/a.go", "live", 1), fn("a/a.go", "caller", 10), fn("a/a.go", "recursive", 20),
			{fqn: model.DefaultFQN("a/a.go", "Total"), name: "Total", line: 30, kind: model.KindConstant},
		}},
		{path: "b/b.go", lang: "go", hash: "h2", content: "01234", syms: []symSpec{
			fn("b/b.go", "impl", 1), fn("b/b.go", "ghost", 10),
			{fqn: model.DefaultFQN("b/b.go", "iface"), name: "iface", line: 20, kind: model.KindInterface},
		}},
	}
	edge := func(src, dst string, kind model.EdgeKind) edgeSpec {
		return edgeSpec{srcFQN: src, dstFQN: dst, kind: kind, conf: model.ConfidenceHigh,
			source: model.SourceSourceCode, evFile: "a/a.go", evLine: 1}
	}
	a := func(n string) string { return model.DefaultFQN("a/a.go", n) }
	b := func(n string) string { return model.DefaultFQN("b/b.go", n) }
	ids := writeContent(t, r, run.ID, files, []edgeSpec{
		edge(a("caller"), a("live"), model.EdgeCalls),
		edge(a("recursive"), a("recursive"), model.EdgeCalls),
		edge(b("impl"), b("iface"), model.EdgeImplements),
		// A cross-file call into a/a.go, so FileStructureFor has a dependent
		// to count that does not originate in the same file.
		edge(b("ghost"), a("live"), model.EdgeCalls),
	})

	var fileID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM files WHERE index_run_id = ? AND path = 'a/a.go'`, run.ID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO symbol_references (index_run_id, file_id, name, kind, resolution)
		VALUES (?, ?, 'ghost', 'reference.call', 'unresolved')`, run.ID, fileID); err != nil {
		t.Fatal(err)
	}
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return r, run.ID, ids
}

func TestUnreferencedSymbolsReportsEachSignal(t *testing.T) {
	r, runID, _ := structureFixture(t)

	got, err := r.UnreferencedSymbols(context.Background(), runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]UnreferencedSymbol{}
	for _, u := range got {
		by[u.Name] = u
	}

	if _, ok := by["live"]; ok {
		t.Error("live has a caller and must not be returned")
	}
	if _, ok := by["Total"]; ok {
		t.Error("a constant must not be returned: reads of a name are not reliably extracted")
	}
	if _, ok := by["iface"]; ok {
		t.Error("iface has an implementer pointing at it and must not be returned")
	}
	for _, name := range []string{"caller", "recursive", "impl", "ghost"} {
		if _, ok := by[name]; !ok {
			t.Errorf("%s has no incoming relationship and should be returned; got %v", name, names(got))
		}
	}
	if u := by["caller"]; u.NameUnbound || u.Overrides || u.PartialParse {
		t.Errorf("caller should carry no signal, got %+v", u)
	}
	if !by["impl"].Overrides {
		t.Error("impl implements iface: Overrides should be set")
	}
	if !by["ghost"].NameUnbound {
		t.Error("an unresolved reference is spelled ghost: NameUnbound should be set")
	}
	if by["ghost"].FilePath != "b/b.go" || by["ghost"].Language != "go" {
		t.Errorf("ghost location = %s (%s), want b/b.go (go)", by["ghost"].FilePath, by["ghost"].Language)
	}
}

// Paging by id must visit every row exactly once whatever the page size.
func TestUnreferencedSymbolsPagesWithoutLossOrRepeat(t *testing.T) {
	r, runID, _ := structureFixture(t)
	ctx := context.Background()

	all, err := r.UnreferencedSymbols(ctx, runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	var paged []UnreferencedSymbol
	var after int64
	for {
		page, err := r.UnreferencedSymbols(ctx, runID, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		after = page[len(page)-1].ID
	}
	if len(paged) != len(all) {
		t.Fatalf("paged %d rows, one query returned %d", len(paged), len(all))
	}
	for i := range all {
		if paged[i].ID != all[i].ID {
			t.Errorf("row %d: paged id %d, full id %d", i, paged[i].ID, all[i].ID)
		}
	}
}

func TestUnreferencedSymbolsIgnoresOtherRuns(t *testing.T) {
	r, runID, _ := structureFixture(t)
	got, err := r.UnreferencedSymbols(context.Background(), runID+1000, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a run that does not exist returned %d rows", len(got))
	}
}

func TestFileStructureForCountsOnlyCrossFileDependents(t *testing.T) {
	r, runID, _ := structureFixture(t)

	got, err := r.FileStructureFor(context.Background(), runID, []string{"a/a.go", "b/b.go", "gone/deleted.go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["gone/deleted.go"]; ok {
		t.Error("a path the run does not hold must be absent, not zero-valued")
	}
	a, ok := got["a/a.go"]
	if !ok {
		t.Fatal("a/a.go missing")
	}
	if a.Symbols != 4 || a.SizeBytes != 10 || a.Language != "go" {
		t.Errorf("a/a.go = %+v, want 4 symbols, 10 bytes, go", a)
	}
	// caller->live and recursive->recursive start in a/a.go itself; only
	// ghost->live crosses a file boundary.
	if a.Dependents != 1 {
		t.Errorf("a/a.go dependents = %d, want 1 (same-file edges must not count)", a.Dependents)
	}
	if b := got["b/b.go"]; b.Dependents != 0 {
		t.Errorf("b/b.go dependents = %d, want 0", b.Dependents)
	}
}

// More paths than one IN list holds must still all be answered.
func TestFileStructureForChunksLargePathLists(t *testing.T) {
	r, runID, _ := structureFixture(t)

	paths := make([]string, 0, structureChunk*2+2)
	for i := 0; i < structureChunk*2; i++ {
		paths = append(paths, "missing/"+string(rune('a'+i%26))+".go")
	}
	paths = append(paths, "a/a.go", "b/b.go")

	got, err := r.FileStructureFor(context.Background(), runID, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d files, want the 2 that exist", len(got))
	}
}

// The dependents subquery must start from the file's own symbols and reach
// edges through idx_edges_dst. The planner's own choice read every edge in the
// run once per file, which made `repocortex hotspots` take minutes on a small
// repository. A plan regresses without any test of the RESULT noticing.
func TestFileStructureForUsesTheDestinationIndex(t *testing.T) {
	r, db := newTestRegistry(t)
	_ = r
	rows, err := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+fileStructureSQL(2), 1, "a.go", "b.go")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_edges_dst") {
		t.Errorf("edges are not reached through idx_edges_dst:\n%s", joined)
	}
	if !strings.Contains(joined, "idx_symbols_file") {
		t.Errorf("the file's symbols are not reached through idx_symbols_file:\n%s", joined)
	}
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN") {
			t.Errorf("the plan contains a full scan: %q\n%s", step, joined)
		}
	}
}

// Every probe in the unreferenced-definition query must go through the index
// written for it. Without statistics the planner otherwise reads every edge in
// the run once per candidate symbol, which the scale suite measured at 8.5
// seconds on a 5,000-symbol fixture.
func TestUnreferencedSymbolsUsesItsIndexes(t *testing.T) {
	_, db := newTestRegistry(t)
	args := []any{1, 0}
	for _, k := range unreferencedKinds {
		args = append(args, k)
	}
	args = append(args, 100)
	rows, err := db.QueryContext(context.Background(),
		"EXPLAIN QUERY PLAN "+unreferencedSQL(len(unreferencedKinds)), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	for _, index := range []string{"idx_edges_dst", "idx_edges_src", "idx_refs_run_name"} {
		if !strings.Contains(joined, index) {
			t.Errorf("the plan does not use %s:\n%s", index, joined)
		}
	}
	if strings.Contains(joined, "sqlite_autoindex_edges") {
		t.Errorf("the plan reads edges through the UNIQUE autoindex, which scans the whole run:\n%s", joined)
	}
}

func TestEdgeBetweenAndSymbolByID(t *testing.T) {
	r, runID, ids := structureFixture(t)
	ctx := context.Background()

	caller := ids[model.DefaultFQN("a/a.go", "caller")]
	live := ids[model.DefaultFQN("a/a.go", "live")]

	e, ok, err := r.EdgeBetween(ctx, runID, caller, live)
	if err != nil || !ok {
		t.Fatalf("EdgeBetween(caller, live) = ok %v, err %v", ok, err)
	}
	if e.Kind != string(model.EdgeCalls) || e.SrcName != "caller" || e.DstName != "live" {
		t.Errorf("edge = %+v", e)
	}
	if _, ok, err := r.EdgeBetween(ctx, runID, live, caller); err != nil || ok {
		t.Errorf("the reverse direction has no edge; got ok %v, err %v", ok, err)
	}

	s, ok, err := r.SymbolByID(ctx, runID, live)
	if err != nil || !ok {
		t.Fatalf("SymbolByID(live) = ok %v, err %v", ok, err)
	}
	if s.Name != "live" || s.FilePath != "a/a.go" {
		t.Errorf("symbol = %+v", s)
	}
	if _, ok, _ := r.SymbolByID(ctx, runID, 1<<40); ok {
		t.Error("an id the run does not hold must report not found")
	}
}

func TestIdentifierMentionsCountsWholeIdentifiersOnly(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	writeContent(t, r, run.ID, []fileSpec{
		{path: "a/def.go", lang: "go", hash: "m1",
			content: "// get_user loads one user.\nfunc get_user() {}\nfunc register() { table[\"x\"] = get_user }\n"},
		{path: "b/near.go", lang: "go", hash: "m2",
			// Longer identifiers and the split words must NOT count.
			content: "func get_user_by_id() {}\nvar forget_user = 1\n// get_user is mentioned here only in a comment\n"},
		{path: "c/other.go", lang: "go", hash: "m3", content: "handler := get_user\n"},
	}, nil)
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	got, ok, err := r.IdentifierMentions(ctx, run.ID, "get_user")
	if err != nil || !ok {
		t.Fatalf("IdentifierMentions: ok %v err %v", ok, err)
	}
	by := map[string][]int{}
	for _, m := range got {
		by[m.Path] = m.Lines
	}
	// a/def.go: line 1 is the doc comment (skipped), line 2 the definition,
	// line 3 the use as a value.
	if !reflect.DeepEqual(by["a/def.go"], []int{2, 3}) {
		t.Errorf("a/def.go lines = %v, want [2 3]: the comment line must not count", by["a/def.go"])
	}
	if !reflect.DeepEqual(by["c/other.go"], []int{1}) {
		t.Errorf("c/other.go lines = %v, want [1]", by["c/other.go"])
	}
	if lines, found := by["b/near.go"]; found {
		t.Errorf("b/near.go holds only longer identifiers and a comment, but reported %v", lines)
	}

	// A name with nothing the tokenizer can index is unknown, not unmentioned.
	if _, ok, err := r.IdentifierMentions(ctx, run.ID, "[]"); err != nil || ok {
		t.Errorf("an operator name should report ok=false, got ok %v err %v", ok, err)
	}
	if none, ok, _ := r.IdentifierMentions(ctx, run.ID, "neverWritten"); !ok || len(none) != 0 {
		t.Errorf("an absent name should report ok=true with no mentions, got %v %v", none, ok)
	}
}

func TestCommentOnly(t *testing.T) {
	for line, want := range map[string]bool{
		"// orphan does nothing":    true,
		"\t# orphan does nothing":   true,
		" * orphan does nothing":    true,
		"/* orphan */":              true,
		"-- orphan":                 true,
		"<!-- orphan -->":           true,
		"register(orphan) // a use": false, // code with a trailing comment
		"x := orphan":               false,
		"#include <orphan.h>":       false, // a directive, not a comment
		"#[derive(orphan)]":         false,
		"*orphan = 1":               false, // a dereference, not a comment
		"":                          false,
	} {
		if got := commentOnly(line); got != want {
			t.Errorf("commentOnly(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestCountIdentifier(t *testing.T) {
	for _, tc := range []struct {
		text, name string
		want       int
	}{
		{"save(x); s.save(y)", "save", 2},
		{"saveAll(x) unsave resave_", "save", 0},
		{"save", "save", 1},
		{"a.save", "save", 1},
		{"$save save$", "save", 0},
		{"", "save", 0},
		{"save", "", 0},
	} {
		if got := countIdentifier(tc.text, tc.name); got != tc.want {
			t.Errorf("countIdentifier(%q, %q) = %d, want %d", tc.text, tc.name, got, tc.want)
		}
	}
}

func names(us []UnreferencedSymbol) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Name
	}
	return out
}
