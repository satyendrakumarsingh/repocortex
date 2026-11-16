package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// History through the git binary, for repositories go-git declines.
//
// go-git refuses a repository that uses an extension it does not implement,
// and `extensions.worktreeConfig` is the common one: git turns it on for
// repositories with linked worktrees. Such a repository opens in degraded mode
// (HEAD is read from the files), and before this every history-backed answer
// on it was empty: hotspots, bus factor, owners, blame and co-change. Measured
// on a 25,946-file monorepo with seven worktrees, all five reported
// "No history was read".
//
// The binary is used only in that case, and only to read. Every option that
// can make `git log` run a program the repository configures is turned off:
// fsmonitor hooks, signature verification (gpg), external diff drivers and
// textconv filters. Paths follow the go-git walk's rules exactly: diffs
// against the first parent only, renames not detected, and no paths for a
// commit at a shallow clone's boundary (see Commit.Paths).

// gitBinary is the git executable, or "" when none is on PATH. A variable so
// a test can make it absent.
var gitBinary = func() string {
	p, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	return p
}

// binaryProbe remembers whether the git binary accepts a repository, so the
// check runs once per Repo rather than once per history call.
type binaryProbe struct {
	once sync.Once
	ok   bool
}

// historyViaBinary reports whether history for this repository is read with
// the git binary: go-git declined it, a git executable is available, and git
// itself accepts the directory. The last check matters: a .git with no object
// store is refused by go-git and by git alike, and must stay "not a
// repository" rather than read as one with no commits.
func (r *Repo) historyViaBinary() bool {
	if !r.isRepo || r.repo != nil || r.degraded == "" || r.probe == nil || gitBinary() == "" {
		return false
	}
	r.probe.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := r.gitReadOnly(ctx, "rev-parse", "--git-dir")
		r.probe.ok = err == nil
	})
	return r.probe.ok
}

// HistoryAvailable reports whether commit history can be read at all.
func (r *Repo) HistoryAvailable() bool {
	return r.isRepo && (r.repo != nil || r.historyViaBinary())
}

// HistorySource says how history is read: "go-git", "git binary" or "".
func (r *Repo) HistorySource() string {
	switch {
	case r.isRepo && r.repo != nil:
		return "go-git"
	case r.historyViaBinary():
		return "git binary"
	}
	return ""
}

// gitReadOnly runs a read-only git command in the repository and returns its
// standard output.
func (r *Repo) gitReadOnly(ctx context.Context, args ...string) ([]byte, error) {
	bin := gitBinary()
	if bin == "" {
		return nil, errors.New("git: no git executable on PATH")
	}
	full := append([]string{
		"-C", r.root,
		"-c", "core.fsmonitor=false",
		"-c", "log.showSignature=false",
		"--no-pager",
	}, args...)
	cmd := exec.CommandContext(ctx, bin, full...) //nolint:gosec // G204: fixed git subcommands with arguments this package builds
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(firstLineOf(stderr.String())))
	}
	return out, nil
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
	bodyEnd   = "\x1d"
)

// recentCommitsCLI is recentCommits read with the git binary.
func (r *Repo) recentCommitsCLI(ctx context.Context, limit int, withPaths bool) ([]Commit, error) {
	if _, err := r.gitReadOnly(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil // no commits yet, the same answer the go-git walk gives
	}
	args := []string{"log", "-n", strconv.Itoa(limit),
		"--no-show-signature", "--no-ext-diff", "--no-textconv", "--no-renames",
		"--format=" + recordSep + "%H" + fieldSep + "%an" + fieldSep + "%ae" + fieldSep + "%at" + fieldSep + "%B" + bodyEnd,
	}
	if withPaths {
		args = append(args, "--name-only", "--diff-merges=first-parent")
	}
	args = append(args, "HEAD", "--")
	out, err := r.gitReadOnly(ctx, args...)
	if err != nil {
		return nil, err
	}
	boundary := r.shallowSet()
	commits := make([]Commit, 0, limit)
	for _, rec := range strings.Split(string(out), recordSep) {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		head, rest, _ := strings.Cut(rec, bodyEnd)
		f := strings.SplitN(head, fieldSep, 5)
		if len(f) < 5 {
			return nil, fmt.Errorf("git log: unexpected record %q", firstLineOf(rec))
		}
		when, _ := strconv.ParseInt(f[3], 10, 64)
		c := Commit{
			SHA: f[0], AuthorName: f[1], AuthorEmail: f[2], When: when,
			Subject:         subject(f[4]),
			Constraints:     extractAntiRegressionConstraints(f[4]),
			ShallowBoundary: boundary[f[0]],
		}
		// A boundary commit has no parent to diff against; git would list its
		// whole tree as "added", which credits one author with everything.
		if withPaths && !c.ShallowBoundary {
			c.Paths = parsePaths(rest)
		}
		commits = append(commits, c)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return commits, nil
}

func parsePaths(block string) []string {
	var paths []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(block))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		p := strings.TrimSpace(sc.Text())
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// fileLastModifiedCLI is FileLastModified read with the git binary.
func (r *Repo) fileLastModifiedCLI(ctx context.Context, path string) (FileRevision, error) {
	out, err := r.gitReadOnly(ctx, "log", "-1", "--no-show-signature", "--format=%H"+fieldSep+"%at", "HEAD", "--", path)
	if err != nil {
		return FileRevision{}, err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		if r.Shallow() {
			return FileRevision{}, ErrShallowHistory
		}
		return FileRevision{}, ErrNoHistory
	}
	sha, ts, _ := strings.Cut(line, fieldSep)
	when, _ := strconv.ParseInt(ts, 10, 64)
	return FileRevision{SHA: sha, When: when}, nil
}

// shallowSet is the set of commits at a shallow clone's boundary, read from
// the shallow file git keeps (in the common directory for a linked worktree).
func (r *Repo) shallowSet() map[string]bool {
	set := map[string]bool{}
	if r.gitDir == "" {
		return set
	}
	dirs := []string{r.gitDir}
	if raw, err := os.ReadFile(filepath.Join(r.gitDir, "commondir")); err == nil { //nolint:gosec // G304: a fixed file inside the repository's git directory
		common := strings.TrimSpace(string(raw))
		if !filepath.IsAbs(common) {
			common = filepath.Join(r.gitDir, common)
		}
		dirs = append(dirs, common)
	}
	for _, d := range dirs {
		raw, err := os.ReadFile(filepath.Join(d, "shallow")) //nolint:gosec // G304: a fixed file inside the repository's git directory
		if err != nil {
			continue
		}
		for _, l := range strings.Fields(string(raw)) {
			set[l] = true
		}
	}
	return set
}
