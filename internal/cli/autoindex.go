package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/autorun"
	"github.com/spf13/cobra"
)

func newAutoIndexCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "auto-index",
		Short:  "Execute serialized background re-indexing with lock folding",
		Hidden: true, // internal command invoked by git hooks and file watchers
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := g.Config().RepoRoot
			metaDir := filepath.Join(root, ".repocortex")
			logDir := filepath.Join(metaDir, "logs")
			if err := os.MkdirAll(logDir, 0o750); err != nil {
				return err
			}

			lockPath := filepath.Join(metaDir, "reindex.lock")
			pendingPath := filepath.Join(metaDir, "reindex.pending")
			logPath := filepath.Join(logDir, "auto-index.log")

			logFile, err := os.OpenFile(filepath.Clean(logPath), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				defer func() { _ = logFile.Close() }()
			}
			logMsg := func(format string, args ...any) {
				msg := fmt.Sprintf("[%s] ", time.Now().Format(time.RFC3339)) + fmt.Sprintf(format, args...) + "\n"
				if logFile != nil {
					_, _ = logFile.WriteString(msg)
				}
			}

			// pass runs index and resolve once, under the lock, and records how
			// it went where `status` and the MCP provenance read it.
			pass := func() {
				start := time.Now()
				logMsg("starting background auto-index pass")

				// Execute incremental index. The subcommands are called directly,
				// not through cobra, so they inherit nothing: without this
				// context the store opens its database with a nil context
				// and panics, and a hook's background run dies silently.
				ctx := cmd.Context()
				if ctx == nil {
					ctx = context.Background()
				}
				// This process holds reindex.lock for the whole pass.
				ctx = withIndexLockHeld(ctx)
				idxCmd := newIndexCmd(g)
				idxCmd.SetContext(ctx)
				idxCmd.SetOut(logWriter(logFile))
				idxCmd.SetErr(logWriter(logFile))
				var failure error
				if err := idxCmd.RunE(idxCmd, nil); err != nil {
					failure = fmt.Errorf("index: %w", err)
				} else {
					resCmd := newResolveCmd(g)
					resCmd.SetContext(ctx)
					resCmd.SetOut(logWriter(logFile))
					resCmd.SetErr(logWriter(logFile))
					if err := resCmd.RunE(resCmd, nil); err != nil {
						failure = fmt.Errorf("resolve: %w", err)
					}
				}
				took := time.Since(start)
				if failure != nil {
					logMsg("auto-index failed at %v", failure)
				} else {
					logMsg("auto-index completed successfully in %v", took)
				}
				_ = autorun.Write(metaDir, start, took, failure)
			}

			// A trigger that arrives while a pass runs is folded into the next
			// pass by the reindex.pending flag. One that arrives after the last
			// check of that flag but before the lock is released finds the lock
			// held, sets the flag and leaves, and nobody would act on it. So
			// the flag is checked again once the lock is let go, and a pass is
			// started for it if no other hook has taken the lock meanwhile.
			for {
				lock, err := fs.TryLock(lockPath)
				if err != nil {
					if errors.Is(err, fs.ErrLocked) {
						// Another indexer holds the lock. Set pending flag so it folds our trigger.
						logMsg("another auto-index holds lock; set reindex.pending and folded")
						_ = os.WriteFile(pendingPath, []byte(fmt.Sprintf("%d\n", time.Now().Unix())), 0o600)
						return nil
					}
					logMsg("failed to acquire lock: %v", err)
					return err
				}
				// Loop while pending triggers exist to fold all overlapping requests.
				for {
					_ = os.Remove(pendingPath)
					pass()
					if _, err := os.Stat(pendingPath); os.IsNotExist(err) {
						break
					}
					logMsg("pending triggers detected; repeating auto-index pass")
				}
				_ = lock.Unlock()
				if _, err := os.Stat(pendingPath); os.IsNotExist(err) {
					return nil
				}
				logMsg("a trigger arrived as the lock was released; taking it")
			}
		},
	}
	return cmd
}

// logWriter is where a subcommand's output goes. When the log file could not
// be opened, a nil *os.File would make every write fail, so output is
// discarded instead: the run itself must still happen.
func logWriter(f *os.File) io.Writer {
	if f == nil {
		return io.Discard
	}
	return f
}
