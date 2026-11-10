package web

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/captest"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Shared fixtures and stub ports for the four pillar screens.
//
// The fixtures drive the REAL registry and the REAL graph over a seeded SQLite
// index, for the reason web_test.go states: the claims worth testing are that a
// confidence tier and a file:line survive the whole way from a database row to
// a JSON field, and that nothing is shortened without a count beside it.
// Neither is testable against a fake graph.
//
// The two STUB ports below are a different thing and are used for a different
// reason. registry.ConfigReport merges declared keys with rows in config_usages,
// and captest seeds symbols and edges but not config_usages — so the finding
// that matters most, "read by code and declared nowhere", cannot be produced
// through the fixture builder at all. Stubbing the port lets the handler's
// CLASSIFICATION be tested exhaustively (which is the part this package owns)
// without pretending to test the registry's merge (which is the part
// internal/registry owns and tests). The same argument applies to the
// dependency report, whose importer counts come from a table captest does not
// write either.

// bareFixture is a run with nothing but plain functions: no table, no test
// file, no configuration key, no dependency manifest.
//
// It is the EMPTY case for all four screens, and it is a real committed run —
// which is the whole point. "There is no index" and "the index found nothing"
// produce identical empty arrays and must not produce identical guidance.
func bareFixture(t *testing.T, opts ...Option) (*Server, capability.Deps, int64) {
	t.Helper()

	syms := []captest.Sym{
		captest.S(1, "compute", "internal/core/compute.go").Export(),
		captest.S(2, "caller", "internal/core/caller.go").Export(),
	}
	edges := []captest.Edge{captest.E(2, 1).WithConf(model.ConfidenceHigh)}
	deps, runID := captest.Build(t, syms, edges, captest.Refs{Resolved: 4, Unresolved: 1})

	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	return newServer(t, reg, deps, config.Config{ProjectName: "bare"}, opts...), deps, runID
}

// mindFixture is a run with a declared configuration key, a dependency, two
// tables and a test, so the four screens have something to say.
//
// The shape matters:
//   - `payments` is written at `high` and read at `medium`, so a raised floor
//     must drop the read and SAY that it did.
//   - `audit_log` is a declared table nothing touches, which is a finding
//     ("declared and never queried here"), not a row to omit.
//   - `TestCharge` reaches `charge` through a recorded CALLS edge, and
//     `TestOrphan` reaches nothing — a test file whose coverage the graph does
//     not prove, which must not render as a symbol without tests.
//   - `DATABASE_URL` is declared and configured by a compose service, so the
//     CONFIGURES edge carries a tier and a real file:line.
func mindFixture(t *testing.T, opts ...Option) (*Server, capability.Deps, int64) {
	t.Helper()

	syms := []captest.Sym{
		captest.S(1, "charge", "internal/billing/charge.go").Export(),
		captest.S(2, "refund", "internal/billing/refund.go").Export(),
		captest.S(3, "payments", "db/schema.sql").WithKind("table"),
		captest.S(4, "audit_log", "db/schema.sql").WithKind("table"),
		captest.S(5, "TestCharge", "internal/billing/charge_test.go"),
		captest.S(6, "TestOrphan", "internal/billing/orphan_test.go"),
		captest.S(7, "DATABASE_URL", ".env").WithKind("configuration"),
		captest.S(8, "API_SECRET_KEY", ".env").WithKind("configuration"),
		captest.S(9, "api", "docker-compose.yml").WithKind("service"),
	}
	edges := []captest.Edge{
		{Src: 1, Dst: 3, Kind: model.EdgeWrites, Conf: model.ConfidenceHigh},
		{Src: 2, Dst: 3, Kind: model.EdgeReads, Conf: model.ConfidenceMedium},
		// A write recorded below the default floor: the screen must hide it and
		// count it, never drop it silently.
		{Src: 2, Dst: 4, Kind: model.EdgeWrites, Conf: model.ConfidenceLow},
		captest.E(5, 1).WithConf(model.ConfidenceHigh),
		{Src: 9, Dst: 7, Kind: model.EdgeConfigures, Conf: model.ConfidenceHigh},
	}
	deps, runID := captest.Build(t, syms, edges, captest.Refs{Resolved: 30, Unresolved: 6})

	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	return newServer(t, reg, deps, config.Config{ProjectName: "mind"}, opts...), deps, runID
}

