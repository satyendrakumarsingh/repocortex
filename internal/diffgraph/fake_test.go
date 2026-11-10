package diffgraph

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A hand-built IndexSource. The port is small enough that a fake is cheap,
// which is the point of declaring it at the consumer: every behavioural test
// below runs without opening a database.
type fakeSource struct {
	runs      map[int64]*model.IndexRun
	files     map[int64][]model.File
	symbols   map[int64][]model.Symbol
	edges     map[int64][]model.Edge
	endpoints map[int64][]registry.Endpoint
	tables    map[int64][]registry.TableUsage
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		runs:      map[int64]*model.IndexRun{},
		files:     map[int64][]model.File{},
		symbols:   map[int64][]model.Symbol{},
		edges:     map[int64][]model.Edge{},
		endpoints: map[int64][]registry.Endpoint{},
		tables:    map[int64][]registry.TableUsage{},
	}
}

func (f *fakeSource) Run(ctx context.Context, id int64) (*model.IndexRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, fmt.Errorf("fake: no run %d", id)
	}
	return r, nil
}

func (f *fakeSource) Files(ctx context.Context, runID int64) ([]model.File, error) {
	return f.files[runID], ctx.Err()
}

func (f *fakeSource) Symbols(ctx context.Context, runID int64) ([]model.Symbol, error) {
	return f.symbols[runID], ctx.Err()
}

func (f *fakeSource) Edges(ctx context.Context, runID int64) ([]model.Edge, error) {
	return f.edges[runID], ctx.Err()
}

func (f *fakeSource) EndpointReport(ctx context.Context, runID int64) ([]registry.Endpoint, error) {
	return f.endpoints[runID], ctx.Err()
}

func (f *fakeSource) TableUsageReport(ctx context.Context, runID int64) ([]registry.TableUsage, error) {
	return f.tables[runID], ctx.Err()
}

var _ IndexSource = (*fakeSource)(nil)

// reverse flips every slice a run exposes.
//
// Used to prove the report does not depend on the order rows arrive in. The
// registry reads under a total ORDER BY, so production order is already fixed;
// this asserts that this package does not quietly depend on it.
func (f *fakeSource) reverse(runID int64) {
	slices.Reverse(f.files[runID])
	slices.Reverse(f.symbols[runID])
	slices.Reverse(f.edges[runID])
	slices.Reverse(f.endpoints[runID])
	slices.Reverse(f.tables[runID])
}

// --- fixture builder --------------------------------------------------------

// runBuilder writes one run into a fakeSource.
//
// idBase deliberately differs per run so that two runs with identical content
// never share a rowid. If any comparison in this package touched an id, every
// "identical content" test below would fail — which is exactly the guard the
// determinism digest applies to the pipeline, applied here to the diff.
type runBuilder struct {
	src    *fakeSource
	runID  int64
	nextID int64
	fileID map[string]int64
	symID  map[string]int64
}

func newRun(src *fakeSource, runID, idBase int64, status model.IndexStatus, rev, accuracy string) *runBuilder {
	src.runs[runID] = &model.IndexRun{
		ID: runID, Status: status, GitRevision: rev, AccuracyMode: accuracy,
	}
	return &runBuilder{
		src: src, runID: runID, nextID: idBase,
		fileID: map[string]int64{}, symID: map[string]int64{},
	}
}

func (b *runBuilder) id() int64 {
	b.nextID++
	return b.nextID
}

func (b *runBuilder) file(path, parseStatus string) *runBuilder {
	id := b.id()
	b.fileID[path] = id
	b.src.files[b.runID] = append(b.src.files[b.runID], model.File{
		ID: id, Path: path, Language: "go", ContentHash: "h-" + path,
		SizeBytes: 100, ParseStatus: parseStatus,
	})
	return b
}

