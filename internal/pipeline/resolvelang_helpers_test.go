package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// langEdge is one resolved reference, named by the symbols it joins rather
// than by row ids so an assertion reads as the claim it makes.
type langEdge struct {
	Src, Dst   string // symbol FQNs, "path:name"
	Kind       model.EdgeKind
	Confidence model.Confidence
	Reason     string
	DstKind    model.EntityKind
}

// langEdges indexes a tree through the whole pipeline and returns every edge
// the binding passes produced, as FQN pairs. The kinds are the reference
// kinds: a structural edge (a file defining a symbol) is not a binding.
func langEdges(t *testing.T, files map[string]string) []langEdge {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, files)
	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))
	syms, err := reg.Symbols(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	fqn := map[int64]string{}
	kinds := map[int64]model.EntityKind{}
	for _, s := range syms {
		fqn[s.ID] = s.FQN
		kinds[s.ID] = s.Kind
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []langEdge
	for _, e := range edges {
		switch e.Kind {
		case model.EdgeCalls, model.EdgeInstantiates, model.EdgeReferences, model.EdgeExtends, model.EdgeImplements:
		default:
			continue
		}
		out = append(out, langEdge{fqn[e.SrcID], fqn[e.DstID], e.Kind, e.Confidence, e.Evidence.Detail, kinds[e.DstID]})
	}
	// Code outside every definition (a busted spec's describe/it callbacks, a
	// script's top level) is attributed to the file's own synthetic caller, and
	// Edges leaves those out because Symbols does not hold them. They are read
	// from the destination side, as the caller queries do.
	for _, s := range syms {
		hits, err := reg.IncomingEdges(ctx, runID, s.ID, 1000, registry.EdgeFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			if h.SrcID == 0 || fqn[h.SrcID] != "" {
				continue
			}
			out = append(out, langEdge{h.SrcPath + ":(top level)", s.FQN, model.EdgeKind(h.Kind), model.Confidence(h.Confidence), h.Detail, s.Kind})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]) < fmt.Sprint(out[j])
	})
	return out
}

// want is one edge a fixture must produce. Src and Dst are matched as
// suffixes of the symbol's FQN so a fixture names "App.swift:run" and not the
// repository-relative prefix; Min is the lowest confidence accepted.
type want struct {
	Src, Dst string
	Min      model.Confidence
}

// scoreEdges checks a fixture: every want must be present at its confidence,
// every edge in forbid must be absent, and no edge the fixture did not expect
// may point into the repository from a file the fixture marked as checked
// (any edge at all, when checked is empty). It reports precision as correct
// edges over all edges produced and recall as correct over expected.
//
// known lists edges the resolver is known to bind wrongly, at low: they are
// counted against precision and do not fail the test, so the number reported
// is the one a user gets and a fix shows up as it rising.
func scoreEdges(t *testing.T, got []langEdge, wants []want, forbid []want, extraOK []want, known ...want) {
	t.Helper()
	match := func(e langEdge, w want) bool {
		return strings.HasSuffix(e.Src, w.Src) && strings.HasSuffix(e.Dst, w.Dst)
	}
	correct := 0
	for _, w := range wants {
		found := false
		for _, e := range got {
			if match(e, w) {
				found = true
				if !e.Confidence.AtLeast(w.Min) {
					t.Errorf("edge %s -> %s is %s, want at least %s (%s)", w.Src, w.Dst, e.Confidence, w.Min, e.Reason)
				}
			}
		}
		if found {
			correct++
		} else {
			t.Errorf("missing edge %s -> %s", w.Src, w.Dst)
		}
	}
	for _, f := range forbid {
		for _, e := range got {
			if match(e, f) {
				t.Errorf("wrong edge %s -> %s (%s %s: %s)", e.Src, e.Dst, e.Kind, e.Confidence, e.Reason)
			}
		}
	}
	good := 0
	for _, e := range got {
		wrong := false
		for _, k := range known {
			if match(e, k) {
				wrong = true
				t.Logf("known wrong edge %s -> %s (%s %s: %s)", e.Src, e.Dst, e.Kind, e.Confidence, e.Reason)
			}
		}
		if wrong {
			continue
		}
		ok := false
		for _, w := range append(append([]want{}, wants...), extraOK...) {
			if match(e, w) {
				ok = true
			}
		}
		if ok {
			good++
		} else {
			t.Errorf("unexpected edge %s -> %s (%s %s: %s)", e.Src, e.Dst, e.Kind, e.Confidence, e.Reason)
		}
	}
	prec, rec := 1.0, 1.0
	if len(got) > 0 {
		prec = float64(good) / float64(len(got))
	}
	if len(wants) > 0 {
		rec = float64(correct) / float64(len(wants))
	}
	t.Logf("precision %d/%d = %.0f%%, recall %d/%d = %.0f%%", good, len(got), prec*100, correct, len(wants), rec*100)
}

// capped fails for an edge that matches one of the weaker entries at a higher
// confidence than the entry allows. scoreEdges only checks that an edge is
// expected; a decoy the resolver may bind, but never as `high`, needs this.
func capped(t *testing.T, got []langEdge, weaker []want) {
	t.Helper()
	for _, e := range got {
		for _, w := range weaker {
			if strings.HasSuffix(e.Src, w.Src) && strings.HasSuffix(e.Dst, w.Dst) && !w.Min.AtLeast(e.Confidence) {
				t.Errorf("edge %s -> %s is %s, allowed at most %s (%s)", e.Src, e.Dst, e.Confidence, w.Min, e.Reason)
			}
		}
	}
}
