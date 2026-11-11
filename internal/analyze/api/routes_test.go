package api

import (
	"os"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Route extraction is framework-specific, so the tests are too: one case per
// convention, written the way that framework is actually written, plus a shared
// body of inputs that must NOT produce routes.

func TestExtractSpring(t *testing.T) {
	src := `package com.pay.controller;

@RestController
@RequestMapping("/payment")
public class CallbackController {

    @PostMapping("/callback")
    public ResponseEntity<Void> handleCallback(@RequestBody CallbackRequest request) {
        return ResponseEntity.ok().build();
    }

    @GetMapping("/status/{id}")
    public Transaction status(@PathVariable String id) { return null; }

    @RequestMapping(value = "/legacy", method = RequestMethod.PUT)
    public void legacy() {}
}`

	routes := Extract("CallbackController.java", []byte(src))
	assertRoute(t, routes, "POST /payment/callback", "handleCallback")
	assertRoute(t, routes, "GET /payment/status/{id}", "status")
	assertRoute(t, routes, "PUT /payment/legacy", "legacy")

	if len(routes) != 3 {
		t.Errorf("got %d routes, want 3: %v", len(routes), signatures(routes))
	}
}

// TestSpringClassPrefixIsApplied is the one that catches a route that does not
// exist. Reporting `/callback` for a controller mapped at `/payment` names an
// endpoint no client can call.
func TestSpringClassPrefixIsApplied(t *testing.T) {
	src := `@RestController
@RequestMapping("/payment")
public class C {
    @PostMapping("/callback")
    public void handle() {}
}`
	routes := Extract("C.java", []byte(src))
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %v", signatures(routes))
	}
	if routes[0].Path != "/payment/callback" {
		t.Errorf("path = %q, want /payment/callback — the class prefix was dropped", routes[0].Path)
	}
}

func TestExtractGo(t *testing.T) {
	src := `package main

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	r.Get("/users/{id}", getUser)
	r.Post("/users", createUser)
	// r.Delete("/commented", nope)
	router.Handle("/legacy", legacyHandler).Methods("PUT")
}`
	routes := Extract("main.go", []byte(src))
	assertRoute(t, routes, "ANY /health", "healthHandler")
	assertRoute(t, routes, "GET /users/{id}", "getUser")
	assertRoute(t, routes, "POST /users", "createUser")
	assertRoute(t, routes, "PUT /legacy", "legacyHandler")

	for _, r := range routes {
		if r.Path == "/commented" {
			t.Error("a commented-out registration was reported as a live route")
		}
	}
}

// TestExtractGoMethodPatterns: Go 1.22's ServeMux takes the method inside the
// pattern. Reading "GET /api/health" as a path made every such route an ANY
// route on a path no client could call.
func TestExtractGoMethodPatterns(t *testing.T) {
	src := `package main

func routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/reindex", s.handleReindex)
	mux.HandleFunc("/assets/", s.handleAssets)
	mux.HandleFunc("BREW /tea", teapot)
}`
	routes := Extract("main.go", []byte(src))
	assertRoute(t, routes, "GET /api/health", "handleHealth")
	assertRoute(t, routes, "POST /api/reindex", "handleReindex")
	assertRoute(t, routes, "ANY /assets/", "handleAssets")
	for _, r := range routes {
		if strings.Contains(r.Path, " ") {
			t.Errorf("a method leaked into the path: %q", r.Path)
		}
		if r.Path == "/tea" {
			t.Errorf("a verb the package does not report was widened to ANY: %s %s", r.Method, r.Path)
		}
	}
}

func TestExtractPython(t *testing.T) {
	src := `from fastapi import FastAPI

app = FastAPI()

@app.get("/items/{item_id}")
async def read_item(item_id: int):
    return {}

@app.post("/items")
def create_item(item: Item):
    return {}

@app.route("/legacy", methods=["POST", "PUT"])
def legacy():
    return ""

@app.route("/flask-default")
def flask_default():
    return ""
`
	routes := Extract("main.py", []byte(src))
	assertRoute(t, routes, "GET /items/{item_id}", "read_item")
	assertRoute(t, routes, "POST /items", "create_item")
	assertRoute(t, routes, "POST /legacy", "legacy")
	assertRoute(t, routes, "PUT /legacy", "legacy")
	// Flask's documented default when methods= is absent is GET. That is a
	// fact about the framework, not a guess about the code.
	assertRoute(t, routes, "GET /flask-default", "flask_default")
}

