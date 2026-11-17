package api

import (
	"path"
	"regexp"
	"strings"
)

// Go routers built in one function and filled in another.
//
// A Go service rarely declares its routes where their prefix is chosen. Gin:
//
//	api := r.Group("/api")
//	users.RegisterRoutes(api.Group("/users"))   // main.go
//	func RegisterRoutes(rg *gin.RouterGroup) {   // users/routes.go
//	    rg.GET("/:id", get)                      // served at /api/users/:id
//
// chi mounts a router another package builds and returns, and nests routes
// in closures that shadow the outer router's name:
//
//	r.Mount("/users", users.Routes())
//	r.Route("/admin", func(r chi.Router) { r.Get("/stats", stats) })
//
// Extract sees one file at a time, so those routes were "GET /:id" and
// "GET /stats". What a file says about its routers is read here: which
// function takes a router or returns one, which router variable each route
// is registered on and what prefix that variable carries inside its
// function, and the calls that hand a router to another function. A pass
// over the whole run joins them, as it does for FastAPI, Flask and Express.

// GoRouterFile is what one Go file says about its routers.
type GoRouterFile struct {
	Package string
	// Imports maps the name a file uses for a package to its import path.
	Imports map[string]string
	Funcs   map[string]*GoRouterFunc
	// Routes maps a route registration's line (1-based, as Route.Line) to the
	// function it is in and the router it is registered on.
	Routes map[int]GoRouteSite
}

// GoRouterFunc is one top-level function or method.
type GoRouterFunc struct {
	Name string
	// Recv is a method's receiver type without the pointer ("" for a
	// function). Methods are keyed Recv+"."+Name in GoRouterFile.Funcs:
	// several handler types in one package each have a Mount.
	Recv string
	// Param is the parameter that receives a router, "" when none does.
	Param string
	// Returns is the local router variable the function returns, "" when it
	// returns none.
	Returns string
	// Calls are the places this function hands a router to another one.
	Calls []GoRouterCall
}

// GoRouterVar is a router expression reduced to the router it was derived
// from (Root: a parameter or a local router) and the prefix added on the way.
type GoRouterVar struct {
	Root   string
	Prefix string
}

// GoRouteSite is where a route is registered.
type GoRouteSite struct {
	Func string
	Var  GoRouterVar
}

// GoRouterCall hands a router to a function: `pkg.Register(api)`,
// `r.Route("/x", pkg.Routes)`, or mounts what a function returns:
// `r.Mount("/x", pkg.Routes())`.
type GoRouterCall struct {
	// Qual is the name before the function's: a package (users.Register),
	// a value (h.Register), or the last field of a chain
	// (deps.MerchantAck.Mount gives "MerchantAck"). "" for a call in the
	// same package.
	Qual string
	Func string
	// Mounted is true for Mount: the callee RETURNS the router it fills.
	Mounted bool
	// Arg is the router the callee's routes end up under.
	Arg  GoRouterVar
	Line int
}

