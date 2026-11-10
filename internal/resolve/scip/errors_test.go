package scip

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"

	scippb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func ingestBytes(t *testing.T, data []byte, opts Options) (Stats, error) {
	t.Helper()
	return New(opts).Ingest(context.Background(), bytes.NewReader(data),
		func(context.Context, *FileFacts) error { return nil })
}

func TestEmptyIndexIsRejectedForMissingProvenance(t *testing.T) {
	_, err := ingestBytes(t, nil, Options{})
	if !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("want ErrNoMetadata, got %v", err)
	}
}

func TestDocumentBeforeMetadataIsRejected(t *testing.T) {
	// An exact edge whose evidence cannot name the indexer that proved it is
	// provenance in name only, so this is fatal rather than tolerated.
	data := indexBytes(t, nil, serviceDoc())
	_, err := ingestBytes(t, data, Options{})
	if !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("want ErrNoMetadata, got %v", err)
	}
}

// TestTruncatedIndexFailsCleanly walks every prefix of a valid index. None may
// panic, and every document that does get emitted must still be well formed —
// a half-read index must not become half-invented facts.
func TestTruncatedIndexFailsCleanly(t *testing.T) {
	data := indexBytes(t, metadata("file:///build/repo"), serviceDoc(), handlerDoc())

	var errored int
	for n := 1; n < len(data); n++ {
		var emitted []FileFacts
		_, err := New(Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()}).
			Ingest(context.Background(), bytes.NewReader(data[:n]),
				func(_ context.Context, f *FileFacts) error {
					emitted = append(emitted, *f)
					return nil
				})
		if err != nil {
			errored++
			if !strings.HasPrefix(err.Error(), "scip:") {
				t.Fatalf("prefix %d: error is not attributed to this package: %v", n, err)
			}
		}
		for _, f := range emitted {
			for _, s := range f.Symbols {
				if s.FQN == "" || s.Confidence != model.ConfidenceExact {
					t.Fatalf("prefix %d: malformed symbol survived truncation: %+v", n, s)
				}
			}
			edges, _, eerr := f.Edges(nil)
			if eerr != nil {
				t.Fatalf("prefix %d: %v", n, eerr)
			}
			for _, e := range edges {
				if err := e.Validate(); err != nil {
					t.Fatalf("prefix %d: %v", n, err)
				}
			}
		}
	}
	if errored == 0 {
		t.Fatal("no truncation was detected; the check proved nothing")
	}
}

func TestTruncatedMidDocumentReportsPosition(t *testing.T) {
	data := indexBytes(t, metadata("file:///build/repo"), serviceDoc())
	_, err := ingestBytes(t, data[:len(data)-5], Options{})
	if err == nil {
		t.Fatal("truncated document accepted")
	}
	if !strings.Contains(err.Error(), "byte(s)") {
		t.Fatalf("error does not say how far it got: %v", err)
	}
}

func TestGarbageInputFailsWithoutPanic(t *testing.T) {
	seeds := [][]byte{
		{0xff, 0xff, 0xff, 0xff},
		{0x00},
		{0x08, 0x01}, // field 1, varint wire type: not a submessage
		{0x3a, 0x05}, // field 7: not an Index field
		[]byte("this is a text file, not a protobuf"),
	}
	for i, seed := range seeds {
		if _, err := ingestBytes(t, seed, Options{}); err == nil {
			t.Fatalf("seed %d accepted as a SCIP index", i)
		}
	}

	// Random bytes, deterministically seeded so a failure is reproducible.
	rng := rand.New(rand.NewSource(1))
	for i := range 200 {
		buf := make([]byte, 1+rng.Intn(512))
		rng.Read(buf)
		if _, err := ingestBytes(t, buf, Options{}); err == nil {
			// Random bytes can legally decode as an empty index; what must
			// never happen is a panic, which the harness would report.
			_ = i
		}
	}
}

// TestOversizedSubmessageIsRefusedBeforeAllocation is the denial-of-service
// case: the streaming parser allocates the declared length before reading a
// byte of the body, so a hostile .scip declaring a terabyte would be an
// out-of-memory kill rather than an error. framingReader decodes the length
// prefix first and refuses. If this test ever starts consuming gigabytes, that
// guard has been lost.
func TestOversizedSubmessageIsRefusedBeforeAllocation(t *testing.T) {
	var data []byte
	data = protowire.AppendTag(data, fieldDocument, protowire.BytesType)
	data = protowire.AppendVarint(data, 1<<40) // 1 TiB

	_, err := ingestBytes(t, data, Options{})
	if err == nil {
		t.Fatal("oversized submessage accepted")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("error does not name the limit: %v", err)
	}
}

func TestMaxBytesStopsAnEndlessIndex(t *testing.T) {
	data := indexBytes(t, metadata("file:///build/repo"), serviceDoc(), handlerDoc())
	_, err := ingestBytes(t, data, Options{MaxBytes: 32})
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("want a size-limit error, got %v", err)
	}
}

