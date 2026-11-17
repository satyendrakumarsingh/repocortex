package dryrun

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FilePatch is one file's part of a unified diff, applied.
type FilePatch struct {
	// Path is the file's path after the change (before it, for a deletion).
	Path string
	// Content is the file as the patch leaves it; empty for a deletion.
	Content string
	Added   bool
	Deleted bool
}

type hunk struct {
	oldStart int
	lines    []string // each with its ' ', '-' or '+' marker
}

type fileDiff struct {
	oldPath, newPath string
	hunks            []hunk
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// hunkSearch is how far from the line a hunk names its context is looked
// for. A diff made against a slightly different version of a file (an
// unrelated edit above it) still applies; one made against another file does
// not.
const hunkSearch = 200

// ApplyUnifiedDiff applies a unified diff (git diff, diff -u) to the files
// read returns, and returns each file as the patch leaves it.
//
// It is exact or it refuses: a hunk whose context and removed lines are not
// found near the line it names fails the whole call, naming the file and the
// hunk. A dry run of a patch that was applied wrongly would report on code
// nobody is about to write.
func ApplyUnifiedDiff(diff string, read func(path string) (string, error)) ([]FilePatch, error) {
	files, err := parseUnifiedDiff(diff)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no file in the diff: expected '--- a/path' and '+++ b/path' headers")
	}
	out := make([]FilePatch, 0, len(files))
	for _, f := range files {
		switch {
		case f.newPath == "":
			out = append(out, FilePatch{Path: f.oldPath, Deleted: true})
		case f.oldPath == "":
			var b strings.Builder
			for _, h := range f.hunks {
				for _, l := range h.lines {
					if strings.HasPrefix(l, "+") {
						b.WriteString(l[1:])
						b.WriteByte('\n')
					}
				}
			}
			out = append(out, FilePatch{Path: f.newPath, Content: b.String(), Added: true})
		default:
			old, err := read(f.oldPath)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.oldPath, err)
			}
			content, err := applyHunks(old, f.hunks)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.newPath, err)
			}
			out = append(out, FilePatch{Path: f.newPath, Content: content})
		}
	}
	return out, nil
}

func parseUnifiedDiff(diff string) ([]fileDiff, error) {
	var files []fileDiff
	var cur *fileDiff
	// Trailing newlines end the text, not a hunk: kept, the last would read
	// as an empty context line the file does not have.
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(diff, "\r\n", "\n"), "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ "):
			files = append(files, fileDiff{oldPath: diffPath(l[4:]), newPath: diffPath(lines[i+1][4:])})
			cur = &files[len(files)-1]
			i++
		case strings.HasPrefix(l, "@@"):
			if cur == nil {
				return nil, fmt.Errorf("a hunk before any file header")
			}
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				return nil, fmt.Errorf("unreadable hunk header %q", l)
			}
			start, _ := strconv.Atoi(m[1])
			cur.hunks = append(cur.hunks, hunk{oldStart: start})
		case cur != nil && len(cur.hunks) > 0 && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")):
			h := &cur.hunks[len(cur.hunks)-1]
			h.lines = append(h.lines, l)
		case cur != nil && len(cur.hunks) > 0 && l == "":
			// An empty context line some tools write without its space.
			h := &cur.hunks[len(cur.hunks)-1]
			h.lines = append(h.lines, " ")
		}
	}
	return files, nil
}

// diffPath strips the a/ or b/ prefix and any trailing timestamp, and maps
// /dev/null to "".
func diffPath(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		s = s[2:]
	}
	return strings.TrimPrefix(s, "./")
}

func applyHunks(old string, hunks []hunk) (string, error) {
	trailingNL := strings.HasSuffix(old, "\n")
	src := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	var out []string
	pos := 0 // next unconsumed line of src
	for n, h := range hunks {
		var want []string // the lines the hunk expects: context and removals
		for _, l := range h.lines {
			if l[0] == ' ' || l[0] == '-' {
				want = append(want, l[1:])
			}
		}
		at := findHunk(src, want, h.oldStart-1, pos)
		if at < 0 {
			return "", fmt.Errorf("hunk %d (at line %d) does not apply: its context is not in the file near that line", n+1, h.oldStart)
		}
		out = append(out, src[pos:at]...)
		for _, l := range h.lines {
			if l[0] == ' ' || l[0] == '+' {
				out = append(out, l[1:])
			}
		}
		pos = at + len(want)
	}
	out = append(out, src[pos:]...)
	res := strings.Join(out, "\n")
	if trailingNL {
		res += "\n"
	}
	return res, nil
}

// findHunk finds where want occurs in src, nearest to hint and not before
// from, within hunkSearch lines; -1 when it does not.
func findHunk(src, want []string, hint, from int) int {
	match := func(at int) bool {
		if at < from || at+len(want) > len(src) {
			return false
		}
		for i, w := range want {
			if strings.TrimRight(src[at+i], " \t") != strings.TrimRight(w, " \t") {
				return false
			}
		}
		return true
	}
	if hint < from {
		hint = from
	}
	for d := 0; d <= hunkSearch; d++ {
		if match(hint + d) {
			return hint + d
		}
		if d > 0 && match(hint-d) {
			return hint - d
		}
	}
	return -1
}