func (b *runBuilder) sym(path, fqn, name string, kind model.EntityKind, line int) *runBuilder {
	fileID, ok := b.fileID[path]
	if !ok {
		panic("fixture: file " + path + " not declared")
	}
	id := b.id()
	b.symID[fqn] = id
	b.src.symbols[b.runID] = append(b.src.symbols[b.runID], model.Symbol{
		ID: id, FileID: fileID, Kind: kind, Name: name, FQN: fqn,
		Range: model.Range{
			Start: model.Position{Line: line, Col: 1},
			End:   model.Position{Line: line + 5, Col: 1},
		},
		Signature: name + "()", Exported: true,
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	})
	return b
}

func (b *runBuilder) edge(srcFQN, dstFQN string, kind model.EdgeKind, conf model.Confidence,
	source model.SourceType, evFile string, evLine int) *runBuilder {
	b.src.edges[b.runID] = append(b.src.edges[b.runID], model.Edge{
		ID:    b.id(),
		SrcID: b.symID[srcFQN], DstID: b.symID[dstFQN],
		Kind: kind, Confidence: conf,
		Evidence: model.Evidence{
			FilePath: evFile,
			Range:    model.Range{Start: model.Position{Line: evLine, Col: 4}},
			Source:   source,
		},
		IndexRunID: b.runID,
	})
	return b
}

func (b *runBuilder) endpoint(route, path string, line int, handler, handlerPath string) *runBuilder {
	b.src.endpoints[b.runID] = append(b.src.endpoints[b.runID], registry.Endpoint{
		SymbolID: b.id(), Signature: route, FilePath: path, Line: line,
		Handler: handler, HandlerPath: handlerPath,
	})
	return b
}

func (b *runBuilder) table(name string, declared bool, readers, writers int) *runBuilder {
	b.src.tables[b.runID] = append(b.src.tables[b.runID], registry.TableUsage{
		Table: name, Declared: declared, Readers: readers, Writers: writers,
		Accesses: readers + writers,
	})
	return b
}

// baseline is the shared starting graph: a controller that exposes a route,
// calls a service, and the service writes a table.
func baseline(src *fakeSource, runID, idBase int64, lineShift int) *runBuilder {
	b := newRun(src, runID, idBase, model.StatusReady, fmt.Sprintf("rev%d", runID), "auto")
	b.file("pkg/api.go", "ok").
		file("pkg/service.go", "ok").
		file("db/schema.sql", "ok").
		sym("pkg/api.go", "pkg/api.go:GET /refunds", "GET /refunds", model.KindEndpoint, 10+lineShift).
		sym("pkg/api.go", "pkg/api.go:handleRefund", "handleRefund", model.KindFunction, 20+lineShift).
		sym("pkg/service.go", "pkg/service.go:Refund", "Refund", model.KindMethod, 5+lineShift).
		sym("db/schema.sql", "db/schema.sql:refunds", "refunds", model.KindTable, 1).
		edge("pkg/api.go:GET /refunds", "pkg/api.go:handleRefund", model.EdgeExposes,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/api.go", 10+lineShift).
		edge("pkg/api.go:handleRefund", "pkg/service.go:Refund", model.EdgeCalls,
			model.ConfidenceExact, model.SourceCompiler, "pkg/api.go", 22+lineShift).
		edge("pkg/service.go:Refund", "db/schema.sql:refunds", model.EdgeReads,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/service.go", 8+lineShift).
		endpoint("GET /refunds", "pkg/api.go", 10+lineShift, "handleRefund", "pkg/api.go").
		table("refunds", true, 1, 0)
	return b
}

func mustDiff(t *testing.T, src IndexSource, base, head int64, opts ...Option) *Report {
	t.Helper()
	return mustDiffAcross(t, Side{Source: src, RunID: base}, Side{Source: src, RunID: head}, opts...)
}

func mustDiffAcross(t *testing.T, base, head Side, opts ...Option) *Report {
	t.Helper()
	d, err := New(opts...)
	if err != nil {
		t.Fatalf("new differ: %v", err)
	}
	rep, err := d.Compare(context.Background(), base, head)
	if err != nil {
		t.Fatalf("compare %d..%d: %v", base.RunID, head.RunID, err)
	}
	return rep
}
