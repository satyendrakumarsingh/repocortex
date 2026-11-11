package watch

import (
	"testing"

	"github.com/fsnotify/fsnotify"
)

// TestFsnotifyBackendRemoveStopsWatchingADirectory: a directory that was added
// and then removed must be released, and removing one that was never watched
// must report an error rather than succeed silently.
func TestFsnotifyBackendRemoveStopsWatchingADirectory(t *testing.T) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Skipf("no fsnotify backend here: %v", err)
	}
	var b fsnotifyBackend
	b.w = w
	t.Cleanup(func() { _ = b.Close() })

	dir := t.TempDir()
	if err := b.Add(dir); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := releaseDir(b, dir); err != nil {
		t.Fatalf("Remove of a watched directory: %v", err)
	}
	if err := releaseDir(b, dir); err == nil {
		t.Error("Remove of a directory that is no longer watched reported success")
	}
}

// releaseDir is the call a watcher makes when a directory leaves the tree.
func releaseDir(b fsnotifyBackend, dir string) error { return b.Remove(dir) }
