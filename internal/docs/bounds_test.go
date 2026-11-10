package docs

import (
	"strings"
	"testing"
)

// TestOutputIsBoundedOnALargeGraph is the test that separates this from every
// "generate a diagram from your codebase" tool that ships a 10,000-node SVG.
//
// The synthetic repository below has 200 components, 600 endpoints and roughly
// 25,000 symbols. Unbounded, the component diagram alone would be 200 boxes and
// the document would carry 600 flow diagrams of 21 nodes each. Every assertion
// here is on a cap, and the last one is on the property the caps exist for: the
// caps must also be DISCLOSED, because a truncated document that does not admit
// it is a lie with a legend.
func TestOutputIsBoundedOnALargeGraph(t *testing.T) {
	t.Parallel()
	reg, m := largeIndex(200, 3, 20)

	// Sanity: the fixture really is large. A bounding test over a small graph
	// passes for the wrong reason.
	if len(reg.symbols) < 10_000 {
		t.Fatalf("fixture has only %d symbols; this test is meant to run against a large one", len(reg.symbols))
	}

	v := deriveFixture(t, reg, m)
	lim := DefaultLimits()

	if got := len(v.Components); got != lim.MaxComponents {
		t.Errorf("components = %d, want the cap %d (200 declared)", got, lim.MaxComponents)
	}
	if got := len(v.Crossings); got > lim.MaxCrossings {
		t.Errorf("crossings = %d, over the cap %d", got, lim.MaxCrossings)
	}
	if got := len(v.Endpoints); got != lim.MaxEndpointFlows {
		t.Errorf("endpoint flows = %d, want the cap %d (600 endpoints)", got, lim.MaxEndpointFlows)
	}
	if got := len(v.Inheritance); got != lim.MaxInheritance {
		t.Errorf("inheritance relationships = %d, want the cap %d", got, lim.MaxInheritance)
	}
	if got := len(v.Capabilities); got > lim.MaxCapabilities {
		t.Errorf("capabilities = %d, over the cap %d", got, lim.MaxCapabilities)
	}

	for _, f := range v.Endpoints {
		if len(f.Calls) > lim.MaxCallsPerFlow {
			t.Errorf("%s: %d calls, over the per-flow cap %d", f.Endpoint.Name, len(f.Calls), lim.MaxCallsPerFlow)
		}
		if len(f.Tables) > lim.MaxTablesPerFlow {
			t.Errorf("%s: %d tables, over the per-flow cap %d", f.Endpoint.Name, len(f.Tables), lim.MaxTablesPerFlow)
		}
	}

	// No single diagram may exceed what a renderer will lay out. The component
	// diagram is the one at risk, and it is capped by the component cap.
	for _, d := range v.Diagrams() {
		if n := len(d.Nodes); n > 128 {
			t.Errorf("diagram %q has %d nodes; nothing renders that", d.ID, n)
		}
		// The bounding must not have cost the evidence invariant.
		if problems := d.Validate(); len(problems) > 0 {
			t.Errorf("diagram %q after truncation: %s", d.ID, strings.Join(problems, "; "))
		}
	}

	// The document must SAY what it dropped, and say it in the body rather than
	// only in the model.
	archDoc := v.ArchitectureMarkdown()
	capDoc := v.CapabilityMarkdown()
	for _, want := range []string{
		"What this document leaves out",
		"declared components: showing 40 of 200",
		"endpoint flows: showing 50 of 600",
	} {
		if !strings.Contains(archDoc, want) {
			t.Errorf("architecture document does not disclose the cap: missing %q", want)
		}
	}
	if !strings.Contains(capDoc, "What this document leaves out") {
		t.Error("capability document does not disclose its caps")
	}

	// And the whole thing must stay a document rather than a data dump. 25,000
	// symbols in, a few hundred kilobytes out; the exact number matters less
	// than that it does not scale with the repository.
	if n := len(archDoc); n > 400_000 {
		t.Errorf("architecture document is %d bytes from a %d-symbol index; the caps are not holding",
			n, len(reg.symbols))
	}
	if n := len(capDoc); n > 200_000 {
		t.Errorf("capability document is %d bytes; the caps are not holding", n)
	}
}

