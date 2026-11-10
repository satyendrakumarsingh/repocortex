// Package api extracts HTTP endpoints from source: the routes a service
// exposes, and the handler behind each one.
//
// # Why this is framework-specific, and why that is the honest design
//
// There is no language-level concept of "an HTTP endpoint". Every framework
// invents its own spelling — a Java annotation, a Go method call on a router, a
// Python decorator, a JavaScript member call — and the only way to find them is
// to know the framework. That makes this Tier-3 work by the project's own
// definition: real value, per-framework cost, and a coverage story that must be
// stated rather than implied.
//
// The alternative, matching anything that looks like a URL path, produces
// endpoints from log format strings and documentation comments. An invented
// endpoint is worse than a missing one: it appears in an API surface report as
// a route that does not exist.
//
// # What is covered
//
// Spring (Java), net/http and the chi/gin/echo family (Go), FastAPI, Flask and
// Django urlconfs (Python), Express and NestJS controllers (JavaScript/TypeScript).
// Coverage is reported by SupportedFrameworks so `doctor` can state it rather
// than leaving a user to infer it from an empty result.
//
// NOT covered, and deliberately: OpenAPI documents, GraphQL schemas, gRPC
// service definitions, and API-gateway configuration. Each is a real discovery
// source from spec §21 and each is a separate body of work; claiming them by
// half-implementing them would be the overclaim this codebase is built to
// avoid.
//
// # Confidence
//
// An annotation or a router registration is an explicit, unambiguous statement
// by the author, so a route found this way is ConfidenceHigh. Nothing here is
// ever exact: exact is reserved for facts a compiler proved, and no compiler
// checked that this string is a route. A path assembled at run time is reported
// with its literal part and a lower confidence, because a partially-known route
// is still worth knowing about.
package api