var (
	goPackage        = regexp.MustCompile(`^package\s+(\w+)`)
	goImportOne      = regexp.MustCompile(`^import\s+(?:(\w+|\.|_)\s+)?"([^"]+)"`)
	goImportLine     = regexp.MustCompile(`^(?:(\w+|\.|_)\s+)?"([^"]+)"`)
	goRouterFuncDecl = regexp.MustCompile(`^func\s*(?:\(\s*\w*\s*\*?\s*(\w+)(?:\[[^\]]*\])?\s*\)\s*)?(\w+)\s*\(([^)]*)\)`)
	goRouterType     = regexp.MustCompile(`^\*?(?:gin\.(?:RouterGroup|Engine|IRouter|IRoutes)|chi\.(?:Router|Mux)|echo\.(?:Group|Echo)|fiber\.(?:Router|App)|mux\.Router)\b`)
	goNewRouter      = regexp.MustCompile(`\b(\w+)\s*:?=\s*(?:gin\.(?:Default|New)|chi\.NewRouter|chi\.NewMux|echo\.New|fiber\.New|mux\.NewRouter)\s*\(`)
	goGroupVar       = regexp.MustCompile(`\b(\w+)\s*:?=\s*(\w+)\s*\.\s*Group\s*\(\s*"([^"]*)"`)
	goSubrouter      = regexp.MustCompile(`\b(\w+)\s*:?=\s*(\w+)\s*\.\s*PathPrefix\s*\(\s*"([^"]*)"\s*\)\s*\.\s*Subrouter\s*\(`)
	goClosure        = regexp.MustCompile(`\b(\w+)\s*\.\s*(?:Route|Group)\s*\(\s*(?:"([^"]*)"\s*,\s*)?func\s*\(\s*(\w+)\s`)
	goMountCall      = regexp.MustCompile(`\b(\w+)\s*\.\s*Mount\s*\(\s*"([^"]*)"\s*,\s*(?:(?:\w+\s*\.\s*)*(\w+)\s*\.\s*)?(\w+)\s*\(\s*\)\s*\)`)
	goRouteFunc      = regexp.MustCompile(`\b(\w+)\s*\.\s*Route\s*\(\s*"([^"]*)"\s*,\s*(?:(?:\w+\s*\.\s*)*(\w+)\s*\.\s*)?(\w+)\s*\)`)
	goPassRouter     = regexp.MustCompile(`(?:^|[^\w.])(?:(?:\w+\s*\.\s*)*(\w+)\s*\.\s*)?(\w+)\s*\(\s*(\w+)(?:\s*\.\s*Group\s*\(\s*"([^"]*)"\s*\))?\s*\)`)
	goReturnIdent    = regexp.MustCompile(`^return\s+(\w+)\s*$`)
	goStringLit      = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`|'(?:[^'\\\\]|\\\\.)*'")
)

// GoRouters reads one Go file's router functions, route sites and calls.
func GoRouters(src []byte) GoRouterFile {
	lines := strings.Split(string(src), "\n")
	out := GoRouterFile{Imports: map[string]string{}, Funcs: map[string]*GoRouterFunc{}, Routes: map[int]GoRouteSite{}}

	inImports := false
	var fn *GoRouterFunc
	var vars map[string]GoRouterVar
	type scope struct {
		name  string
		v     GoRouterVar
		depth int
	}
	var closures []scope
	depth := 0

	resolve := func(name string) (GoRouterVar, bool) {
		for i := len(closures) - 1; i >= 0; i-- {
			if closures[i].name == name {
				return closures[i].v, true
			}
		}
		if v, ok := vars[name]; ok {
			return v, true
		}
		if fn != nil && name == fn.Param && name != "" {
			return GoRouterVar{Root: name}, true
		}
		return GoRouterVar{Root: name}, false
	}

	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if m := goPackage.FindStringSubmatch(line); m != nil {
			out.Package = m[1]
			continue
		}
		if line == "import (" {
			inImports = true
			continue
		}
		if inImports {
			if line == ")" {
				inImports = false
			} else if m := goImportLine.FindStringSubmatch(line); m != nil {
				out.Imports[importName(m[1], m[2])] = m[2]
			}
			continue
		}
		if m := goImportOne.FindStringSubmatch(line); m != nil {
			out.Imports[importName(m[1], m[2])] = m[2]
			continue
		}

		// A top-level declaration starts at column 0 and ends at a lone "}".
		if strings.HasPrefix(raw, "func") {
			if m := goRouterFuncDecl.FindStringSubmatch(line); m != nil {
				fn = &GoRouterFunc{Recv: m[1], Name: m[2], Param: routerParam(m[3])}
				out.Funcs[fn.Key()] = fn
				vars = map[string]GoRouterVar{}
				closures = nil
				depth = 0
			}
		}
		if fn == nil {
			continue
		}

		// Scopes: a closure that receives a router opens one at the depth
		// the line starts at, and closes when the braces return to it. It is
		// opened before the line's routes are read, so a closure written on
		// one line puts its own routes under its prefix.
		if m := goClosure.FindStringSubmatch(line); m != nil {
			if parent, ok := resolve(m[1]); ok {
				closures = append(closures, scope{name: m[3],
					v: GoRouterVar{Root: parent.Root, Prefix: joinRoutePath(parent.Prefix, m[2])}, depth: depth})
			}
		}
		if m := goNewRouter.FindStringSubmatch(line); m != nil {
			vars[m[1]] = GoRouterVar{Root: m[1]}
		}
		if m := goGroupVar.FindStringSubmatch(line); m != nil {
			if parent, ok := resolve(m[2]); ok {
				vars[m[1]] = GoRouterVar{Root: parent.Root, Prefix: joinRoutePath(parent.Prefix, m[3])}
			}
		}
		if m := goSubrouter.FindStringSubmatch(line); m != nil {
			if parent, ok := resolve(m[2]); ok {
				vars[m[1]] = GoRouterVar{Root: parent.Root, Prefix: joinRoutePath(parent.Prefix, m[3])}
			}
		}
		if m := goRouterVerb.FindStringSubmatch(line); m != nil {
			if v, ok := resolve(m[1]); ok {
				out.Routes[i+1] = GoRouteSite{Func: fn.Key(), Var: v}
			}
		}
		if m := goMountCall.FindStringSubmatch(line); m != nil {
			if v, ok := resolve(m[1]); ok {
				fn.Calls = append(fn.Calls, GoRouterCall{Qual: m[3], Func: m[4], Mounted: true,
					Arg: GoRouterVar{Root: v.Root, Prefix: joinRoutePath(v.Prefix, m[2])}, Line: i + 1})
			}
		} else if m := goRouteFunc.FindStringSubmatch(line); m != nil && m[4] != "func" {
			if v, ok := resolve(m[1]); ok {
				fn.Calls = append(fn.Calls, GoRouterCall{Qual: m[3], Func: m[4],
					Arg: GoRouterVar{Root: v.Root, Prefix: joinRoutePath(v.Prefix, m[2])}, Line: i + 1})
			}
		} else {
			for _, m := range goPassRouter.FindAllStringSubmatch(line, -1) {
				qual, callee, arg := m[1], m[2], m[3]
				if _, isRouter := resolve(qual); isRouter && qual != "" {
					continue // a method of the router itself: r.Use(mw)
				}
				// A function named like a router method (audit.Mount(g)) is
				// still a function: methods of a router are the qual check above.
				v, ok := resolve(arg)
				if !ok {
					continue
				}
				fn.Calls = append(fn.Calls, GoRouterCall{Qual: qual, Func: callee,
					Arg: GoRouterVar{Root: v.Root, Prefix: joinRoutePath(v.Prefix, m[4])}, Line: i + 1})
			}
		}
		if m := goReturnIdent.FindStringSubmatch(line); m != nil {
			if v, ok := vars[m[1]]; ok && v.Root == m[1] {
				fn.Returns = m[1]
			}
		}

		depth += goBraceDelta(line)
		for len(closures) > 0 && depth <= closures[len(closures)-1].depth {
			closures = closures[:len(closures)-1]
		}
		if raw == "}" {
			fn, vars, closures, depth = nil, nil, nil, 0
		}
	}
	return out
}

// Key is how GoRouterFile.Funcs and GoRouteSite.Func name the function:
// "Type.Method" for a method, the name for a function.
func (f *GoRouterFunc) Key() string {
	if f.Recv != "" {
		return f.Recv + "." + f.Name
	}
	return f.Name
}

// routerParam names the parameter of a parameter list whose type is a router.
func routerParam(params string) string {
	for _, p := range strings.Split(params, ",") {
		f := strings.Fields(strings.TrimSpace(p))
		if len(f) == 2 && goRouterType.MatchString(f[1]) {
			return f[0]
		}
	}
	return ""
}

// importName is the name a file refers to an import by: its alias, or the
// last element of the path (the one before a major-version suffix).
func importName(alias, importPath string) string {
	if alias != "" {
		return alias
	}
	segs := strings.Split(importPath, "/")
	last := segs[len(segs)-1]
	if len(segs) > 1 && len(last) > 1 && last[0] == 'v' && strings.Trim(last[1:], "0123456789") == "" {
		last = segs[len(segs)-2]
	}
	return strings.ReplaceAll(last, "-", "_")
}

func goBraceDelta(line string) int {
	s := goStringLit.ReplaceAllString(line, `""`)
	if i := strings.Index(s, "//"); i >= 0 {
		s = s[:i]
	}
	return strings.Count(s, "{") - strings.Count(s, "}")
}

// GoPackageDir is the directory of an import path inside the repository,
// given the repository's Go modules (module path -> directory). ok is false
// for a package outside every module, the standard library and dependencies.
func GoPackageDir(importPath string, modules map[string]string) (string, bool) {
	best, dir := "", ""
	for mod, d := range modules {
		if (importPath == mod || strings.HasPrefix(importPath, mod+"/")) && len(mod) > len(best) {
			best, dir = mod, d
		}
	}
	if best == "" {
		return "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(importPath, best), "/")
	return strings.TrimPrefix(path.Join(dir, rest), "./"), true
}

// GoModulePath reads the module path of a go.mod file.
func GoModulePath(gomod string) string {
	for _, l := range strings.Split(gomod, "\n") {
		l = strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(l, "module"); ok && (strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t")) {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}
