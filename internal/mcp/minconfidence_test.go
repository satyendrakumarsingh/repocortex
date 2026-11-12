package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// minConfidenceSurface is the intended answer to "which tools take a
// confidence floor", written down once.
//
// A tool belongs here when its ITEMS carry a confidence tier. A floor on a
// tool whose results carry no confidence is a false affordance: the agent
// passes it, nothing happens, and it has been told the answer was filtered
// when it was not. The skipped tools each say why below.
var minConfidenceSurface = map[string]bool{
	// Confidence-bearing: every item is a definition or a relationship with a
	// tier attached, so a floor selects a real subset.
	"find_symbol":      true,
	"get_symbol":       true,
	"find_references":  true,
	"find_callers":     true,
	"plan_rename":      true,
	"get_task_context": true,
	"find_tests":       true,
	"analyze_impact":   true,
	"plan_change":      true,

	// Not confidence-bearing:
	//   get_project_overview / get_resolution_coverage — counts and rates about
	//     the index itself, not facts about code that could be graded.
	//   search_code — full-text matches. A hit is textual and exact by
	//     construction; there is no inferred binding to grade.
	//   list_api_endpoints — routes read from a framework's own syntax; the
	//     tool reports them or does not.
	//   list_db_tables — per-table read/write counts. Its uncertainty is
	//     already reported as the run-time-assembled statement count, which is
	//     a different axis from binding confidence.
	//   get_config_key — a declaration site and a reader count; no tier.
	"get_project_overview":    false,
	"get_resolution_coverage": false,
	"search_code":             false,
	"list_api_endpoints":      false,
	"list_db_tables":          false,
	"get_config_key":          false,
	"get_file_outline":        false,
	"minimize_diff":           false,
	"list_blindspots":         false,
}

// minimalArgsFor is the smallest valid argument set for each tool, so a call
// exercises the parameter rather than the empty-argument guard.
var minimalArgsFor = map[string]map[string]any{
	"get_project_overview":    {},
	"search_code":             {"query": "Login"},
	"find_symbol":             {"name": "Login"},
	"get_symbol":              {"name": "Login"},
	"find_references":         {"name": "Login"},
	"find_callers":            {"name": "Login"},
	"plan_rename":             {"name": "Login", "new_name": "SignIn"},
	"list_api_endpoints":      {},
	"list_db_tables":          {},
	"get_config_key":          {"key": "DATABASE_URL"},
	"get_resolution_coverage": {},
	"get_file_outline":        {"path": "pkg/model/symbol.go"},
	"minimize_diff":           {"diff": "--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n-old\n+new\n"},
	"get_task_context":        {"task": "change the login flow"},
	// Fully qualified: two definitions share the bare name in this
	// fixture, and the capability tools refuse to guess between them.
	"analyze_impact":  {"symbol": "auth.Login"},
	"find_tests":      {"symbol": "auth.Login"},
	"plan_change":     {"symbol": "auth.Login"},
	"list_blindspots": {},
}

func argsFor(t *testing.T, name string, extra map[string]any) map[string]any {
	t.Helper()
	base, ok := minimalArgsFor[name]
	if !ok {
		t.Fatalf("no minimal arguments recorded for tool %q — add them", name)
	}
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// advertisedMinConfidence reports, per tool, whether the JSON Schema an agent
// actually receives declares min_confidence.
func advertisedMinConfidence(t *testing.T, sess *sdk.ClientSession) map[string]bool {
	t.Helper()
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("the test server registered no tools; this test would pass vacuously")
	}
	out := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatalf("unmarshal %s schema: %v", tool.Name, err)
		}
		_, declared := schema.Properties["min_confidence"]
		out[tool.Name] = declared
	}
	return out
}

