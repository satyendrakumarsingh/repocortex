package api

import (
	"path"
	"regexp"
	"strings"
)

// JSMount is one app.use('/prefix', router) call across files.
type JSMount struct {
	Parent string
	Child  string
	Prefix string
	Line   int
}

// JSRouters is what one JS/TS file says about its Express routers and mounts.
type JSRouters struct {
	Mounts  []JSMount
	Imports map[string]string // variableName -> relative module path (e.g. "./routes/users")
}

var (
	jsRequireImport = regexp.MustCompile(`(?:const|let|var)\s+([a-zA-Z_$][\w$]*)\s*=\s*require\s*\(\s*['"]([^'"]+)['"]\s*\)`)
	jsES6Import     = regexp.MustCompile(`import\s+(?:([a-zA-Z_$][\w$]*)|(?:\*\s+as\s+([a-zA-Z_$][\w$]*)))\s+from\s+['"]([^'"]+)['"]`)
	jsMountUse      = regexp.MustCompile(`\b([a-zA-Z_$][\w$]*)\s*\.\s*use\s*\(\s*['"]([^'"]+)['"]\s*,\s*([a-zA-Z_$][\w$]*)\s*\)`)
)

// ExpressRouters reads one JS/TS file's imports and app.use mounts.
func ExpressRouters(src []byte) JSRouters {
	lines := strings.Split(string(src), "\n")
	out := JSRouters{
		Imports: make(map[string]string),
	}
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") {
			continue
		}
		// CJS require
		if m := jsRequireImport.FindStringSubmatch(t); len(m) > 2 {
			out.Imports[m[1]] = m[2]
		}
		// ESM import
		if m := jsES6Import.FindStringSubmatch(t); len(m) > 3 {
			varName := m[1]
			if varName == "" {
				varName = m[2]
			}
			out.Imports[varName] = m[3]
		}
		// app.use('/prefix', router)
		if m := jsMountUse.FindStringSubmatch(t); len(m) > 3 {
			out.Mounts = append(out.Mounts, JSMount{
				Parent: m[1],
				Prefix: m[2],
				Child:  m[3],
				Line:   i + 1,
			})
		}
	}
	return out
}

// ResolveJSModulePath finds the target file path in the workspace for a relative module import.
func ResolveJSModulePath(importerPath, modImport string, allFiles []string) string {
	if !strings.HasPrefix(modImport, ".") {
		return ""
	}
	dir := path.Dir(importerPath)
	targetBase := path.Clean(path.Join(dir, modImport))

	candidates := []string{
		targetBase,
		targetBase + ".js",
		targetBase + ".ts",
		targetBase + ".mjs",
		targetBase + ".cjs",
		path.Join(targetBase, "index.js"),
		path.Join(targetBase, "index.ts"),
	}

	for _, cand := range candidates {
		for _, f := range allFiles {
			if f == cand {
				return f
			}
		}
	}
	return ""
}
