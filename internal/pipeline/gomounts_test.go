package pipeline

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
)

func goSignatures(t *testing.T, tree map[string]string) []string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, tree)
	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))
	eps, err := reg.EndpointReport(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range eps {
		out = append(out, e.Signature)
	}
	sort.Strings(out)
	return out
}

func hasAll(t *testing.T, got []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing %q; got %v", w, got)
		}
	}
}

// Gin: main.go builds the groups and hands them to packages that fill them,
// one of which hands its group on again.
func TestGinRouterGroupsHandedToOtherPackages(t *testing.T) {
	got := goSignatures(t, map[string]string{
		"backend/go.mod": "module example.com/shop\n\ngo 1.22\n",
		"backend/cmd/server/main.go": `package main

import (
	"example.com/shop/internal/users"
	"example.com/shop/internal/admin"
	"github.com/gin-gonic/gin"
)

func main() {
	r := gin.Default()
	api := r.Group("/api")
	users.RegisterRoutes(api.Group("/users"))
	admin.Register(api)
	r.Run()
}
`,
		"backend/internal/users/routes.go": `package users

import "github.com/gin-gonic/gin"

func RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/:id", get)
	rg.POST("", create)
}
`,
		"backend/internal/admin/routes.go": `package admin

import (
	"example.com/shop/internal/admin/audit"
	"github.com/gin-gonic/gin"
)

func Register(rg *gin.RouterGroup) {
	g := rg.Group("/admin")
	g.DELETE("/cache", clearCache)
	audit.Mount(g)
}
`,
		"backend/internal/admin/audit/audit.go": `package audit

import "github.com/gin-gonic/gin"

func Mount(rg *gin.RouterGroup) {
	rg.GET("/audit", list)
}
`,
	})
	hasAll(t, got,
		"GET /api/users/:id",
		"POST /api/users",
		"DELETE /api/admin/cache",
		"GET /api/admin/audit", // a group handed on twice
	)
}

// chi: a router another package builds and returns is mounted, and a
// function value is handed to Route.
func TestChiMountedAndRoutedFromOtherPackages(t *testing.T) {
	got := goSignatures(t, map[string]string{
		"go.mod": "module example.com/pay\n\ngo 1.22\n",
		"main.go": `package main

import (
	"example.com/pay/billing"
	"example.com/pay/reports"
	"github.com/go-chi/chi/v5"
)

func main() {
	r := chi.NewRouter()
	r.Mount("/billing", billing.Routes())
	r.Route("/reports", reports.Register)
}
`,
		"billing/routes.go": `package billing

import "github.com/go-chi/chi/v5"

func Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/invoices", list)
	r.Route("/v2", func(r chi.Router) {
		r.Post("/charge", charge)
	})
	return r
}
`,
		"reports/routes.go": `package reports

import "github.com/go-chi/chi/v5"

func Register(r chi.Router) {
	r.Get("/daily", daily)
}
`,
	})
	hasAll(t, got, "GET /billing/invoices", "POST /billing/v2/charge", "GET /reports/daily")
}

// Served under two prefixes: left as written. A test's own router does not
// decide where the service serves a package's routes.
func TestGoRoutersUnderTwoPrefixesOrInTests(t *testing.T) {
	got := goSignatures(t, map[string]string{
		"go.mod": "module example.com/x\n\ngo 1.22\n",
		"main.go": `package main

import (
	"example.com/x/users"
	"example.com/x/items"
	"github.com/gin-gonic/gin"
)

func main() {
	r := gin.Default()
	users.Register(r.Group("/v1"))
	users.Register(r.Group("/v2"))
	items.Register(r.Group("/items"))
}
`,
		"users/users.go":      "package users\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc Register(rg *gin.RouterGroup) {\n\trg.GET(\"/me\", me)\n}\n",
		"items/items.go":      "package items\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc Register(rg *gin.RouterGroup) {\n\trg.GET(\"/:id\", get)\n}\n",
		"items/items_test.go": "package items\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc setup() {\n\tr := gin.New()\n\tRegister(r.Group(\"/t\"))\n}\n",
	})
	hasAll(t, got, "GET /me", "GET /items/:id")
}