// TestMinConfidenceIsAdvertisedExactlyWhereItIsAccepted checks BOTH directions:
// that minConfidenceSurface accounts for every registered tool (via the
// t.Fatalf below), and that the schema's declaration of min_confidence matches
// the handler ACTUALLY DECODING AND VALIDATING it — not merely matches the
// handler tolerating the field's presence in the JSON.
//
// That distinction is the whole test. "high" succeeding only proves the JSON
// bound into the Go struct without error — reflection-based (de)serialization
// happens whether or not any handler code ever reads the field afterward.
// get_symbol shared symbolRefArgs with find_references and was measured
// exactly this way once: min_confidence was schema-advertised, "high" decoded
// cleanly, and the handler still ignored it and returned unfiltered results,
// because "advertised" and the old "accepted" (err == nil) were both read off
// the same generated schema rather than off two independent mechanisms — a
// mutation test confirmed the ORIGINAL two-way check here still PASSED with
// that bug reintroduced.
//
// "pretty_sure" is the independent second signal: it is exactly as
// decode-valid as "high" (min_confidence is a bare string in the schema), so
// it reaches the handler whenever "high" does. It is only REJECTED, and only
// with parseMinConfidence's "unknown confidence" wording, if the handler
// actually calls parseMinConfidence on the decoded value — which is the one
// thing a hardcoded ConfidenceLow skips. A tool that ignores the parameter
// either accepts "pretty_sure" outright or fails it with the SDK's schema-
// validation wording ("unexpected additional properties"), never with
// parseMinConfidence's, so the message content — not just error-or-not — is
// what this asserts.
func TestMinConfidenceIsAdvertisedExactlyWhereItIsAccepted(t *testing.T) {
	sess := newGraphTestServer(t, seedMixedConfidence)
	advertised := advertisedMinConfidence(t, sess)

	var names []string
	for name := range advertised {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			want, listed := minConfidenceSurface[name]
			if !listed {
				t.Fatalf("tool %q is registered but minConfidenceSurface does not decide it "+
					"— a new tool must state whether a confidence floor means anything for it", name)
			}
			if advertised[name] != want {
				t.Errorf("advertised min_confidence = %v, want %v", advertised[name], want)
			}

			res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
				Name: name, Arguments: argsFor(t, name, map[string]any{"min_confidence": "high"}),
			})
			accepted := err == nil && !res.IsError
			if accepted != want {
				detail := ""
				if err != nil {
					detail = err.Error()
				} else if res.IsError {
					detail = resultText(res)
				}
				t.Errorf("accepted min_confidence = %v, want %v (%s)", accepted, want, detail)
			}

			// The behavioural half: a valid tier succeeding is necessary but
			// not sufficient, so also require an invalid one to fail THROUGH
			// parseMinConfidence specifically — see the doc comment above.
			invalid, ierr := sess.CallTool(context.Background(), &sdk.CallToolParams{
				Name: name, Arguments: argsFor(t, name, map[string]any{"min_confidence": "pretty_sure"}),
			})
			msg := ""
			switch {
			case ierr != nil:
				msg = ierr.Error()
			case invalid.IsError:
				msg = resultText(invalid)
			}
			decodedByHandler := strings.Contains(msg, "unknown confidence")
			if decodedByHandler != want {
				t.Errorf("min_confidence routed through parseMinConfidence = %v, want %v (response: %s)",
					decodedByHandler, want, msg)
			}
		})
	}
}

// TestInvalidMinConfidenceIsRejectedNamingTheTiers pins the failure mode.
//
// A misspelled tier that silently degrades to "everything" hands an agent the
// low-confidence results it explicitly tried to exclude, and the agent has no
// way to notice. The error must also NAME the tiers: "invalid value" leaves the
// caller guessing at the vocabulary.
func TestInvalidMinConfidenceIsRejectedNamingTheTiers(t *testing.T) {
	sess := newGraphTestServer(t, seedMixedConfidence)
	for name, takesIt := range minConfidenceSurface {
		if !takesIt {
			continue
		}
		t.Run(name, func(t *testing.T) {
			res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
				Name: name, Arguments: argsFor(t, name, map[string]any{"min_confidence": "pretty_sure"}),
			})
			var msg string
			switch {
			case err != nil:
				msg = err.Error()
			case res.IsError:
				msg = resultText(res)
			default:
				t.Fatalf("want an error for an invalid confidence tier, got: %s", resultText(res))
			}
			for _, tier := range []string{"exact", "high", "medium", "low"} {
				if !strings.Contains(msg, tier) {
					t.Errorf("error should name every valid tier; %q missing from: %s", tier, msg)
				}
			}
		})
	}
}