// unindexedServer is a portal over a store with no committed run.
//
// The third empty, and the one most easily confused with the other two: there
// is no index at all. Each of the four screens must distinguish it from a run
// that found nothing, because the remedy is different and a reader who is told
// "no configuration keys" when the answer is "nothing has been indexed" will go
// looking for a bug in their configuration.
func unindexedServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	return newServer(t, reg, capability.NewDeps(reg, graph.New(db)),
		config.Config{ProjectName: "unindexed"}, opts...)
}

// ---------------------------------------------------------------- stub ports

// stubConfigReader supplies a ConfigReport verbatim. See the file comment.
type stubConfigReader struct {
	keys []registry.ConfigKeyUsage
	err  error
}

func (s stubConfigReader) ConfigReport(context.Context, int64) ([]registry.ConfigKeyUsage, error) {
	return s.keys, s.err
}

// stubDependencyReader supplies a DependencyReport verbatim, and records
// whether an ImportMatcher was passed — because a nil matcher leaves Importers
// at zero on every row, which the registry documents as "not measured" and
// which this screen must never report as "nothing imports this".
type stubDependencyReader struct {
	list          []registry.DependencyUsage
	err           error
	gotMatcherPtr *bool
}

func (s stubDependencyReader) DependencyReport(_ context.Context, _ int64,
	matches registry.ImportMatcher) ([]registry.DependencyUsage, error) {
	if s.gotMatcherPtr != nil {
		*s.gotMatcherPtr = matches != nil
	}
	return s.list, s.err
}

// registryWithoutPorts is a Registry that satisfies exactly web.Registry and
// neither ConfigReader nor DependencyReader, so the missing-wiring path is
// reachable in a test rather than only in theory.
type registryWithoutPorts struct{ inner *registry.Registry }

func (n registryWithoutPorts) CurrentRun(ctx context.Context) (*model.IndexRun, error) {
	return n.inner.CurrentRun(ctx)
}
func (n registryWithoutPorts) Counts(ctx context.Context, runID int64) (registry.Counts, error) {
	return n.inner.Counts(ctx, runID)
}
func (n registryWithoutPorts) LanguageCounts(ctx context.Context, runID int64) (map[string]int, error) {
	return n.inner.LanguageCounts(ctx, runID)
}
func (n registryWithoutPorts) ResolutionStats(ctx context.Context, runID int64) (registry.ResolutionStats, error) {
	return n.inner.ResolutionStats(ctx, runID)
}
func (n registryWithoutPorts) Files(ctx context.Context, runID int64) ([]model.File, error) {
	return n.inner.Files(ctx, runID)
}
func (n registryWithoutPorts) Symbols(ctx context.Context, runID int64) ([]model.Symbol, error) {
	return n.inner.Symbols(ctx, runID)
}
func (n registryWithoutPorts) Edges(ctx context.Context, runID int64) ([]model.Edge, error) {
	return n.inner.Edges(ctx, runID)
}
func (n registryWithoutPorts) EndpointReport(ctx context.Context, runID int64) ([]registry.Endpoint, error) {
	return n.inner.EndpointReport(ctx, runID)
}
func (n registryWithoutPorts) TableUsageReport(ctx context.Context, runID int64) ([]registry.TableUsage, error) {
	return n.inner.TableUsageReport(ctx, runID)
}

// serverWithoutNarrowPorts builds a portal whose Registry implements neither
// narrow port.
func serverWithoutNarrowPorts(t *testing.T, deps capability.Deps) *Server {
	t.Helper()
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	srv, err := New(Deps{
		Registry: registryWithoutPorts{reg}, Caps: deps,
		Config: config.Config{ProjectName: "narrow-ports"}, IndexPath: "/tmp/fixture/index.db",
	})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv
}

// omissionFor returns the recorded omissions carrying reason, so a test can
// assert that a bound was COUNTED rather than merely applied.
func omissionsFor(o Omissions, reason string) []Omission {
	var out []Omission
	for _, om := range o.Omitted {
		if om.Reason == reason {
			out = append(out, om)
		}
	}
	return out
}
