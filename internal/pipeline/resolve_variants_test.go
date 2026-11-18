package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// siteEdge is one resolved call, by the line the call is written on and the
// line the callee is defined on. Two Ruby methods may share a qualified name
// (`def self.start` and `def start`), so a name cannot say which was reached.
type siteEdge struct {
	callLine, defLine int
	confidence        model.Confidence
	reason            string
}

// siteEdges indexes a tree and returns the edges evidenced in one file, keyed
// by the callee's name.
func siteEdges(t *testing.T, files map[string]string, evidenceFile string) map[string][]siteEdge {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, files)
	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))
	syms, err := reg.Symbols(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]model.Symbol{}
	for _, s := range syms {
		byID[s.ID] = s
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]siteEdge{}
	for _, e := range edges {
		if e.Kind != model.EdgeCalls || e.Evidence.FilePath != evidenceFile {
			continue
		}
		dst, ok := byID[e.DstID]
		if !ok {
			continue
		}
		out[dst.Name] = append(out[dst.Name], siteEdge{e.Evidence.Range.Start.Line, dst.Range.Start.Line, e.Confidence, e.Evidence.Detail})
	}
	return out
}

func edgeAt(t *testing.T, got map[string][]siteEdge, name string, callLine int) siteEdge {
	t.Helper()
	for _, e := range got[name] {
		if e.callLine == callLine {
			return e
		}
	}
	t.Fatalf("no edge to %s from line %d; have %+v", name, callLine, got)
	return siteEdge{}
}

// A bare call written inside an instance method reaches the instance method of
// that name, and inside `def self.x` the class method. stdlib: resolv.rb
// `each_address` and net/http.rb `start`, 17 calls bound to the other kind.
func TestRubyBareCallReachesItsOwnKindOfMethod(t *testing.T) {
	got := siteEdges(t, map[string]string{"net.rb": `class Net
  def self.start
    1
  end

  def start
    2
  end

  def run
    start()
  end

  def self.build
    start()
  end
end
`}, "net.rb")
	inInstance := edgeAt(t, got, "start", 11)
	if inInstance.defLine != 6 || inInstance.confidence != model.ConfidenceHigh {
		t.Errorf("start in an instance method: defined at line %d, %s; want the instance method (6) at high", inInstance.defLine, inInstance.confidence)
	}
	inClass := edgeAt(t, got, "start", 15)
	if inClass.defLine != 2 || inClass.confidence != model.ConfidenceHigh {
		t.Errorf("start in a class method: defined at line %d, %s; want the class method (2) at high", inClass.defLine, inClass.confidence)
	}
}

// A type that defines one name twice: the first is what a compiler index
// attaches to, and a call cannot say it means that one.
func TestRubyRedefinedMethodIsLow(t *testing.T) {
	got := siteEdges(t, map[string]string{"money.rb": `class Money
  def fmt
    1
  end

  def fmt
    2
  end

  def show
    fmt()
  end
end
`}, "money.rb")
	if e := edgeAt(t, got, "fmt", 11); e.confidence != model.ConfidenceLow {
		t.Errorf("a call to a method defined twice: %s (%s), want low", e.confidence, e.reason)
	}
}

// A bare DSL call in a method is not the method of the file that happens to be
// required and define the name. Homebrew: 71.8% right at high, and 2 of 378 at
// medium.
func TestRubyNameInARequiredFileIsLowEndToEnd(t *testing.T) {
	got := siteEdges(t, map[string]string{
		"dsl.rb": `class Formula
  def depends_on(name)
    name
  end
end
`,
		"recipe.rb": `require_relative "dsl"

class Recipe
  def build
    depends_on "zlib"
  end
end
`}, "recipe.rb")
	if e := edgeAt(t, got, "depends_on", 5); e.confidence != model.ConfidenceLow {
		t.Errorf("a name found in a required file: %s (%s), want low", e.confidence, e.reason)
	}
}

// A spec that defines a helper `patch` and calls `patch do ... end` is calling
// the DSL method of Formula. The call that passes a block is low; one that
// does not is still the helper at high.
func TestRubySpecHelperShadowingADslMethodWithABlock(t *testing.T) {
	got := siteEdges(t, map[string]string{
		"formula.rb": `class Formula
  def self.patch(&block)
    block
  end
end
`,
		"spec/patch_spec.rb": `def patch(extra = nil)
  extra
end

def example
  patch do
    1
  end
  patch()
end
`}, "spec/patch_spec.rb")
	if e := edgeAt(t, got, "patch", 6); e.defLine != 1 || e.confidence != model.ConfidenceLow {
		t.Errorf("patch do ... end: defined at %d, %s (%s); want the helper at low", e.defLine, e.confidence, e.reason)
	}
	if e := edgeAt(t, got, "patch", 9); e.defLine != 1 || e.confidence != model.ConfidenceHigh {
		t.Errorf("patch with no block: defined at %d, %s (%s); want the helper at high", e.defLine, e.confidence, e.reason)
	}
}

// `#ifdef _WIN32` and `#else` define one function twice. Which one a build
// sees is not in the source, so a call is low; a function defined once and
// unconditionally is high, and a header guard is no alternative.
func TestCPreprocessorAlternativesEndToEnd(t *testing.T) {
	got := siteEdges(t, map[string]string{"compat.c": `#ifndef COMPAT_C
#define COMPAT_C
#ifdef _WIN32
int plat(void) { return 1; }
#else
int plat(void) { return 2; }
#endif

int solo(void) { return 0; }

int use(void) {
  return plat() + solo();
}
#endif
`}, "compat.c")
	if e := edgeAt(t, got, "plat", 12); e.confidence != model.ConfidenceLow {
		t.Errorf("a function with a variant per platform: %s (%s), want low", e.confidence, e.reason)
	}
	if e := edgeAt(t, got, "solo", 12); e.confidence != model.ConfidenceHigh {
		t.Errorf("one unconditional definition: %s (%s), want high", e.confidence, e.reason)
	}
}
