package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	acqfs "github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/indexer"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scip"
	"github.com/satyendrakumarsingh/repocortex/internal/resolve/scipimport"
)

// CompilerRestoreOutcome is what RestoreCompilerProof did.
type CompilerRestoreOutcome struct {
	// Projects is how many cached indexes were read again.
	Projects int
	// Restored is the edges put back as exact (new or upgraded from heuristic).
	Restored int
	// Elapsed is the wall time of the whole step.
	Elapsed time.Duration
	// Err is the first failure; the step is best effort and never stops a refresh.
	Err error
}

// RestoreCompilerProof puts back the compiler-proven edges an incremental
// refresh deleted, from the indexes `resolve` cached, without running an
// indexer.
//
// THE LOSS. Replacing a changed file deletes its symbols and with them every
// edge into them, including the exact edges from files that did not change. The
// heuristic resolver then binds those references again at high or medium, so a
// call the compiler had proven stops being proven until the next `resolve`,
// which runs the indexer for minutes. Go does not have this problem (the type
// check is re-run in process); SCIP indexes do, and `watch` and `serve`'s
// per-request check never run `resolve`.
//
// THE REPAIR. For each project with a cached index, the index is read again and
// an edge is written when its source is in a document that still describes its
// file and its target is a definition in a document that does not (the files
// the refresh replaced). Everything about a document of a changed file except
// its definitions is ignored, because its line numbers describe the file as it
// was.
//
// STALENESS is decided by content hash, never by time. The sidecar written
// beside each index records the hash of every file the index was built from
// (cacheSidecar.Files); a document is fresh when the registry's current hash of
// that file is the same. A file edited and edited back is fresh again, which a
// modification time would call stale. An index with no recorded hashes (written
// before they were) is not used until the next `resolve` records them: the
// answer then stays heuristic, never wrong.
//
// It runs only when a file of a project's language under its directory was
// modified, so an edit to a README or to another language costs a directory
// listing. When it runs, the cost is the importer's fixed work on the run's
// tables plus two reads of the project's index. The fixed part, measured on a
// copy of this repository's own index (1,187 files, 14,607 symbols, 91,482
// parsed call sites, a one-document index): 0.8 to 1.1 s, of which reading the
// symbols is 0.18 s, the parser's call sites 0.5 s and marking the compiler's
// resolutions 0.35 s. That is paid once per project, not per file, and `serve`
// already spaces its checks by ten times their own duration. Reading the index
// is bounded by its size at the ingester's streaming rate (see
// internal/resolve/scip bench_test.go); the vscode index of 1.4 MB in this
// repository's cache is the largest one on this machine.
//
// It takes the same lock as RunCompilerIndexers: an index being rewritten is
// never read, and when a pass holds the lock the repair is skipped, because
// that pass is about to import a current index anyway.
func RestoreCompilerProof(ctx context.Context, reg *registry.Registry, runID int64, root string, changes []FileChange) CompilerRestoreOutcome {
	start := time.Now()
	var out CompilerRestoreOutcome
	defer func() { out.Elapsed = time.Since(start) }()

	modified := map[string]struct{}{}
	for _, ch := range changes {
		if ch.Kind == ChangeModified {
			modified[ch.Path] = struct{}{}
		}
	}
	if len(modified) == 0 || root == "" {
		return out
	}
	dir := filepath.Join(root, filepath.FromSlash(CompilerIndexDir))
	sidecars, err := filepath.Glob(filepath.Join(dir, "*.key"))
	if err != nil || len(sidecars) == 0 {
		return out
	}

	files, err := reg.Files(ctx, runID)
	if err != nil {
		out.Err = err
		return out
	}
	current := make(map[string]string, len(files))
	langOf := make(map[string]string, len(files))
	for _, f := range files {
		current[f.Path] = f.ContentHash
		langOf[f.Path] = f.Language
	}

	var todo []cacheSidecar
	for _, sc := range sidecars {
		rec, ok := readSidecar(sc)
		if !ok || rec.Files == nil || rec.Language == "" {
			continue
		}
		family := languageFamily(indexer.Language(rec.Language))
		prefix := ""
		if rec.Dir != "." && rec.Dir != "" {
			prefix = rec.Dir + "/"
		}
		for p := range modified {
			if family[langOf[p]] && (prefix == "" || strings.HasPrefix(p, prefix)) {
				todo = append(todo, rec)
				break
			}
		}
	}
	if len(todo) == 0 {
		return out
	}

	passLock, err := acqfs.TryLock(filepath.Join(dir, "pass.lock"))
	if err != nil {
		if !errors.Is(err, acqfs.ErrLocked) {
			out.Err = err
		}
		return out
	}
	defer func() { _ = passLock.Unlock() }()

	// One table read for every project, as resolve shares them.
	shared := &scipimport.SharedTables{}
	for _, rec := range todo {
		if err := ctx.Err(); err != nil {
			out.Err = err
			return out
		}
		proj := indexer.Project{Language: indexer.Language(rec.Language), Dir: rec.Dir}
		path := indexPath(dir, proj)
		// The index must still be the file the sidecar describes. The key is
		// not compared: it is expected to differ now, which is why this runs.
		if fi, err := os.Stat(path); err != nil || fi.Size() == 0 || fi.Size() != rec.Size {
			continue
		}
		recorded := rec.Files
		impOpts := scip.Options{RepoRoot: root, MaxBytes: scip.DefaultMaxBytes}
		if rec.Dir != "" && rec.Dir != "." {
			impOpts.PathPrefix = rec.Dir
		}
		res, err := scipimport.NewImporter(reg, runID, impOpts).ShareTables(shared).Restore(ctx, path, scipimport.RestoreSpec{
			Fresh: func(p string) bool {
				h, ok := recorded[p]
				cur, here := current[p]
				return ok && here && h == cur
			},
		})
		if err != nil {
			if out.Err == nil {
				out.Err = err
			}
			continue
		}
		out.Projects++
		out.Restored += res.Edges.Inserted + res.Edges.Upgraded
	}
	if out.Projects > 0 {
		// The same two steps resolve takes after an import: a heuristic call the
		// compiler's edge contradicts goes, and its reference moves to the
		// proven target.
		if _, _, err := reg.WithdrawContradictedCalls(ctx, runID); err != nil && out.Err == nil {
			out.Err = err
		}
	}
	return out
}