import (
	"regexp"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Framework identifies which convention matched, so a report can say how a
// route was found rather than presenting all routes as equally certain.
type Framework string

const (
	FrameworkSpring  Framework = "spring"
	FrameworkGoHTTP  Framework = "go-http"
	FrameworkGoChi   Framework = "go-router"
	FrameworkFastAPI Framework = "fastapi"
	FrameworkFlask   Framework = "flask"
	FrameworkDjango  Framework = "django"
	FrameworkExpress Framework = "express"
	FrameworkNest    Framework = "nest"
)

// Route is one discovered HTTP endpoint.
type Route struct {
	// Method is the HTTP verb, uppercased. Empty means the framework registered
	// a handler for every method (Spring's bare @RequestMapping, Express's
	// app.all), which is reported as "ANY" rather than guessed at.
	Method string
	// Path is the route as written, including the framework's parameter syntax.
	// It is NOT normalised across frameworks: `/users/{id}`, `/users/:id` and
	// `/users/<id>` are how each is actually written, and rewriting them would
	// make the report disagree with the source a reader is about to open.
	Path string
	// Handler is the function or method name the route dispatches to, where
	// the framework's syntax makes it recoverable.
	Handler string
	// Framework is the convention that matched.
	Framework Framework
	// Line is the 1-based line the route was declared on.
	Line int
	// Confidence reflects how certain the extraction is, never how correct the
	// route is at run time.
	Confidence model.Confidence
	// Dynamic marks a path that is assembled rather than literal. The literal
	// part is still reported.
	Dynamic bool
}

// Signature renders a route the way a person writes it in a ticket.
func (r Route) Signature() string {
	m := r.Method
	if m == "" {
		m = "ANY"
	}
	return m + " " + r.Path
}

// SupportedFrameworks is the coverage statement. `cortex doctor` prints it so
// "no endpoints found" can be read correctly: it means either that the service
// exposes none, or that it uses a framework not in this list, and a user must
// be able to tell those apart.
func SupportedFrameworks() []Framework {
	return []Framework{
		FrameworkSpring, FrameworkGoHTTP, FrameworkGoChi,
		FrameworkFastAPI, FrameworkFlask, FrameworkDjango, FrameworkExpress,
		FrameworkNest,
	}
}

// httpMethods are the verbs treated as routes. HEAD and OPTIONS are omitted
// deliberately: frameworks synthesise them, so reporting them as declared
// endpoints would inflate an API surface with routes nobody wrote.
var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// Extract finds every route in one file.
//
// Takes the source text rather than a syntax tree. That is a real trade and
// worth naming: a tree would be more precise, but it would need a per-language
// query set maintained against six grammars, and the constructs here are
// line-local and syntactically distinctive enough that a scan gets them right.
// The gate against false positives is that every pattern requires the
// framework's own spelling — an annotation, a router method call — not a bare
// string that looks like a path.
func Extract(path string, src []byte) []Route {
	lang := languageOf(path)
	if lang == "" {
		return nil
	}
	lines := strings.Split(string(src), "\n")

	switch lang {
	case "java":
		return withPath(extractSpring(lines))
	case "go":
		return withPath(extractGo(lines))
	case "python":
		routes := extractPython(lines)
		if isDjangoURLConf(path) {
			routes = append(routes, extractDjango(lines)...)
		}
		return withPath(routes)
	case "js":
		return withPath(append(extractExpress(lines), extractNest(lines)...))
	}
	return nil
}

// withPath is the single exit filter: a route with no path is dropped.
//
// Every framework here registers on a path, and none of them accepts an empty
// one — `app.get("", h)` and `mux.HandleFunc("", h)` are refused at run time by
// Express and net/http respectively, so a route with an empty path is a route
// that CANNOT EXIST. It reaches the extractors anyway, from a call whose first
// argument is built at run time (`app.get(prefix + "/x", h)` scans as `""` once
// the literal is empty) and from source that never compiled.
//
// Reporting one puts a row in an API surface report that names no endpoint,
// which is the fabricated-route failure this package is built to avoid — and it
// renders as the bare "GET " in a ticket, where a reader cannot even tell what
// was meant. Filtering at one place rather than in four extractors is what stops
// the next framework added here from forgetting the rule.
func withPath(routes []Route) []Route {
	kept := routes[:0]
	for _, r := range routes {
		if r.Path == "" {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func languageOf(path string) string {
	switch {
	case strings.HasSuffix(path, ".java"):
		return "java"
	case strings.HasSuffix(path, ".go"):
		return "go"
	case strings.HasSuffix(path, ".py"):
		return "python"
	case strings.HasSuffix(path, ".ts"), strings.HasSuffix(path, ".js"),
		strings.HasSuffix(path, ".tsx"), strings.HasSuffix(path, ".jsx"),
		strings.HasSuffix(path, ".mjs"),
		// Single-file components. The <script> block is ordinary JS or TS, and
		// the line-oriented scan this package uses reads it perfectly well —
		// the template and style blocks contain none of the patterns it looks
		// for, so scanning the whole file costs a few lines and risks nothing.
		strings.HasSuffix(path, ".vue"), strings.HasSuffix(path, ".svelte"),
		strings.HasSuffix(path, ".astro"):
		return "js"
	}
	return ""
}

// ---------------------------------------------------------------- Spring

var (
	// @RequestMapping("/payment") or @RequestMapping(value = "/payment", method = RequestMethod.POST)
	springClassMapping = regexp.MustCompile(`@RequestMapping\s*\(([^)]*)\)`)
	// @GetMapping("/callback"), @PostMapping(value = "/callback")
	springVerbMapping = regexp.MustCompile(`@(Get|Post|Put|Patch|Delete|Request)Mapping\s*(?:\(([^)]*)\))?`)
	// The method declaration following an annotation.
	javaMethodDecl = regexp.MustCompile(`(?:public|protected|private)\s+[\w<>\[\],.\s]+\s+(\w+)\s*\(`)
	quotedString   = regexp.MustCompile(`"([^"]*)"`)
	springMethodIn = regexp.MustCompile(`RequestMethod\.(\w+)`)
)

// extractSpring reads Spring MVC annotations.
//
// The class-level @RequestMapping is a prefix for every method in the class, so
// it is tracked and prepended. Missing that would report `/callback` for a
// controller whose real route is `/payment/callback` — a route that does not
// exist, which is the failure this package is built to avoid.
func extractSpring(lines []string) []Route {
	var out []Route
	var classPrefix string
	sawClassAnnotation := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// A class-level mapping is one that precedes a class declaration. The
		// simplest reliable signal is that no method mapping has been seen yet
		// and the next non-blank line declares a class.
		if !sawClassAnnotation && strings.Contains(trimmed, "@RequestMapping") {
			if declaresClassSoon(lines, i) {
				if m := springClassMapping.FindStringSubmatch(trimmed); m != nil {
					classPrefix = firstQuoted(m[1])
					sawClassAnnotation = true
					continue
				}
			}
		}

		m := springVerbMapping.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		verb := strings.ToUpper(m[1])
		args := m[2]

		method := verb
		if verb == "REQUEST" {
			// @RequestMapping on a method: the verb is in method=, or absent.
			method = ""
			if mm := springMethodIn.FindStringSubmatch(args); mm != nil {
				method = strings.ToUpper(mm[1])
			}
		}
		if method != "" && !httpMethods[method] {
			continue
		}

		r := Route{
			Method:     method,
			Path:       joinPath(classPrefix, firstQuoted(args)),
			Framework:  FrameworkSpring,
			Line:       i + 1,
			Confidence: model.ConfidenceHigh,
		}
		r.Handler = handlerAfter(lines, i, javaMethodDecl)
		out = append(out, r)
	}
	return out
}

// declaresClassSoon reports whether a class declaration follows within a few
// lines, which is what distinguishes a class-level annotation from a
// method-level one when both spell themselves @RequestMapping.
func declaresClassSoon(lines []string, from int) bool {
	for i := from + 1; i < len(lines) && i < from+6; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "//") {
			continue
		}
		return strings.Contains(t, "class ") || strings.Contains(t, "interface ")
	}
	return false
}

// ---------------------------------------------------------------- Go

var (
	// http.HandleFunc("/path", handler) / mux.HandleFunc("/path", handler)
	goHandleFunc = regexp.MustCompile(`\.\s*(HandleFunc|Handle)\s*\(\s*"([^"]*)"\s*,\s*([\w.]+)?`)
	// r.Get("/path", handler) — chi, gin, echo, fiber all share this shape.
	goRouterVerb = regexp.MustCompile(`\.\s*(Get|Post|Put|Patch|Delete|GET|POST|PUT|PATCH|DELETE)\s*\(\s*"([^"]*)"\s*,\s*([\w.]+)?`)
	// mux.Handle("/path", ...).Methods("POST") — gorilla/mux.
	goMuxMethods = regexp.MustCompile(`\.Methods\s*\(\s*"([A-Z]+)"`)
)

func extractGo(lines []string) []Route {
	var out []Route
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		if m := goRouterVerb.FindStringSubmatch(trimmed); m != nil {
			verb := strings.ToUpper(m[1])
			if httpMethods[verb] {
				out = append(out, Route{
					Method: verb, Path: m[2], Handler: lastIdent(m[3]),
					Framework: FrameworkGoChi, Line: i + 1,
					Confidence: model.ConfidenceHigh,
				})
				continue
			}
		}

		if m := goHandleFunc.FindStringSubmatch(trimmed); m != nil {
			r := Route{
				Path: m[2], Handler: lastIdent(m[3]),
				Framework: FrameworkGoHTTP, Line: i + 1,
				Confidence: model.ConfidenceHigh,
			}
			// gorilla/mux chains .Methods("POST") onto the registration.
			//
			// The verb is gated the same way every other framework's is. Without
			// the gate this was the one path where an arbitrary uppercase string
			// became an HTTP method: `.Methods("HEAD")` reported a synthesised
			// verb that httpMethods exists to keep out, and a line reading
			// `.Methods("A")` put `A /x` in an API surface report. A restriction
			// to verbs this package does not report means the registration is not
			// one of the endpoints it claims to cover, so it is skipped outright
			// rather than reported as answering every method.
			if mm := goMuxMethods.FindStringSubmatch(trimmed); mm != nil {
				verb := strings.ToUpper(mm[1])
				if !httpMethods[verb] {
					continue
				}
				r.Method = verb
			}
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------- Python

var (
	// @app.get("/path"), @router.post("/path"), @app.route("/path", methods=["POST"])
	pyDecorator  = regexp.MustCompile(`^@\s*[\w.]+\.\s*(get|post|put|patch|delete|route)\s*\(\s*["']([^"']*)["']`)
	pyMethodsArg = regexp.MustCompile(`methods\s*=\s*\[([^\]]*)\]`)
	pyFuncDecl   = regexp.MustCompile(`^\s*(?:async\s+)?def\s+(\w+)\s*\(`)
)

func extractPython(lines []string) []Route {
	var out []Route
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		m := pyDecorator.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		verb := strings.ToUpper(m[1])
		framework := FrameworkFastAPI

		if verb == "ROUTE" {
			// Flask spells the verb in methods=[...]; without it the route
			// answers GET only, which is Flask's documented default and a fact
			// rather than a guess.
			framework = FrameworkFlask
			verb = "GET"
			if mm := pyMethodsArg.FindStringSubmatch(trimmed); mm != nil {
				for _, v := range strings.Split(mm[1], ",") {
					v = strings.ToUpper(strings.Trim(strings.TrimSpace(v), `"'`))
					if httpMethods[v] {
						out = append(out, Route{
							Method: v, Path: m[2], Framework: FrameworkFlask,
							Line: i + 1, Confidence: model.ConfidenceHigh,
							Handler: handlerAfter(lines, i, pyFuncDecl),
						})
					}
				}
				continue
			}
		}
		if !httpMethods[verb] {
			continue
		}
		out = append(out, Route{
			Method: verb, Path: m[2], Framework: framework,
			Line: i + 1, Confidence: model.ConfidenceHigh,
			Handler: handlerAfter(lines, i, pyFuncDecl),
		})
	}
	return out
}

// ---------------------------------------------------------------- Django

var (
	// path("users/<int:id>/", views.user_detail, name="x")
	// re_path(r"^legacy/$", views.legacy)
	// path("x/", csrf_exempt(views.f)) — decorators wrap the view in the
	// urlconf itself (csrf_exempt, login_required, cache_page(60)(...)), so the
	// second group must capture through any nesting of call-wrappers rather
	// than stopping at the first identifier.
	djangoPath = regexp.MustCompile(`^(?:re_)?path\s*\(\s*r?["']([^"']*)["']\s*,\s*((?:\w+(?:\([^()]*\))?\s*\(\s*)*[\w.]+)`)
	// djangoInner picks out the innermost dotted name at the end of a
	// (possibly decorator-wrapped) second argument, e.g. "views.f" out of
	// "csrf_exempt(views.f" or "cache_page(60)(views.f".
	djangoInner = regexp.MustCompile(`([\w.]+)\s*$`)
)

// isDjangoURLConf: Django registers routes in a module conventionally named
// urls.py. `path` is also pathlib's constructor and an ordinary word, so the
// extractor runs only where Django itself looks.
func isDjangoURLConf(p string) bool {
	base := p
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		base = p[i+1:]
	}
	return base == "urls.py" || strings.HasSuffix(base, "_urls.py")
}

// extractDjango reads urlpatterns entries.
//
// Django routes carry no verb — the view dispatches on request.method — so
// Method is empty and the route renders as ANY, which is a fact. The path is
// reported relative to the enclosing include(): a urls.py mounted at "api/"
// by another module declares "users/" and serves "/api/users/", and the
// mount is not visible from this file. That is a partially-known route, and
// the package's rule for those is to report the literal part at reduced
// confidence rather than invent the prefix or drop the route.
//
// include() entries are mounts, not routes, and are skipped. So is
// admin.site.urls: the raw second argument ends in ".urls", the same mount
// shape as include(), and it names no single handler either.
func extractDjango(lines []string) []Route {
	var out []Route
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := djangoPath.FindStringSubmatch(trimmed)
		if m == nil || strings.HasPrefix(m[2], "include") || strings.HasSuffix(m[2], ".urls") {
			continue
		}
		p := strings.TrimSuffix(strings.TrimPrefix(m[1], "^"), "$")
		// Unwrap call-wrapping decorators before naming the handler: the
		// innermost dotted name is the view, and the wrapper around it
		// (csrf_exempt, login_required, cache_page(60)) is not the endpoint's
		// handler — naming it would be exactly the invented-handler failure
		// this package's own rule for handlerAfter refuses to commit.
		inner := m[2]
		if im := djangoInner.FindStringSubmatch(inner); im != nil {
			inner = im[1]
		}
		handler := strings.TrimSuffix(inner, ".as_view")
		out = append(out, Route{
			Method:     "",
			Path:       "/" + strings.TrimPrefix(p, "/"),
			Handler:    lastIdent(handler),
			Framework:  FrameworkDjango,
			Line:       i + 1,
			Confidence: model.ConfidenceMedium,
		})
	}
	return out
}

