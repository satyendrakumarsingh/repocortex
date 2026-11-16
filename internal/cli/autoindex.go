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
			defer func() { _ = lock.Unlock() }()

			// Loop while pending triggers exist to fold all overlapping requests
			for {
				_ = os.Remove(pendingPath)
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
				idxCmd := newIndexCmd(g)
				idxCmd.SetContext(ctx)
				idxCmd.SetOut(logWriter(logFile))
				idxCmd.SetErr(logWriter(logFile))
				idxErr := idxCmd.RunE(idxCmd, nil)

				var resErr error
				if idxErr == nil {
					resCmd := newResolveCmd(g)
					resCmd.SetContext(ctx)
					resCmd.SetOut(logWriter(logFile))
					resCmd.SetErr(logWriter(logFile))
					resErr = resCmd.RunE(resCmd, nil)
				}

				duration := time.Since(start)
				finalStatus := "ok"
				if idxErr != nil {
					finalStatus = fmt.Sprintf("index_error: %v", idxErr)
					logMsg("auto-index failed at index: %v", idxErr)
				} else if resErr != nil {
					finalStatus = fmt.Sprintf("resolve_error: %v", resErr)
					logMsg("auto-index failed at resolve: %v", resErr)
				} else {
					logMsg("auto-index completed successfully in %v", duration)
				}

				statusFile := filepath.Join(metaDir, "reindex.status")
				statusContent := fmt.Sprintf("timestamp=%s\nduration_ms=%d\nstatus=%s\n",
					start.Format(time.RFC3339),
					duration.Milliseconds(),
					finalStatus,
				)
				_ = os.WriteFile(statusFile, []byte(statusContent), 0o600)

				// Check if another trigger arrived while we were indexing
				if _, err := os.Stat(pendingPath); os.IsNotExist(err) {
					break
				}
				logMsg("pending triggers detected; repeating auto-index pass")
			}
			return nil
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
