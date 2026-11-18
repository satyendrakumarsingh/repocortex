package pipeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
)

// A version probe that times out on a loaded machine left the key to the pin
// version, so the cached index of a run whose probe answered never matched
// again and the indexer re-ran. The same binary's recorded version is used
// instead; a changed binary is not trusted with the old version.
func TestRecordedToolVersionStandsInForAFailedProbe(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "scip")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "scip-clang")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho scip-clang 0.3.2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := indexer.Project{Language: indexer.LangCPP, Dir: "."}
	inst := indexer.Installed{Path: bin}
	if err := os.WriteFile(indexPath(dir, p), []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCacheKey(dir, p, "k1", nil, "scip-clang 0.3.2", toolFingerprint(inst)); err != nil {
		t.Fatal(err)
	}

	if v, ok := recordedToolVersion(dir, p, toolFingerprint(inst)); !ok || v != "scip-clang 0.3.2" {
		t.Fatalf("same binary: got %q, %v; want the recorded version", v, ok)
	}

	// A rebuilt or upgraded binary may report another version.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	if v, ok := recordedToolVersion(dir, p, toolFingerprint(inst)); ok {
		t.Fatalf("changed binary: got %q; want no recorded version", v)
	}
	if _, ok := recordedToolVersion(dir, p, ""); ok {
		t.Fatal("an unidentifiable tool must not borrow a recorded version")
	}
}
