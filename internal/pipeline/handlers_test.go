package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// handlersTree declares routes in one file and their handlers in others. The
// cases are chosen so each outcome is distinguishable:
//   - handleHealth is declared once, in another file: bound.
//   - handleDup is declared in two non-test files: refused as ambiguous.
//   - handleOnlyInTests is declared only in a test file: never a candidate.
//   - handleMissing is declared nowhere: unmatched.
func handlersTree() map[string]string {
	return map[string]string{
		"server/routes.go": `package server

import "net/http"

func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("POST /api/dup", handleDup)
	mux.HandleFunc("GET /api/tests", handleOnlyInTests)
	mux.HandleFunc("GET /api/missing", handleMissing)
}
`,
		"server/health.go": `package server

import "net/http"

func handleHealth(w http.ResponseWriter, r *http.Request) {}
`,
		"server/dup_a.go": `package server

import "net/http"

func handleDup(w http.ResponseWriter, r *http.Request) {}
`,
		"server/dup_b.go": `package server

import "net/http"

func handleDup(w http.ResponseWriter, r *http.Request) {}
`,
		"server/helpers_test.go": `package server

import "testing"

func handleOnlyInTests(t *testing.T) {}
`,
	}
}

func TestEndpointHandlersBindAcrossFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, handlersTree())

	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))

	idx, err := reg.LoadSymbolIndex(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := reg.EndpointReport(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}

	handlerOf := map[string]model.Edge{}
	for _, e := range edges {
		if e.Kind == model.EdgeExposes {
			handlerOf[endpointName(endpoints, e.SrcID)] = e
		}
	}

	h := handlerOf["GET /api/health"]
	if h.DstID == 0 {
		t.Fatal("GET /api/health was not bound to handleHealth in another file")
	}
	if h.Confidence != model.ConfidenceMedium {
		t.Errorf("cross-file name binding confidence = %s, want medium", h.Confidence)
	}
	if !hasSymbolIn(idx["handleHealth"], h.DstID, "server/health.go") {
		t.Errorf("GET /api/health bound to %d, which is not handleHealth in server/health.go", h.DstID)
	}

	if _, ok := handlerOf["POST /api/dup"]; ok {
		t.Error("POST /api/dup was bound although handleDup is declared in two files")
	}
	if _, ok := handlerOf["GET /api/tests"]; ok {
		t.Error("GET /api/tests was bound to a helper that exists only in test code")
	}
	if _, ok := handlerOf["GET /api/missing"]; ok {
		t.Error("GET /api/missing was bound to a handler that does not exist")
	}
}

func endpointName(eps []registry.Endpoint, id int64) string {
	for _, ep := range eps {
		if ep.SymbolID == id {
			return ep.Signature
		}
	}
	return ""
}

func hasSymbolIn(entries []registry.SymbolIndexEntry, id int64, path string) bool {
	for _, e := range entries {
		if e.ID == id && strings.HasSuffix(e.FilePath, path) {
			return true
		}
	}
	return false
}
