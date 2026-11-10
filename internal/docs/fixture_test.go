package docs

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/arch"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fakeRegistry is the whole cost of the port in ports.go: three methods and a
// struct literal. A port a fake cannot cheaply implement has stopped being a
// test aid, and this is what "cheaply" is supposed to mean.
type fakeRegistry struct {
	files   []model.File
	symbols []model.Symbol
	edges   []model.Edge
	err     error
}

func (f *fakeRegistry) Files(_ context.Context, _ int64) ([]model.File, error) {
	return f.files, f.err
}
func (f *fakeRegistry) Symbols(_ context.Context, _ int64) ([]model.Symbol, error) {
	return f.symbols, f.err
}
func (f *fakeRegistry) Edges(_ context.Context, _ int64) ([]model.Edge, error) {
	return f.edges, f.err
}

// shuffled returns the same index with every slice reordered.
//
// This is the test that determinism is a property of this package rather than of
// the registry's ORDER BY clauses. Same seed every run, so a failure reproduces.
func (f *fakeRegistry) shuffled(seed int64) *fakeRegistry {
	rng := rand.New(rand.NewSource(seed))
	out := &fakeRegistry{
		files:   append([]model.File(nil), f.files...),
		symbols: append([]model.Symbol(nil), f.symbols...),
		edges:   append([]model.Edge(nil), f.edges...),
		err:     f.err,
	}
	rng.Shuffle(len(out.files), func(i, j int) { out.files[i], out.files[j] = out.files[j], out.files[i] })
	rng.Shuffle(len(out.symbols), func(i, j int) { out.symbols[i], out.symbols[j] = out.symbols[j], out.symbols[i] })
	rng.Shuffle(len(out.edges), func(i, j int) { out.edges[i], out.edges[j] = out.edges[j], out.edges[i] })
	return out
}

// ---------------------------------------------------------------- the fixture

// Symbol and file ids used by the small fixture. Named constants rather than
// literals so a test that adds a symbol cannot silently reuse an id and produce
// a graph the registry could never have written.
const (
	fileAPI = int64(iota + 1)
	fileService
	fileStore
	fileSchema
)

const (
	symEndpointList = int64(iota + 100)
	symEndpointCreate
	symEndpointOrphan
	symListUsers
	symCreateUser
	symUserServiceList
	symUserRepoQuery
	symRepositoryIface
	symUserRepoClass
	symTableUsers
	symTableAudit
)

func sym(id int64, fileID int64, kind model.EntityKind, name, fqn string, line int) model.Symbol {
	return model.Symbol{
		ID: id, FileID: fileID, Kind: kind, Name: name, FQN: fqn,
		Range:      model.Range{Start: model.Position{Line: line}},
		Exported:   true,
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceSourceCode,
	}
}

func edge(id, src, dst int64, kind model.EdgeKind, path string, line int, conf model.Confidence) model.Edge {
	return model.Edge{
		ID: id, SrcID: src, DstID: dst, Kind: kind, Confidence: conf,
		Evidence: model.Evidence{
			FilePath: path,
			Range:    model.Range{Start: model.Position{Line: line}},
			Source:   model.SourceSourceCode,
			Detail:   string(kind) + " from " + path,
		},
	}
}

// smallIndex is a three-component service: routes in api, logic in service,
// SQL in store, plus one rule the code breaks.
func smallIndex() *fakeRegistry {
	return &fakeRegistry{
		files: []model.File{
			{ID: fileAPI, Path: "internal/api/users.go", Language: "go", ParseStatus: "ok"},
			{ID: fileService, Path: "internal/service/user.go", Language: "go", ParseStatus: "ok"},
			{ID: fileStore, Path: "internal/store/users.go", Language: "go", ParseStatus: "ok"},
			{ID: fileSchema, Path: "migrations/0001_users.sql", Language: "sql", ParseStatus: "ok"},
		},
		symbols: []model.Symbol{
			sym(symEndpointList, fileAPI, model.KindEndpoint, "GET /users", "endpoint:GET /users", 10),
			sym(symEndpointCreate, fileAPI, model.KindEndpoint, "POST /users", "endpoint:POST /users", 20),
			sym(symEndpointOrphan, fileAPI, model.KindEndpoint, "GET /health", "endpoint:GET /health", 25),
			sym(symListUsers, fileAPI, model.KindFunction, "ListUsers", "api.ListUsers", 30),
			sym(symCreateUser, fileAPI, model.KindFunction, "CreateUser", "api.CreateUser", 40),
			sym(symUserServiceList, fileService, model.KindMethod, "List", "service.UserService.List", 12),
			sym(symUserRepoQuery, fileStore, model.KindMethod, "Query", "store.UserRepo.Query", 15),
			sym(symRepositoryIface, fileService, model.KindInterface, "Repository", "service.Repository", 5),
			sym(symUserRepoClass, fileStore, model.KindClass, "UserRepo", "store.UserRepo", 8),
			sym(symTableUsers, fileSchema, model.KindTable, "users", "table:users", 1),
			sym(symTableAudit, fileSchema, model.KindTable, "audit_log", "table:audit_log", 9),
		},
		edges: []model.Edge{
			edge(1, symEndpointList, symListUsers, model.EdgeExposes, "internal/api/users.go", 10, model.ConfidenceHigh),
			edge(2, symEndpointCreate, symCreateUser, model.EdgeExposes, "internal/api/users.go", 20, model.ConfidenceHigh),
			edge(3, symListUsers, symUserServiceList, model.EdgeCalls, "internal/api/users.go", 32, model.ConfidenceHigh),
			edge(4, symUserServiceList, symUserRepoQuery, model.EdgeCalls, "internal/service/user.go", 14, model.ConfidenceMedium),
			edge(5, symUserRepoQuery, symTableUsers, model.EdgeReads, "internal/store/users.go", 17, model.ConfidenceHigh),
			// The rule-breaker: a route handler writing a table directly.
			edge(6, symCreateUser, symTableUsers, model.EdgeWrites, "internal/api/users.go", 44, model.ConfidenceLow),
			edge(7, symCreateUser, symTableAudit, model.EdgeWrites, "internal/api/users.go", 46, model.ConfidenceLow),
			edge(8, symUserRepoClass, symRepositoryIface, model.EdgeImplements, "internal/store/users.go", 8, model.ConfidenceHigh),
		},
	}
}

