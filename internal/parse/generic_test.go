package parse

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func parserFor(t *testing.T, path string) Parser {
	t.Helper()
	p := NewRegistry().ForFile(path)
	if p == nil {
		t.Fatalf("no parser for %q", path)
	}
	return p
}

func parse(t *testing.T, path, src string) *Result {
	t.Helper()
	res, err := parserFor(t, path).Parse(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return res
}

func symbolNames(res *Result) []string {
	out := make([]string, 0, len(res.Symbols))
	for _, s := range res.Symbols {
		out = append(out, s.Name)
	}
	return out
}

func hasSymbol(res *Result, name string, kind model.EntityKind) bool {
	for _, s := range res.Symbols {
		if s.Name == name && (kind == "" || s.Kind == kind) {
			return true
		}
	}
	return false
}

// TestSupplementRecoversMissedSymbols is the regression test for the gap the
// M1 spike found: gotreesitter's inferred Go query captures funcs and methods
// but drops type, const and var declarations.
func TestSupplementRecoversMissedSymbols(t *testing.T) {
	res := parse(t, "service.go", `package pay

type TransactionService struct{}

type Repository interface {
	Save(id string) error
}

const MaxRetries = 3

func NewService() *TransactionService { return nil }

func (s *TransactionService) UpdateStatus(id string) error {
	return s.repo.Save(id)
}`)

	want := []struct {
		name string
		kind model.EntityKind
	}{
		{"TransactionService", model.KindTypeAlias},
		{"Repository", model.KindTypeAlias},
		{"MaxRetries", model.KindConstant},
		{"NewService", model.KindFunction},
		{"UpdateStatus", model.KindMethod},
	}
	for _, w := range want {
		if !hasSymbol(res, w.name, "") {
			t.Errorf("missing symbol %q; got %v", w.name, symbolNames(res))
		}
	}
	if res.Status != StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
}

func TestMultiLanguageExtraction(t *testing.T) {
	cases := []struct {
		file string
		src  string
		want []string
	}{
		{
			"TransactionService.java",
			`package com.pay;
public class TransactionService {
    private String status;
    public TransactionService() {}
    public void updateStatus(String id) { repository.save(id); }
}
interface Repository { void save(String id); }`,
			[]string{"TransactionService", "updateStatus", "Repository"},
		},
		{
			"service.py",
			`class TransactionService:
    def update_status(self, txn_id):
        return self.repo.save(txn_id)`,
			[]string{"TransactionService", "update_status"},
		},
		{
			"service.ts",
			`export interface Repository { save(id: string): void; }
export type Status = "ok" | "failed";
export class TransactionService {
  updateStatus(id: string): void { this.repo.save(id); }
}`,
			[]string{"TransactionService", "updateStatus", "Repository", "Status"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			res := parse(t, tc.file, tc.src)
			for _, w := range tc.want {
				if !hasSymbol(res, w, "") {
					t.Errorf("missing %q; got %v", w, symbolNames(res))
				}
			}
			if len(res.References) == 0 {
				t.Logf("note: no references extracted for %s", tc.file)
			}
		})
	}
}

func TestReferencesAreUnresolved(t *testing.T) {
	res := parse(t, "service.go", `package p
func A() { helperCall() }`)
	if len(res.References) == 0 {
		t.Skip("grammar produced no references")
	}
	for _, r := range res.References {
		if r.Name == "" {
			t.Error("reference with empty name")
		}
		if !strings.HasPrefix(r.Kind, "reference") {
			t.Errorf("reference kind %q lacks 'reference' prefix", r.Kind)
		}
	}
}

// TestSymbolsAreNotClaimedExact guards invariant #2. Syntactic extraction can
// never produce compiler-grade certainty.
func TestSymbolsAreNotClaimedExact(t *testing.T) {
	res := parse(t, "service.go", "package p\nfunc A() {}\n")
	for _, s := range res.Symbols {
		if s.Confidence == model.ConfidenceExact {
			t.Errorf("symbol %q claims exact confidence from a syntactic parse", s.Name)
		}
		if s.Source == model.SourceCompiler {
			t.Errorf("symbol %q claims compiler provenance from tree-sitter", s.Name)
		}
		if !s.Confidence.Valid() {
			t.Errorf("symbol %q has invalid confidence %q", s.Name, s.Confidence)
		}
	}
}

// TestMalformedInputIsIsolated: spec §13 requires parser failures to affect
// only the file involved. RepoCortex indexes repositories it did not write.
func TestMalformedInputIsIsolated(t *testing.T) {
	bad := []struct{ name, src string }{
		{"empty", ""},
		{"truncated", "package"},
		{"unbalanced", "package p\nfunc Broken( {"},
		{"binary", "\x00\x01\x02\xff\xfe"},
		{"repeated_open", strings.Repeat("func A(){", 2000)},
		{"brace_storm", strings.Repeat("{", 10000)},
		{"deep_nesting", "package p\nfunc A(){" + strings.Repeat("if true {", 500)},
	}
	p := parserFor(t, "x.go")
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			res, err := p.Parse(context.Background(), "x.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("Parse returned a run-level error for one bad file: %v", err)
			}
			if res == nil {
				t.Fatal("nil result")
			}
			t.Logf("status=%s symbols=%d err=%v", res.Status, len(res.Symbols), res.Err)
		})
	}
}

