package multirepo

import (
	"context"
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// fakeSource is a hand-built MemberSource.
//
// Every behavioural test in this package runs against one of these and never
// opens a database, which is the whole return on declaring MemberSource as a
// port: the federation logic is exercised without the SQLite driver, and a test
// that wants a degraded run six weeks old just says so.
type fakeSource struct {
	run       *model.IndexRun
	calls     []registry.ClientCallUsage
	endpoints []registry.Endpoint
	tables    []registry.TableUsage
	// edges is keyed by source symbol id.
	edges map[int64][]registry.EdgeHit
	// failOn names the method that should return an error, for the paths where
	// a broken index must surface rather than be swallowed.
	failOn string
}

func (f *fakeSource) Run(_ context.Context, id int64) (*model.IndexRun, error) {
	if f.failOn == "Run" {
		return nil, fmt.Errorf("fake: run %d unreadable", id)
	}
	return f.run, nil
}

func (f *fakeSource) ClientCallReport(_ context.Context, _ int64) ([]registry.ClientCallUsage, error) {
	if f.failOn == "ClientCallReport" {
		return nil, fmt.Errorf("fake: calls unreadable")
	}
	return f.calls, nil
}

func (f *fakeSource) EndpointReport(_ context.Context, _ int64) ([]registry.Endpoint, error) {
	if f.failOn == "EndpointReport" {
		return nil, fmt.Errorf("fake: endpoints unreadable")
	}
	return f.endpoints, nil
}

func (f *fakeSource) OutgoingEdges(_ context.Context, _, symbolID int64, limit int, filter registry.EdgeFilter) ([]registry.EdgeHit, error) {
	if f.failOn == "OutgoingEdges" {
		return nil, fmt.Errorf("fake: edges unreadable")
	}
	edges := f.edges[symbolID]

	var want map[string]bool
	if len(filter.Kinds) > 0 {
		want = make(map[string]bool, len(filter.Kinds))
		for _, k := range filter.Kinds {
			want[k] = true
		}
	}

	var out []registry.EdgeHit
	for _, e := range edges {
		if want != nil && !want[e.Kind] {
			continue
		}
		if rank := filter.MinConfidence.Rank(); rank > 0 && model.Confidence(e.Confidence).Rank() < rank {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeSource) TableUsageReport(_ context.Context, _ int64) ([]registry.TableUsage, error) {
	if f.failOn == "TableUsageReport" {
		return nil, fmt.Errorf("fake: tables unreadable")
	}
	return f.tables, nil
}

// readyRun is a healthy, current run stamped by the build the tests claim to be
// running, so a test asserting a warning is asserting the condition it set up
// rather than the fixture's own noise.
func readyRun(id int64, rev string, finishedUnix int64) *model.IndexRun {
	return &model.IndexRun{
		ID:              id,
		Status:          model.StatusReady,
		GitRevision:     rev,
		AccuracyMode:    "auto",
		ProducerVersion: testVersion,
		IndexState:      model.IndexStateComplete,
		FinishedUnix:    finishedUnix,
	}
}

const testVersion = "1.2.3"

// exposes builds the EXPOSES edge an endpoint node points at its handler with.
func exposes(endpointID, handlerID int64, conf model.Confidence) registry.EdgeHit {
	return registry.EdgeHit{
		SrcID: endpointID, DstID: handlerID,
		Kind: string(model.EdgeExposes), Confidence: string(conf),
		Source: string(model.SourceSourceCode),
	}
}
