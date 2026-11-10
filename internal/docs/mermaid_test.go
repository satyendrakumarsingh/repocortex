package docs

import (
	"strings"
	"testing"
)

// TestAnEmptyDiagramsPlaceholderIsALabelNotAnEssay pins the split between the
// node label and the prose beside it.
//
// The explanation for an empty section is necessarily a paragraph — "declares no
// components" and "we failed to find components" have opposite consequences, and
// distinguishing them takes sentences. Putting that paragraph inside the node
// produced a box wider than the page holding text the reader then read again
// directly underneath. The diagram gets the first sentence; the argument stays in
// the prose.
func TestAnEmptyDiagramsPlaceholderIsALabelNotAnEssay(t *testing.T) {
	const note = "This repository declares no components. RepoCortex does not infer " +
		"them: a component model it guessed would be a description of the code's " +
		"current shape, and calling that the architecture is exactly the " +
		"unverifiable assertion this document exists to avoid."

	got := Diagram{Kind: KindComponentGraph, Note: note}.Mermaid()

	label := "This repository declares no components."
	if !strings.Contains(got, label) {
		t.Errorf("placeholder node lost the first sentence:\n%s", got)
	}
	if strings.Contains(got, "unverifiable assertion") {
		t.Errorf("the whole note was rendered inside the node; only the first sentence belongs there:\n%s", got)
	}
}