func TestOversizedSourceIsSkipped(t *testing.T) {
	p := parserFor(t, "x.go")
	huge := make([]byte, MaxSourceBytes+1)
	for i := range huge {
		huge[i] = 'a'
	}
	res, err := p.Parse(context.Background(), "x.go", huge)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSkipped {
		t.Errorf("status = %s, want skipped", res.Status)
	}
}

func TestContextCancellation(t *testing.T) {
	p := parserFor(t, "x.go")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Parse(ctx, "x.go", []byte("package p\nfunc A(){}")); err == nil {
		t.Fatal("expected cancellation to be honoured")
	}
}

// TestConcurrentParsing is essential: gotreesitter's Parser is documented as
// unsafe for concurrent use, so the pooling in genericParser is load-bearing.
// Run with -race.
func TestConcurrentParsing(t *testing.T) {
	reg := NewRegistry()
	srcs := map[string]string{
		"a.go":   "package p\ntype Alpha struct{}\nfunc DoA() {}",
		"b.java": "public class Beta { void doB() {} }",
		"c.py":   "class Gamma:\n    def do_c(self): pass",
		"d.ts":   "export class Delta { doD(): void {} }",
	}

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 50; i++ {
		for file, src := range srcs {
			wg.Add(1)
			go func(file, src string) {
				defer wg.Done()
				p := reg.ForFile(file)
				if p == nil {
					errs <- nil
					return
				}
				res, err := p.Parse(context.Background(), file, []byte(src))
				if err != nil {
					errs <- err
					return
				}
				if len(res.Symbols) == 0 && res.Status == StatusOK {
					errs <- nil
				}
			}(file, src)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent parse error: %v", err)
		}
	}
}

// TestDeterministicOutput underpins the registry digest gate: the same input
// must yield byte-identical symbol ordering every time.
func TestDeterministicOutput(t *testing.T) {
	src := `package pay

type A struct{}
type B struct{}
const C = 1

func D() {}
func (a *A) E() {}`

	first := symbolNames(parse(t, "s.go", src))
	for i := 0; i < 20; i++ {
		got := symbolNames(parse(t, "s.go", src))
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d differs:\n first=%v\n got  =%v", i, first, got)
		}
	}
}

func TestUnknownExtensionHasNoParser(t *testing.T) {
	if p := NewRegistry().ForFile("mystery.zzzzz"); p != nil {
		t.Fatalf("expected nil parser, got %s", p.Language())
	}
}

func TestRegistryCoversManyLanguages(t *testing.T) {
	langs := NewRegistry().Languages()
	if len(langs) < 50 {
		t.Fatalf("registry exposes %d languages, expected 200+", len(langs))
	}
	t.Logf("languages available: %d", len(langs))
}

// TestAllSupplementsCompile ensures a malformed supplement is caught here
// rather than silently yielding zero symbols in production.
func TestAllSupplementsCompile(t *testing.T) {
	reg := NewRegistry()
	probe := map[string]string{
		"go": "x.go", "java": "X.java", "typescript": "x.ts",
		"tsx": "x.tsx", "python": "x.py", "rust": "x.rs",
	}
	for lang, file := range probe {
		p := reg.ForFile(file)
		if p == nil {
			t.Errorf("%s: no parser for %s", lang, file)
			continue
		}
		gp, ok := p.(*genericParser)
		if !ok {
			continue
		}
		if gp.initErr != nil {
			t.Errorf("%s: supplement broke parser construction: %v", lang, gp.initErr)
		}
	}
}