func TestExtractExpress(t *testing.T) {
	src := `const app = express();

app.get('/users', listUsers);
app.post("/users", createUser);
router.delete('/users/:id', removeUser);
app.all('/any', anyHandler);
// app.get('/commented', nope);
`
	routes := Extract("server.js", []byte(src))
	assertRoute(t, routes, "GET /users", "listUsers")
	assertRoute(t, routes, "POST /users", "createUser")
	assertRoute(t, routes, "DELETE /users/:id", "removeUser")
	assertRoute(t, routes, "ANY /any", "anyHandler")

	for _, r := range routes {
		if strings.Contains(r.Path, "commented") {
			t.Error("commented-out route reported")
		}
	}
}

// TestExtractNeverInventsEndpoints is the counterpart to the SQL recogniser's
// no-fabricated-tables test, and matters for the same reason: an endpoint in an
// API surface report is read as a route that exists.
func TestExtractNeverInventsEndpoints(t *testing.T) {
	cases := []struct{ path, src string }{
		{"log.go", `log.Printf("GET /users returned %d", code)`},
		{"doc.go", `// The service exposes GET /users and POST /users.`},
		{"client.go", `resp, err := http.Get("https://api.example.com/users")`},
		{"c.java", `logger.info("POST /payment/callback failed");`},
		{"s.py", `print("GET /items")`},
		{"a.js", `console.log('app.get("/fake")');`},
		{"README.md", `app.get('/not-code', handler)`}, // not a source file
	}
	for _, c := range cases {
		if routes := Extract(c.path, []byte(c.src)); len(routes) > 0 {
			t.Errorf("%s: invented %v from %q", c.path, signatures(routes), c.src)
		}
	}
}

// TestHandlerIsNeverGuessed. An endpoint whose handler cannot be determined is
// still a real endpoint; naming the wrong function would send a reader to the
// wrong place, which is worse than naming none.
func TestHandlerIsNeverGuessed(t *testing.T) {
	src := `@RestController
public class C {
    @PostMapping("/orphan")

    private static final String UNRELATED = "x";

    public void notTheHandler() {}
}`
	routes := Extract("C.java", []byte(src))
	if len(routes) != 1 {
		t.Fatalf("want 1 route, got %v", signatures(routes))
	}
	if routes[0].Handler == "notTheHandler" {
		t.Error("attached the route to a function it does not annotate")
	}
}

func TestExtractIgnoresUnknownLanguages(t *testing.T) {
	for _, path := range []string{"a.rb", "b.php", "c.txt", "d.md", "e"} {
		if r := Extract(path, []byte(`app.get('/x', h)`)); r != nil {
			t.Errorf("%s produced routes from an unsupported language: %v", path, signatures(r))
		}
	}
}

// TestConfidenceIsNeverExact. Exact is reserved for facts a compiler proved,
// and no compiler checked that a string literal is a live route.
func TestConfidenceIsNeverExact(t *testing.T) {
	srcs := map[string]string{
		"C.java":    "@RestController\npublic class C {\n@GetMapping(\"/a\")\npublic void a() {}\n}",
		"m.go":      `mux.HandleFunc("/a", h)`,
		"m.py":      "@app.get(\"/a\")\ndef a():\n    pass",
		"server.js": `app.get('/a', h);`,
	}
	for path, src := range srcs {
		for _, r := range Extract(path, []byte(src)) {
			if r.Confidence == model.ConfidenceExact {
				t.Errorf("%s: route %q claims exact confidence", path, r.Signature())
			}
			if r.Confidence == "" {
				t.Errorf("%s: route %q carries no confidence", path, r.Signature())
			}
		}
	}
}

// TestExtractOnTheRealFixture drives the payment-platform controller the golden
// task is written against, so the extractor is exercised on committed source
// rather than only on strings in this file.
func TestExtractOnTheRealFixture(t *testing.T) {
	const path = "../../../fixtures/payment-platform/src/main/java/com/pay/controller/CallbackController.java"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	routes := Extract(path, src)
	assertRoute(t, routes, "POST /payment/callback", "handleCallback")
}

func TestExtractIsSafeOnHostileInput(t *testing.T) {
	for _, src := range []string{
		strings.Repeat("@GetMapping(\"", 10000),
		strings.Repeat("app.get('", 10000),
		"\x00\x01\x02",
		strings.Repeat("\n", 100000),
		"@RequestMapping(" + strings.Repeat("(", 5000),
	} {
		for _, path := range []string{"a.java", "a.go", "a.py", "a.js"} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic on %s with %.30q: %v", path, src, r)
					}
				}()
				_ = Extract(path, []byte(src))
			}()
		}
	}
}

