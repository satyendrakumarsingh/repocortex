package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// stageManifest writes one manifest file plus its unit and endpoint nodes, and
// returns the ids the assertions need.
//
// Deliberately built through the real Writer rather than raw SQL: the point of
// several of these tests is that the write path and the read path agree, and a
// fixture that bypasses the writer can only prove the reader agrees with itself.
func stageManifest(
	t *testing.T,
	r *Registry,
	runID int64,
	manifest string,
	units []InfraUnit,
	keys map[string][]InfraKey,
) map[string]int64 {
	t.Helper()
	ctx := context.Background()

	w, err := r.NewWriter(ctx, runID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	fileID, err := w.AddFile(ctx, &model.File{
		Path: manifest, ContentHash: "h-" + manifest, ParseStatus: "ok",
	})
	if err != nil {
		t.Fatalf("add manifest file: %v", err)
	}

	unitIDs := map[string]int64{}
	for _, u := range units {
		sym := model.Symbol{
			Kind: "deployment_unit", Name: u.Name, FQN: "unit:" + manifest + "#" + u.Name,
			FileID: fileID, Range: model.Range{Start: model.Position{Line: u.Line}},
			Exported: true, Confidence: model.ConfidenceHigh, Source: model.SourceConfig,
		}
		symID, err := w.AddSymbol(ctx, &sym)
		if err != nil {
			t.Fatalf("add unit symbol %q: %v", u.Name, err)
		}
		u.SymbolID = symID
		unitID, err := w.AddInfraUnit(ctx, fileID, u)
		if err != nil {
			t.Fatalf("add unit %q: %v", u.Name, err)
		}
		unitIDs[u.Name] = symID
		for _, k := range keys[u.Name] {
			k.UnitID = unitID
			if err := w.AddInfraKey(ctx, fileID, k); err != nil {
				t.Fatalf("add key %q: %v", k.Name, err)
			}
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return unitIDs
}

// addEndpoint writes one endpoint node in a source file, which is what
// BindInfraUnits joins a build context against.
func addEndpoint(t *testing.T, r *Registry, runID int64, path, sig string) int64 {
	t.Helper()
	ctx := context.Background()

	w, err := r.NewWriter(ctx, runID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	fileID, err := w.AddFile(ctx, &model.File{Path: path, ContentHash: "h-" + path, ParseStatus: "ok"})
	if err != nil {
		t.Fatalf("add source file: %v", err)
	}
	sym := model.Symbol{
		Kind: model.KindEndpoint, Name: sig, FQN: path + "#" + sig, FileID: fileID,
		Range: model.Range{Start: model.Position{Line: 10}},
		//nolint:lll // mirrors what analyze.File writes for an endpoint node.
		Exported: true, Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	id, err := w.AddSymbol(ctx, &sym)
	if err != nil {
		t.Fatalf("add endpoint symbol: %v", err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return id
}

// TestBindInfraUnitsJoinsEndpointsToTheServiceTheyShipIn is the join this whole
// feature exists to make possible.
//
// Before deployment facts were indexed there was no answer at all: the endpoint
// is declared in Go, the service is declared in YAML, and nothing in the graph
// connected them. The connection is a build context, and it has to survive from
// the manifest through the index to a query.
func TestBindInfraUnitsJoinsEndpointsToTheServiceTheyShipIn(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	units := stageManifest(t, r, run.ID, "docker-compose.yml", []InfraUnit{
		{Name: "api", Type: "service", Format: "compose", BuildContext: "./services/api", Line: 3},
		{Name: "worker", Type: "service", Format: "compose", BuildContext: "./services/worker", Line: 9},
	}, nil)

	apiEndpoint := addEndpoint(t, r, run.ID, "services/api/http/orders.go", "GET /orders")
	workerEndpoint := addEndpoint(t, r, run.ID, "services/worker/http/health.go", "GET /health")
	// Outside every build context: a shared library. It must bind to nothing
	// rather than to whichever service happens to sort first.
	addEndpoint(t, r, run.ID, "pkg/shared/debug.go", "GET /debug")

	st, err := r.BindInfraUnits(ctx, run.ID)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if st.Units != 2 || st.WithContext != 2 {
		t.Fatalf("staged units: %+v", st)
	}
	if st.Bound != 2 || st.Edges != 2 {
		t.Fatalf("want 2 bound and 2 edges, got %+v", st)
	}

	edges, err := r.Edges(ctx, run.ID)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	got := map[int64]int64{}
	for _, e := range edges {
		if e.Kind != model.EdgeContains {
			continue
		}
		got[e.DstID] = e.SrcID
		if e.Confidence != model.ConfidenceMedium {
			// Directory containment is strong evidence and not proof: a
			// Dockerfile can COPY a subset of its context. If this ever
			// becomes `high`, the graph is claiming more than the manifest.
			t.Errorf("CONTAINS edge claims %s confidence, want medium", e.Confidence)
		}
	}
	if got[apiEndpoint] != units["api"] {
		t.Errorf("GET /orders did not bind to the api service: %v", got)
	}
	if got[workerEndpoint] != units["worker"] {
		t.Errorf("GET /health did not bind to the worker service: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("an endpoint outside every build context acquired a service: %v", got)
	}
}

// TestUnitsForPathPrefersTheMostSpecificContext guards the rule that makes a
// monorepo answerable.
//
// A root `build: .` and a nested `build: ./services/api` both contain
// services/api/main.go. Both are true; only the nested one is the answer, and
// every build system reads it that way. Returning the root would tell a
// developer that changing one service restarts everything.
func TestUnitsForPathPrefersTheMostSpecificContext(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	stageManifest(t, r, run.ID, "docker-compose.yml", []InfraUnit{
		{Name: "monolith", Type: "service", Format: "compose", BuildContext: ".", Line: 2},
		{Name: "api", Type: "service", Format: "compose", Image: "example/api", BuildContext: "./services/api", Line: 6},
	}, nil)

	got, err := r.UnitsForPath(ctx, run.ID, "services/api/main.go")
	if err != nil {
		t.Fatalf("units for path: %v", err)
	}
	if len(got) != 1 || got[0].Name != "api" {
		t.Fatalf("want the api service alone, got %+v", got)
	}
	if got[0].Ambiguous {
		t.Error("a nested context beating a root context is a rule, not an ambiguity")
	}
	if got[0].Image != "example/api" || got[0].Manifest != "docker-compose.yml" || got[0].Line != 6 {
		// The answer has to be verifiable: a bare service name is something a
		// reader must take on trust.
		t.Errorf("match is missing the evidence a reader needs: %+v", got[0])
	}

	// A file the root context contains and no nested one does still answers.
	root, err := r.UnitsForPath(ctx, run.ID, "cmd/tool/main.go")
	if err != nil {
		t.Fatalf("units for path: %v", err)
	}
	if len(root) != 1 || root[0].Name != "monolith" {
		t.Fatalf("want the root context, got %+v", root)
	}
}

// TestUnitsForPathRefusesToPickBetweenTwoServicesOnOneDirectory.
//
// An api and a worker built from one directory is an ordinary pattern, and
// nothing in the manifests picks one. Naming one would be a fabricated
// dependency; naming both is the honest answer.
func TestUnitsForPathRefusesToPickBetweenTwoServicesOnOneDirectory(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	stageManifest(t, r, run.ID, "docker-compose.yml", []InfraUnit{
		{Name: "api", Type: "service", Format: "compose", BuildContext: "./app", Line: 2},
		{Name: "worker", Type: "service", Format: "compose", BuildContext: "./app", Line: 6},
	}, nil)

	got, err := r.UnitsForPath(ctx, run.ID, "app/main.go")
	if err != nil {
		t.Fatalf("units for path: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want both candidates, got %+v", got)
	}
	for _, u := range got {
		if !u.Ambiguous {
			t.Errorf("candidate %q is not flagged ambiguous: %+v", u.Name, u)
		}
	}

	addEndpoint(t, r, run.ID, "app/http/orders.go", "GET /orders")
	st, err := r.BindInfraUnits(ctx, run.ID)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if st.Ambiguous != 1 || st.Edges != 0 {
		t.Fatalf("an ambiguous match must produce no edge: %+v", st)
	}
}

// TestBuildContextIsResolvedAgainstTheManifestDirectory.
//
// `build: ./api` in deploy/docker-compose.yml means deploy/api, not api. This
// does not fail loudly when it is wrong — it silently matches nothing, or
// silently matches another team's directory.
func TestBuildContextIsResolvedAgainstTheManifestDirectory(t *testing.T) {
	cases := []struct {
		manifest string
		context  string
		want     string
		ok       bool
	}{
		{"docker-compose.yml", ".", "", true},
		{"docker-compose.yml", "./services/api", "services/api", true},
		{"deploy/docker-compose.yml", "./api", "deploy/api", true},
		{"deploy/compose.yml", "../services/api", "services/api", true},
		{"deploy/compose.yml", "../../escapes", "", false},
		{"docker-compose.yml", "https://github.com/x/y#main", "", false},
		{"docker-compose.yml", "", "", false},
		{"a/b/compose.yml", "..", "a", true},
	}
	for _, c := range cases {
		got, ok := repoRelativeContext(c.manifest, c.context)
		if ok != c.ok || got != c.want {
			t.Errorf("repoRelativeContext(%q, %q) = %q,%v want %q,%v",
				c.manifest, c.context, got, ok, c.want, c.ok)
		}
	}
}

// TestContextContainsRequiresASeparator: `services/api` must not contain
// `services/api-gateway/main.go`. A raw prefix match here would attach one
// team's endpoints to another team's service.
func TestContextContainsRequiresASeparator(t *testing.T) {
	if contextContains("services/api", "services/api-gateway/main.go") {
		t.Error("prefix match crossed a directory boundary")
	}
	if !contextContains("services/api", "services/api/main.go") {
		t.Error("a file inside the context was not contained")
	}
	if !contextContains("", "anything/at/all.go") {
		t.Error("the root context must contain everything")
	}
}

// TestUnitsForConfigKeyNamesWhatARestartWouldTouch.
//
// The second join: a key name is a node, units that set it are rows, and the
// question "what does changing this restart" becomes answerable. Matching is
// deliberately tight — exact then case-insensitive — because a restart claim
// made through a loose spelling fold would name workloads that do not set the
// key at all.
func TestUnitsForConfigKeyNamesWhatARestartWouldTouch(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	stageManifest(t, r, run.ID, "k8s/deployment.yaml", []InfraUnit{
		{Name: "api/api", Type: "Deployment", Format: "kubernetes", Line: 12},
		{Name: "worker/worker", Type: "Deployment", Format: "kubernetes", Line: 40},
	}, map[string][]InfraKey{
		"api/api": {
			{Name: "DATABASE_URL", From: "secret-ref", Ref: "db-creds:url", Secret: true, Line: 20},
			{Name: "LOG_LEVEL", From: "literal", Line: 22},
		},
		"worker/worker": {
			{Name: "DATABASE_URL", From: "configmap-ref", Ref: "settings:url", Line: 48},
		},
	})

	impact, err := r.UnitsForConfigKey(ctx, run.ID, "DATABASE_URL")
	if err != nil {
		t.Fatalf("units for key: %v", err)
	}
	if len(impact.Units) != 2 {
		t.Fatalf("want both workloads, got %+v", impact.Units)
	}
	if !impact.Secret {
		t.Error("a key supplied by a Secret is sensitive whatever else sets it")
	}
	if len(impact.Sources) != 2 || impact.Sources[0] != "configmap-ref" || impact.Sources[1] != "secret-ref" {
		t.Errorf("sources must be distinct and sorted, got %v", impact.Sources)
	}

	// A key only one workload sets names only that workload.
	one, err := r.UnitsForConfigKey(ctx, run.ID, "LOG_LEVEL")
	if err != nil {
		t.Fatalf("units for key: %v", err)
	}
	if len(one.Units) != 1 || one.Units[0].Name != "api/api" {
		t.Fatalf("want the api workload alone, got %+v", one.Units)
	}
	if one.Secret {
		t.Error("LOG_LEVEL is not a credential by name or by source")
	}

	// A key nothing sets is an empty answer, not an error: a Secret created
	// outside this repository is invisible from here.
	none, err := r.UnitsForConfigKey(ctx, run.ID, "NOT_SET_ANYWHERE")
	if err != nil {
		t.Fatalf("units for key: %v", err)
	}
	if len(none.Units) != 0 {
		t.Fatalf("want no units, got %+v", none.Units)
	}
}

// TestInfraReportKeepsUnreadableManifestsVisible.
//
// The property the problems table exists for. "This repository has no
// Kubernetes manifests" and "this repository's Kubernetes manifests could not
// be parsed" have opposite consequences, and an index that stored only
// successes makes them the same answer.
func TestInfraReportKeepsUnreadableManifestsVisible(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	w, err := r.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	fileID, err := w.AddFile(ctx, &model.File{
		Path: "k8s/broken.yaml", ContentHash: "h", ParseStatus: "ok",
	})
	if err != nil {
		t.Fatalf("add file: %v", err)
	}
	if err := w.AddInfraProblem(ctx, fileID, InfraProblem{
		Format: "kubernetes", Reason: "malformed_yaml", Line: 12,
	}); err != nil {
		t.Fatalf("add problem: %v", err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	_ = w.Close()

	report, err := r.InfraReport(ctx, run.ID)
	if err != nil {
		t.Fatalf("infra report: %v", err)
	}
	if len(report) != 1 || len(report[0].Problems) != 1 {
		t.Fatalf("an unreadable manifest vanished from the report: %+v", report)
	}
	p := report[0].Problems[0]
	if p.Reason != "malformed_yaml" || p.Line != 12 || report[0].Path != "k8s/broken.yaml" {
		t.Errorf("problem lost its identity: %+v in %q", p, report[0].Path)
	}
}

// TestSymbolsByNameIsDeterministicAcrossInsertionOrder.
//
// The first-wins lookup that BindConfigUsages depends on read its rows with no
// ORDER BY. That was latent while configuration keys came only from .env files,
// where one repository rarely declares a key twice; deployment manifests make
// duplicates the ordinary case — a Deployment, a ConfigMap and a compose file
// all naming DATABASE_URL — so the same source could bind to different nodes on
// two runs. Same shape, same measured consequence, as the table-symbol bug in
// dataaccess_test.go.
func TestSymbolsByNameIsDeterministicAcrossInsertionOrder(t *testing.T) {
	ctx := context.Background()

	early := fileSpec{
		path: "compose.yml", lang: "", hash: "h1",
		syms: []symSpec{{fqn: "config:DATABASE_URL", name: "DATABASE_URL", line: 4, kind: model.KindConfiguration}},
	}
	late := fileSpec{
		path: "k8s/deployment.yaml", lang: "", hash: "h2",
		syms: []symSpec{{fqn: "config:DATABASE_URL", name: "DATABASE_URL", line: 20, kind: model.KindConfiguration}},
	}

	pick := func(t *testing.T, files []fileSpec) string {
		t.Helper()
		r, _ := newTestRegistry(t)
		run, err := r.BeginRun(ctx, "auto", "rev")
		if err != nil {
			t.Fatalf("begin run: %v", err)
		}
		writeContent(t, r, run.ID, files, nil)

		got, err := r.symbolsByName(ctx, run.ID, model.KindConfiguration)
		if err != nil {
			t.Fatalf("symbolsByName: %v", err)
		}
		id, ok := got["DATABASE_URL"]
		if !ok {
			t.Fatalf("the key did not resolve at all: %v", got)
		}
		var path string
		if err := r.db.QueryRowContext(ctx,
			`SELECT f.path FROM symbols s JOIN files f ON f.id = s.file_id WHERE s.id = ?`,
			id).Scan(&path); err != nil {
			t.Fatalf("locate winner: %v", err)
		}
		return path
	}

	forward := pick(t, []fileSpec{early, late})
	reverse := pick(t, []fileSpec{late, early})
	if forward != reverse {
		t.Fatalf("the winning declaration depends on insertion order: %q vs %q", forward, reverse)
	}
	if forward != "compose.yml" {
		// Ordered by path, so "first" means first in the repository rather
		// than whichever row the storage engine handed over first.
		t.Errorf("winner is %q, want the path-first declaration", forward)
	}
}