// ---------------------------------------------------------------- Express

var expressRoute = regexp.MustCompile(
	`\b(?:app|router)\s*\.\s*(get|post|put|patch|delete|all)\s*\(\s*['"` + "`" + `]([^'"` + "`" + `]*)['"` + "`" + `]\s*,\s*([\w.]+)?`)

func extractExpress(lines []string) []Route {
	var out []Route
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		m := expressRoute.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		verb := strings.ToUpper(m[1])
		if verb == "ALL" {
			verb = "" // every method; reported as ANY
		} else if !httpMethods[verb] {
			continue
		}
		out = append(out, Route{
			Method: verb, Path: m[2], Handler: lastIdent(m[3]),
			Framework: FrameworkExpress, Line: i + 1,
			Confidence: model.ConfidenceHigh,
		})
	}
	return out
}

// ---------------------------------------------------------------- NestJS

var (
	// @Controller('users') / @Controller() / @Controller({ path: 'users' }) /
	// @Controller({ version: '1' }). The argument is mandatory: an
	// @Controller(...) whose shape is not one of these is left to
	// nestControllerAny below rather than matched here with an empty capture.
	nestController = regexp.MustCompile(`^@Controller\s*\(\s*(?:['"` + "`" + `]([^'"` + "`" + `]*)['"` + "`" + `]|\{[^}]*path\s*:\s*['"` + "`" + `]([^'"` + "`" + `]*)['"` + "`" + `]|\{[^}]*\}|\))`)
	// nestControllerAny matches any @Controller( call, including the shapes
	// nestController does not read (an array of prefixes, an object wrapped
	// across lines). It exists only to detect that miss.
	nestControllerAny = regexp.MustCompile(`^@Controller\s*\(`)
	// @Get(':id') / @Post() / @Delete('x') / @All(). The argument is
	// mandatory, same reasoning as nestController: a shape this does not
	// read (an array, a template expression) simply does not match, and the
	// decorator is skipped rather than reported at a guessed path.
	nestVerb = regexp.MustCompile(`^@(Get|Post|Put|Patch|Delete|All)\s*\(\s*(?:['"` + "`" + `]([^'"` + "`" + `]*)['"` + "`" + `]|\))`)
	// The method declared under the decorator: `async findOne(`, `remove(`.
	tsMethodDecl = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+)?(?:async\s+)?(\w+)\s*\(`)
	// A class declaration, which is what a pending @Controller attaches to.
	tsClassDecl = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\b`)
)

