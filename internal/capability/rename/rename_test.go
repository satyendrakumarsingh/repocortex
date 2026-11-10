package rename

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

var _ Index = (*registry.Registry)(nil)

type fakeIndex struct {
	symbols map[string][]registry.SymbolHit
	edges   map[int64][]registry.EdgeHit
	count   map[int64]int
	files   []model.File
}

var _ Index = (*fakeIndex)(nil)

func (f *fakeIndex) FindSymbolsByName(_ context.Context, _ int64, name, _ string, limit int) ([]registry.SymbolHit, error) {
	hits := f.symbols[name]
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (f *fakeIndex) IncomingEdges(_ context.Context, _ int64, id int64, limit int, _ registry.EdgeFilter) ([]registry.EdgeHit, error) {
	e := f.edges[id]
	if len(e) > limit {
		e = e[:limit]
	}
	return e, nil
}

func (f *fakeIndex) IncomingEdgeCount(_ context.Context, _ int64, id int64, _ registry.EdgeFilter, _ []string) (int, error) {
	if n, ok := f.count[id]; ok {
		return n, nil
	}
	return len(f.edges[id]), nil
}

func (f *fakeIndex) Files(context.Context, int64) ([]model.File, error) { return f.files, nil }

// baseIndex mirrors what `cortex index && cortex resolve` records for
// seedVerifyRepo (internal/cli/verify_test.go), measured: one CALLS edge, high,
// evidence column 55 — the start of `s.repo.CreateOrder(id)`.
func baseIndex() *fakeIndex {
	return &fakeIndex{
		symbols: map[string][]registry.SymbolHit{
			"CreateOrder": {{ID: 4, Name: "CreateOrder", FQN: "app.Repo#CreateOrder", Kind: "method",
				FilePath: "store.go", StartLine: 5, Confidence: "high"}},
		},
		edges: map[int64][]registry.EdgeHit{
			4: {{SrcName: "PlaceOrder", Kind: "CALLS", Confidence: "high", Detail: "same package directory",
				EvidenceFile: "service.go", EvidenceLine: 5, EvidenceCol: 55}},
		},
		files: []model.File{{Path: "store.go", ContentHash: "h-store"}, {Path: "service.go", ContentHash: "h-svc"}},
	}
}

func req() Request { return Request{Old: "CreateOrder", New: "InsertOrder"} }

func TestCollectIncludesTheDefinitionAndEveryReference(t *testing.T) {
	p, err := Collect(context.Background(), baseIndex(), 1, req())
	if err != nil {
		t.Fatal(err)
	}
	want := []Site{
		{Path: "service.go", Line: 5, Col: 55, Confidence: model.ConfidenceHigh, Kind: "CALLS", Why: "same package directory"},
		{Path: "store.go", Line: 5, Col: -1, Confidence: model.ConfidenceHigh, Kind: KindDefinition, Why: "the definition itself"},
	}
	if len(p.Sites) != len(want) {
		t.Fatalf("sites = %+v", p.Sites)
	}
	for i := range want {
		if p.Sites[i] != want[i] {
			t.Errorf("site %d = %+v, want %+v", i, p.Sites[i], want[i])
		}
	}
	if r := p.Refusals(model.ConfidenceHigh); len(r) != 0 {
		t.Errorf("unexpected refusals at the high floor: %+v", r)
	}
	files := p.ByFile()
	if len(files) != 2 || files[0].Path != "service.go" || files[0].Hash != "h-svc" ||
		len(files[1].Lines[5]) != 1 || files[1].Lines[5][0] != -1 {
		t.Errorf("ByFile = %+v", files)
	}
}

// TestTheDefaultFloorIsExact: high is an opt-in. At the default floor every
// high site — the definition included — is refused and named.
func TestTheDefaultFloorIsExact(t *testing.T) {
	if DefaultFloor != model.ConfidenceExact {
		t.Fatalf("DefaultFloor = %s, want exact", DefaultFloor)
	}
	p, err := Collect(context.Background(), baseIndex(), 1, req())
	if err != nil {
		t.Fatal(err)
	}
	r := p.Refusals(DefaultFloor)
	if len(r) != 2 {
		t.Fatalf("refusals = %+v, want both sites", r)
	}
	for _, x := range r {
		if x.Site == nil || !strings.Contains(x.Reason, "confidence high is below exact") {
			t.Errorf("refusal %+v", x)
		}
	}
}

// TestNoFloorBelowHigh: a caller cannot loosen the floor past high.
func TestNoFloorBelowHigh(t *testing.T) {
	ix := baseIndex()
	ix.edges[4][0].Confidence = "medium"
	p, err := Collect(context.Background(), ix, 1, req())
	if err != nil {
		t.Fatal(err)
	}
	for _, floor := range []model.Confidence{model.ConfidenceMedium, model.ConfidenceLow, ""} {
		r := p.Refusals(floor)
		if len(r) != 1 || !strings.Contains(r[0].Reason, "below high") {
			t.Errorf("floor %q: refusals = %+v, want the medium site refused against high", floor, r)
		}
	}
}

func TestRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeIndex)
		want   string
	}{
		{name: "a reference below high", want: "below high", mutate: func(f *fakeIndex) {
			f.edges[4][0].Confidence = "medium"
		}},
		{name: "a reference with no confidence", want: "confidence none is below high", mutate: func(f *fakeIndex) {
			f.edges[4][0].Confidence = ""
		}},
		{name: "more references than were fetched", want: "5 references exist but only 1 were fetched", mutate: func(f *fakeIndex) {
			f.count = map[int64]int{4: 5}
		}},
		{name: "a file the index has no hash for", want: "not in the index", mutate: func(f *fakeIndex) {
			f.files = f.files[:1]
		}},
		{name: "the new name is already defined in an edited file", want: "already defined", mutate: func(f *fakeIndex) {
			f.symbols["InsertOrder"] = []registry.SymbolHit{{ID: 9, Name: "InsertOrder", Kind: "method", FilePath: "store.go", StartLine: 9}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ix := baseIndex()
			tc.mutate(ix)
			p, err := Collect(context.Background(), ix, 1, req())
			if err != nil {
				t.Fatal(err)
			}
			refusals := p.Refusals(model.ConfidenceHigh)
			var all []string
			for _, r := range refusals {
				all = append(all, r.Reason)
			}
			if !strings.Contains(strings.Join(all, "\n"), tc.want) {
				t.Errorf("refusals %q do not mention %q", all, tc.want)
			}
		})
	}
}

