package api

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// Express routers mounted from another file.
//
// An Express application declares each router in its own module and mounts it
// from the application root:
//
//	const users = require('./routes/users');
//	app.use('/api/users', requireAuth, users);
//
// Extract reads one file at a time, so the route in users.js is "GET /:id".
// What a file says about its routers is read here; a pass that sees the whole
// run joins the files, as it does for FastAPI and Flask.

// JSMount is one `parent.use('/prefix', ..., child)` call.
type JSMount struct {
	// Parent is the app or router the child is mounted on.
	Parent string
	// Child is the mounted router as written: `users`, `routes.users`. Empty
	// when the router is required inline, and then ChildModule is set.
	Child string
	// ChildModule is the module of `app.use('/x', require('./x'))`.
	ChildModule string
	Prefix      string
	Line        int
}

// JSImport is one name an import or require binds. Name is the export it
// names; empty for the module's default export or the whole module.
type JSImport struct {
	Module string
	Name   string
}

// JSRouters is what one JS/TS file says about its Express routers.
type JSRouters struct {
	Mounts []JSMount
	// Imports maps each local name to what it was imported as.
	Imports map[string]JSImport
	// Prefixes is the prefix each router variable carries within the file,
	// from mounts in the same file.
	Prefixes map[string]string
	// Receivers maps a route call's line (1-based, as Route.Line) to the
	// router variable it is called on.
	Receivers map[int]string
	// Default is the variable the module exports as its default (or as
	// module.exports), "" when the file does not say.
	Default string
	// Exports maps an exported name to the local variable it exports.
	Exports map[string]string
}

var (
	jsRequireImport = regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+([a-zA-Z_$][\w$]*)\s*=\s*require\s*\(\s*['"]([^'"]+)['"]\s*\)\s*(?:\.\s*([a-zA-Z_$][\w$]*))?`)
	jsRequireDestr  = regexp.MustCompile(`^(?:const|let|var)\s*\{([^}]*)\}\s*=\s*require\s*\(\s*['"]([^'"]+)['"]\s*\)`)
	jsImportFrom    = regexp.MustCompile(`^import\s+(?:type\s+)?(.+?)\s+from\s+['"]([^'"]+)['"]`)
	jsUseStart      = regexp.MustCompile(`\b([a-zA-Z_$][\w$]*)\s*\.\s*use\s*\(`)
	jsInlineRequire = regexp.MustCompile(`^require\s*\(\s*['"]([^'"]+)['"]\s*\)$`)
	jsIdentPath     = regexp.MustCompile(`^[a-zA-Z_$][\w$]*(?:\.[a-zA-Z_$][\w$]*)*$`)
	jsQuoted        = regexp.MustCompile(`^['"` + "`" + `]([^'"` + "`" + `$]*)['"` + "`" + `]$`)
	jsModuleExports = regexp.MustCompile(`^(?:module\.)?exports\s*=\s*([a-zA-Z_$][\w$]*)\s*;?$`)
	jsNamedExport   = regexp.MustCompile(`^(?:module\.)?exports\.([a-zA-Z_$][\w$]*)\s*=\s*([a-zA-Z_$][\w$]*)\s*;?$`)
	jsExportDefault = regexp.MustCompile(`^export\s+default\s+([a-zA-Z_$][\w$]*)\s*;?$`)
	jsExportDecl    = regexp.MustCompile(`^export\s+(?:const|let|var)\s+([a-zA-Z_$][\w$]*)\b`)
	jsExportList    = regexp.MustCompile(`^export\s*\{([^}]*)\}\s*;?$`)
	jsObjectExports = regexp.MustCompile(`^module\.exports\s*=\s*\{([^}]*)\}\s*;?$`)
)

