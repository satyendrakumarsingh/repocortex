//go:build unix

package security

import (
	"os"
	"syscall"
)

// openForRead opens rel for reading with O_NONBLOCK, so that a FIFO or device
// returns at once instead of blocking until a writer appears. O_NONBLOCK has
// no effect on reads of a regular file, and the caller refuses anything that
// is not one.
func openForRead(root *os.Root, rel string) (*os.File, error) {
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
