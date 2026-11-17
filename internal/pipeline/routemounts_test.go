package pipeline

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A FastAPI service whose routers live in their own modules and are mounted by
// the application module, which is how FastAPI's own documentation lays one
// out. Every route was indexed without the prefix it is served under.
func mountTree() map[string]string {
	return map[string]string{
		"backend/app/main.py": `from fastapi import FastAPI
from app.api import users
from app.api.items import router as items_router
from .admin import panel
from app.api import twice

app = FastAPI()
app.include_router(users.router, prefix="/users")
app.include_router(
    items_router,
    prefix='/items',
)
app.include_router(panel.router, prefix="/admin")
app.include_router(twice.router, prefix="/v1")
app.include_router(twice.router, prefix="/v2")
`,
		"backend/app/api/__init__.py": "",
		"backend/app/api/users.py": `from fastapi import APIRouter

router = APIRouter()

@router.get("/{user_id}")
def get_user(user_id: int):
    return {}
`,
		"backend/app/api/items.py": `from fastapi import APIRouter
from . import sub

router = APIRouter(prefix="/catalog")
router.include_router(sub.router, prefix="/sub")

@router.get('/')
def list_items():
    return []
`,
		"backend/app/api/sub.py": `from fastapi import APIRouter

router = APIRouter()

@router.post("/x")
def make_x():
    return {}
`,
		"backend/app/admin/panel.py": `from fastapi import APIRouter

router = APIRouter()

@router.delete("/cache")
def clear_cache():
    return {}
`,
		"backend/app/api/twice.py": `from fastapi import APIRouter

router = APIRouter()

@router.get("/status")
def status():
    return {}
`,
		"web/client.js": `export async function loadUser() {
	const res = await fetch("/users/42");
	return res.json();
}
`,
	}
}

func TestPythonRoutersMountedFromAnotherFileGetTheirPrefix(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, mountTree())
	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))

	endpoints, err := reg.EndpointReport(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range endpoints {
		got = append(got, e.Signature)
	}
	sort.Strings(got)
	want := map[string]bool{
		"GET /users/{user_id}":      true, // module.attribute
		"GET /items/catalog":        true, // imported name, the router's own prefix kept below the mount
		"POST /items/catalog/sub/x": true, // a mount of a mounted router
		"DELETE /admin/cache":       true, // relative import
		"GET /status":               true, // served under /v1 and /v2: one symbol cannot say both
	}
	for w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing endpoint %q; got %v", w, got)
		}
	}

	// A client calling the mounted URL now binds to the route.
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	consumes := 0
	for _, e := range edges {
		if e.Kind == model.EdgeConsumes {
			consumes++
		}
	}
	if consumes != 1 {
		t.Errorf("fetch(\"/users/42\") bound %d times, want 1", consumes)
	}

	// Recomputed from the route's own path, so a second pass changes nothing
	// and a stale name is put right.
	st, err := MountPythonRouters(ctx, reg, runID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Renamed != 0 {
		t.Errorf("second pass renamed %d endpoints, want 0", st.Renamed)
	}
	if st.Ambiguous != 1 {
		t.Errorf("ambiguous = %d, want 1 (twice.router)", st.Ambiguous)
	}
	var usersID int64
	for _, e := range endpoints {
		if e.Signature == "GET /users/{user_id}" {
			usersID = e.SymbolID
		}
	}
	if err := reg.RenameEndpoints(ctx, map[int64]string{usersID: "GET /stale"}); err != nil {
		t.Fatal(err)
	}
	if st, err = MountPythonRouters(ctx, reg, runID); err != nil || st.Renamed != 1 {
		t.Fatalf("restore pass: renamed %d, err %v; want 1", st.Renamed, err)
	}
}

func TestResolvePyModule(t *testing.T) {
	files := []string{
		"a/svc/api/users.py",
		"b/svc/api/users.py",
		"pkg/__init__.py",
		"svc/api/orders.py",
	}
	sort.Strings(files)
	cases := []struct {
		importer, module, want string
		tie                    bool
	}{
		{"svc/main.py", "api.orders", "svc/api/orders.py", false},
		{"a/svc/main.py", "svc.api.users", "a/svc/api/users.py", false}, // nearest wins
		{"main.py", "svc.api.users", "", true},                          // two equally near
		{"x/main.py", "pkg", "pkg/__init__.py", false},
		{"svc/api/v.py", ".orders", "svc/api/orders.py", false},
		{"svc/api/deep/v.py", "..orders", "svc/api/orders.py", false},
		{"svc/main.py", "fastapi_users", "", false}, // outside the repository
	}
	for _, c := range cases {
		got, tie := resolvePyModule(c.importer, c.module, files)
		if got != c.want || tie != c.tie {
			t.Errorf("resolvePyModule(%q, %q) = %q, %v; want %q, %v", c.importer, c.module, got, tie, c.want, c.tie)
		}
	}
}

// A test that mounts a router under its own app does not decide where the
// service serves it.
func TestMountsInTestsAreIgnored(t *testing.T) {
	files := []string{"app/api/users.py", "app/main.py", "tests/test_users.py"}
	mountFiles := []registry.FileText{
		{Path: "app/main.py", Content: "from app.api import users\napp.include_router(users.router, prefix=\"/users\")\n"},
		{Path: "tests/test_users.py", Content: "from app.api import users\napp.include_router(users.router, prefix=\"/t\")\n"},
	}
	got, st := pythonMountPrefixes(mountFiles, files)
	if p := got[mountNode{"app/api/users.py", "router"}]; p != "/users" || st.Ambiguous != 0 {
		t.Fatalf("prefix = %q, ambiguous = %d; want /users, 0", p, st.Ambiguous)
	}
}

func TestMountExpressRouters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	tree := map[string]string{
		"app.js": `const express = require('express');
const usersRouter = require('./routes/users');
const app = express();
app.use('/api/v1/users', usersRouter);
`,
		"routes/users.js": `const express = require('express');
const router = express.Router();
router.get('/:id', getUser);
module.exports = router;
`,
	}
	writeTree(t, root, tree)
	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))

	endpoints, err := reg.EndpointReport(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range endpoints {
		if e.Signature == "GET /api/v1/users/:id" {
			found = true
			break
		}
	}
	if !found {
		var sigs []string
		for _, e := range endpoints {
			sigs = append(sigs, e.Signature)
		}
		t.Fatalf("did not find GET /api/v1/users/:id in %v", sigs)
	}
}
