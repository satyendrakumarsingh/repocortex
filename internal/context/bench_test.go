package context

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// syntheticRepo builds a registry the size of a real service so the latency
// target is measured against something, not asserted.
//
// Shape matters as much as size. The names are drawn from a small vocabulary
// so that a task's terms hit many symbols — the expensive case — and the edges
// form a layered call graph (controller -> service -> repository) with
// cross-file fan-out, which is what makes the traversal do work rather than
// terminate after one level.
func syntheticRepo(tb testing.TB, classes int) (*registry.Registry, *graph.Graph, int64) {
	tb.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		tb.Fatalf("begin run: %v", err)
	}
	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		tb.Fatalf("writer: %v", err)
	}

	layers := []string{"controller", "service", "repository", "notification", "marketing"}
	nouns := []string{"Callback", "Payment", "Transaction", "Settlement", "Refund",
		"Ledger", "Merchant", "Payout", "Dispute", "Invoice"}
	verbs := []string{"handle", "process", "update", "find", "save", "send", "build",
		"validate", "render", "export"}

	const methodsPerClass = 19 // + the class itself = 20 symbols per file

	// methodIDs[i] holds one file's method symbol ids, for wiring edges after
	// every symbol exists.
	methodIDs := make([][]int64, 0, classes)

	for i := 0; i < classes; i++ {
		layer := layers[i%len(layers)]
		noun := nouns[(i/len(layers))%len(nouns)]
		class := fmt.Sprintf("%s%s%d", noun, title(layer), i)
		path := fmt.Sprintf("src/main/java/com/pay/%s/%s.java", layer, class)

		body := fmt.Sprintf("package com.pay.%s;\npublic class %s {\n", layer, class)
		for m := 0; m < methodsPerClass; m++ {
			body += fmt.Sprintf("  public void %s%s%d() { repository.save(txn); }\n",
				verbs[m%len(verbs)], noun, m)
		}
		body += fmt.Sprintf("  // SELECT id FROM %s_records WHERE id = ?\n}\n", layer)

		file := model.File{
			Path: path, Language: "java", ContentHash: fmt.Sprintf("h%d", i),
			SizeBytes: int64(len(body)), ModTimeUnix: int64(1_700_000_000 + i), ParseStatus: "ok",
		}
		fileID, err := w.AddFile(ctx, &file)
		if err != nil {
			tb.Fatalf("add file: %v", err)
		}
		if err := w.IndexFileText(ctx, fileID, path, body); err != nil {
			tb.Fatalf("index text: %v", err)
		}

		classSym := model.Symbol{
			Kind: model.KindClass, Name: class, FQN: path + ":" + class, FileID: fileID,
			Range:      model.Range{Start: model.Position{Line: 2}, End: model.Position{Line: methodsPerClass + 3}},
			Exported:   true,
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		if _, err := w.AddSymbol(ctx, &classSym); err != nil {
			tb.Fatalf("add class: %v", err)
		}

		ids := make([]int64, 0, methodsPerClass)
		for m := 0; m < methodsPerClass; m++ {
			name := fmt.Sprintf("%s%s%d", verbs[m%len(verbs)], noun, m)
			sym := model.Symbol{
				Kind: model.KindMethod, Name: name, FQN: path + ":" + name, FileID: fileID,
				Range:      model.Range{Start: model.Position{Line: 3 + m}, End: model.Position{Line: 3 + m}},
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
			}
			id, err := w.AddSymbol(ctx, &sym)
			if err != nil {
				tb.Fatalf("add symbol: %v", err)
			}
			ids = append(ids, id)
		}
		methodIDs = append(methodIDs, ids)
	}
	if err := w.Flush(ctx); err != nil {
		tb.Fatalf("flush symbols: %v", err)
	}

	// Layered edges: every method calls two methods in the next file along,
	// which gives a wide, deep graph rather than a set of isolated stars.
	for i, ids := range methodIDs {
		next := methodIDs[(i+1)%len(methodIDs)]
		skip := methodIDs[(i+7)%len(methodIDs)]
		for m, src := range ids {
			for _, dst := range []int64{next[m%len(next)], skip[(m+3)%len(skip)]} {
				edge := model.Edge{
					SrcID: src, DstID: dst, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh,
					Evidence: model.Evidence{
						FilePath: fmt.Sprintf("f%d", i),
						Range:    model.Range{Start: model.Position{Line: m + 1}},
						Source:   model.SourceSourceCode,
					},
				}
				if _, err := w.AddEdge(ctx, &edge); err != nil {
					tb.Fatalf("add edge: %v", err)
				}
			}
		}
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("close writer: %v", err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	return reg, graph.New(db), run.ID
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// TestSyntheticP95Latency is the spec's performance gate, measured rather than
// asserted. It reports the distribution so a regression shows up as a number in
// the log, not just a pass or a fail.
func TestSyntheticP95Latency(t *testing.T) {
	skipUnlessTimingIsMeaningful(t)
	reg, g, runID := syntheticRepo(t, 500) // 500 classes x 20 symbols = 10,000
	reportLatency(t, reg, g, runID, "synthetic 10k symbols")
}

func TestPaymentFixtureP95Latency(t *testing.T) {
	skipUnlessTimingIsMeaningful(t)
	reg, g, runID := paymentFixture(t)
	reportLatency(t, reg, g, runID, "payment fixture")
}

// skipUnlessTimingIsMeaningful guards the latency gate. It runs before the
// fixture is built, because constructing a 10,000-symbol registry only to skip
// costs half a minute on every -race run.
func skipUnlessTimingIsMeaningful(t *testing.T) {
	t.Helper()
	if raceEnabled {
		t.Skip("timing under -race measures the race detector, not the engine; see race_off_test.go")
	}
	if testing.Short() {
		t.Skip("latency measurement is slow by design")
	}
}

func reportLatency(t *testing.T, reg *registry.Registry, g *graph.Graph, runID int64, label string) {
	t.Helper()
	ctx := context.Background()

	tasks := []string{
		goldenTask,
		"make the transaction service idempotent when saving",
		"why does the payment callback notify twice",
		"add a retry to the notification send path",
	}

	const runs = 40
	samples := make([]time.Duration, 0, runs*len(tasks))
	for i := 0; i < runs; i++ {
		for _, task := range tasks {
			start := time.Now()
			if _, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: task}); err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			samples = append(samples, time.Since(start))
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	p50 := samples[len(samples)*50/100]
	p95 := samples[len(samples)*95/100]
	p99 := samples[len(samples)*99/100]
	t.Logf("%s: n=%d p50=%v p95=%v p99=%v max=%v",
		label, len(samples), p50, p95, p99, samples[len(samples)-1])

	// Spec target. Deliberately checked at p95 rather than at the mean: an
	// agent waiting on the slow tail is what a user actually experiences.
	if p95 > 2*time.Second {
		t.Errorf("%s: p95 = %v, want <= 2s", label, p95)
	}
}

func BenchmarkAssemblePaymentFixture(b *testing.B) {
	reg, g, runID := paymentFixture(b)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAssembleSynthetic10k(b *testing.B) {
	reg, g, runID := syntheticRepo(b, 500)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewAssembler().Assemble(ctx, reg, g, runID, Request{Task: goldenTask}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoadCatalogue isolates the two full-table reads, which are the fixed
// cost every request pays and the first thing to look at if latency regresses.
func BenchmarkLoadCatalogue(b *testing.B) {
	reg, _, runID := syntheticRepo(b, 500)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := loadCatalogue(ctx, reg, runID); err != nil {
			b.Fatal(err)
		}
	}
}