// nestHandlerAfter finds the method a verb decorator applies to, stepping over
// the other decorators stacked between them — including ones whose arguments
// span lines.
//
// handlerAfter skips lines that BEGIN with `@` and stops at the first other
// non-declaration line. Under a Swagger-annotated controller the line after
// `@ApiResponse({` is `status: 200,`, which is neither, so the scan gave up and
// the handler was "" on exactly the controllers people document most
// carefully; worse, `@UseGuards(` followed by `AuthGuard('jwt'),` matched the
// method pattern and named a guard as the handler. So a decorator's open
// brackets are counted, and every line until they close is part of the
// decorator, whatever it looks like.
func nestHandlerAfter(lines []string, from int) string {
	depth := 0
	for i := from + 1; i < len(lines) && i < from+40; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		if depth > 0 {
			depth += bracketDelta(t)
			continue
		}
		if strings.HasPrefix(t, "@") {
			depth = bracketDelta(t)
			continue
		}
		if m := tsMethodDecl.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
		// Something that is neither a decorator nor a declaration: the verb
		// was not attached to what this scan expected, and naming the wrong
		// method would be worse than naming none.
		return ""
	}
	return ""
}

// bracketDelta is the net count of brackets opened on one line, so a
// decorator's argument list can be followed across the lines it spans.
// Strings are not lexed: a bracket inside a quoted description will skew the
// count, and the consequence is a lost handler on that route, never a wrong
// one, because an unbalanced count only ever makes the scan wait longer.
func bracketDelta(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case '(', '[', '{':
			n++
		case ')', ']', '}':
			n--
		}
	}
	return n
}

