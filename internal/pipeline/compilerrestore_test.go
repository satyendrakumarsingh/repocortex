package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"
)

// These tests are the SCIP half of review finding A1. An incremental refresh
// replaces a changed file's rows, which deletes every edge into its symbols,
// including the exact edges from files that did not change; for Go the type
// check is re-run (see TestIncrementalRefreshPreservesExactGoTypesEdges), and
// for everything else the cached index is read again. The stand-in indexer is
// the one the Rust tests use: a script that copies a hand-made .scip.

const (
	restoreCaller = "rust-analyzer cargo geo 0.1.0 caller()."
	restoreTarget = "rust-analyzer cargo geo 0.1.0 target()."
)

var restoreFiles = map[string]string{
	"Cargo.toml":   singlePackage,
	"src/lib.rs":   "pub mod other;\npub fn caller() {\n    other::target();\n}\n",
	"src/other.rs": "pub fn target() {\n}\n",
}

// restoreIndex is what the compiler wrote for restoreFiles: caller (lib.rs
// line 2) calls target (other.rs line 1).
func restoreIndex() []*scippb.Document {
	return []*scippb.Document{
		{
			RelativePath: "src/lib.rs", Language: "rust",
			PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
			Symbols:          []*scippb.SymbolInformation{{Symbol: restoreCaller, Kind: scippb.SymbolInformation_Function, DisplayName: "caller"}},
			Occurrences: []*scippb.Occurrence{
				defOcc(restoreCaller, 1, 7, 13, 1, 3),
				refOcc(restoreTarget, 2, 11, 17),
			},
		},
		{
			RelativePath: "src/other.rs", Language: "rust",
			PositionEncoding: scippb.PositionEncoding_UTF8CodeUnitOffsetFromLineStart,
			Symbols:          []*scippb.SymbolInformation{{Symbol: restoreTarget, Kind: scippb.SymbolInformation_Function, DisplayName: "target"}},
			Occurrences:      []*scippb.Occurrence{defOcc(restoreTarget, 0, 7, 13, 0, 1)},
		},
	}
}

// restoreRepo indexes and resolves restoreFiles, runs the stand-in indexer
// once, and returns the harness with the proven edge in place.
func restoreRepo(t *testing.T) (*harness, CompilerIndexOptions) {
	t.Helper()
	skipWithoutShell(t)
	ctx := context.Background()
	h := newHarness(t, restoreFiles)
	if _, err := Resolve(ctx, h.reg, h.runID, ResolveOptions{}); err != nil {
		t.Fatal(err)
	}
	bins, scratch := t.TempDir(), t.TempDir()
	fixture, logFile := filepath.Join(scratch, "rust.scip"), filepath.Join(scratch, "runs.log")
	writeScip(t, fixture, "rust-analyzer", restoreIndex()...)
	standIn(t, bins, "rust-analyzer", "rust-analyzer 1.80.0", " --output", fixture, logFile)
	// cargo is checked for, not run, before rust-analyzer is.
	if err := os.WriteFile(filepath.Join(bins, "cargo"), []byte("#!/bin/sh\necho cargo 1.80.0\n"), 0o755); err != nil { //nolint:gosec // G306: a test executable
		t.Fatal(err)
	}
	onlyPath(t, bins)
	opts := CompilerIndexOptions{Root: h.root, Config: allowAll(), AllowRepoBuild: true}
	out := RunCompilerIndexers(ctx, h.reg, h.runID, opts)
	if out.Err != nil || out.Imported() != 1 {
		t.Fatalf("stand-in pass: imported %d, err %v, steps %+v", out.Imported(), out.Err, out.Steps)
	}
	if n := h.exactFrom("caller"); n != 1 {
		t.Fatalf("before any edit: %d exact edges from caller, want the one to target", n)
	}
	return h, opts
}

// exactFrom counts the compiler-proven edges whose source is the named symbol.
func (h *harness) exactFrom(src string) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM edges e JOIN symbols s ON s.id = e.src_id
		 WHERE e.index_run_id = ? AND s.name = ? AND e.confidence = 'exact' AND e.source = 'compiler'
		   AND e.kind IN ('CALLS','REFERENCES')`, h.runID, src).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) edgeCount() int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM edges WHERE index_run_id = ?`, h.runID).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// replaceWithoutRestore edits and re-indexes the way Refresh does, minus the
// repair, so a test can show the loss before it shows the repair.
func (h *harness) replaceWithoutRestore() []FileChange {
	h.t.Helper()
	ctx := context.Background()
	changes, err := Diff(ctx, h.reg, h.runID, h.walker, harnessBuild)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := Incremental(ctx, h.reg, h.runID, h.walker, changes, IncrementalOptions{Resolve: true}); err != nil {
		h.t.Fatal(err)
	}
	return changes
}