// ---------------------------------------------------------------- helpers

func assertRoute(t *testing.T, routes []Route, signature, handler string) {
	t.Helper()
	for _, r := range routes {
		if r.Signature() != signature {
			continue
		}
		if handler != "" && r.Handler != handler {
			t.Errorf("%s: handler = %q, want %q", signature, r.Handler, handler)
		}
		return
	}
	t.Errorf("route %q not found; got %v", signature, signatures(routes))
}

func signatures(routes []Route) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		out = append(out, r.Signature()+" -> "+r.Handler)
	}
	return out
}

// A .vue file's <script> block is JavaScript. ExtractCalls returned nil for
// it because languageOf did not name the extension, so every fetch and axios
// call in a single-file component was invisible to the API surface.
//
// The client call (fetch, axios) is found by ExtractCalls in clients.go, not
// by Extract in this file: Extract only recognises server-side route
// registrations (app.get, router.post, ...), and this component declares
// none, so Extract legitimately returns nil for it both before and after
// this fix — that is correct behaviour, not the bug. Both functions share
// languageOf, so naming the extension there fixes the real gap in
// ExtractCalls, which is what this test verifies.
func TestExtractReadsSingleFileComponents(t *testing.T) {
	src := []byte(`<template>
  <div>{{ user.name }}</div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const user = ref(null)

async function load() {
  const res = await fetch('/api/users/me')
  user.value = await res.json()
}
</script>

<style scoped>
div { color: red; }
</style>
`)

	for _, path := range []string{"App.vue", "App.svelte", "App.astro"} {
		t.Run(path, func(t *testing.T) {
			got := ExtractCalls(path, src)
			if len(got) == 0 {
				t.Fatalf("ExtractCalls(%s) returned nothing; the <script> block is "+
					"JavaScript and calls /api/users/me", path)
			}
			var found bool
			for _, c := range got {
				if strings.Contains(c.Path, "/api/users/me") {
					found = true
				}
			}
			if !found {
				t.Errorf("ExtractCalls(%s) = %+v, want a call for /api/users/me", path, got)
			}

			// Extract (server routes) correctly finds nothing here: the
			// component declares no app.METHOD/router.METHOD registration.
			if routes := Extract(path, src); len(routes) != 0 {
				t.Errorf("Extract(%s) = %+v, want no server routes", path, routes)
			}
		})
	}
}

func TestLanguageOfNamesTheComponentExtensions(t *testing.T) {
	for _, path := range []string{"a.vue", "a.svelte", "a.astro"} {
		if got := languageOf(path); got != "js" {
			t.Errorf("languageOf(%q) = %q, want js", path, got)
		}
	}
}

// A single-file component's <template> block is full of href/src attributes
// that look like paths but are not framework calls. Neither Extract nor
// ExtractCalls may turn them into routes/calls: extractExpress only matches
// app.METHOD(...) / router.METHOD(...) calls, and the JS client scanner only
// matches fetch(...) / axios.METHOD(...) calls, so a bare href/src attribute
// produces nothing from either.
func TestExtractIgnoresTemplateAttributesInComponents(t *testing.T) {
	src := []byte(`<template>
  <a href="/api/not-a-call">link</a>
  <img src="/api/also-not-a-call" />
</template>
<script setup>
</script>
`)
	for _, path := range []string{"Decoy.vue", "Decoy.svelte", "Decoy.astro"} {
		t.Run(path, func(t *testing.T) {
			if got := Extract(path, src); len(got) != 0 {
				t.Errorf("Extract(%s) = %+v, want no routes from template attributes", path, got)
			}
			if got := ExtractCalls(path, src); len(got) != 0 {
				t.Errorf("ExtractCalls(%s) = %+v, want no calls from template attributes", path, got)
			}
		})
	}
}