// extractNest reads @Controller and the verb decorators beneath it.
//
// The class prefix is composed the way Spring's is, and for the same reason:
// `@Get(':id')` in a controller mounted at 'users' is `/users/:id`, and
// reporting `/:id` names a route that does not exist. A verb decorator with
// no @Controller in scope is not a route at all. A global prefix set with
// app.setGlobalPrefix is as invisible from here as Spring's servlet context
// path, and is treated the same way: not composed, not guessed.
func extractNest(lines []string) []Route {
	var out []Route
	prefix := ""
	// @Controller decorates the NEXT class and only that one. `pending` is
	// set by the decorator and consumed by the class declaration that
	// follows; a class declared without a pending @Controller ends the
	// previous controller's scope, so a verb-named decorator inside a helper
	// class later in the file cannot borrow the prefix above it.
	pending := false
	inController := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := nestController.FindStringSubmatch(trimmed); m != nil {
			prefix = m[1]
			if prefix == "" {
				prefix = m[2]
			}
			pending = true
			continue
		}
		// An @Controller( call whose argument is a shape this scan does not
		// read — an array of prefixes, an object wrapped across lines — is
		// not a route this package can compose a correct path for. A route
		// with a guessed prefix is a route that does not exist, so the
		// section is treated as not-a-controller rather than as prefix "".
		if nestControllerAny.MatchString(trimmed) {
			pending = false
			inController = false
			continue
		}
		if tsClassDecl.MatchString(trimmed) {
			inController, pending = pending, false
			continue
		}
		if !inController {
			continue
		}
		m := nestVerb.FindStringSubmatch(trimmed)
		if m == nil {
			// A verb decorator whose argument this scan does not read (an
			// array, a template expression) is skipped the same way: no
			// match, no guessed route.
			continue
		}
		verb := strings.ToUpper(m[1])
		if verb == "ALL" {
			verb = ""
		} else if !httpMethods[verb] {
			continue
		}
		// The handler is usually the declaration on a following line, but a
		// decorator and its method can share one line (`@Get('x') x() {}`).
		// Try the rest of this line first; only fall back to scanning ahead
		// when nothing follows the decorator's own closing paren.
		handler := ""
		rest := strings.TrimLeft(trimmed[len(m[0]):], ") \t")
		if mm := tsMethodDecl.FindStringSubmatch(rest); mm != nil {
			handler = mm[1]
		} else {
			handler = nestHandlerAfter(lines, i)
		}
		out = append(out, Route{
			Method:     verb,
			Path:       joinPath(prefix, m[2]),
			Handler:    handler,
			Framework:  FrameworkNest,
			Line:       i + 1,
			Confidence: model.ConfidenceHigh,
		})
	}
	return out
}