func TestEmitErrorAbortsIngest(t *testing.T) {
	sentinel := errors.New("registry is down")
	data := indexBytes(t, metadata("file:///build/repo"), serviceDoc(), handlerDoc())

	var seen int
	_, err := New(Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()}).
		Ingest(context.Background(), bytes.NewReader(data),
			func(context.Context, *FileFacts) error {
				seen++
				return sentinel
			})
	if !errors.Is(err, sentinel) {
		t.Fatalf("emit error not propagated: %v", err)
	}
	if seen != 1 {
		t.Fatalf("ingest continued after emit failed (%d documents)", seen)
	}
}

func TestContextCancellationStopsIngest(t *testing.T) {
	data := indexBytes(t, metadata("file:///build/repo"), serviceDoc(), handlerDoc())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New(Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()}).
		Ingest(ctx, bytes.NewReader(data), func(context.Context, *FileFacts) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestIngestRequiresEmitFunc(t *testing.T) {
	if _, err := New(Options{}).Ingest(context.Background(), bytes.NewReader(nil), nil); err == nil {
		t.Fatal("nil EmitFunc accepted")
	}
}

// TestEmitPanicIsNotDisguised: the recover in Ingest exists to convert parser
// crashes on hostile input into errors. It must not also swallow a bug in the
// caller's own callback, or their stack trace becomes our error string.
func TestEmitPanicIsNotDisguised(t *testing.T) {
	data := indexBytes(t, metadata("file:///build/repo"), serviceDoc())
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("panic from EmitFunc was swallowed")
		}
		if s, ok := p.(string); !ok || s != "caller bug" {
			t.Fatalf("panic value was rewritten: %v", p)
		}
	}()
	_, _ = New(Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()}).
		Ingest(context.Background(), bytes.NewReader(data),
			func(context.Context, *FileFacts) error { panic("caller bug") })
}

func TestDuplicateDefinitionsDoNotViolateRegistryUniqueness(t *testing.T) {
	sym := goSymbol("internal/svc/Service#")
	doc := &scippb.Document{
		RelativePath: "internal/svc/service.go",
		Occurrences: []*scippb.Occurrence{
			def(sym, rng(5, 5, 5, 12), rng(5, 0, 20, 1)),
			def(sym, rng(5, 5, 5, 12), rng(5, 0, 20, 1)),
		},
	}
	facts, st := ingestFixture(t, Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()}, doc)

	// UNIQUE(index_run_id, fqn, file_id, start_line) in migration 0001 would
	// reject the second row.
	svc := factsFor(t, facts, "internal/svc/service.go")
	seen := map[definitionKey]bool{}
	for _, s := range svc.Symbols {
		k := definitionKey{fqn: s.FQN, line: int32(s.Range.Start.Line)}
		if seen[k] {
			t.Fatalf("duplicate (fqn, start_line): %+v", k)
		}
		seen[k] = true
	}
	if st.DuplicateDefinitions != 1 {
		t.Fatalf("DuplicateDefinitions = %d, want 1", st.DuplicateDefinitions)
	}
}

func TestExternalSymbolsAreCountedNotEmitted(t *testing.T) {
	var buf bytes.Buffer
	writeIndex(t, &buf, metadata("file:///build/repo"), serviceDoc())
	if err := writeMessage(&buf, fieldExternalSymbol,
		&scippb.SymbolInformation{Symbol: goSymbol("net/http/Handler#")}); err != nil {
		t.Fatalf("write external symbol: %v", err)
	}

	facts, st, err := Collect(context.Background(), bytes.NewReader(buf.Bytes()),
		Options{RepoRoot: "/build/repo", KnownPaths: knownPaths()})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if st.ExternalSymbols != 1 {
		t.Fatalf("ExternalSymbols = %d, want 1", st.ExternalSymbols)
	}
	for i := range facts {
		for _, s := range facts[i].Symbols {
			if s.FQN == goSymbol("net/http/Handler#") {
				t.Fatal("external symbol emitted with no file to belong to")
			}
		}
	}
}

// FuzzIngest is required by CONTRIBUTING for anything reading untrusted input.
// A .scip file arrives with the repository, so it qualifies.
func FuzzIngest(f *testing.F) {
	f.Add(indexBytes(f, metadata("file:///build/repo"), serviceDoc()))
	f.Add(indexBytes(f, metadata(""), handlerDoc()))
	f.Add([]byte{})
	f.Add([]byte{0x0a, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		facts, _, err := Collect(context.Background(), bytes.NewReader(data), Options{
			RepoRoot:        "/build/repo",
			MaxBytes:        1 << 20,
			MaxMessageBytes: 1 << 20,
		})
		if err != nil {
			return
		}
		// Whatever survived parsing must still satisfy the graph invariants:
		// a hostile index must not be able to manufacture an edge that claims
		// exactness without compiler evidence, nor a path outside the tree.
		for i := range facts {
			if strings.HasPrefix(facts[i].Path, "/") || strings.Contains(facts[i].Path, "..") {
				t.Fatalf("path escaped the repository: %q", facts[i].Path)
			}
			edges, _, eerr := facts[i].Edges(nil)
			if eerr != nil {
				t.Fatalf("Edges: %v", eerr)
			}
			for _, e := range edges {
				if err := e.Validate(); err != nil {
					t.Fatalf("invalid edge from fuzzed index: %v", err)
				}
				if e.Evidence.Source != model.SourceCompiler {
					t.Fatalf("edge escaped the compiler-source stamp: %+v", e)
				}
			}
		}
	})
}