// TestRefreshRestoresSCIPProofToAChangedDefinition: the caller's file did not
// change, the target's did, and the definition is still there.
func TestRefreshRestoresSCIPProofToAChangedDefinition(t *testing.T) {
	h, _ := restoreRepo(t)
	ctx := context.Background()

	// A line added above the definition: the index's line for it is now wrong.
	h.write("src/other.rs", "// edited\npub fn target() {\n}\n")
	changes := h.replaceWithoutRestore()
	if n := h.exactFrom("caller"); n != 0 {
		t.Fatalf("%d exact edges from caller survived replacing the target's file; the test shows nothing", n)
	}

	r := RestoreCompilerProof(ctx, h.reg, h.runID, h.root, changes)
	if r.Err != nil || r.Projects != 1 || r.Restored != 1 {
		t.Fatalf("restore = %+v, want the one project read and the one edge put back", r)
	}
	if n := h.exactFrom("caller"); n != 1 {
		t.Fatalf("%d exact edges from caller after the repair, want 1", n)
	}

	// Idempotent: the same repair again adds and changes nothing.
	edges := h.edgeCount()
	again := RestoreCompilerProof(ctx, h.reg, h.runID, h.root, changes)
	if again.Err != nil || again.Restored != 0 || h.edgeCount() != edges || h.exactFrom("caller") != 1 {
		t.Fatalf("second restore = %+v, edges %d -> %d, exact %d: want no change", again, edges, h.edgeCount(), h.exactFrom("caller"))
	}
}

// TestRefreshRunsTheRepair: the same through Refresh, the entry point of
// `index`, `watch`, the hooks and `serve`.
func TestRefreshRunsTheRepair(t *testing.T) {
	h, _ := restoreRepo(t)
	h.write("src/other.rs", "// edited\npub fn target() {\n}\n")
	stats, err := Refresh(context.Background(), h.reg, h.runID, h.walker, RefreshOptions{RunningAs: harnessBuild, Root: h.root})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Modified != 1 || stats.CompilerProofRestored != 1 {
		t.Fatalf("stats = %+v, want one file modified and one edge restored", stats)
	}
	if n := h.exactFrom("caller"); n != 1 {
		t.Fatalf("%d exact edges from caller after Refresh, want 1", n)
	}
}

// TestRefreshDoesNotProveAnEdgeToARemovedDefinition: the target is gone, so
// there is nothing for the compiler's edge to point at and nothing is invented.
func TestRefreshDoesNotProveAnEdgeToARemovedDefinition(t *testing.T) {
	h, _ := restoreRepo(t)
	h.write("src/other.rs", "pub fn renamed() {\n}\n")
	stats, err := Refresh(context.Background(), h.reg, h.runID, h.walker, RefreshOptions{RunningAs: harnessBuild, Root: h.root})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if stats.CompilerProofRestored != 0 {
		t.Errorf("restored %d edges to a definition that no longer exists", stats.CompilerProofRestored)
	}
	if n := h.exactFrom("caller"); n != 0 {
		t.Errorf("%d exact edges from caller, want none: its target was removed", n)
	}
}

// TestRefreshDoesNotTrustAChangedFilesOwnDocument: the edited file is the
// caller's, so the index's reference positions in it describe the old text.
func TestRefreshDoesNotTrustAChangedFilesOwnDocument(t *testing.T) {
	h, _ := restoreRepo(t)
	h.write("src/lib.rs", "// edited\npub mod other;\npub fn caller() {\n    other::target();\n}\n")
	stats, err := Refresh(context.Background(), h.reg, h.runID, h.walker, RefreshOptions{RunningAs: harnessBuild, Root: h.root})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if stats.CompilerProofRestored != 0 {
		t.Errorf("restored %d edges from a document of a changed file", stats.CompilerProofRestored)
	}
	if n := h.exactFrom("caller"); n != 0 {
		t.Errorf("%d exact edges from caller, want none: they were evidenced by a stale document", n)
	}

	// Editing it back makes the document describe the file again, which a
	// modification time could not say.
	h.write("src/lib.rs", restoreFiles["src/lib.rs"])
	h.write("src/other.rs", "// edited\npub fn target() {\n}\n")
	if _, err := Refresh(context.Background(), h.reg, h.runID, h.walker, RefreshOptions{RunningAs: harnessBuild, Root: h.root}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n := h.exactFrom("caller"); n != 1 {
		t.Errorf("%d exact edges from caller once its file is as indexed again, want 1", n)
	}
}