// ExpressRouters reads one JS/TS file's imports, exports, route receivers and
// app.use mounts.
func ExpressRouters(src []byte) JSRouters {
	lines := strings.Split(string(src), "\n")
	out := JSRouters{
		Imports:   map[string]JSImport{},
		Prefixes:  map[string]string{},
		Receivers: map[int]string{},
		Exports:   map[string]string{},
	}
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
			continue
		}
		out.readImport(t)
		out.readExport(t)
		if m := expressRouteStart.FindStringSubmatch(t); m != nil && !expressNoiseReceivers[m[1]] {
			out.Receivers[i+1] = m[1]
		}
		m := jsUseStart.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		call, _ := readBalancedParen(lines, i, 10)
		if mount, ok := parseUse(m[1], call); ok {
			mount.Line = i + 1
			out.Mounts = append(out.Mounts, mount)
		}
	}
	// In-file prefixes, applied in source order so a chain declared top-down
	// composes: app.use('/api', api); api.use('/v1', v1).
	for _, m := range out.Mounts {
		if m.Child != "" && !strings.Contains(m.Child, ".") {
			out.Prefixes[m.Child] = joinRoutePath(out.Prefixes[m.Parent], m.Prefix)
		}
	}
	return out
}

// parseUse reads a use() call: a quoted path first, the router last, any
// middleware between.
func parseUse(parent, call string) (JSMount, bool) {
	open := strings.Index(call, "(")
	if open < 0 {
		return JSMount{}, false
	}
	args := splitTopLevelArgs(call[open+1:])
	if len(args) < 2 {
		return JSMount{}, false
	}
	q := jsQuoted.FindStringSubmatch(args[0])
	if q == nil {
		return JSMount{}, false
	}
	last := args[len(args)-1]
	mount := JSMount{Parent: parent, Prefix: q[1]}
	if r := jsInlineRequire.FindStringSubmatch(last); r != nil {
		mount.ChildModule = r[1]
		return mount, true
	}
	if !jsIdentPath.MatchString(last) {
		return JSMount{}, false // an inline handler, express.static(...)
	}
	mount.Child = last
	return mount, true
}

// splitTopLevelArgs splits a call's argument text at commas outside brackets
// and strings, stopping at the call's closing parenthesis.
func splitTopLevelArgs(s string) []string {
	var args []string
	depth := 0
	var quote rune
	start := 0
	for i, r := range s {
		if quote != 0 {
			if r == quote && (i == 0 || s[i-1] != '\\') {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				if a := strings.TrimSpace(s[start:i]); a != "" {
					args = append(args, a)
				}
				return args
			}
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return args
}

func (r *JSRouters) readImport(t string) {
	if m := jsRequireImport.FindStringSubmatch(t); m != nil {
		r.Imports[m[1]] = JSImport{Module: m[2], Name: m[3]}
		return
	}
	if m := jsRequireDestr.FindStringSubmatch(t); m != nil {
		for _, p := range pairs(m[1], ":") {
			r.Imports[p[1]] = JSImport{Module: m[2], Name: p[0]}
		}
		return
	}
	m := jsImportFrom.FindStringSubmatch(t)
	if m == nil {
		return
	}
	clause, mod := m[1], m[2]
	if i := strings.Index(clause, "{"); i >= 0 {
		named := clause[i:]
		named = strings.TrimSuffix(strings.TrimPrefix(named, "{"), "}")
		if j := strings.Index(named, "}"); j >= 0 {
			named = named[:j]
		}
		for _, p := range pairs(named, " as ") {
			r.Imports[p[1]] = JSImport{Module: mod, Name: p[0]}
		}
		clause = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(clause[:i]), ","))
	}
	for _, part := range strings.Split(clause, ",") {
		part = strings.TrimSpace(part)
		if ns, ok := strings.CutPrefix(part, "* as "); ok {
			r.Imports[strings.TrimSpace(ns)] = JSImport{Module: mod}
		} else if part != "" && jsIdentPath.MatchString(part) && !strings.Contains(part, ".") {
			r.Imports[part] = JSImport{Module: mod} // the default export
		}
	}
}

