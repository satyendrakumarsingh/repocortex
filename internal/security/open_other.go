//go:build !unix

package security

import "os"

// openForRead opens rel for reading. Off Unix there are no FIFOs in the
// filesystem namespace to block on, so a plain open suffices.
func openForRead(root *os.Root, rel string) (*os.File, error) {
	return root.Open(rel)
}
