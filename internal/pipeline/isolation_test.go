package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestOneUnpersistableFileDoesNotAbortTheRun is the guarantee that makes this
// tool survive languages and projects nobody tested it against.
//
// Parser failures were already isolated per file, as the Parser contract
// requires. PERSISTENCE failures were not: a statement the schema rejected
// latched an error on the writer, poisoned the batch and aborted the whole
// index. On a real 8,094-file repository, one line of one TypeScript test —
// a function and a same-named method signature on the same line — left
// NOTHING committed.
//
// The specific collision that caused it is prevented now. This test is about
// the next one, which will come from a language or a construct nobody has
// looked at yet: whatever it is, it must cost one file and be reported, not
// the repository.
//
// The bad file here writes two symbols that are identical in every column the
// UNIQUE constraint covers, which is the shape the schema genuinely rejects
// rather than a mock of one.
func TestOneUnpersistableFileDoesNotAbortTheRun(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "isolation")
	if err != nil {
		t.Fatal(err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewRegistrySink(w)

	good := func(path string) FileFacts {
		return FileFacts{
			File: model.File{Path: path, Language: "go", ContentHash: path,
				SizeBytes: 10, ParseStatus: "ok"},
			Symbols: []model.Symbol{{
				Kind: model.KindFunction, Name: "Ok", FQN: path + ":Ok",
				Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
			}},
		}
	}
	// A symbol the registry refuses: exact confidence without a compiler
	// behind it. Chosen because it is rejected by an invariant this project
	// enforces deliberately, so the test exercises a real refusal rather than
	// a mocked one — and unlike the duplicate-declaration case, nothing
	// upstream quietly repairs it first.
	bad := FileFacts{
		File: model.File{Path: "bad.go", Language: "go", ContentHash: "bad",
			SizeBytes: 10, ParseStatus: "ok"},
		Symbols: []model.Symbol{
			{Kind: model.KindFunction, Name: "Ok", FQN: "bad.go:Ok",
				Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode},
			{Kind: model.KindFunction, Name: "Bogus", FQN: "bad.go:Bogus",
				Range:      model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 6}},
				Confidence: model.ConfidenceExact, Source: model.SourceSourceCode},
		},
	}

	for _, facts := range []FileFacts{good("a.go"), bad, good("b.go"), good("c.go")} {
		if err := sink.WriteFile(ctx, facts); err != nil {
			t.Fatalf("WriteFile(%s) aborted the run: %v", facts.File.Path, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Every good file survived.
	for _, path := range []string{"a.go", "b.go", "c.go"} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.path = ?`,
			path).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s has %d symbols, want 1: a later file lost rows to an earlier failure", path, n)
		}
	}

	// The bad file is present, marked failed, with a reason — not vanished.
	var status, reason string
	if err := db.QueryRowContext(ctx,
		`SELECT parse_status, COALESCE(parse_error,'') FROM files WHERE path = 'bad.go'`).
		Scan(&status, &reason); err != nil {
		t.Fatalf("bad.go is not recorded at all; a file whose facts could not be "+
			"stored must still be visible as a gap: %v", err)
	}
	if status != "failed" {
		t.Errorf("bad.go parse_status = %q, want failed", status)
	}
	if reason == "" {
		t.Error("bad.go has no parse_error; the gap is visible but unexplained")
	}

	// And none of its symbols leaked through.
	var leaked int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM symbols s JOIN files f ON f.id = s.file_id WHERE f.path = 'bad.go'`).
		Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Errorf("bad.go left %d symbols behind; the savepoint did not undo them", leaked)
	}

	if n, last := sink.UnpersistableFiles(); n != 1 || !strings.Contains(last, "bad.go") {
		t.Errorf("UnpersistableFiles() = %d, %q; want 1 naming bad.go", n, last)
	}
}
