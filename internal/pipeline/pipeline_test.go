package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
)

// collector is a Sink that records everything, safely under concurrency.
type collector struct {
	mu    sync.Mutex
	facts []FileFacts
	err   error // when set, WriteFile fails on the Nth call
	failN int
	calls int
}

func (c *collector) WriteFile(_ context.Context, f FileFacts) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil && c.calls >= c.failN {
		return c.err
	}
	c.facts = append(c.facts, f)
	return nil
}

func (c *collector) paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.facts))
	for _, f := range c.facts {
		out = append(out, f.File.Path)
	}
	sort.Strings(out)
	return out
}

func (c *collector) byPath(p string) (FileFacts, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.facts {
		if f.File.Path == p {
			return f, true
		}
	}
	return FileFacts{}, false
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runnerFor(t *testing.T, root string, opts Options) *Runner {
	t.Helper()
	w, err := fs.NewWalker(root, config.Defaults())
	if err != nil {
		t.Fatalf("NewWalker: %v", err)
	}
	return NewRunner(w, opts)
}

func TestRunExtractsFactsAcrossLanguages(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"svc/service.go": `package svc

import "context"

type TransactionService struct{}

func (s *TransactionService) UpdateStatus(ctx context.Context, id string) error {
	return s.repo.Save(id)
}`,
		"api/Controller.java": `package api;

import com.pay.service.TransactionService;

public class CallbackController {
    public void handleCallback(String id) { service.updateStatus(id); }
}`,
		"app/main.py": `from app.service import TransactionService

class Runner:
    def run(self):
        return self.svc.update_status("1")`,
	})

	var c collector
	stats, err := runnerFor(t, root, Options{}).Run(context.Background(), &c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if stats.FilesIndexed != 3 {
		t.Errorf("FilesIndexed = %d, want 3 (stats: %+v)", stats.FilesIndexed, stats)
	}
	if stats.Symbols == 0 {
		t.Error("no symbols extracted")
	}
	if stats.Imports == 0 {
		t.Error("no imports extracted; resolution confidence depends on these")
	}

	got, ok := c.byPath("svc/service.go")
	if !ok {
		t.Fatalf("svc/service.go missing; got %v", c.paths())
	}
	if got.File.Language != "go" {
		t.Errorf("language = %q, want go", got.File.Language)
	}
	if got.File.ContentHash == "" {
		t.Error("content hash not populated")
	}
	if len(got.Content) == 0 {
		t.Error("content not carried for search indexing")
	}
}

// TestSkippedFilesAreStillRecorded: a file absent from the index is
// indistinguishable from a file that does not exist. Gaps must be visible.
func TestSkippedFilesAreStillRecorded(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"main.go":    "package main",
		"notes.zzzz": "unknown file type",
	})
	if err := os.WriteFile(filepath.Join(root, "blob.bin"),
		[]byte{0x00, 0x01, 0x02, 0xff}, 0o644); err != nil {
		t.Fatal(err)
	}

	var c collector
	stats, err := runnerFor(t, root, Options{}).Run(context.Background(), &c)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesSkipped == 0 {
		t.Error("expected skipped files to be counted")
	}

	for _, p := range []string{"notes.zzzz", "blob.bin"} {
		f, ok := c.byPath(p)
		if !ok {
			t.Errorf("%s was not recorded at all", p)
			continue
		}
		if f.File.ParseStatus != "skipped" {
			t.Errorf("%s: status = %q, want skipped", p, f.File.ParseStatus)
		}
		if f.File.ParseError == "" {
			t.Errorf("%s: skipped without a reason", p)
		}
	}
}

// TestDeterministicAcrossRuns underpins the registry digest gate.
func TestDeterministicAcrossRuns(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for _, n := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"} {
		files["pkg/"+n+".go"] = "package pkg\n\ntype " + strings.ToUpper(n[:1]) + n[1:] +
			" struct{}\n\nfunc Do" + strings.ToUpper(n[:1]) + n[1:] + "() {}\n"
	}
	writeTree(t, root, files)

	var reference []string
	for run := 0; run < 8; run++ {
		var c collector
		if _, err := runnerFor(t, root, Options{Workers: 4}).Run(context.Background(), &c); err != nil {
			t.Fatal(err)
		}

		var fingerprint []string
		for _, f := range c.facts {
			names := make([]string, 0, len(f.Symbols))
			for _, s := range f.Symbols {
				names = append(names, s.Name+"@"+string(s.Kind))
			}
			sort.Strings(names)
			fingerprint = append(fingerprint, f.File.Path+"="+strings.Join(names, ","))
		}
		sort.Strings(fingerprint)

		if run == 0 {
			reference = fingerprint
			continue
		}
		if strings.Join(fingerprint, "|") != strings.Join(reference, "|") {
			t.Fatalf("run %d differs from run 0:\n ref=%v\n got=%v", run, reference, fingerprint)
		}
	}
}

