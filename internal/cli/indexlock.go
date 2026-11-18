package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
)

// indexLockWait is how long `index` and `resolve` wait for another writer.
//
// 30 seconds: long enough for the passes that are normally short (a git hook's
// or the watcher's incremental pass, another incremental `index`) to finish
// and let this command run on top of their result, and short enough that a
// script or CI job queued behind a full rebuild or a wedged process gets a
// clear message naming the holder instead of hanging in silence. A full
// rebuild can run longer than this, and then the second command stopping with
// "another index is running" is the right answer, not waiting behind it.
// A var so tests need not wait it out.
var indexLockWait = 30 * time.Second

type indexLockHeldKey struct{}

// withIndexLockHeld marks ctx as belonging to a caller that already holds the
// write lock (the hook's background pass), so the commands it runs in-process
// do not wait on a lock their own process holds.
func withIndexLockHeld(ctx stdctx.Context) stdctx.Context {
	return stdctx.WithValue(ctx, indexLockHeldKey{}, true)
}

// lockIndex takes .repocortex/reindex.lock, the one write lock that `serve`'s
// edit refresher, `watch` and the git hooks also take, and returns what
// releases it. It is held for the whole of a write command, and reclaiming
// abandoned runs happens only under it: with the lock held, a run that never
// committed is really abandoned, and without it the same rows may belong to a
// live indexer in another process.
//
// The lock is advisory and tied to the process, so a crashed holder never
// leaves it stuck.
func lockIndex(ctx stdctx.Context, dbPath string) (func(), error) {
	if held, _ := ctx.Value(indexLockHeldKey{}).(bool); held {
		return func() {}, nil
	}
	lockPath := filepath.Join(filepath.Dir(dbPath), "reindex.lock")
	lock, err := lockWithin(ctx, lockPath, indexLockWait)
	if err != nil {
		if errors.Is(err, fs.ErrLocked) {
			return nil, fmt.Errorf("another index is running (%s, lock %s); waited %s. "+
				"Let it finish, or stop it, then run this again",
				lockHolder(lockPath), lockPath, indexLockWait)
		}
		return nil, fmt.Errorf("take the index lock: %w", err)
	}
	// Best effort: names this process for whoever has to wait on it.
	_ = os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	return func() { _ = lock.Unlock() }, nil
}

// lockHolder names the process that wrote its pid into the lock file.
func lockHolder(lockPath string) string {
	b, err := os.ReadFile(filepath.Clean(lockPath))
	if err != nil {
		return "holder unknown"
	}
	if pid := strings.TrimSpace(string(b)); pid != "" {
		return "pid " + pid
	}
	return "holder unknown"
}
