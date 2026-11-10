package scip

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeSyntheticIndexFile builds a fixture on disk so the ingest under test
// reads from a file, as it will in production, rather than from a []byte that
// is already resident.
func writeSyntheticIndexFile(t testing.TB, nDocs, perDoc int) (path string, paths []string, occurrences int) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "index.scip")
	f, err := os.Create(path) //nolint:gosec // test fixture in t.TempDir()
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	paths, occurrences = syntheticIndex(t, w, nDocs, perDoc)
	if err := w.Flush(); err != nil {
		t.Fatalf("flush fixture: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return path, paths, occurrences
}

// peakSampler tracks the high-water heap across an ingest. It samples rather
// than forcing GC so the number reflects what the process actually holds while
// running, which is the number that matters for "does this fit in memory".
type peakSampler struct {
	every uint64
	n     uint64
	peak  uint64
	total uint64
	sys   uint64
}

func (p *peakSampler) sample() {
	p.n++
	if p.n%p.every != 0 {
		return
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > p.peak {
		p.peak = m.HeapAlloc
	}
	p.total = m.TotalAlloc
	p.sys = m.Sys
}

// TestIngestMemoryIsBoundedByDocument is the claim this package makes about
// large indexes: peak memory follows the largest document, not the index size.
// A regression that buffered documents would still pass every correctness test
// here and only show up as an out-of-memory kill on a real monorepo.
func TestIngestMemoryIsBoundedByDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-megabyte fixture")
	}

	measure := func(nDocs int) (peak uint64, bytesRead int64, occ int) {
		path, paths, occurrences := writeSyntheticIndexFile(t, nDocs, 40)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat fixture: %v", err)
		}
		t.Logf("fixture: %d documents, %d occurrences, %.1f MiB on disk",
			nDocs, occurrences, float64(info.Size())/(1<<20))

		runtime.GC()
		s := &peakSampler{every: 64}
		st, err := New(Options{RepoRoot: "/build/repo", KnownPaths: paths}).
			IngestFile(context.Background(), path, func(_ context.Context, f *FileFacts) error {
				// Simulate the caller consuming and discarding each document,
				// which is what the streaming contract assumes.
				if len(f.Symbols) == 0 {
					t.Fatalf("%s: no symbols", f.Path)
				}
				s.sample()
				return nil
			})
		if err != nil {
			t.Fatalf("IngestFile: %v", err)
		}
		if st.DocumentsUnmatched != 0 {
			t.Fatalf("%d documents did not match: %v", st.DocumentsUnmatched, st.UnmatchedPaths)
		}
		t.Logf("ingested %.1f MiB: peak heap %.1f MiB, total allocated %.1f MiB, sys %.1f MiB",
			float64(st.BytesRead)/(1<<20), float64(s.peak)/(1<<20),
			float64(s.total)/(1<<20), float64(s.sys)/(1<<20))
		return s.peak, st.BytesRead, occurrences
	}

	smallPeak, smallBytes, _ := measure(2_000)
	largePeak, largeBytes, _ := measure(40_000)

	if largeBytes < smallBytes*10 {
		t.Fatalf("fixtures are not different enough to prove anything: %d vs %d bytes", smallBytes, largeBytes)
	}
	// The index grew twentyfold. Peak heap is allowed to move — the known-path
	// set the caller supplies genuinely does scale with the repository — but it
	// must not track the index.
	const slack = 48 << 20
	if largePeak > smallPeak*3+slack {
		t.Fatalf("peak heap scales with index size: %.1f MiB for %.1f MiB of index vs %.1f MiB for %.1f MiB",
			float64(largePeak)/(1<<20), float64(largeBytes)/(1<<20),
			float64(smallPeak)/(1<<20), float64(smallBytes)/(1<<20))
	}
}

// BenchmarkIngest measures end-to-end ingest throughput from a file, reporting
// documents and occurrences per second alongside the usual byte rate.
func BenchmarkIngest(b *testing.B) {
	const nDocs, perDoc = 20_000, 40
	path, paths, occurrences := writeSyntheticIndexFile(b, nDocs, perDoc)
	info, err := os.Stat(path)
	if err != nil {
		b.Fatalf("stat fixture: %v", err)
	}

	ing := New(Options{RepoRoot: "/build/repo", KnownPaths: paths})
	discard := func(context.Context, *FileFacts) error { return nil }

	b.SetBytes(info.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := ing.IngestFile(context.Background(), path, discard); err != nil {
			b.Fatalf("IngestFile: %v", err)
		}
	}
	b.StopTimer()

	perOp := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(float64(nDocs)/perOp, "docs/s")
	b.ReportMetric(float64(occurrences)/perOp, "occurrences/s")
}

// BenchmarkIngestInMemory isolates parsing and mapping from file I/O.
func BenchmarkIngestInMemory(b *testing.B) {
	const nDocs, perDoc = 4_000, 40
	var buf bytes.Buffer
	paths, occurrences := syntheticIndex(b, &buf, nDocs, perDoc)
	data := buf.Bytes()

	ing := New(Options{RepoRoot: "/build/repo", KnownPaths: paths})
	discard := func(context.Context, *FileFacts) error { return nil }

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := ing.Ingest(context.Background(), bytes.NewReader(data), discard); err != nil {
			b.Fatalf("Ingest: %v", err)
		}
	}
	b.StopTimer()

	perOp := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(float64(nDocs)/perOp, "docs/s")
	b.ReportMetric(float64(occurrences)/perOp, "occurrences/s")
}

// BenchmarkEdges measures the FQN-to-rowid resolution the caller runs after
// ingest, since that is the other half of the cost of landing an index.
func BenchmarkEdges(b *testing.B) {
	var buf bytes.Buffer
	paths, _ := syntheticIndex(b, &buf, 200, 40)

	facts, _, err := Collect(context.Background(), bytes.NewReader(buf.Bytes()),
		Options{RepoRoot: "/build/repo", KnownPaths: paths})
	if err != nil {
		b.Fatalf("Collect: %v", err)
	}
	table := map[string]int64{}
	var id int64 = 1
	for i := range facts {
		for _, r := range facts[i].Relations {
			if _, ok := table[r.DstFQN]; !ok {
				table[r.DstFQN] = id
				id++
			}
		}
	}
	lookup := func(fqn string) (int64, bool) { v, ok := table[fqn]; return v, ok }

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for i := range facts {
			if _, _, err := facts[i].Edges(lookup); err != nil {
				b.Fatalf("Edges: %v", err)
			}
		}
	}
}