// ---------------------------------------------------------------- helpers

// handlerAfter finds the function or method declared just below an annotation
// or decorator, which is the handler it applies to.
func handlerAfter(lines []string, from int, decl *regexp.Regexp) string {
	for i := from + 1; i < len(lines) && i < from+8; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "//") ||
			strings.HasPrefix(t, "#") {
			continue
		}
		if m := decl.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
		// A non-blank, non-annotation line that is not a declaration means the
		// annotation was not attached to what we expected. Returning "" is
		// right: an endpoint whose handler is unknown is still a real endpoint,
		// and naming the wrong function would be worse than naming none.
		return ""
	}
	return ""
}

func firstQuoted(s string) string {
	if m := quotedString.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// joinPath composes a class-level prefix with a method-level path, without
// producing the double slashes that make two spellings of one route.
func joinPath(prefix, path string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	switch {
	case prefix == "" && path == "":
		return "/"
	case prefix == "":
		return ensureLeadingSlash(path)
	case path == "":
		return ensureLeadingSlash(prefix)
	}
	return ensureLeadingSlash(prefix) + ensureLeadingSlash(path)
}

func ensureLeadingSlash(s string) string {
	if s == "" || strings.HasPrefix(s, "/") {
		return s
	}
	return "/" + s
}

// lastIdent reduces `pkg.Handler` or `h.ServeHTTP` to the final identifier,
// which is what matches a symbol name in the index.
func lastIdent(s string) string {
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}