// Recomputed from each route's own path: a second pass changes nothing and a
// stale name (a call removed since) is put right.
func TestGoMountNamesAreRecomputed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":         "module example.com/y\n\ngo 1.22\n",
		"main.go":        "package main\n\nimport (\n\t\"example.com/y/users\"\n\t\"github.com/gin-gonic/gin\"\n)\n\nfunc main() {\n\tr := gin.Default()\n\tusers.Register(r.Group(\"/users\"))\n}\n",
		"users/users.go": "package users\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc Register(rg *gin.RouterGroup) {\n\trg.GET(\"/:id\", get)\n}\n",
	})
	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))
	st, err := MountGoRouters(ctx, reg, runID)
	if err != nil || st.Renamed != 0 || st.Mounts != 1 {
		t.Fatalf("second pass: %+v, %v; want 1 mount and nothing renamed", st, err)
	}
	eps, err := reg.EndpointReport(ctx, runID)
	if err != nil || len(eps) != 1 || eps[0].Signature != "GET /users/:id" {
		t.Fatalf("endpoints = %+v, %v", eps, err)
	}
	if err := reg.RenameEndpoints(ctx, map[int64]string{eps[0].SymbolID: "GET /old/:id"}); err != nil {
		t.Fatal(err)
	}
	if st, err = MountGoRouters(ctx, reg, runID); err != nil || st.Renamed != 1 {
		t.Fatalf("restore pass renamed %d, err %v; want 1", st.Renamed, err)
	}
}

// NestJS: app.setGlobalPrefix in an application's bootstrap prefixes that
// application's controllers, and no one else's.
func TestNestGlobalPrefixAppliesToItsOwnControllers(t *testing.T) {
	got := goSignatures(t, map[string]string{
		"apps/shop/package.json": `{"name":"shop"}`,
		"apps/shop/src/main.ts": `import { NestFactory } from '@nestjs/core';
async function bootstrap() {
  const app = await NestFactory.create(AppModule);
  app.setGlobalPrefix('api');
  await app.listen(3000);
}
`,
		"apps/shop/src/users.controller.ts": `import { Controller, Get } from '@nestjs/common';
@Controller('users')
export class UsersController {
  @Get(':id')
  find() {}
}
`,
		"apps/shop/src/health.js": `const express = require('express');
const app = express();
app.get('/health', ok);
`,
		"apps/admin/package.json": `{"name":"admin"}`,
		"apps/admin/src/main.ts": `async function bootstrap() {
  const app = await NestFactory.create(AppModule);
  app.setGlobalPrefix('admin', { exclude: ['health'] });
}
`,
		"apps/admin/src/stats.controller.ts": `@Controller('stats')
export class StatsController {
  @Get()
  all() {}
}
`,
	})
	hasAll(t, got,
		"GET /api/users/:id", // the shop's prefix
		"GET /health",        // an Express route beside the controllers is not Nest's
		"GET /stats",         // a prefix with exclusions is not guessed at
	)
}

// The shape of a real chi service: handlers are struct types in one package,
// each with a Mount method, reached through a dependencies struct inside
// Route and Group closures, called and handed over as a function value.
func TestChiHandlersMountedThroughADependencyStruct(t *testing.T) {
	got := goSignatures(t, map[string]string{
		"svc/go.mod": "module example.com/notif\n\ngo 1.22\n",
		"svc/internal/server/server.go": `package server

import (
	"example.com/notif/internal/api/routes"
	"github.com/go-chi/chi/v5"
)

type Deps struct {
	Notifications *routes.NotificationsHandler
	MerchantAck   *routes.MerchantAckHandler
}

func New(deps Deps) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/notifications", func(sub chi.Router) {
		sub.Get("/webhook", verify)
		deps.Notifications.Mount(sub)
		sub.Group(func(internal chi.Router) {
			internal.Route("/merchant-ack", deps.MerchantAck.Mount)
		})
	})
	return r
}
`,
		"svc/internal/api/routes/notifications.go": `package routes

import "github.com/go-chi/chi/v5"

type NotificationsHandler struct{}

func (h *NotificationsHandler) Mount(r chi.Router) {
	r.Get("/log", h.list)
}
`,
		"svc/internal/api/routes/merchant_ack.go": `package routes

import "github.com/go-chi/chi/v5"

type MerchantAckHandler struct{}

func (h *MerchantAckHandler) Mount(r chi.Router) {
	r.Post("/{id}", h.ack)
}
`,
	})
	hasAll(t, got,
		"GET /api/notifications/webhook",            // in-file closure
		"GET /api/notifications/log",                // deps.Notifications.Mount(sub)
		"POST /api/notifications/merchant-ack/{id}", // Route with a method value, inside a Group
	)
}
