package diffmini

import (
	"strings"
	"testing"
)

func TestParseAndCompressDiff(t *testing.T) {
	sampleDiff := `diff --git a/internal/context/task.go b/internal/context/task.go
index 123456..789abc 100644
--- a/internal/context/task.go
+++ b/internal/context/task.go
@@ -45,6 +45,7 @@ const (
 // knobs because they describe the grammar of a sentence, not a ranking policy.
 const (
 	weightIdentifier = 1.5
+	weightConcept    = 1.2
 	weightNoun       = 1.0
 	weightVerb       = 0.35
 )
@@ -128,6 +129,8 @@ func ParseTask(task string) Intent {
 		default:
 			note(lower, weightNoun)
 		}
 	}
+
+	expandConcepts(strings.ToLower(task), note)
+
 	in.Terms = make([]Term, 0, len(weights))
`

	rep, err := ParseAndCompress(sampleDiff)
	if err != nil {
		t.Fatalf("ParseAndCompress failed: %v", err)
	}

	if len(rep.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(rep.Files))
	}

	file := rep.Files[0]
	if file.Path != "internal/context/task.go" {
		t.Errorf("file path = %q, want internal/context/task.go", file.Path)
	}
	if file.AddedCount != 4 {
		t.Errorf("added lines = %d, want 4", file.AddedCount)
	}
	if len(file.Hunks) != 2 {
		t.Errorf("hunks count = %d, want 2", len(file.Hunks))
	}

	text := rep.FormatText()

	// Metadata headers should be stripped
	if strings.Contains(text, "diff --git") || strings.Contains(text, "index 123456") || strings.Contains(text, "--- a/") {
		t.Errorf("raw git diff metadata was not stripped:\n%s", text)
	}

	// Changes should be preserved
	if !strings.Contains(text, "+	weightConcept    = 1.2") {
		t.Errorf("missing added line:\n%s", text)
	}
	if !strings.Contains(text, "+	expandConcepts(strings.ToLower(task), note)") {
		t.Errorf("missing second added line:\n%s", text)
	}

	// Enclosing symbol headers should be extracted
	if !strings.Contains(text, "@@ const ( @@") {
		t.Errorf("missing hunk symbol header @@ const ( @@:\n%s", text)
	}
	if !strings.Contains(text, "@@ func ParseTask(task string) Intent { @@") {
		t.Errorf("missing hunk symbol header @@ func ParseTask... @@:\n%s", text)
	}

	if rep.SavedTokensPct <= 0 {
		t.Errorf("expected positive token savings, got %.1f%%", rep.SavedTokensPct)
	}
}
