package pipeline

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/api"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// MountGoRouters gives each Gin, chi, echo or gorilla route the prefix of the
// router another function hands it.
//
// A Go service fills a router in one package and chooses its prefix in
// another: `users.RegisterRoutes(api.Group("/users"))` in main.go, routes on
// the `rg *gin.RouterGroup` parameter in users/routes.go; or
// `r.Mount("/billing", billing.Routes())`, with Routes building and returning
// a chi router. Extract sees one file at a time, so those routes carried only
// the path written beside them.
//
// It works as the FastAPI and Express passes do: a function that takes a
// router (or returns one) is a node, a call that hands it a router is an edge
// with the prefix added on the way, chains compose through functions that
// pass their own parameter on, a function reached under two different
// prefixes keeps its routes as written and is counted, calls in test files
// are ignored, and every name is recomputed from the route's own path so a
// removed call takes its prefix with it. A package is found through the
// module path in its go.mod, so a dependency named like a repository folder
// is never mistaken for it.
func MountGoRouters(ctx context.Context, reg *registry.Registry, runID int64) (RouteMountStats, error) {
	var st RouteMountStats
	endpoints, err := reg.GoEndpoints(ctx, runID)
	if err != nil || len(endpoints) == 0 {
		return st, err
	}
	files, err := reg.GoRouterFiles(ctx, runID)
	if err != nil {
		return st, err
	}

	modules := map[string]string{}
	parsed := map[string]api.GoRouterFile{}
	for _, f := range files {
		if path.Base(f.Path) == "go.mod" {
			if mod := api.GoModulePath(f.Content); mod != "" {
				modules[mod] = path.Dir(f.Path)
			}
			continue
		}
		if strings.HasSuffix(f.Path, ".go") {
			parsed[f.Path] = api.GoRouters([]byte(f.Content))
		}
	}

	type fnKey struct{ dir, name string }
	funcs := map[fnKey]*api.GoRouterFunc{}
	byName := map[string][]fnKey{}
	paths := make([]string, 0, len(parsed))
	for p := range parsed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		dir := path.Dir(p)
		for key, fn := range parsed[p].Funcs {
			if fn.Param == "" && fn.Returns == "" {
				continue
			}
			k := fnKey{dir, key}
			if _, dup := funcs[k]; !dup {
				byName[fn.Name] = append(byName[fn.Name], k)
			}
			funcs[k] = fn
		}
	}
	// carries reports whether a router expression in fn stands for what fn
	// itself is handed or returns, so fn's own prefix applies to it.
	carries := func(fn *api.GoRouterFunc, v api.GoRouterVar) bool {
		return v.Root != "" && (v.Root == fn.Param || v.Root == fn.Returns)
	}

	incoming := map[mountNode][]mountEdge{}
	for _, p := range paths {
		if capability.IsTestPath(p) {
			continue // a test's own router, not where the service serves it
		}
		f := parsed[p]
		dir := path.Dir(p)
		names := make([]string, 0, len(f.Funcs))
		for n := range f.Funcs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			caller := f.Funcs[n]
			for _, c := range caller.Calls {
				var target fnKey
				switch imp, isImport := f.Imports[c.Qual]; {
				case c.Qual == "":
					target = fnKey{dir, c.Func}
				case isImport:
					d, ok := api.GoPackageDir(imp, modules)
					if !ok {
						continue // the standard library or a dependency
					}
					target = fnKey{d, c.Func}
				default:
					// A method called on a value: h.Register(r), or through
					// a field, deps.MerchantAck.Mount. The value's type is not
					// read. The method is taken when one router-taking method
					// in the repository has the name, or, when several do,
					// when exactly one's receiver type starts with the name
					// it is called through (MerchantAck -> MerchantAckHandler):
					// a service names the field after the handler it holds.
					k, ok := pickMethod(byName[c.Func], funcs, c.Qual)
					if !ok {
						if len(byName[c.Func]) > 1 {
							st.Ambiguous++
						}
						continue
					}
					target = k
				}
				fn := funcs[target]
				if fn == nil || (c.Mounted && fn.Returns == "") || (!c.Mounted && fn.Param == "") {
					continue
				}
				if target == (fnKey{dir, caller.Key()}) {
					continue // a function handing its router to itself
				}
				parent := mountNode{} // a router built where it is used: the prefix is absolute
				if carries(caller, c.Arg) {
					parent = mountNode{dir, caller.Key()}
				}
				child := mountNode{target.dir, target.name}
				incoming[child] = append(incoming[child], mountEdge{parent: parent, prefix: joinPrefix("", c.Arg.Prefix)})
			}
		}
	}
	mounted, st := resolveMountChains(incoming, st)

	renames := map[int64]string{}
	for _, e := range endpoints {
		raw, ok := strings.CutPrefix(e.FQN, e.Path+"#")
		if !ok {
			continue
		}
		want := raw
		if f, ok := parsed[e.Path]; ok {
			if site, ok := f.Routes[e.Line]; ok {
				if fn := f.Funcs[site.Func]; fn != nil && carries(fn, site.Var) {
					if prefix, ok := mounted[mountNode{path.Dir(e.Path), fn.Key()}]; ok {
						if method, p, ok := strings.Cut(raw, " "); ok {
							want = method + " " + api.JoinRoutePath(prefix, p)
						}
					}
				}
			}
		}
		if want != e.Name {
			renames[e.ID] = want
		}
	}
	st.Renamed = len(renames)
	return st, reg.RenameEndpoints(ctx, renames)
}

// pickMethod chooses among router-taking functions sharing a name: the only
// one, or the only one whose receiver type starts with the name it is called
// through.
func pickMethod[K comparable](cands []K, funcs map[K]*api.GoRouterFunc, qual string) (K, bool) {
	var zero K
	if len(cands) == 1 {
		return cands[0], true
	}
	var found K
	n := 0
	for _, k := range cands {
		if fn := funcs[k]; fn != nil && fn.Recv != "" && qual != "" &&
			strings.HasPrefix(strings.ToLower(fn.Recv), strings.ToLower(qual)) {
			found = k
			n++
		}
	}
	if n == 1 {
		return found, true
	}
	return zero, false
}