// TestOmittingMinConfidenceChangesNothing is the compatibility guard.
//
// Adding a parameter must not move any existing caller's results. For most
// tools "no floor" means low, which admits everything. analyze_impact is the
// exception and it is stated here rather than hidden: its traversal has always
// defaulted to impact.DefaultMinConfidence (medium), and quietly lowering that
// would widen every blast radius and weaken the risk score built on it.
func TestOmittingMinConfidenceChangesNothing(t *testing.T) {
	equivalent := map[string]string{
		"find_symbol":      "low",
		"get_symbol":       "low",
		"find_references":  "low",
		"find_callers":     "low",
		"plan_rename":      "low",
		"get_task_context": "low",
		"find_tests":       "low",
		"analyze_impact":   "medium",
	}
	sess := newGraphTestServer(t, seedMixedConfidence)
	for name, floor := range equivalent {
		t.Run(name, func(t *testing.T) {
			absent := callTool(t, sess, name, argsFor(t, name, nil))
			explicit := callTool(t, sess, name, argsFor(t, name, map[string]any{"min_confidence": floor}))
			if got, want := resultText(absent), resultText(explicit); got != want {
				t.Errorf("omitting min_confidence differs from min_confidence=%q\nabsent:\n%s\nexplicit:\n%s",
					floor, got, want)
			}
		})
	}
}

// TestMinConfidenceFiltersWeakResults proves the floor does work rather than
// merely being accepted, and that the response SAYS what it dropped.
//
// A filter that silently shrinks the answer is the failure this whole feature
// exists to avoid: a short list read as "nothing else exists".
func TestMinConfidenceFiltersWeakResults(t *testing.T) {
	sess := newGraphTestServer(t, seedMixedConfidence)
	for _, tool := range []string{"find_symbol", "get_symbol", "find_references", "find_callers"} {
		t.Run(tool, func(t *testing.T) {
			all := resultText(callTool(t, sess, tool, argsFor(t, tool, nil)))
			strong := resultText(callTool(t, sess, tool, argsFor(t, tool, map[string]any{"min_confidence": "exact"})))
			if strings.Count(all, "low") <= strings.Count(strong, "low") {
				t.Errorf("min_confidence=exact did not drop the low-confidence results\nall:\n%s\nstrong:\n%s", all, strong)
			}
			if !strings.Contains(strong, "omitted") {
				t.Errorf("a filtered response must say lower-confidence results were omitted, got:\n%s", strong)
			}
		})
	}
}

