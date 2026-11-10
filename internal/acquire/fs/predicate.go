package fs

// IgnorePredicate returns a function reporting whether a repo-relative path
// should be ignored, for callers that need ignore decisions outside a walk.
//
// The file watcher is the motivating caller. fsnotify has no recursive mode, so
// a watch must be registered per directory; registering one on .git or
// node_modules would consume scarce OS watch descriptors and deliver thousands
// of events nobody wants.
//
// SCOPE, and why it is not a weakness:
//
// This applies configuration-level include/exclude rules only. Per-directory
// .gitignore rule sets are assembled during a walk — each rule has to remember
// the directory its file came from, so `build/` in src/ does not match
// docs/build/ — and they cannot be reconstructed for an arbitrary path without
// walking to it.
//
// That is acceptable because the predicate is an OPTIMISATION, not the
// correctness boundary. A file that slips past it merely causes an incremental
// pass to run; Diff then re-walks with full gitignore semantics and classifies
// it correctly. The predicate can be too permissive without harming the index —
// it must simply never be too aggressive, which is why it consults the config
// exclusions a repository cannot weaken rather than any repo-supplied rule.
func (w *Walker) IgnorePredicate() func(relPath string, isDir bool) bool {
	return func(relPath string, isDir bool) bool {
		if relPath == "" || relPath == "." {
			return false
		}
		if w.configExcluded(relPath, isDir) {
			return true
		}
		// Include patterns apply to files only: a restrictive pattern such as
		// "**/*.go" matches no directory, so testing directories against it
		// would prune the tree at depth one.
		if !isDir && len(w.include) > 0 && !w.included(relPath) {
			return true
		}
		return false
	}
}
