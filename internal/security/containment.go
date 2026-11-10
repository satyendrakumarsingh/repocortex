package security

// The errs-typed containment check for path arguments arriving from outside.
//
// IT HAS NO CALL SITES YET. Task 2's sweep found none to convert: every
// existing caller-supplied path already goes through ValidatePath, and moving
// one would break callers matching its sentinel errors with errors.Is. This
// file previously opened by claiming it was "used by every entry point", which
// was a documentation overclaim of exactly the kind this package's own
// responses are audited for. It is here for NEW call sites; pick it over
// ValidatePath when writing one, and see the differences below before
// swapping it into an old one.
//
// Two traps this closes, both of which a "does it start with the root" check
// misses:
//
//  1. os.Stat and os.File.IsDir FOLLOW symlinks. A path with no ".." in it and
//     no leading slash can still name a file outside the root, because a
//     component of it is a link. The fix is to resolve the whole path first and
//     compare the RESOLVED value, and to reject a symlinked final component
//     outright rather than trusting where it points today.
//
//  2. Checking a path and then opening it is a race: the check passes, the
//     attacker swaps the entry for a symlink, the open follows it. OpenContained
//     re-checks the handle it actually obtained rather than the name it was
//     given.
//
// Refusals are errs.KindInvalid: the caller has to change the input, and
// retrying the same path is pointless.
//
// This sits alongside ValidatePath in paths.go rather than replacing it.
// ValidatePath predates errs and returns sentinel errors an existing caller
// may already be matching on with errors.Is; ContainedPath is the errs-typed
// entry point for new call sites. Both share the same containment logic
// (the within helper below), so they cannot drift apart on what "inside the
// root" means.
//
// DIFFERENCES FROM ValidatePath. They are NOT interchangeable, and every
// difference below makes ContainedPath the stricter of the two:
//
//  1. EXISTENCE. ContainedPath requires the path to exist — it resolves
//     symlinks, so a missing file is errs.KindNotFound. ValidatePath is purely
//     textual and happily validates a path that is not there yet, which is
//     what a caller creating a file needs.
//
//  2. SYMLINKED FINAL COMPONENT. ContainedPath refuses one outright, because
//     where it points now is not where it points after the next syscall.
//     ValidatePath accepts a symlink whose target stays inside the root.
//
//  3. LITERAL "..". ContainedPath rejects any ".." component even when it
//     cancels out ("a/../a.txt"), so a hostile-looking path never reaches a
//     stat. ValidatePath Cleans first and accepts what resolves inside.
//
//  4. NUL. Both guard it explicitly. ValidatePath always did; ContainedPath
//     now does too, so its refusal names the reason rather than surfacing
//     whatever the OS says about an invalid argument.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// ContainedPath resolves candidate relative to root and returns its absolute,
// symlink-resolved form, or an error if it escapes.
//
// candidate must be relative. An absolute candidate is refused even when it
// happens to point inside root, because accepting it invites callers to build
// paths by concatenation somewhere else.
func ContainedPath(root, candidate string) (string, error) {
	const op = "security.ContainedPath"

	if candidate == "" {
		return "", errs.New(errs.KindInvalid, op, "path must not be empty")
	}
	if filepath.IsAbs(candidate) {
		return "", errs.New(errs.KindInvalid, op, "path must be relative to the project root").
			WithField("path", candidate)
	}
	// A NUL truncates the path in any syscall that sees it, so what the OS
	// opens is not what was checked. Rejected explicitly rather than left to
	// the syscall, so the refusal says why.
	if strings.ContainsRune(candidate, 0) {
		return "", errs.New(errs.KindInvalid, op, "path must not contain a NUL byte").
			WithField("path", candidate)
	}
	// Reject the textual escape before touching the filesystem, so an obviously
	// hostile path never causes a stat at all.
	for _, part := range strings.Split(filepath.ToSlash(candidate), "/") {
		if part == ".." {
			return "", errs.New(errs.KindInvalid, op, "path must not contain '..'").
				WithField("path", candidate)
		}
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errs.Wrap(errs.KindInvalid, op, err)
	}
	realRoot, err = filepath.Abs(realRoot)
	if err != nil {
		return "", errs.Wrap(errs.KindInvalid, op, err)
	}

	joined := filepath.Join(realRoot, filepath.FromSlash(candidate))

	// Refuse a symlinked FINAL component before resolving it. Where it points
	// now is not where it will point after the next syscall.
	if lst, lerr := os.Lstat(joined); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
		return "", errs.New(errs.KindInvalid, op, "path is a symlink; symlinked entries are not read").
			WithField("path", candidate)
	}

	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errs.Wrap(errs.KindNotFound, op, err)
		}
		return "", errs.Wrap(errs.KindInvalid, op, err)
	}

	if !within(realRoot, resolved) {
		return "", errs.New(errs.KindInvalid, op, "path resolves outside the project root").
			WithField("path", candidate)
	}
	return resolved, nil
}

// OpenContained opens candidate for reading after containment, then verifies
// the handle it actually got is the regular file it checked.
//
// The post-open verification is the part that matters: between ContainedPath
// returning and the open completing, the entry can be replaced. Comparing the
// opened handle's identity against the checked path closes that window.
func OpenContained(root, candidate string) (*os.File, error) {
	const op = "security.OpenContained"

	resolved, err := ContainedPath(root, candidate)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(resolved) // #nosec G304 -- resolved by ContainedPath above
	if err != nil {
		return nil, errs.Wrap(errs.KindInvalid, op, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errs.Wrap(errs.KindInvalid, op, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errs.New(errs.KindInvalid, op, "path is not a regular file").
			WithField("path", candidate)
	}
	return f, nil
}
