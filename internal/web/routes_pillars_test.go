package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// The pillar screens are exercised through their handlers rather than through
// the mux, because the mux registration lives in web.go, which this work does
// not own. registerPillarRoutes is asserted separately, on a mux of its own, so
// the route table is still covered.

// pillarGet drives one handler and decodes the body.
func pillarGet(t *testing.T, h http.HandlerFunc, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", path, rec.Code, rec.Body.String())
	}
	if v != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("GET %s: decode body %q: %v", path, rec.Body.String(), err)
		}
	}
	return rec
}

// pillarRaw decodes into a generic map, so a test can assert a field is
// PRESENT — which a typed struct cannot distinguish from a zero value.
func pillarRaw(t *testing.T, h http.HandlerFunc, path string) map[string]any {
	t.Helper()
	var m map[string]any
	pillarGet(t, h, path, &m)
	return m
}

// realRegistry recovers the concrete registry behind the capability deps, so a
// test can wrap it in a port the production wiring does not yet supply.
func realRegistry(t *testing.T, deps capability.Deps) *registry.Registry {
	t.Helper()
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	return reg
}

// repoFixture is fixture() with a repository root, for the three screens that
// read an artefact out of the tree. Returns the root so a test can write one.
func repoFixture(t *testing.T) (*Server, capability.Deps, int64, string) {
	t.Helper()
	srv, deps, runID := fixture(t)
	root := t.TempDir()
	withRoot := newServer(t, realRegistry(t, deps), deps,
		config.Config{ProjectName: "fixture", RepoRoot: root})
	_ = srv
	return withRoot, deps, runID, root
}

// pillarUnindexedServer is a portal over a store with no committed run.
//
// Its own helper rather than a shared one: every pillar screen must answer this
// state with a 200 and guidance, and a test for that contract should not be
// able to break because a fixture elsewhere in the package was renamed.
func pillarUnindexedServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	return newServer(t, reg, capability.NewDeps(reg, graph.New(db)),
		config.Config{ProjectName: "unindexed", RepoRoot: t.TempDir()})
}

func writeRepoFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestPillarRoutesAreRegistered pins the six routes and their methods.
//
// Registered onto a mux this test owns, because the server's own mux is built
// in web.go. When the composition root calls registerPillarRoutes, the same six
// patterns are what it gets.
func TestPillarRoutesAreRegistered(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	mux := http.NewServeMux()
	srv.registerPillarRoutes(mux)

	for _, tc := range []struct {
		route string
		typ   string
	}{
		{"/api/accuracy", "AccuracyResponse"},
		{"/api/diff", "FactDiffResponse"},
		{"/api/who", "OwnershipResponse"},
		{"/api/trace", "TraceResponse"},
		{"/api/bench", "BenchResponse"},
		{"/api/system", "EstateResponse"},
	} {
		t.Run(tc.route, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.route, nil)
			req.Host = "127.0.0.1:7777"
			h, pattern := mux.Handler(req)
			if pattern == "" {
				t.Fatalf("%s is not registered", tc.route)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d, body %s", tc.route, rec.Code, rec.Body.String())
			}

			// Every pillar response carries the standard header and an omission
			// block, and neither is optional.
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: decode: %v", tc.route, err)
			}
			if _, ok := body["meta"]; !ok {
				t.Errorf("%s (%s) carries no meta header", tc.route, tc.typ)
			}
			om, ok := body["omissions"].(map[string]any)
			if !ok {
				t.Fatalf("%s carries no omissions block", tc.route)
			}
			for _, field := range []string{"omitted", "totals", "complete"} {
				if _, ok := om[field]; !ok {
					t.Errorf("%s: omissions.%s is absent; it is derived at marshal time and cannot be", tc.route, field)
				}
			}
			meta, _ := body["meta"].(map[string]any)
			if _, ok := meta["index_health"]; !ok {
				t.Errorf("%s: meta.index_health is absent; it is never omitempty", tc.route)
			}
		})
	}
}