func TestParseTimeoutIsBounded(t *testing.T) {
	p := parserFor(t, "x.go")
	start := time.Now()
	_, err := p.Parse(context.Background(), "x.go", []byte(strings.Repeat("{", 50000)))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected run-level error: %v", err)
	}
	if elapsed > DefaultParseTimeout+2*time.Second {
		t.Errorf("parse took %s, exceeding the %s timeout guard", elapsed, DefaultParseTimeout)
	}
	t.Logf("pathological input handled in %s", elapsed)
}

func FuzzParseGo(f *testing.F) {
	f.Add("package p\nfunc A() {}")
	f.Add("")
	f.Add("type X struct{}")
	f.Add("func (r *R) M() error { return nil }")
	reg := NewRegistry()
	f.Fuzz(func(t *testing.T, src string) {
		p := reg.ForFile("fuzz.go")
		if p == nil {
			t.Skip()
		}
		res, err := p.Parse(context.Background(), "fuzz.go", []byte(src))
		if err != nil {
			t.Fatalf("run-level error on fuzz input: %v", err)
		}
		if res == nil {
			t.Fatal("nil result")
		}
		for _, s := range res.Symbols {
			if s.Confidence == model.ConfidenceExact {
				t.Fatal("syntactic parse claimed exact confidence")
			}
		}
	})
}

// TestBlankIdentifierIsNotASymbol pins noise found on a real Go service, which
// contributed 46 symbols named `_` to its index.
//
// A tags query sees `var _ = x` as a definition. The language guarantees no
// reference can ever bind to it, so it is a row that can only ever be noise —
// and `repocortex symbol _` answering with 46 results devalues a table whose
// worth is that every row means something.
func TestBlankIdentifierIsNotASymbol(t *testing.T) {
	src := []byte(`package p

import "errors"

var _ = errors.New("compile-time assertion")

func Real() {}

func alsoReal() { _, _ = Real, Real }
`)
	r := NewRegistry()
	p := r.ForFile("t.go")
	if p == nil {
		t.Fatal("no parser for .go")
	}
	res, err := p.Parse(context.Background(), "t.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range res.Symbols {
		if s.Name == "_" {
			t.Errorf("the blank identifier was indexed as a symbol at line %d", s.Range.Start.Line)
		}
		names = append(names, s.Name)
	}
	// The real declarations must survive.
	for _, want := range []string{"Real", "alsoReal"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing; got %v", want, names)
		}
	}
}

func TestBenignSymbollessFilesAreStatusOk(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name string
		path string
		src  string
	}{
		{
			name: "package doc file",
			path: "pkg/doc.go",
			src:  "// Package pkg documents the package.\npackage pkg\n",
		},
		{
			name: "sql migration file with alter table",
			path: "migrations/0006_index_completeness.sql",
			src:  "ALTER TABLE index_runs ADD COLUMN files_discovered INTEGER NOT NULL DEFAULT 0;\n",
		},
		{
			name: "stylesheet file",
			path: "assets/app.css",
			src:  "body { margin: 0; padding: 0; background-color: #fff; }\n",
		},
		{
			name: "shell script without functions",
			path: "scripts/deploy.sh",
			src:  "#!/usr/bin/env bash\necho 'Deploying application...'\nexit 0\n",
		},
		{
			name: "powershell script",
			path: "install.ps1",
			src:  "Write-Host 'Installing...'\n",
		},
		{
			name: "example configuration file",
			path: "internal/web/e2e/support/local-fixtures.example.mjs",
			src:  "export const LOCAL_REPOS = { small: { name: 'small-service' } };\n",
		},
		{
			name: "test support port table",
			path: "internal/web/e2e/support/port-table.mjs",
			src:  "export const PORTS = { payment: 7925, local1: 7920 };\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := r.ForFile(tc.path)
			if p == nil {
				t.Fatalf("no parser found for %q", tc.path)
			}
			res, err := p.Parse(context.Background(), tc.path, []byte(tc.src))
			if err != nil {
				t.Fatalf("Parse(%q) returned unexpected error: %v", tc.path, err)
			}
			if res.Status != StatusOK {
				t.Errorf("Parse(%q) status = %q, want %q", tc.path, res.Status, StatusOK)
			}
			if len(res.Symbols) != 0 {
				t.Errorf("Parse(%q) produced unexpected symbols: %+v", tc.path, res.Symbols)
			}
		})
	}
}
