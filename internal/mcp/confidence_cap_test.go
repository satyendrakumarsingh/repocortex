package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newCappedTestServer is newTestServer with an injectable item cap.
//
// MaxItems is made small deliberately rather than generating thousands of
// rows: the bug this exercises is about ORDER between the registry's LIMIT
// and the MCP-layer confidence filter, and that ordering question is fully
// exercised by a handful of edges once the cap itself is small enough to bite.
func newCappedTestServer(t *testing.T, maxItems int, seed func(context.Context, *registry.Registry, int64)) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(ctx, reg, run.ID)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.ProjectName = "cappedproj"

	limits := DefaultLimits()
	limits.MaxItems = maxItems

	srv, err := New(Deps{Registry: reg, Config: cfg, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}

	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), serverT) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// seedManyLowConfidenceCallersOneHigh builds a target symbol with a flood of
// low-confidence CALLS edges and exactly one high-confidence CALLS edge, so
// low-confidence edges vastly outnumber the item cap.
func seedManyLowConfidenceCallersOneHigh(lowCount int) func(context.Context, *registry.Registry, int64) {
	return func(ctx context.Context, reg *registry.Registry, runID int64) {
		w, err := reg.NewWriter(ctx, runID)
		if err != nil {
			panic(err)
		}
		defer w.Close()

		tgtFile := model.File{Path: "svc/Target.java", Language: "java", ContentHash: "ht", ParseStatus: "ok"}
		tgtFileID, _ := w.AddFile(ctx, &tgtFile)
		target := model.Symbol{
			FileID: tgtFileID, Kind: model.KindMethod, Name: "process",
			FQN: "svc.Target#process", Exported: true,
			Range:      model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 10}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		targetID, _ := w.AddSymbol(ctx, &target)

		var edges []model.Edge
		for i := 0; i < lowCount; i++ {
			path := fmt.Sprintf("callers/Low%d.java", i)
			f := model.File{Path: path, Language: "java", ContentHash: fmt.Sprintf("hl%d", i), ParseStatus: "ok"}
			fID, _ := w.AddFile(ctx, &f)
			sym := model.Symbol{
				FileID: fID, Kind: model.KindMethod, Name: fmt.Sprintf("lowCaller%d", i),
				FQN:        fmt.Sprintf("callers.Low%d#lowCaller%d", i, i),
				Exported:   true,
				Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
				Confidence: model.ConfidenceLow, Source: model.SourceSourceCode,
			}
			symID, _ := w.AddSymbol(ctx, &sym)
			edges = append(edges, model.Edge{
				SrcID: symID, DstID: targetID, Kind: model.EdgeCalls,
				Confidence: model.ConfidenceLow,
				Evidence: model.Evidence{
					FilePath: path,
					Range:    model.Range{Start: model.Position{Line: 1}},
					Source:   model.SourceSourceCode,
					Detail:   "weak name match",
				},
			})
		}

		hiFile := model.File{Path: "callers/HighCaller.java", Language: "java", ContentHash: "hh", ParseStatus: "ok"}
		hiFileID, _ := w.AddFile(ctx, &hiFile)
		hiSym := model.Symbol{
			FileID: hiFileID, Kind: model.KindMethod, Name: "highCaller",
			FQN: "callers.HighCaller#highCaller", Exported: true,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		hiSymID, _ := w.AddSymbol(ctx, &hiSym)
		edges = append(edges, model.Edge{
			SrcID: hiSymID, DstID: targetID, Kind: model.EdgeCalls,
			Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{
				FilePath: hiFile.Path,
				Range:    model.Range{Start: model.Position{Line: 1}},
				Source:   model.SourceSourceCode,
				Detail:   "receiver type matches",
			},
		})

		_ = w.IndexFileText(ctx, tgtFileID, tgtFile.Path, "class Target { void process() {} }")
		_ = w.Flush(ctx)
		_ = w.Close()

		if _, err := reg.AddResolvedEdges(ctx, runID, edges); err != nil {
			panic(err)
		}
	}
}

// TestMinConfidenceSurvivesTheItemCap is the failing-test-first evidence for
// the bug fixed alongside it: min_confidence="high" must not silently lose a
// real high-confidence caller to truncation that happens before the
// confidence filter runs.
//
// The fixture stacks far more low-confidence edges than the item cap allows,
// plus exactly one high-confidence edge. If the cap is applied before the
// confidence filter (and the truncated set happens not to preserve the
// high-confidence edge), find_callers with min_confidence="high" returns
// nothing — while a real high-confidence caller exists.
func TestMinConfidenceSurvivesTheItemCap(t *testing.T) {
	const maxItems = 3
	const lowCount = 20

	sess := newCappedTestServer(t, maxItems, seedManyLowConfidenceCallersOneHigh(lowCount))
	text := resultText(callTool(t, sess, "find_callers", map[string]any{
		"name":           "process",
		"min_confidence": "high",
	}))

	if !strings.Contains(text, "highCaller") {
		t.Fatalf("min_confidence=high dropped the only high-confidence caller because low-confidence "+
			"edges exhausted the item cap before the confidence filter ran:\n%s", text)
	}

	// Negative assertions: an implementation that dropped the confidence
	// predicate entirely would still pass the positive check above, because
	// the pre-existing ORDER BY sorts the high-confidence edge first anyway.
	// These two checks are the ones that actually depend on the predicate
	// running: with it removed, low-confidence callers survive into the
	// result and the count exceeds one.
	if strings.Contains(text, "lowCaller") {
		t.Fatalf("min_confidence=high let a low-confidence caller through — the confidence "+
			"predicate did not run:\n%s", text)
	}
	if !strings.Contains(text, "1 caller(s)") {
		t.Fatalf("min_confidence=high should return exactly one caller (the high-confidence one), "+
			"got:\n%s", text)
	}
}

// seedSameConfidenceMixedKind builds a target symbol with a flood of
// high-confidence REFERENCES edges (a kind find_callers excludes) that all
// sort ahead of the one high-confidence CALLS edge alphabetically, plus that
// one CALLS edge itself.
func seedSameConfidenceMixedKind(refCount int) func(context.Context, *registry.Registry, int64) {
	return func(ctx context.Context, reg *registry.Registry, runID int64) {
		w, err := reg.NewWriter(ctx, runID)
		if err != nil {
			panic(err)
		}
		defer w.Close()

		tgtFile := model.File{Path: "svc/Target.java", Language: "java", ContentHash: "ht", ParseStatus: "ok"}
		tgtFileID, _ := w.AddFile(ctx, &tgtFile)
		target := model.Symbol{
			FileID: tgtFileID, Kind: model.KindMethod, Name: "process",
			FQN: "svc.Target#process", Exported: true,
			Range:      model.Range{Start: model.Position{Line: 5}, End: model.Position{Line: 10}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		targetID, _ := w.AddSymbol(ctx, &target)

		var edges []model.Edge
		for i := 0; i < refCount; i++ {
			path := fmt.Sprintf("aaa/Ref%02d.java", i)
			f := model.File{Path: path, Language: "java", ContentHash: fmt.Sprintf("hr%d", i), ParseStatus: "ok"}
			fID, _ := w.AddFile(ctx, &f)
			sym := model.Symbol{
				FileID: fID, Kind: model.KindMethod, Name: fmt.Sprintf("refFrom%d", i),
				FQN:        fmt.Sprintf("aaa.Ref%02d#refFrom%d", i, i),
				Exported:   true,
				Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
				Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
			}
			symID, _ := w.AddSymbol(ctx, &sym)
			edges = append(edges, model.Edge{
				SrcID: symID, DstID: targetID, Kind: model.EdgeReferences,
				Confidence: model.ConfidenceHigh,
				Evidence: model.Evidence{
					FilePath: path,
					Range:    model.Range{Start: model.Position{Line: 1}},
					Source:   model.SourceSourceCode,
					Detail:   "plain reference",
				},
			})
		}

		hiFile := model.File{Path: "zzz/HighCaller.java", Language: "java", ContentHash: "hh", ParseStatus: "ok"}
		hiFileID, _ := w.AddFile(ctx, &hiFile)
		hiSym := model.Symbol{
			FileID: hiFileID, Kind: model.KindMethod, Name: "highCaller",
			FQN: "zzz.HighCaller#highCaller", Exported: true,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 2}},
			Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
		}
		hiSymID, _ := w.AddSymbol(ctx, &hiSym)
		edges = append(edges, model.Edge{
			SrcID: hiSymID, DstID: targetID, Kind: model.EdgeCalls,
			Confidence: model.ConfidenceHigh,
			Evidence: model.Evidence{
				FilePath: hiFile.Path,
				Range:    model.Range{Start: model.Position{Line: 1}},
				Source:   model.SourceSourceCode,
				Detail:   "receiver type matches",
			},
		})

		_ = w.IndexFileText(ctx, tgtFileID, tgtFile.Path, "class Target { void process() {} }")
		_ = w.Flush(ctx)
		_ = w.Close()

		if _, err := reg.AddResolvedEdges(ctx, runID, edges); err != nil {
			panic(err)
		}
	}
}

// TestFindCallersSurvivesKindCrowding is the dominant real-world instance of
// the truncate-before-filter bug: it does not even need low-confidence edges.
// find_callers restricts to CALLS after the registry query, and the query's
// LIMIT is ordered by confidence only, never by kind — so a symbol with many
// same-or-higher-confidence REFERENCES edges (imports, type usages) that
// merely sort ahead of its one CALLS edge could have that CALLS edge crowded
// out of the capped fetch entirely, before find_callers's own kind filter
// ever got a chance to keep it. A caller asking find_callers with
// min_confidence="high" must still see a real high-confidence caller in that
// situation.
func TestFindCallersSurvivesKindCrowding(t *testing.T) {
	const maxItems = 3
	const refCount = 20

	sess := newCappedTestServer(t, maxItems, seedSameConfidenceMixedKind(refCount))
	text := resultText(callTool(t, sess, "find_callers", map[string]any{
		"name":           "process",
		"min_confidence": "high",
	}))

	if !strings.Contains(text, "highCaller") {
		t.Fatalf("min_confidence=high dropped the only CALLS caller because same-confidence "+
			"REFERENCES edges, which sort ahead of it but are not calls, exhausted the item cap "+
			"before find_callers's kind filter ran:\n%s", text)
	}
}