// TestRepoFileStaysInsideTheRepository is the containment rule the three
// file-reading screens rest on.
//
// A local portal that opens any path a query string names is an
// arbitrary-file-read primitive one rebinding mistake away from being remote.
func TestRepoFileStaysInsideTheRepository(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_, deps, _ := fixture(t)
	srv := newServer(t, realRegistry(t, deps), deps,
		config.Config{ProjectName: "fixture", RepoRoot: root})

	for _, tc := range []struct {
		name      string
		query     string
		wantPath  string
		wantOK    bool
		defaulted bool
	}{
		{name: "defaults to the conventional name", query: "", wantPath: "traces.json", wantOK: true, defaulted: true},
		{name: "accepts a repo-relative path", query: "?export=exports/a.json", wantPath: "exports/a.json", wantOK: true},
		{name: "refuses an escape", query: "?export=../../etc/passwd", wantOK: false},
		{name: "refuses an absolute path", query: "?export=/etc/passwd", wantOK: false},
		{name: "normalises a dotted path", query: "?export=./a/../b.json", wantPath: "b.json", wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/trace"+tc.query, nil)
			ref := srv.repoFile(req, "export", "traces.json")
			if tc.wantOK && ref.Problem != "" {
				t.Fatalf("%s was refused: %s", tc.query, ref.Problem)
			}
			if !tc.wantOK {
				if ref.Problem == "" {
					t.Fatalf("%s was accepted and resolved to %s", tc.query, ref.Absolute)
				}
				if ref.Absolute != "" {
					t.Errorf("%s produced an absolute path %q despite being refused", tc.query, ref.Absolute)
				}
				return
			}
			if ref.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", ref.Path, tc.wantPath)
			}
			if ref.Defaulted != tc.defaulted {
				t.Errorf("defaulted = %v, want %v", ref.Defaulted, tc.defaulted)
			}
			if tc.defaulted && ref.DefaultedFrom == "" {
				t.Error("a defaulted file must say what it defaulted to")
			}
			if !filepath.IsAbs(ref.Absolute) {
				t.Errorf("absolute = %q, want an absolute path", ref.Absolute)
			}
		})
	}
}

// TestRepoFileWithoutARepositoryRootSaysSo: an embedder that wired no root gets
// a sentence, not a silent empty screen.
func TestRepoFileWithoutARepositoryRootSaysSo(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t) // config.Config{} — no RepoRoot
	req := httptest.NewRequest(http.MethodGet, "/api/trace", nil)
	ref := srv.repoFile(req, "export", "traces.json")
	if ref.Problem == "" {
		t.Fatal("a portal with no repository root resolved a file anyway")
	}
	if ref.Found {
		t.Error("found must be false when nothing was opened")
	}
}

// TestRepoFileRefusesASymlinkEscape: a symlink inside the repository that
// points outside it must be refused by the routes that read repository files
// (?export=, ?estate=, ?cases=), not merely a lexical "..".
func TestRepoFileRefusesASymlinkEscape(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "x.json"), []byte(`{"secret":"OUTSIDE"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "new.json"), filepath.Join(root, "dangling.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, deps, _ := fixture(t)
	srv := newServer(t, realRegistry(t, deps), deps,
		config.Config{ProjectName: "fixture", RepoRoot: root})

	for _, param := range []string{"export", "estate", "cases"} {
		for _, p := range []string{"linkdir/x.json", "dangling.json"} {
			req := httptest.NewRequest(http.MethodGet, "/x?"+param+"="+p, nil)
			ref := srv.repoFile(req, param, "default.json")
			if ref.Problem == "" || ref.Absolute != "" {
				t.Errorf("?%s=%s: escape accepted (absolute=%q)", param, p, ref.Absolute)
			}
		}
	}

	var got TraceResponse
	pillarGet(t, srv.handleTrace, "/api/trace?export=linkdir/x.json", &got)
	if got.Correlated || got.Export.Found || got.Export.Problem == "" {
		t.Fatalf("trace route read through a symlink escape: %+v", got.Export)
	}
}
