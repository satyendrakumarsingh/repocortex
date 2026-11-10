//go:build unix

package security

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadFileContainedRefusesAFIFO: a FIFO with no writer would block a
// plain open forever. The read must return at once, refused.
func TestReadFileContainedRefusesAFIFO(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := ReadFileContained(root, "pipe")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("err = %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadFileContained blocked on a FIFO")
	}
}