// TestSinkErrorAbortsRun: a persistence failure is not survivable — continuing
// would produce a silently incomplete index.
func TestSinkErrorAbortsRun(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 40; i++ {
		files["pkg/f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".go"] = "package pkg\nfunc A(){}"
	}
	writeTree(t, root, files)

	sentinel := errors.New("disk on fire")
	c := &collector{err: sentinel, failN: 3}

	_, err := runnerFor(t, root, Options{Workers: 4}).Run(context.Background(), c)
	if err == nil {
		t.Fatal("expected the run to abort when the sink fails")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
}

// TestParseFailureDoesNotAbortRun is the counterpart: bad source is data, not
// a run-level failure. Spec §13.
func TestParseFailureDoesNotAbortRun(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"good.go":   "package p\nfunc Good() {}",
		"broken.go": "package p\nfunc Broken( { { {",
		"also.go":   "package p\nfunc Also() {}",
	})

	var c collector
	stats, err := runnerFor(t, root, Options{}).Run(context.Background(), &c)
	if err != nil {
		t.Fatalf("a malformed file aborted the whole run: %v", err)
	}
	if len(c.paths()) != 3 {
		t.Errorf("recorded %v, want all 3 files", c.paths())
	}
	t.Logf("stats: %+v", stats)
}

func TestContextCancellationStopsRun(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 200; i++ {
		files["pkg/f"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+".go"] = "package pkg\nfunc A(){}"
	}
	writeTree(t, root, files)

	ctx, cancel := context.WithCancel(context.Background())
	c := &collector{}
	var once sync.Once
	sink := SinkFunc(func(ctx context.Context, f FileFacts) error {
		once.Do(cancel)
		return c.WriteFile(ctx, f)
	})

	if _, err := runnerFor(t, root, Options{Workers: 2}).Run(ctx, sink); err != nil {
		t.Fatalf("cancellation should end the run cleanly, got: %v", err)
	}
	if len(c.paths()) >= 200 {
		t.Error("cancellation did not actually stop work")
	}
}

func TestProgressCallback(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.go": "package p", "b.go": "package p", "c.go": "package p",
	})

	var mu sync.Mutex
	var maxDone, seenTotal int64
	var c collector
	_, err := runnerFor(t, root, Options{
		Workers: 2,
		OnProgress: func(done, total int64) {
			mu.Lock()
			defer mu.Unlock()
			if done > maxDone {
				maxDone = done
			}
			seenTotal = total
		},
	}).Run(context.Background(), &c)
	if err != nil {
		t.Fatal(err)
	}
	if maxDone != 3 || seenTotal != 3 {
		t.Errorf("progress = %d/%d, want 3/3", maxDone, seenTotal)
	}
}

// TestFixtureEndToEnd runs the payment-platform fixture through the pipeline.
func TestFixtureEndToEnd(t *testing.T) {
	root := filepath.Join("..", "..", "fixtures", "payment-platform")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	var c collector
	stats, err := runnerFor(t, root, Options{}).Run(context.Background(), &c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("stats: %+v", stats)

	if stats.ParseFailures != 0 {
		t.Errorf("fixture produced %d parse failures", stats.ParseFailures)
	}

	all := map[string]bool{}
	for _, f := range c.facts {
		for _, s := range f.Symbols {
			all[s.Name] = true
		}
	}
	for _, want := range []string{
		"CallbackController", "handleCallback",
		"TransactionService", "updateStatus",
		"TransactionRepository", "NotificationService",
		"CallbackControllerTest", "TransactionServiceTest",
	} {
		if !all[want] {
			t.Errorf("golden entity %q missing from pipeline output", want)
		}
	}
}

type mockLargeParser struct{}

func (m *mockLargeParser) Language() string { return "go" }

func (m *mockLargeParser) Parse(ctx context.Context, path string, src []byte) (*parse.Result, error) {
	return &parse.Result{Language: "go", Status: parse.StatusOK}, nil
}

type trackingDrainableRegistry struct {
	parse.Registry
	drainCalls atomic.Int32
}

func (m *trackingDrainableRegistry) ForFile(path string) parse.Parser {
	return &mockLargeParser{}
}

func (m *trackingDrainableRegistry) DrainPools() {
	m.drainCalls.Add(1)
	if d, ok := m.Registry.(parse.Drainable); ok {
		d.DrainPools()
	}
}

func TestRunnerDrainsOnLargeFile(t *testing.T) {
	root := t.TempDir()

	// Write a file that is > LargeFileDrainThreshold (256 KiB)
	var largeBuilder strings.Builder
	largeBuilder.WriteString("package main\n\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&largeBuilder, "func F%d() string { return %q }\n", i, strings.Repeat("x", 140))
	}
	largeSrc := largeBuilder.String()
	if int64(len(largeSrc)) <= LargeFileDrainThreshold {
		t.Fatalf("test source size %d must exceed LargeFileDrainThreshold %d", len(largeSrc), LargeFileDrainThreshold)
	}

	writeTree(t, root, map[string]string{
		"large.go": largeSrc,
	})

	tracker := &trackingDrainableRegistry{
		Registry: parse.NewRegistry(),
	}

	opts := Options{
		Parsers:             tracker,
		DrainThresholdBytes: 1, // trigger drain whenever HeapInuse > 1 byte
	}

	var c collector
	runner := runnerFor(t, root, opts)
	stats, err := runner.Run(context.Background(), &c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.FilesIndexed != 1 {
		t.Fatalf("expected 1 file indexed, got %d", stats.FilesIndexed)
	}

	// 1 call from worker loop immediately after large file (>256KB), plus 1 final drain at end of run = 2 total.
	if calls := tracker.drainCalls.Load(); calls < 2 {
		t.Errorf("expected at least 2 drain calls (1 mid-run for large file + 1 final), got %d", calls)
	}
}

