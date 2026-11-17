package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Files the compiler pass writes into the user's tree, and how a crash is
// cleaned up after.
//
// To make the indexers read what they need, a run writes files beside the
// user's code: a pyrightconfig.json naming src/, a .vue.ts mirror next to
// every Vue component, and scip-typescript writes the tsconfig.json it infers.
// They are removed when the run ends, but a run that is killed (a hook's
// background pass, Ctrl-C at the wrong moment) left them behind: on a
// repository with 1,462 Vue components that is 1,462 stray files in
// `git status`, indexed as TypeScript on the next pass.
//
// The ledger records each file before the indexers run. The next run removes
// what a dead run left — but only a file whose content is still exactly what
// RepoCortex wrote, so nothing the user has since edited or created is ever
// deleted. A tsconfig.json the indexer inferred has no content RepoCortex
// chose, so it is reported and left in place.

const stagingLedger = "staged-files.json"

type stagedFile struct {
	Path string `json:"path"`
	// SHA256 is the digest of what RepoCortex wrote; empty for a file a tool
	// creates during the run (an inferred tsconfig.json).
	SHA256 string `json:"sha256,omitempty"`
}

func fileDigest(path string) (string, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a path from this package's own ledger, inside the repository
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), true
}

// writeStagingLedger records the files about to be left in the tree.
func writeStagingLedger(dir string, written, inferred []string) error {
	entries := make([]stagedFile, 0, len(written)+len(inferred))
	for _, p := range written {
		if d, ok := fileDigest(p); ok {
			entries = append(entries, stagedFile{Path: p, SHA256: d})
		}
	}
	for _, p := range inferred {
		entries = append(entries, stagedFile{Path: p})
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, stagingLedger), raw, 0o600)
}

// clearStagingLedger is called once a run has removed its own files.
func clearStagingLedger(dir string) { _ = os.Remove(filepath.Join(dir, stagingLedger)) }

// recoverStaged removes what an interrupted run left behind and returns the
// files it could not prove were its own (inferred tsconfig.json files that
// still exist), for the caller to report.
func recoverStaged(dir, root string) (removed int, leftover []string) {
	raw, err := os.ReadFile(filepath.Join(dir, stagingLedger)) //nolint:gosec // G304: this package's own ledger
	if err != nil {
		return 0, nil
	}
	var entries []stagedFile
	if json.Unmarshal(raw, &entries) != nil {
		clearStagingLedger(dir)
		return 0, nil
	}
	for _, e := range entries {
		// The ledger sits in the repository's own directory, so its paths
		// are not trusted to point inside the repository: anything outside
		// is ignored.
		if !insideRootResolved(root, e.Path) {
			continue
		}
		fi, err := os.Lstat(e.Path)
		if err != nil {
			continue // already gone
		}
		// Only ever a regular file: a symlink planted at a staged path must
		// not lead the cleanup to whatever it points at.
		if !fi.Mode().IsRegular() {
			continue
		}
		if e.SHA256 == "" {
			leftover = append(leftover, e.Path)
			continue
		}
		if d, ok := fileDigest(e.Path); ok && d == e.SHA256 {
			if os.Remove(e.Path) == nil {
				removed++
			}
		}
	}
	clearStagingLedger(dir)
	return removed, leftover
}

// insideRootResolved is insideRoot after resolving symlinks in the root and in
// p's directory, so a symlinked folder inside the repository cannot carry a
// lexically contained path to a file outside it.
func insideRootResolved(root, p string) bool {
	if !insideRoot(root, p) {
		return false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return false
	}
	return insideRoot(realRoot, filepath.Join(realDir, filepath.Base(p)))
}

// insideRoot reports whether p is root or lies beneath it.
func insideRoot(root, p string) bool {
	if root == "" || !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