func TestCollectRejectsBadRequests(t *testing.T) {
	twoDefs := func(f *fakeIndex) {
		f.symbols["CreateOrder"] = append(f.symbols["CreateOrder"],
			registry.SymbolHit{ID: 7, Name: "CreateOrder", Kind: "method", FilePath: "legacy/store.go", StartLine: 3, Confidence: "high"})
	}
	tests := []struct {
		name    string
		mutate  func(*fakeIndex)
		req     Request
		wantErr error
		wantAmb int
	}{
		{name: "old is not an identifier", req: Request{Old: "Create Order", New: "X"}, wantErr: ErrInvalidName},
		{name: "new is not an identifier", req: Request{Old: "CreateOrder", New: "1x"}, wantErr: ErrInvalidName},
		{name: "same name", req: Request{Old: "CreateOrder", New: "CreateOrder"}, wantErr: ErrInvalidName},
		{name: "unknown", req: Request{Old: "Nope", New: "X"}, wantErr: ErrNotFound},
		{name: "two definitions", mutate: twoDefs, req: req(), wantAmb: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ix := baseIndex()
			if tc.mutate != nil {
				tc.mutate(ix)
			}
			_, err := Collect(context.Background(), ix, 1, tc.req)
			if tc.wantAmb > 0 {
				var amb *AmbiguousError
				if !errors.As(err, &amb) || len(amb.Candidates) != tc.wantAmb {
					t.Fatalf("err = %v, want an AmbiguousError with %d candidates", err, tc.wantAmb)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	ix := baseIndex()
	twoDefs(ix)
	p, err := Collect(context.Background(), ix, 1, Request{Old: "CreateOrder", New: "InsertOrder", Path: "legacy/"})
	if err != nil || p.Target.Path != "legacy/store.go" {
		t.Fatalf("--path did not disambiguate: %+v %v", p.Target, err)
	}
}

func TestCollectKeepsTheStrongestClaimPerPosition(t *testing.T) {
	ix := baseIndex()
	ix.edges[4] = append(ix.edges[4], registry.EdgeHit{
		Kind: "REFERENCES", Confidence: "exact", EvidenceFile: "service.go", EvidenceLine: 5, EvidenceCol: 55})
	p, err := Collect(context.Background(), ix, 1, req())
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, s := range p.Sites {
		if s.Path == "service.go" {
			n++
			if s.Confidence != model.ConfidenceExact {
				t.Errorf("kept %s, want the exact claim", s.Confidence)
			}
		}
	}
	if n != 1 {
		t.Errorf("service.go:5:55 appears %d times, want once", n)
	}
}