// TestBoundingScalesSublinearly makes the claim above precise: growing the
// repository tenfold must not grow the document tenfold.
//
// Without this, a "bounded" generator that merely has a very large cap passes
// the test above on a fixture that happens to sit under the cap.
func TestBoundingScalesSublinearly(t *testing.T) {
	t.Parallel()
	smallReg, smallM := largeIndex(20, 3, 20)
	bigReg, bigM := largeIndex(200, 3, 20)

	small := len(deriveFixture(t, smallReg, smallM).ArchitectureMarkdown())
	big := len(deriveFixture(t, bigReg, bigM).ArchitectureMarkdown())

	symbolGrowth := float64(len(bigReg.symbols)) / float64(len(smallReg.symbols))
	docGrowth := float64(big) / float64(small)
	if docGrowth > symbolGrowth/2 {
		t.Errorf("index grew %.1fx and the document grew %.1fx; the caps are not bounding the output",
			symbolGrowth, docGrowth)
	}
}

// TestTightLimitsStillProduceAValidDocument: the caps are configurable, and a
// caller who sets them very low must get a small honest document rather than a
// broken one.
func TestTightLimitsStillProduceAValidDocument(t *testing.T) {
	t.Parallel()
	tight := Limits{
		MaxComponents: 1, MaxCrossings: 1, MaxEndpointFlows: 1,
		MaxCallDepth: 1, MaxCallsPerFlow: 1, MaxTablesPerFlow: 1,
		MaxInheritance: 1, MaxCapabilities: 1,
		MaxEndpointsPerCapability: 1, MaxTablesPerCapability: 1,
	}
	v := deriveFixture(t, smallIndex(), smallModel(), WithLimits(tight))

	if len(v.Components) != 1 || len(v.Endpoints) != 1 || len(v.Capabilities) != 1 {
		t.Fatalf("tight caps not applied: %d components, %d flows, %d capabilities",
			len(v.Components), len(v.Endpoints), len(v.Capabilities))
	}
	for _, d := range v.Diagrams() {
		if problems := d.Validate(); len(problems) > 0 {
			t.Errorf("diagram %q under tight caps: %s", d.ID, strings.Join(problems, "; "))
		}
		if !strings.Contains(d.Mermaid(), "\n") {
			t.Errorf("diagram %q rendered no Mermaid at all", d.ID)
		}
	}
	doc := v.ArchitectureMarkdown()
	if !strings.Contains(doc, "showing 1 of") {
		t.Error("tight caps were applied without being disclosed")
	}
}

// TestEmptyIndexIsADocumentNotACrash. An index with nothing in it is a real
// state — a fresh repository, a failed parse — and the generator must produce
// something readable rather than an empty file or a panic.
func TestEmptyIndexIsADocumentNotACrash(t *testing.T) {
	t.Parallel()
	v := deriveFixture(t, &fakeRegistry{}, smallModel())

	for _, d := range v.Diagrams() {
		m := d.Mermaid()
		if m == "" {
			t.Errorf("diagram %q rendered nothing", d.ID)
		}
		if problems := d.Validate(); len(problems) > 0 {
			t.Errorf("diagram %q: %s", d.ID, strings.Join(problems, "; "))
		}
	}
	doc := v.ArchitectureMarkdown()
	if !strings.Contains(doc, "no endpoints") {
		t.Error("document does not say the index contains no endpoints")
	}
	if !strings.Contains(doc, "no IMPLEMENTS or EXTENDS edges") {
		t.Error("document does not distinguish 'no inheritance in the index' from 'no inheritance in the code'")
	}
}
