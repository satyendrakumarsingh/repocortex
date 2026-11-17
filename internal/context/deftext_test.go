package context

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A long module whose one function about auditing sits under 1,500 others,
// beside short notes that say every word of the task. File-level text ranks
// the notes first and the long module out of reach; the function's own text
// is what finds it.
func deepDefinitionTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{}
	var b strings.Builder
	b.WriteString("import logging\n\n")
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&b, "def helper_%d(value):\n    return value + %d\n\n", i, i)
	}
	b.WriteString(`def _record_transition(session, task, previous, current):
    entry = AuditLogEntry(task_id=task.id, old_status=previous, new_status=current)
    session.add(entry)
    logging.info("audit log entry written when the status changes")
`)
	files["app/services/task_service.py"] = b.String()
	for i := 0; i < 8; i++ {
		files[fmt.Sprintf("notes/note_%d.py", i)] = fmt.Sprintf(
			"# audit log entry when a task status changes, see the design notes\ndef note_%d():\n    return %d\n", i, i)
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestADefinitionDeepInALongFileIsReachedByItsText(t *testing.T) {
	reg, g, runID := indexFixture(t, deepDefinitionTree(t))
	const task = "add an audit log entry when a task status changes"

	// Five files per term instead of fifty, so eight short notes are enough
	// to push the long module out of the file-level results, as two thousand
	// short files did on the repository this was found on.
	tuning := DefaultTuning()
	tuning.LexicalLimit = 5

	has := func(p Pack) bool {
		for _, it := range p.Items {
			if it.Name == "_record_transition" {
				return true
			}
		}
		return false
	}

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: task, Tuning: &tuning})
	if err != nil {
		t.Fatal(err)
	}
	if !has(pack) {
		var names []string
		for _, it := range pack.Items {
			names = append(names, it.Name+" ("+it.Path+")")
		}
		t.Fatalf("_record_transition is not in the pack; got %v", names)
	}

	// And it is the definition text that reached it.
	off := tuning
	off.DefinitionLimit = 0
	without, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: task, Tuning: &off})
	if err != nil {
		t.Fatal(err)
	}
	if has(without) {
		t.Fatal("reached without definition text too: the fixture does not reproduce the gap")
	}
}
