package cli

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// editSession indexes a small Go project and opens the tool server over it,
// as `serve` does when checkEdits is set.
func editSession(t *testing.T, checkEdits bool) (root string, inv *devmcp.Invoker) {
	t.Helper()
	root = isolate(t)
	writeFiles(t, root, map[string]string{
		"go.mod": "module example.com/edit\n\ngo 1.22\n",
		"a.go":   "package edit\n\nfunc Alpha() int { return 1 }\n",
	})
	for _, args := range [][]string{{"init"}, {"index"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	g := &Global{Project: root, Quiet: true}
	cmd := NewRoot()
	cmd.SetArgs([]string{"--project", root, "--quiet", "version"})
	cmd.SetOut(io.Discard)
	if err := g.setup(cmd); err != nil {
		t.Fatalf("configure: %v", err)
	}
	ctx := context.Background()
	srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{Profile: config.MCPFull, CheckEdits: checkEdits})
	if err != nil {
		t.Fatalf("open agent server: %v", err)
	}
	t.Cleanup(closeServer)
	inv, err = srv.NewInvoker(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inv.Close() })
	return root, inv
}

type provenanceOf struct {
	Provenance struct {
		EditsCheckedAt string   `json:"edits_checked_at"`
		RefreshedFiles []string `json:"refreshed_files"`
		IndexBusy      bool     `json:"index_busy"`
		Caveat         string   `json:"caveat"`
	} `json:"provenance"`
	Items []struct {
		Name string `json:"name"`
	} `json:"items"`
}

func ask(t *testing.T, inv *devmcp.Invoker, tool, args string) (devmcp.CallResult, provenanceOf) {
	t.Helper()
	res, err := inv.Call(context.Background(), tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var p provenanceOf
	if err := json.Unmarshal(res.Structured, &p); err != nil {
		t.Fatalf("%s: structured: %v", tool, err)
	}
	return res, p
}

// The agent's loop: ask, edit, ask again. The second answer comes from the
// edited code and says which file it re-indexed to get there.
func TestTheNextAnswerAfterAnEditIncludesIt(t *testing.T) {
	root, inv := editSession(t, true)

	_, p := ask(t, inv, "find_symbol", `{"name":"Beta"}`)
	if len(p.Items) != 0 {
		t.Fatalf("Beta found before it was written: %+v", p.Items)
	}
	if p.Provenance.EditsCheckedAt == "" || !strings.Contains(p.Provenance.Caveat, "Working-tree edits") {
		t.Errorf("the first answer does not say edits were checked: %+v", p.Provenance)
	}

	// A save the agent makes. The check runs at most every couple of
	// seconds; waiting past that is what a second question naturally does.
	writeFiles(t, root, map[string]string{
		"b.go": "package edit\n\nfunc Beta() int { return Alpha() + 1 }\n",
	})
	time.Sleep(checkFloor + 100*time.Millisecond)

	res, p := ask(t, inv, "find_symbol", `{"name":"Beta"}`)
	if len(p.Items) == 0 {
		t.Fatalf("Beta, saved before the question, is not in the answer:\n%s", res.Text)
	}
	if len(p.Provenance.RefreshedFiles) != 1 || p.Provenance.RefreshedFiles[0] != "b.go" {
		t.Errorf("refreshed_files = %v, want [b.go]", p.Provenance.RefreshedFiles)
	}
	if !strings.Contains(res.Text, "refreshed before answering: b.go") {
		t.Errorf("the answer text does not name the re-indexed file:\n%s", res.Text)
	}
}

// Nothing changed, nothing written: re-stamping the run on every check would
// make every cache in the server reload for no change at all.
func TestAnUnchangedTreeIsNotRewritten(t *testing.T) {
	root, inv := editSession(t, true)
	finished := func() int64 {
		t.Helper()
		db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(root, IndexDir, IndexFile), ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var f int64
		if err := db.QueryRowContext(context.Background(),
			`SELECT finished_unix FROM index_runs WHERE committed = 1 ORDER BY id DESC LIMIT 1`).Scan(&f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	ask(t, inv, "find_symbol", `{"name":"Alpha"}`)
	before := finished()
	time.Sleep(checkFloor + 1100*time.Millisecond) // past the throttle, and into a later second
	_, p := ask(t, inv, "find_symbol", `{"name":"Alpha"}`)
	if len(p.Provenance.RefreshedFiles) != 0 {
		t.Errorf("refreshed %v with nothing changed", p.Provenance.RefreshedFiles)
	}
	if after := finished(); after != before {
		t.Errorf("the run was re-stamped (%d -> %d) though nothing changed", before, after)
	}
}

// A git hook's pass holding the index is not waited out: the answer is given
// as the index stands and says another update was running.
func TestABusyIndexIsReportedNotWaitedFor(t *testing.T) {
	root, inv := editSession(t, true)
	lock, err := fs.TryLock(filepath.Join(root, IndexDir, "reindex.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	start := time.Now()
	_, p := ask(t, inv, "find_symbol", `{"name":"Alpha"}`)
	if !p.Provenance.IndexBusy || !strings.Contains(p.Provenance.Caveat, "Another process was updating the index") {
		t.Errorf("a held lock was not reported: %+v", p.Provenance)
	}
	if took := time.Since(start); took > lockWait+3*time.Second {
		t.Errorf("the answer waited %s for a lock held elsewhere", took)
	}
}

// The portal promises never to cause a write, and goes through the same
// server: without CheckEdits there is no check and the caveat says edits are
// invisible.
func TestWithoutCheckEditsNothingIsChecked(t *testing.T) {
	root, inv := editSession(t, false)
	writeFiles(t, root, map[string]string{"b.go": "package edit\n\nfunc Beta() int { return 2 }\n"})
	res, p := ask(t, inv, "find_symbol", `{"name":"Beta"}`)
	if len(p.Items) != 0 || p.Provenance.EditsCheckedAt != "" {
		t.Fatalf("an edit was picked up with checking off:\n%s", res.Text)
	}
	if !strings.Contains(p.Provenance.Caveat, "invisible to the index") {
		t.Errorf("the caveat does not say edits are not reflected: %q", p.Provenance.Caveat)
	}
}