func TestExtractDjango(t *testing.T) {
	src := `from django.urls import path, re_path, include
from . import views

urlpatterns = [
    path("", views.index, name="index"),
    path("users/<int:id>/", views.user_detail),
    path("orders/", OrderListView.as_view(), name="orders"),
    re_path(r"^legacy/(?P<slug>[\w-]+)/$", views.legacy),
    path("api/", include("api.urls")),
    path("admin/", admin.site.urls),
    path("csrf/", csrf_exempt(views.csrf_view)),
    path("cached/", cache_page(60)(views.cached_view)),
    path("guarded/", login_required(OrderListView.as_view())),
    # path("dead/", views.dead),
]`
	routes := Extract("urls.py", []byte(src))
	assertRoute(t, routes, "ANY /", "index")
	assertRoute(t, routes, "ANY /users/<int:id>/", "user_detail")
	assertRoute(t, routes, "ANY /orders/", "OrderListView")
	assertRoute(t, routes, "ANY /legacy/(?P<slug>[\\w-]+)/", "legacy")
	// Decorators wrap the view in the urlconf itself. Naming the wrapper
	// (csrf_exempt, cache_page(60), login_required) as the handler would be
	// an invented handler; the innermost dotted name is the real view.
	assertRoute(t, routes, "ANY /csrf/", "csrf_view")
	assertRoute(t, routes, "ANY /cached/", "cached_view")
	assertRoute(t, routes, "ANY /guarded/", "OrderListView")
	if len(routes) != 7 {
		t.Errorf("got %d routes, want 7 (include() and admin.site.urls are mounts, not routes; the commented line produces nothing): %v", len(routes), signatures(routes))
	}
	for _, r := range routes {
		if strings.Contains(r.Path, "admin") {
			t.Errorf("admin.site.urls is a mount, not a handler, but produced %s", r.Signature())
		}
		if strings.Contains(r.Path, "dead") {
			t.Errorf("a commented-out registration was reported as a live route: %s", r.Signature())
		}
		if r.Framework != FrameworkDjango {
			t.Errorf("%s: framework = %q, want django", r.Signature(), r.Framework)
		}
		if r.Confidence != model.ConfidenceMedium {
			t.Errorf("%s: confidence = %v, want medium — the include() prefix is unknowable file-locally", r.Signature(), r.Confidence)
		}
	}
}

// A path() call outside a urls.py — a test helper named path, or pathlib —
// must not become an endpoint.
func TestDjangoOnlyReadsURLConfs(t *testing.T) {
	src := `from pathlib import Path
p = path("data/", loader)`
	if routes := Extract("views.py", []byte(src)); len(routes) != 0 {
		t.Errorf("views.py produced routes: %v", signatures(routes))
	}
}

func TestExtractNest(t *testing.T) {
	src := `import { Controller, Get, Post, Delete } from '@nestjs/common';

@Controller('users')
export class UsersController {
  @Get()
  findAll() {}

  @Get(':id')
  findOne(@Param('id') id: string) {}

  @Post('bulk')
  async createMany(@Body() dto: CreateDto) {}

  @Delete(':id')
  @HttpCode(204)
  remove() {}

  @Get('inline') inline() {}
}`
	routes := Extract("users.controller.ts", []byte(src))
	assertRoute(t, routes, "GET /users", "findAll")
	assertRoute(t, routes, "GET /users/:id", "findOne")
	assertRoute(t, routes, "POST /users/bulk", "createMany")
	assertRoute(t, routes, "DELETE /users/:id", "remove")
	assertRoute(t, routes, "GET /users/inline", "inline")
	if len(routes) != 5 {
		t.Errorf("got %d routes, want 5: %v", len(routes), signatures(routes))
	}
	for _, r := range routes {
		if r.Framework != FrameworkNest {
			t.Errorf("%s: framework = %q, want nest", r.Signature(), r.Framework)
		}
		if r.Confidence != model.ConfidenceHigh {
			t.Errorf("%s: confidence = %v, want high", r.Signature(), r.Confidence)
		}
	}
}

// A @Get with no @Controller above it is a decorator on something that is
// not a controller — a test double, a mixin — and must not become a route.
func TestNestRequiresAController(t *testing.T) {
	src := `class NotAController {
  @Get('x')
  x() {}
}`
	if routes := Extract("a.ts", []byte(src)); len(routes) != 0 {
		t.Errorf("a @Get outside @Controller produced routes: %v", signatures(routes))
	}
}

// A decorator whose name merely starts with a verb — @GetUser is not @Get —
// must not match. Without the word boundary this would report a route for a
// decorator no NestJS release has ever defined.
func TestNestVerbMustMatchExactly(t *testing.T) {
	src := `@Controller('users')
export class UsersController {
  @GetUser('x')
  x() {}
}`
	if routes := Extract("a.ts", []byte(src)); len(routes) != 0 {
		t.Errorf("@GetUser produced routes: %v", signatures(routes))
	}
}

