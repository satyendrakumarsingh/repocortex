package security

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var (
	// ErrNotRegular: the path names a directory, FIFO, device or socket. Only
	// regular files are read — a FIFO with no writer would block the reader,
	// and a device need never end.
	ErrNotRegular = errors.New("security: not a regular file")
	// ErrTooLarge: the file is larger than the WithMaxBytes cap.
	ErrTooLarge = errors.New("security: file exceeds the size limit")
)

// ReadOption configures ReadFileContained.
type ReadOption func(*readOptions)

type readOptions struct {
	maxBytes int64
}

// WithMaxBytes refuses, with ErrTooLarge, a file larger than n bytes, and
// never reads more than n+1 of them. n <= 0 means no cap.
func WithMaxBytes(n int64) ReadOption {
	return func(o *readOptions) {
		if n > 0 {
			o.maxBytes = n
		}
	}
}

// ReadFileContained reads requested (a path relative to repoRoot) and returns
// its bytes along with the symlink-resolved absolute path ValidatePath
// vouched for, which callers use to classify the file (a link named notes.txt
// that points at .env is still a .env).
//
// ValidatePath answers "does this name stay inside the root" at one instant;
// reading by the resolved path afterwards leaves a window in which a component
// can be swapped for a symlink or a Windows junction. So the validation is kept
// for its error sentinels and messages, and the read itself goes through
// os.Root, which refuses at open time any component that leads outside the
// directory — closing the check-then-open race.
//
// The relative path handed to the Root is the cleaned request, not the
// ValidatePath result, so the two checks are independent: either one alone
// rejects an escape.
//
// Only a regular file is read (ErrNotRegular otherwise). The open is
// non-blocking on Unix, so a FIFO swapped in after any earlier check cannot
// hang the reader; the type is judged on the opened handle, not on a name.
func ReadFileContained(repoRoot, requested string, opts ...ReadOption) (data []byte, resolved string, err error) {
	var o readOptions
	for _, opt := range opts {
		opt(&o)
	}
	resolved, err = ValidatePath(repoRoot, requested)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %q: %w", ErrBadRoot, repoRoot, err)
	}
	defer func() { _ = root.Close() }()

	f, err := openForRead(root, filepath.Clean(filepath.FromSlash(requested)))
	if err != nil {
		return nil, "", fmt.Errorf("read %q within %q: %w", requested, repoRoot, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("read %q within %q: %w", requested, repoRoot, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, "", fmt.Errorf("read %q within %q: %w (%s)", requested, repoRoot, ErrNotRegular, fi.Mode().Type())
	}
	if o.maxBytes > 0 && fi.Size() > o.maxBytes {
		return nil, "", fmt.Errorf("read %q within %q: %w: %d bytes, limit %d",
			requested, repoRoot, ErrTooLarge, fi.Size(), o.maxBytes)
	}

	var r io.Reader = f
	if o.maxBytes > 0 {
		// The file can grow between Stat and read; never take more than one
		// byte past the cap.
		r = io.LimitReader(f, o.maxBytes+1)
	}
	data, err = io.ReadAll(r)
	if err != nil {
		return nil, "", fmt.Errorf("read %q within %q: %w", requested, repoRoot, err)
	}
	if o.maxBytes > 0 && int64(len(data)) > o.maxBytes {
		return nil, "", fmt.Errorf("read %q within %q: %w: limit %d", requested, repoRoot, ErrTooLarge, o.maxBytes)
	}
	return data, resolved, nil
}