func smallModel() arch.Model {
	return arch.Model{
		Components: []arch.Component{
			{Name: "api", Include: []string{"internal/api/**"}},
			{Name: "service", Include: []string{"internal/service/**"}},
			{Name: "store", Include: []string{"internal/store/**", "migrations/**"}},
			{Name: "frontend", Include: []string{"web/**"}}, // matches nothing, on purpose
		},
		Rules: []arch.Rule{{
			ID:          "api-goes-through-service",
			Description: "Route handlers must not reach the database directly.",
			From:        []string{"api"},
			Forbidden:   []string{"store"},
			Severity:    arch.SeverityHigh,
		}},
	}
}

// ---------------------------------------------------------- coverage fixture

// Ids for the coverage fixture, kept apart from the small one's so a test that
// mixes them fails loudly rather than silently sharing a symbol.
const (
	covFileAPI = int64(iota + 20)
	covFileLegacy
	covFileSchema
)

const (
	covEndpointOrders = int64(iota + 200)
	covEndpointJobs
	covEndpointPing
	covHandleOrders
	covRunJobs
	covTableOrders
	covTableJobs
	covTableArchive
)

// unclaimedIndex is a repository whose declarations cover only part of it.
//
// This is the normal state of a real repository three months after somebody
// added an `architecture:` block, and it is the case the capability model is
// most likely to misrepresent: everything outside the declared globs simply
// vanishes from a model derived only from what the globs cover. The fixture
// contains one of each way that can happen — an endpoint whose handler is also
// outside, an endpoint with no EXPOSES edge at all, a table touched only from
// outside, and a table nothing in the index touches.
func unclaimedIndex() *fakeRegistry {
	return &fakeRegistry{
		files: []model.File{
			{ID: covFileAPI, Path: "internal/api/orders.go", Language: "go", ParseStatus: "ok"},
			{ID: covFileLegacy, Path: "internal/legacy/cron.go", Language: "go", ParseStatus: "ok"},
			{ID: covFileSchema, Path: "migrations/0002.sql", Language: "sql", ParseStatus: "ok"},
		},
		symbols: []model.Symbol{
			sym(covEndpointOrders, covFileAPI, model.KindEndpoint, "GET /orders", "endpoint:GET /orders", 10),
			sym(covHandleOrders, covFileAPI, model.KindFunction, "ListOrders", "api.ListOrders", 20),
			sym(covEndpointJobs, covFileLegacy, model.KindEndpoint, "POST /jobs", "endpoint:POST /jobs", 5),
			sym(covEndpointPing, covFileLegacy, model.KindEndpoint, "GET /ping", "endpoint:GET /ping", 8),
			sym(covRunJobs, covFileLegacy, model.KindFunction, "RunJobs", "legacy.RunJobs", 12),
			sym(covTableOrders, covFileSchema, model.KindTable, "orders", "table:orders", 1),
			sym(covTableJobs, covFileSchema, model.KindTable, "jobs", "table:jobs", 5),
			sym(covTableArchive, covFileSchema, model.KindTable, "archive", "table:archive", 9),
		},
		edges: []model.Edge{
			edge(1, covEndpointOrders, covHandleOrders, model.EdgeExposes, "internal/api/orders.go", 10, model.ConfidenceHigh),
			edge(2, covEndpointJobs, covRunJobs, model.EdgeExposes, "internal/legacy/cron.go", 5, model.ConfidenceHigh),
			// GET /ping deliberately has no EXPOSES edge.
			edge(3, covHandleOrders, covTableOrders, model.EdgeReads, "internal/api/orders.go", 22, model.ConfidenceHigh),
			edge(4, covRunJobs, covTableJobs, model.EdgeWrites, "internal/legacy/cron.go", 14, model.ConfidenceMedium),
			// `archive` is declared and never touched.
		},
	}
}

