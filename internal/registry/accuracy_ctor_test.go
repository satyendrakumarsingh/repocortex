package registry

import (
	"context"
	"fmt"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// seedConstructions builds a C++ run in which the compiler resolved four
// construction sites to the constructor of a class, and the resolver answered
// each in a different way:
//
//	10  INSTANTIATES to the class that owns the constructor          same answer
//	11  INSTANTIATES to a class of the same name in another header   wrong
//	12  CALLS to the class (an edge kind that is not a construction) wrong
//	13  INSTANTIATES to `ns::Foo`, another class of that name         wrong
//
// The compiler's target is `Foo.Foo`, a method named after its class, as
// scip-clang records a constructor.
func seedConstructions(t *testing.T) *Registry {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stmts := []string{
		`INSERT INTO index_runs(id, status, committed, started_unix) VALUES(1,'ready',1,0)`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash) VALUES(1,1,'src/use.cpp','cpp','h1')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash) VALUES(2,1,'src/foo.hpp','cpp','h2')`,
		`INSERT INTO files(id, index_run_id, path, language, content_hash) VALUES(3,1,'other/foo.hpp','cpp','h3')`,
	}
	for _, s := range []struct {
		id, file        int
		kind, name, fqn string
	}{
		{1, 1, "function", "use", "src/use.cpp:use"},
		{2, 2, "class", "Foo", "src/foo.hpp:Foo"},
		{3, 2, "function", "Foo", "src/foo.hpp:Foo.Foo"},
		{4, 3, "class", "Foo", "other/foo.hpp:Foo"},
		{5, 2, "class", "Foo", "src/foo.hpp:ns.Foo"},
	} {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO symbols(id, index_run_id, file_id, kind, name, fqn, confidence, source)
			   VALUES(%d,1,%d,'%s','%s','%s','high','source_code')`, s.id, s.file, s.kind, s.name, s.fqn))
	}
	for _, line := range []int{10, 11, 12, 13} {
		name := "Foo"
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO symbol_references(index_run_id, file_id, name, kind, start_line, resolution)
			   VALUES(1,1,'%s','reference.call.new',%d,'resolved')`, name, line))
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, evidence_file, evidence_line, source)
			   VALUES(1,1,3,'REFERENCES','exact','src/use.cpp',%d,'compiler')`, line))
	}
	for _, e := range []struct {
		dst, line int
		kind      string
	}{
		{2, 10, "INSTANTIATES"},
		{4, 11, "INSTANTIATES"},
		{2, 12, "CALLS"},
		{5, 13, "INSTANTIATES"},
	} {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO edges(index_run_id, src_id, dst_id, kind, confidence, evidence_file, evidence_line, source)
			   VALUES(1,1,%d,'%s','high','src/use.cpp',%d,'source_code')`, e.dst, e.kind, e.line))
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed constructions: %v\n%s", err, s)
		}
	}
	return New(db)
}

// TestAccuracyAdjudicationAcceptsTheConstructorForItsType pins which answers
// the adjudication takes for a construction, so that the choice to bind
// INSTANTIATES to the type (docs/accuracy.md) is not scored as an error where a
// compiler index reports the constructor.
//
// Of the four sites only the first is the same answer: line 11 names another
// class of the same name, 12 is a CALLS edge, and 13 is another class. The
// test fails if the rule accepts any of them, as much as if it rejects line 10.
func TestAccuracyAdjudicationAcceptsTheConstructorForItsType(t *testing.T) {
	reg := seedConstructions(t)
	adj, err := reg.AccuracyAdjudication(context.Background(), 1)
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	var right, wrong int64
	for _, o := range adj.Outcomes {
		if o.Tier != "high" {
			continue
		}
		if o.Correct {
			right += o.Count
		} else {
			wrong += o.Count
		}
	}
	if right != 1 || wrong != 3 {
		t.Errorf("high: %d right and %d wrong, want 1 right (line 10) and 3 wrong (lines 11, 12, 13); outcomes %+v",
			right, wrong, adj.Outcomes)
	}
}
