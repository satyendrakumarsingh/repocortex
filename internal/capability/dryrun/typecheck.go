package dryrun

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/resolve/gotypes"
)

// typeCheckTimeout bounds one of the two checks a Go patch costs. It is a
// tool call, not resolve: an answer in a minute is worth more than a complete
// one in five.
const typeCheckTimeout = 60 * time.Second

// GoBreakage is one compile error a patch introduces.
type GoBreakage struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Msg  string `json:"message"`
}

// TypeCheckPatch reports the type errors a Go patch introduces, without
// writing anything: the files in scope are type-checked as they are and again
// with overlay (repo path -> new content) in place of their text, and only
// the errors the second run has and the first does not are returned. A
// repository that already fails to compile somewhere (a missing dependency,
// generated code not generated) is not blamed on the patch.
//
// scope is the repo-relative Go files whose packages are checked: the patched
// files and the files that import their packages, so a caller in another
// package that stops compiling is found.
//
// Errors are matched by file and message, not by line: the patch moves lines
// in the files it touches, and an error that only moved is not new.
func TypeCheckPatch(ctx context.Context, root string, scope []string, overlay map[string]string) ([]GoBreakage, error) {
	if len(overlay) == 0 {
		return nil, nil
	}
	ov := make(map[string][]byte, len(overlay))
	for p, c := range overlay {
		ov[p] = []byte(c)
	}
	before, err := gotypes.Check(ctx, gotypes.Options{Root: root, Files: scope, CollectErrors: true, Timeout: typeCheckTimeout})
	if err != nil {
		return nil, fmt.Errorf("type check before the patch: %w", err)
	}
	after, err := gotypes.Check(ctx, gotypes.Options{Root: root, Files: scope, CollectErrors: true, Timeout: typeCheckTimeout, Overlay: ov})
	if err != nil {
		return nil, fmt.Errorf("type check with the patch: %w", err)
	}
	type key struct{ path, msg string }
	had := map[key]int{}
	for _, e := range before.TypeErrors {
		had[key{e.Path, e.Msg}]++
	}
	var out []GoBreakage
	for _, e := range after.TypeErrors {
		k := key{e.Path, e.Msg}
		if had[k] > 0 {
			had[k]--
			continue
		}
		out = append(out, GoBreakage{Path: e.Path, Line: e.Line, Msg: e.Msg})
	}
	return out, nil
}

// GoImportPath is the import path of the package in a repository directory,
// read from the nearest go.mod above it; "" when there is none.
func GoImportPath(root, relDir string) string {
	dir := filepath.ToSlash(filepath.Clean(relDir))
	for d := dir; ; d = path.Dir(d) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(d), "go.mod")) //nolint:gosec // the repository's own go.mod
		if err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
					mod := strings.Trim(strings.TrimSpace(rest), `"`)
					sub := strings.TrimPrefix(strings.TrimPrefix(dir, d), "/")
					if d == "." {
						sub = dir
						if sub == "." {
							sub = ""
						}
					}
					if sub == "" {
						return mod
					}
					return mod + "/" + sub
				}
			}
			return ""
		}
		if d == "." || d == "/" || d == "" {
			return ""
		}
	}
}