// Two controllers in one file each keep their own prefix; an object-form
// @Controller({ path }) and a bare @Controller() are read too.
func TestNestControllerForms(t *testing.T) {
	src := `@Controller({ path: 'orders', version: '1' })
export class OrdersController {
  @Get('open')
  open() {}
}

@Controller()
export class RootController {
  @All('health')
  health() {}
}`
	routes := Extract("c.ts", []byte(src))
	assertRoute(t, routes, "GET /orders/open", "open")
	assertRoute(t, routes, "ANY /health", "health")
	if len(routes) != 2 {
		t.Errorf("got %d routes, want 2: %v", len(routes), signatures(routes))
	}
}

// An argument shape extractNest cannot read — an array of prefixes, an
// object wrapped across lines — must be skipped rather than matched with an
// empty capture: a guessed prefix produces a route that does not exist.
func TestNestSkipsShapesItCannotRead(t *testing.T) {
	t.Run("verb argument is an array", func(t *testing.T) {
		src := `@Controller('u')
export class C {
  @Get(['a', 'b'])
  list() {}
}`
		if routes := Extract("a.ts", []byte(src)); len(routes) != 0 {
			t.Errorf("array-argument @Get produced routes: %v", signatures(routes))
		}
	})

	t.Run("controller argument is an array", func(t *testing.T) {
		src := `@Controller(['a', 'b'])
export class C {
  @Get('x')
  x() {}
}`
		if routes := Extract("a.ts", []byte(src)); len(routes) != 0 {
			t.Errorf("array-argument @Controller produced routes: %v", signatures(routes))
		}
	})

	t.Run("controller object wrapped across lines", func(t *testing.T) {
		src := `@Controller({
  path: 'multi',
})
export class C {
  @Get('x')
  x() {}
}`
		if routes := Extract("a.ts", []byte(src)); len(routes) != 0 {
			t.Errorf("wrapped @Controller produced routes: %v", signatures(routes))
		}
	})

	t.Run("controller object with no path key", func(t *testing.T) {
		src := `@Controller({ version: '1' })
export class C {
  @Get('x')
  x() {}
}`
		routes := Extract("a.ts", []byte(src))
		assertRoute(t, routes, "GET /x", "x")
		if len(routes) != 1 {
			t.Errorf("got %d routes, want 1: %v", len(routes), signatures(routes))
		}
	})
}

// TestNestPrefixDoesNotLeakIntoALaterClass: @Controller decorates ONE class.
// A class declared after it with no decorator of its own is not a controller,
// and a verb-named decorator inside it — a mixin, a helper, a test double —
// must not become a route under the previous class's prefix.
func TestNestPrefixDoesNotLeakIntoALaterClass(t *testing.T) {
	src := `@Controller('users')
export class UsersController {
  @Get(':id')
  findOne() {}
}

export class UsersHelper {
  @Get('leaked')
  helper() {}
}

@Controller('orders')
export class OrdersController {
  @Post()
  create() {}
}`
	routes := Extract("users.controller.ts", []byte(src))
	assertRoute(t, routes, "GET /users/:id", "findOne")
	assertRoute(t, routes, "POST /orders", "create")
	if len(routes) != 2 {
		t.Errorf("got %d routes, want 2 — a class without @Controller is not one: %v", len(routes), signatures(routes))
	}
}

// TestNestHandlerSurvivesMultiLineDecorators: Swagger-annotated controllers
// stack decorators whose arguments span lines between the verb and the
// method. The continuation lines are not declarations and not decorators, so
// a scan that stops at the first such line loses the handler. The route was
// still emitted; the handler was "" on exactly the controllers people
// document most carefully.
func TestNestHandlerSurvivesMultiLineDecorators(t *testing.T) {
	src := `@Controller('users')
export class UsersController {
  @Get(':id')
  @ApiResponse({
    status: 200,
    description: 'The user',
    type: UserDto,
  })
  @ApiParam({ name: 'id' })
  async findOne(@Param('id') id: string) {}

  @Post()
  @UseGuards(
    AuthGuard('jwt'),
    RolesGuard,
  )
  create(@Body() dto: CreateDto) {}
}`
	routes := Extract("users.controller.ts", []byte(src))
	assertRoute(t, routes, "GET /users/:id", "findOne")
	assertRoute(t, routes, "POST /users", "create")
}