// partialModel declares the api component and nothing else, leaving
// internal/legacy and everything only it reaches outside the model.
func partialModel() arch.Model {
	return arch.Model{
		Components: []arch.Component{{Name: "api", Include: []string{"internal/api/**"}}},
	}
}

// deriveFixture is the common setup: a Generator over reg, derived once.
func deriveFixture(t *testing.T, reg Registry, m arch.Model, opts ...Option) *View {
	t.Helper()
	g, err := New(reg, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v, err := g.Derive(context.Background(), 7, m)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return v
}

// ---------------------------------------------------------------- large graph

// largeIndex builds a synthetic repository big enough that an unbounded
// generator would produce something nobody can open.
//
// The shape is the one that actually breaks these tools: many components, many
// endpoints, a handler that fans out widely, and a table every component
// touches. A flat 10,000 unrelated symbols would pass a cap test that a real
// monorepo still defeats.
func largeIndex(components, endpointsPer, fanout int) (*fakeRegistry, arch.Model) {
	reg := &fakeRegistry{}
	m := arch.Model{}
	var nextFile, nextSym, nextEdge int64

	tableFile := int64(1)
	nextFile = 2
	reg.files = append(reg.files, model.File{ID: tableFile, Path: "migrations/0001.sql", Language: "sql", ParseStatus: "ok"})
	nextSym = 1
	tableSym := nextSym
	nextSym++
	reg.symbols = append(reg.symbols, sym(tableSym, tableFile, model.KindTable, "shared_table", "table:shared_table", 1))

	// prevRep is one symbol from the previous component, so the ring of
	// cross-component CALLS edges below really does cross a boundary. Linking
	// two symbols in the same file — the easy mistake — would produce a
	// component diagram with no edges and a bounding test that proves nothing.
	var prevRep int64

	for c := 0; c < components; c++ {
		name := fmt.Sprintf("component%03d", c)
		dir := fmt.Sprintf("svc/%s", name)
		m.Components = append(m.Components, arch.Component{Name: name, Include: []string{dir + "/**"}})

		fileID := nextFile
		nextFile++
		reg.files = append(reg.files, model.File{
			ID: fileID, Path: dir + "/handlers.go", Language: "go", ParseStatus: "ok",
		})

		var firstHandler int64
		for e := 0; e < endpointsPer; e++ {
			epID := nextSym
			nextSym++
			hID := nextSym
			nextSym++
			if firstHandler == 0 {
				firstHandler = hID
			}
			route := fmt.Sprintf("GET /%s/%d", name, e)
			reg.symbols = append(reg.symbols,
				sym(epID, fileID, model.KindEndpoint, route, "endpoint:"+route, 10+e*100),
				sym(hID, fileID, model.KindFunction, fmt.Sprintf("Handle%03d", e), fmt.Sprintf("%s.Handle%03d", name, e), 11+e*100),
			)
			nextEdge++
			reg.edges = append(reg.edges, edge(nextEdge, epID, hID, model.EdgeExposes, dir+"/handlers.go", 10+e*100, model.ConfidenceHigh))

			// A handler that calls a lot of things, which is what makes an
			// unbounded flow diagram unreadable.
			for k := 0; k < fanout; k++ {
				cID := nextSym
				nextSym++
				reg.symbols = append(reg.symbols, sym(cID, fileID, model.KindFunction,
					fmt.Sprintf("helper%03d_%03d", e, k), fmt.Sprintf("%s.helper%03d_%03d", name, e, k), 12+e*100+k))
				nextEdge++
				reg.edges = append(reg.edges, edge(nextEdge, hID, cID, model.EdgeCalls, dir+"/handlers.go", 12+e*100+k, model.ConfidenceMedium))
				nextEdge++
				reg.edges = append(reg.edges, edge(nextEdge, cID, tableSym, model.EdgeReads, dir+"/handlers.go", 13+e*100+k, model.ConfidenceLow))
			}

			// One inheritance edge per endpoint, so that view is loaded too.
			iID := nextSym
			nextSym++
			reg.symbols = append(reg.symbols, sym(iID, fileID, model.KindClass,
				fmt.Sprintf("Impl%03d", e), fmt.Sprintf("%s.Impl%03d", name, e), 90+e*100))
			nextEdge++
			reg.edges = append(reg.edges, edge(nextEdge, iID, hID, model.EdgeImplements, dir+"/handlers.go", 90+e*100, model.ConfidenceHigh))
		}

		// Cross-component dependency, so the component diagram has edges from
		// every component to the previous one round the ring.
		if prevRep != 0 && firstHandler != 0 {
			nextEdge++
			reg.edges = append(reg.edges, edge(nextEdge, firstHandler, prevRep, model.EdgeCalls, dir+"/handlers.go", 500, model.ConfidenceHigh))
		}
		if firstHandler != 0 {
			prevRep = firstHandler
		}
	}
	return reg, m
}