func (r *JSRouters) readExport(t string) {
	switch {
	case jsModuleExports.MatchString(t):
		r.Default = jsModuleExports.FindStringSubmatch(t)[1]
	case jsExportDefault.MatchString(t):
		r.Default = jsExportDefault.FindStringSubmatch(t)[1]
	case jsNamedExport.MatchString(t):
		m := jsNamedExport.FindStringSubmatch(t)
		r.Exports[m[1]] = m[2]
	case jsExportDecl.MatchString(t):
		m := jsExportDecl.FindStringSubmatch(t)
		r.Exports[m[1]] = m[1]
	case jsExportList.MatchString(t):
		// export { local as exported }
		for _, p := range pairs(jsExportList.FindStringSubmatch(t)[1], " as ") {
			if p[1] == "default" {
				r.Default = p[0]
			} else {
				r.Exports[p[1]] = p[0]
			}
		}
	case jsObjectExports.MatchString(t):
		// module.exports = { users: usersRouter } exports usersRouter as users.
		for _, p := range pairs(jsObjectExports.FindStringSubmatch(t)[1], ":") {
			r.Exports[p[0]] = p[1]
		}
	}
}

// pairs reads `a, b: c` (sep ":") or `a, b as c` (sep " as ") into
// [left, right] pairs; a bare name is its own pair.
func pairs(list, sep string) [][2]string {
	var out [][2]string
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		left, right := item, item
		if a, b, ok := strings.Cut(item, sep); ok {
			left, right = strings.TrimSpace(a), strings.TrimSpace(b)
		}
		out = append(out, [2]string{left, right})
	}
	return out
}

// ResolveMountTarget turns a mount into the module that declares the router
// and the export it names there ("" for the default export). ok is false for
// a router the imports do not explain, which includes one declared in the
// mounting file itself (Extract has applied that prefix).
func (r JSRouters) ResolveMountTarget(m JSMount) (module, export string, ok bool) {
	if m.ChildModule != "" {
		return m.ChildModule, "", true
	}
	head, rest, dotted := strings.Cut(m.Child, ".")
	imp, found := r.Imports[head]
	if !found {
		return "", "", false
	}
	if dotted {
		if imp.Name != "" || strings.Contains(rest, ".") {
			return "", "", false
		}
		return imp.Module, rest, true // routes.users from `import * as routes`
	}
	return imp.Module, imp.Name, true
}

// RouterFor names the local variable a module's export refers to: the
// default export when export is "", "" when the file does not say.
func (r JSRouters) RouterFor(export string) string {
	if export == "" {
		return r.Default
	}
	if v, ok := r.Exports[export]; ok {
		return v
	}
	return ""
}

// jsExtensions are the files a relative import can name, in the order Node
// and the TypeScript compiler try them.
var jsExtensions = []string{".js", ".ts", ".mjs", ".cjs", ".jsx", ".tsx", ".mts", ".cts"}

// ResolveJSModulePath finds the file a relative import names. sortedFiles must
// be sorted. A package import, or an alias such as "@/routes", is outside what
// can be resolved from the import alone and returns "".
//
// TypeScript compiled to ES modules imports "./users.js" for users.ts, so a
// .js specifier also tries the TypeScript sources.
func ResolveJSModulePath(importerPath, modImport string, sortedFiles []string) string {
	if !strings.HasPrefix(modImport, "./") && !strings.HasPrefix(modImport, "../") && modImport != "." && modImport != ".." {
		return ""
	}
	base := path.Clean(path.Join(path.Dir(importerPath), modImport))
	stem := base
	switch ext := path.Ext(base); ext {
	case ".js", ".jsx", ".mjs", ".cjs":
		stem = strings.TrimSuffix(base, ext)
	}
	cands := []string{base}
	for _, ext := range jsExtensions {
		cands = append(cands, stem+ext)
	}
	for _, ext := range jsExtensions {
		cands = append(cands, path.Join(base, "index"+ext))
	}
	for _, c := range cands {
		c = strings.TrimPrefix(c, "./")
		i := sort.SearchStrings(sortedFiles, c)
		if i < len(sortedFiles) && sortedFiles[i] == c {
			return c
		}
	}
	return ""
}