// TestFloorNoteOnlyClaimsRealOmissions is the other half of
// TestMinConfidenceFiltersWeakResults: that test proves the note fires when
// something WAS dropped, this one proves it stays silent — and never
// self-contradicts — when nothing was.
//
// Both cases are real, measured regressions, not hypotheticals: floorNote used
// to fire on minConf != low alone, so a floor that admitted everything it was
// handed still claimed an omission that never happened.
func TestFloorNoteOnlyClaimsRealOmissions(t *testing.T) {
	sess := newGraphTestServer(t, seedMixedConfidence)

	t.Run("find_symbol: the only match already clears the floor", func(t *testing.T) {
		// provenCaller has exactly one definition, and it is already exact —
		// find_symbol name=provenCaller min_confidence=exact drops nothing.
		// The old floorNote fired here anyway, because it read only minConf
		// and never asked whether atLeastConfidence actually removed a row.
		got := resultText(callTool(t, sess, "find_symbol",
			argsFor(t, "find_symbol", map[string]any{"name": "provenCaller", "min_confidence": "exact"})))
		if strings.Contains(got, "omitted") || strings.Contains(got, "were dropped") {
			t.Errorf("no result was below the floor, so the response must not claim an omission:\n%s", got)
		}
	})

	t.Run("get_task_context: an empty pack must not contradict itself", func(t *testing.T) {
		// A task that names nothing in the index produces a zero-item pack.
		// The old code appended "%d item(s) were dropped by this floor" to
		// the SAME sentence that opened with "lower-confidence results exist
		// but were omitted" — dropped was 0, so the note asserted an omission
		// and then, in its own next clause, said how many: none.
		got := resultText(callTool(t, sess, "get_task_context",
			argsFor(t, "get_task_context", map[string]any{
				"task": "zzzzz nonexistent qqqqqq unrelated", "min_confidence": "exact",
			})))
		if strings.Contains(got, "omitted") || strings.Contains(got, "were dropped") {
			t.Errorf("an empty pack has nothing to omit; the response must not claim otherwise:\n%s", got)
		}
	})

	t.Run("get_task_context: a real drop is still disclosed, with a true count", func(t *testing.T) {
		// The fix must not trade a false claim for silence on a real one: a
		// task that DOES pull in mixed-confidence items must still say so,
		// and say how many — the true disclosure this task must not lose
		// while removing the false one above.
		got := resultText(callTool(t, sess, "get_task_context",
			argsFor(t, "get_task_context", map[string]any{
				"task":           "change the login flow across proven and guessed callers",
				"min_confidence": "exact",
			})))
		if !strings.Contains(got, "omitted") {
			t.Errorf("a real drop must still be disclosed, got:\n%s", got)
		}
		if !strings.Contains(got, "6 lower-confidence result(s) were omitted") {
			t.Errorf("the disclosed count must be the true number dropped (6), got:\n%s", got)
		}
	})
}

// sourceFor keeps the seed inside the registry's own invariant that only a
// compiler-sourced fact may claim 'exact'.
func sourceFor(c model.Confidence) model.SourceType {
	if c == model.ConfidenceExact {
		return model.SourceCompiler
	}
	return model.SourceSourceCode
}

// seedMixedConfidence builds one name with a proven definition and an inferred
// one, and references into the proven definition at both tiers, so a floor has
// something real to remove at every tool that takes one.
func seedMixedConfidence(ctx context.Context, reg *registry.Registry, runID int64) {
	w, err := reg.NewWriter(ctx, runID)
	if err != nil {
		panic(err)
	}
	defer w.Close()

	authFile := model.File{Path: "auth/Login.java", Language: "java", ContentHash: "ha", ParseStatus: "ok"}
	authID, _ := w.AddFile(ctx, &authFile)
	proven := model.Symbol{
		FileID: authID, Kind: model.KindClass, Name: "Login", FQN: "auth.Login", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 40}},
		Confidence: model.ConfidenceExact, Source: model.SourceCompiler,
	}
	provenID, _ := w.AddSymbol(ctx, &proven)

	legacyFile := model.File{Path: "legacy/Login.java", Language: "java", ContentHash: "hb", ParseStatus: "partial"}
	legacyID, _ := w.AddFile(ctx, &legacyFile)
	inferred := model.Symbol{
		FileID: legacyID, Kind: model.KindClass, Name: "Login", FQN: "legacy.Login", Exported: true,
		Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 9}},
		Confidence: model.ConfidenceLow, Source: model.SourceInferred,
	}
	_, _ = w.AddSymbol(ctx, &inferred)

	var edges []model.Edge
	for _, c := range []struct {
		name string
		conf model.Confidence
		kind model.EdgeKind
	}{
		{"provenCaller", model.ConfidenceExact, model.EdgeCalls},
		{"guessedCaller", model.ConfidenceLow, model.EdgeCalls},
		{"provenUser", model.ConfidenceExact, model.EdgeReferences},
		{"guessedUser", model.ConfidenceLow, model.EdgeReferences},
	} {
		path := fmt.Sprintf("callers/%s.java", c.name)
		f := model.File{Path: path, Language: "java", ContentHash: "h" + c.name, ParseStatus: "ok"}
		fID, _ := w.AddFile(ctx, &f)
		sym := model.Symbol{
			FileID: fID, Kind: model.KindMethod, Name: c.name,
			FQN: "callers." + c.name, Exported: true,
			Range:      model.Range{Start: model.Position{Line: 1}, End: model.Position{Line: 3}},
			Confidence: c.conf, Source: sourceFor(c.conf),
		}
		symID, serr := w.AddSymbol(ctx, &sym)
		if serr != nil {
			panic(serr)
		}
		edges = append(edges, model.Edge{
			SrcID: symID, DstID: provenID, Kind: c.kind, Confidence: c.conf,
			Evidence: model.Evidence{
				FilePath: path,
				Range:    model.Range{Start: model.Position{Line: 2}},
				Source:   sourceFor(c.conf),
				Detail:   "binding recorded at " + string(c.conf),
			},
		})
	}

	_ = w.IndexFileText(ctx, authID, authFile.Path, "public class Login { void authenticate() {} }")
	_ = w.Flush(ctx)
	_ = w.Close()

	if _, err := reg.AddResolvedEdges(ctx, runID, edges); err != nil {
		panic(err)
	}
}

func TestWeakConfidenceWarningAdvisoryNote(t *testing.T) {
	sess := newGraphTestServer(t, seedMixedConfidence)

	t.Run("find_callers warns when low confidence callers are included", func(t *testing.T) {
		res := resultText(callTool(t, sess, "find_callers", argsFor(t, "find_callers", nil)))
		want := "Note: 1 result(s) are medium or low confidence (heuristic inference with higher error rates). Pass min_confidence='high' to restrict to compiler-verified and high-precision edges."
		if !strings.Contains(res, want) {
			t.Errorf("find_callers without min_confidence should warn about weak edges; got:\n%s", res)
		}

		resHigh := resultText(callTool(t, sess, "find_callers", argsFor(t, "find_callers", map[string]any{"min_confidence": "high"})))
		if strings.Contains(resHigh, "heuristic inference with higher error rates") {
			t.Errorf("find_callers with min_confidence=high should not warn about weak edges; got:\n%s", resHigh)
		}
	})

	t.Run("find_references warns when low confidence references are included", func(t *testing.T) {
		res := resultText(callTool(t, sess, "find_references", argsFor(t, "find_references", nil)))
		want := "Note: 2 result(s) are medium or low confidence (heuristic inference with higher error rates). Pass min_confidence='high' to restrict to compiler-verified and high-precision edges."
		if !strings.Contains(res, want) {
			t.Errorf("find_references without min_confidence should warn about weak edges; got:\n%s", res)
		}

		resHigh := resultText(callTool(t, sess, "find_references", argsFor(t, "find_references", map[string]any{"min_confidence": "high"})))
		if strings.Contains(resHigh, "heuristic inference with higher error rates") {
			t.Errorf("find_references with min_confidence=high should not warn about weak edges; got:\n%s", resHigh)
		}
	})

	t.Run("proven only results do not emit weak confidence advisory note", func(t *testing.T) {
		res := resultText(callTool(t, sess, "find_symbol", map[string]any{"name": "provenCaller"}))
		if strings.Contains(res, "heuristic inference with higher error rates") {
			t.Errorf("provenCaller has exact confidence and should not emit weak confidence warning; got:\n%s", res)
		}
	})
}

func TestMinConfidenceParameterDescriptions(t *testing.T) {
	sess := newGraphTestServer(t, seedMixedConfidence)
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	wantDesc := "Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives."

	for _, tool := range res.Tools {
		if !minConfidenceSurface[tool.Name] {
			continue
		}
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatalf("unmarshal %s schema: %v", tool.Name, err)
		}
		prop, ok := schema.Properties["min_confidence"]
		if !ok {
			t.Fatalf("%s should have min_confidence property", tool.Name)
		}
		if prop.Description != wantDesc {
			t.Errorf("%s min_confidence description = %q, want %q", tool.Name, prop.Description, wantDesc)
		}
	}
}
