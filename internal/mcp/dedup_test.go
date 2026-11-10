package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ------------------------------------------------------------- A23 test fixture
//
// These tests go through a real MCP client over a real transport, because the
// unit under test is a property of a SESSION and a session only exists on the
// transport. Calling handlers directly would test the dedup arithmetic while
// skipping the one thing that can silently break it: which conversation a call
// belongs to.

// dedupFixture is a server whose index can be rewritten between calls, so a
// test can make a file genuinely change underneath a live session.
type dedupFixture struct {
	db  *store.DB
	reg *registry.Registry
	srv *Server
}

func newDedupFixture(t *testing.T) *dedupFixture {
	t.Helper()
	ctx := context.Background()

	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	reg := registry.New(db)
	cfg := config.Defaults()
	cfg.ProjectName = "dedupproj"

	srv, err := New(Deps{Registry: reg, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	return &dedupFixture{db: db, reg: reg, srv: srv}
}

// index commits a fresh run holding one file and one symbol over it.
func (f *dedupFixture) index(t *testing.T, path, body string, startLine, endLine int, name string) {
	t.Helper()
	ctx := context.Background()

	run, err := f.reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := f.reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	file := model.File{Path: path, Language: "java", ContentHash: fmt.Sprintf("h%d", len(body)), ParseStatus: "ok"}
	fileID, err := w.AddFile(ctx, &file)
	if err != nil {
		t.Fatal(err)
	}
	sym := model.Symbol{
		FileID: fileID, Kind: model.KindMethod, Name: name,
		FQN: "svc.Ledger#" + name, Exported: true,
		Range: model.Range{
			Start: model.Position{Line: startLine},
			End:   model.Position{Line: endLine},
		},
		Confidence: model.ConfidenceHigh, Source: model.SourceSourceCode,
	}
	if _, err := w.AddSymbol(ctx, &sym); err != nil {
		t.Fatal(err)
	}
	if err := w.IndexFileText(ctx, fileID, path, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
}

// connect opens a NEW MCP session against the same server. Two sessions from
// one fixture are two conversations, which is exactly what the per-session
// guarantee has to be tested against.
func (f *dedupFixture) connect(t *testing.T) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = f.srv.Run(context.Background(), serverT) }()

	sess, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// ledgerBody builds a file whose symbol body is large enough to be worth
// deduplicating, with a marker so a changed version is distinguishable.
func ledgerBody(marker string, lines int) string {
	var b strings.Builder
	b.WriteString("public class Ledger {\n")
	for i := 2; i <= lines; i++ {
		fmt.Fprintf(&b, "  // %s line %02d — body padding so this excerpt is worth a back-reference\n", marker, i)
	}
	return b.String()
}

const (
	dedupPath   = "svc/Ledger.java"
	dedupSymbol = "settle"
	pointerMark = "source withheld"
)

// ------------------------------------------------------------- the four rules

// TestSecondCallForAnUnchangedRangeReturnsAPointer is A23's whole point: the
// second delivery of the same lines costs a sentence, not the lines.
//
// It also pins rule 1 — the withheld excerpt is REPLACED, not dropped. An item
// that simply lost its source reads to an agent as "RepoCortex found nothing
// here", which is a worse answer than the duplication it removed.
func TestSecondCallForAnUnchangedRangeReturnsAPointer(t *testing.T) {
	f := newDedupFixture(t)
	f.index(t, dedupPath, ledgerBody("original", 14), 1, 14, dedupSymbol)
	sess := f.connect(t)

	first := resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))
	if !strings.Contains(first, "body padding") {
		t.Fatalf("the first call did not serve the source at all:\n%s", first)
	}
	if strings.Contains(first, pointerMark) {
		t.Fatalf("the first call withheld source this session had never sent:\n%s", first)
	}

	second := resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))

	if strings.Contains(second, "body padding") {
		t.Errorf("the second call re-sent source the session already held:\n%s", second)
	}
	if !strings.Contains(second, pointerMark) {
		t.Fatalf("the source vanished without a back-reference; an agent reads that as "+
			"'not found':\n%s", second)
	}
	// Rule 1 in detail: the pointer has to say WHAT and WHERE, or it is not a
	// pointer. The path is on the same provenance line; the span is here.
	if !strings.Contains(second, "lines 1-14") {
		t.Errorf("the back-reference does not name the lines it stands for:\n%s", second)
	}
	if !strings.Contains(second, dedupPath) {
		t.Errorf("the back-reference is not attached to a located item:\n%s", second)
	}
	// The item itself must survive. Withholding source is not withholding the
	// answer.
	if !strings.Contains(second, dedupSymbol) {
		t.Errorf("the item disappeared along with its excerpt:\n%s", second)
	}
	// And the trust boundary holds: RepoCortex's own sentence must not be inside
	// the block framed as verbatim untrusted repository content.
	if ev := strings.Index(second, sectionEvidence); ev >= 0 && strings.Index(second, pointerMark) > ev {
		t.Errorf("the back-reference was rendered inside SOURCE EVIDENCE, where an agent is "+
			"told to treat everything as untrusted repository text:\n%s", second)
	}
	if len(second) >= len(first) {
		t.Errorf("dedup made the response bigger: first %d bytes, second %d", len(first), len(second))
	}
	t.Logf("first call %d bytes, second call %d bytes (%.0f%% smaller)",
		len(first), len(second), 100*(1-float64(len(second))/float64(len(first))))
}

// TestChangedFileIsServedInFullAgain is rule 2. The proof is a content
// fingerprint, so a file whose bytes moved is a file the agent does not hold.
//
// The index is genuinely rebuilt between the two calls, on one live session —
// the case an mtime check or an index-freshness flag would get wrong.
func TestChangedFileIsServedInFullAgain(t *testing.T) {
	f := newDedupFixture(t)
	f.index(t, dedupPath, ledgerBody("original", 14), 1, 14, dedupSymbol)
	sess := f.connect(t)

	first := resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))
	if !strings.Contains(first, "original") {
		t.Fatalf("the first call did not serve the original body:\n%s", first)
	}

	// Same path, same span, different bytes.
	f.index(t, dedupPath, ledgerBody("rewritten", 14), 1, 14, dedupSymbol)

	second := resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))

	if strings.Contains(second, pointerMark) {
		t.Fatalf("a CHANGED file was withheld as already-served; the agent holds the old "+
			"body and would act on it:\n%s", second)
	}
	if !strings.Contains(second, "rewritten") {
		t.Errorf("the new body was not served:\n%s", second)
	}

	// And the new bytes are now what the ledger holds: a third, unchanged call
	// deduplicates against the REWRITTEN content, not the original.
	third := resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))
	if !strings.Contains(third, pointerMark) {
		t.Errorf("the ledger did not pick up the new content, so it can never dedup again:\n%s", third)
	}
}

// TestSubFloorRangeIsNeverDeduped is rule 3.
//
// Under minDedupLines the pointer sentence is not reliably smaller than the
// excerpt it replaces, and a response peppered with back-references to four
// lines each reads as full of holes while saving nothing. The floor is a floor:
// repeated, unchanged, provably held — and still served.
func TestSubFloorRangeIsNeverDeduped(t *testing.T) {
	f := newDedupFixture(t)
	f.index(t, dedupPath, ledgerBody("tiny", minDedupLines-1), 1, minDedupLines-1, dedupSymbol)
	sess := f.connect(t)

	var last string
	for call := 1; call <= 3; call++ {
		last = resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))
		if strings.Contains(last, pointerMark) {
			t.Fatalf("call %d withheld a %d-line excerpt, below the %d-line floor:\n%s",
				call, minDedupLines-1, minDedupLines, last)
		}
		if !strings.Contains(last, "body padding") && minDedupLines > 2 {
			t.Fatalf("call %d served no source at all:\n%s", call, last)
		}
	}
}

// TestFreshSessionServesEverything is the design constraint that makes the
// whole mechanism safe: the ledger is per-session and dies with it.
//
// A second conversation against the SAME server has seen nothing, whatever the
// first one was told. Getting this wrong would suppress source a new agent has
// never seen — the one failure worse than the duplication being removed.
func TestFreshSessionServesEverything(t *testing.T) {
	f := newDedupFixture(t)
	f.index(t, dedupPath, ledgerBody("original", 14), 1, 14, dedupSymbol)

	first := f.connect(t)
	if got := resultText(callTool(t, first, "get_symbol", map[string]any{"name": dedupSymbol})); !strings.Contains(got, "body padding") {
		t.Fatalf("session one was not served the source:\n%s", got)
	}
	if got := resultText(callTool(t, first, "get_symbol", map[string]any{"name": dedupSymbol})); !strings.Contains(got, pointerMark) {
		t.Fatalf("session one did not dedup, so this test proves nothing:\n%s", got)
	}

	second := f.connect(t)
	got := resultText(callTool(t, second, "get_symbol", map[string]any{"name": dedupSymbol}))
	if strings.Contains(got, pointerMark) {
		t.Fatalf("a NEW session was pointed at source it never received:\n%s", got)
	}
	if !strings.Contains(got, "body padding") {
		t.Errorf("a new session was not served the source:\n%s", got)
	}
}

// TestConcurrentCallsInOneSessionAreRaceFree runs under -race, which is the
// only reason it exists: an MCP server handles concurrent tool calls, and the
// ledger is shared mutable state on the session.
//
// The assertion is the invariant that survives any interleaving — a response
// either carries the source or carries a back-reference to it, never neither.
// Which of the concurrent calls wins the race to serve first is not, and cannot
// be, deterministic; that every one of them is answerable is.
func TestConcurrentCallsInOneSessionAreRaceFree(t *testing.T) {
	f := newDedupFixture(t)
	f.index(t, dedupPath, ledgerBody("original", 14), 1, 14, dedupSymbol)
	sess := f.connect(t)

	const workers = 16
	var wg sync.WaitGroup
	results := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := sess.CallTool(context.Background(), &sdk.CallToolParams{
				Name: "get_symbol", Arguments: map[string]any{"name": dedupSymbol},
			})
			if err != nil {
				results[i] = "ERROR: " + err.Error()
				return
			}
			results[i] = resultText(res)
		}(i)
	}
	wg.Wait()

	served, pointed := 0, 0
	for i, got := range results {
		switch {
		case strings.HasPrefix(got, "ERROR:"):
			t.Fatalf("call %d failed under concurrency: %s", i, got)
		case strings.Contains(got, "body padding"):
			served++
		case strings.Contains(got, pointerMark):
			pointed++
		default:
			t.Fatalf("call %d returned neither the source nor a back-reference to it:\n%s", i, got)
		}
	}
	if served == 0 {
		t.Fatal("no concurrent call served the source; every one pointed at something nobody sent")
	}
	t.Logf("%d concurrent calls: %d served source, %d pointed back", workers, served, pointed)
}

// TestLedgerIsRaceFreeUnderDirectConcurrency hammers the ledger itself, past
// the transport's own serialisation, so -race sees the shared maps and slices
// rather than whatever the SDK happens to funnel.
func TestLedgerIsRaceFreeUnderDirectConcurrency(t *testing.T) {
	l := newSessionLedger()

	var wg sync.WaitGroup
	for w := 0; w < 24; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				r := &Response{Summary: "concurrent"}
				for j := 0; j < 4; j++ {
					r.Items = append(r.Items, Item{
						Kind:    "method",
						Path:    fmt.Sprintf("pkg/file%d.go", (w+j)%8),
						Line:    1,
						Excerpt: strings.Repeat("x-x-x-x-x-x-x-x-x-x-x-x-x-x-x-x-x\n", 30),
					})
				}
				l.apply("dedupproj", r)
				for _, it := range r.Items {
					if it.Excerpt == "" && it.Elided == "" {
						t.Errorf("an item lost its source without gaining a pointer")
					}
				}
			}
		}(w)
	}
	wg.Wait()

	served, withheld, _, saved := l.stats()
	if served == 0 {
		t.Fatal("nothing was recorded as served")
	}
	if withheld == 0 {
		t.Fatal("nothing was ever deduped, so the concurrent path was never exercised")
	}
	t.Logf("served=%d withheld=%d saved=%d bytes", served, withheld, saved)
}

// ------------------------------------------------------------- engine details

// TestDedupNeverBreaksTheRenderedCap is the guarantee framing.go exists to
// make, checked on the other side of dedup.
//
// Dedup may only ever shrink a bounded response. It runs after Bound and
// appends a note, so "it only shrinks" is an arithmetic claim, not an obvious
// one — every withheld item must save strictly more than its pointer costs, and
// the note is charged against the total before anything is applied.
func TestDedupNeverBreaksTheRenderedCap(t *testing.T) {
	l := newSessionLedger()
	limits := DefaultLimits()

	build := func() *Response {
		r := &Response{Summary: "capped", IndexRevision: 7, IndexStatus: "fresh"}
		for i := 0; i < limits.MaxItems; i++ {
			r.Items = append(r.Items, Item{
				Kind: "method", Name: fmt.Sprintf("Sym%d", i),
				Path: fmt.Sprintf("pkg/a/file%d.go", i), Line: 40,
				Confidence: "high", Source: "inference", Why: "definition",
				Excerpt: strings.Repeat("func f() { return nil } // padding padding\n", 20),
			})
		}
		return r
	}

	first := build()
	first.Bound(limits)
	l.apply("p", first)
	firstLen := len(first.RenderText())

	second := build()
	second.Bound(limits)
	beforeDedup := len(second.RenderText())
	l.apply("p", second)
	afterDedup := len(second.RenderText())

	if afterDedup > limits.MaxTotalBytes {
		t.Fatalf("deduped response renders to %d bytes, over the %d budget; dedup grew a "+
			"response that Bound had already fitted", afterDedup, limits.MaxTotalBytes)
	}
	if afterDedup >= beforeDedup {
		t.Fatalf("dedup did not shrink the response: %d -> %d", beforeDedup, afterDedup)
	}
	t.Logf("first %d bytes; second %d -> %d after dedup (%.0f%% of the duplicate residue removed)",
		firstLen, beforeDedup, afterDedup, 100*(1-float64(afterDedup)/float64(beforeDedup)))
}

// TestSmallSavingIsNotWorthTheNote pins minNetSaving.
//
// The explanatory note is a fixed cost a per-item threshold cannot see. One
// modest excerpt withheld to save less than the note costs makes the response
// LARGER while asking the agent to reason about an indirection, so the whole
// dedup is abandoned for that response.
func TestSmallSavingIsNotWorthTheNote(t *testing.T) {
	l := newSessionLedger()

	// Ten lines: over the line floor, but only just enough bytes that the
	// pointer beats the excerpt — and nowhere near enough to pay for the note.
	body := strings.Repeat("ab\n", minDedupLines+2)

	build := func() *Response {
		return &Response{Summary: "small", Items: []Item{{
			Kind: "method", Path: "pkg/tiny.go", Line: 3, Excerpt: body,
		}}}
	}

	first := build()
	l.apply("p", first)
	if first.Items[0].Excerpt == "" {
		t.Fatal("the first delivery was withheld")
	}

	second := build()
	beforeLen := len(second.RenderText())
	l.apply("p", second)
	if second.Items[0].Elided != "" {
		t.Fatalf("a saving too small to pay for the note was taken anyway: %q", second.Items[0].Elided)
	}
	if got := len(second.RenderText()); got != beforeLen {
		t.Fatalf("the response changed size (%d -> %d) despite nothing being withheld", beforeLen, got)
	}
}

// TestEvictedRangesAreServedAgain covers the bounded-state constraint.
//
// The caps are memory limits, and forgetting must never turn into assuming: an
// entry the ledger dropped is an entry the agent must be sent again. The
// counters keep counting through it, which is the only way to tell "nothing
// repeated" from "the ledger threw it all away".
func TestEvictedRangesAreServedAgain(t *testing.T) {
	l := newSessionLedger()
	body := strings.Repeat("padding line for a range that is worth deduplicating\n", 20)

	item := func(i int) Item {
		return Item{Kind: "method", Path: fmt.Sprintf("pkg/f%04d.go", i), Line: 1, Excerpt: body}
	}

	// The first range, delivered and provably deduplicable.
	r := &Response{Summary: "first", Items: []Item{item(0)}}
	l.apply("p", r)
	again := &Response{Summary: "again", Items: []Item{item(0)}}
	l.apply("p", again)
	if again.Items[0].Elided == "" {
		t.Fatal("the range was not deduped before eviction, so this test proves nothing")
	}

	// Push it out of the range cap.
	overflow := &Response{Summary: "overflow"}
	for i := 1; i <= maxRangesPerSession+8; i++ {
		overflow.Items = append(overflow.Items, item(i))
	}
	l.apply("p", overflow)

	evicted := &Response{Summary: "after eviction", Items: []Item{item(0)}}
	l.apply("p", evicted)
	if evicted.Items[0].Excerpt == "" {
		t.Fatalf("an EVICTED range was still withheld: %q — the ledger forgot the content "+
			"but kept assuming the agent had it", evicted.Items[0].Elided)
	}

	served, withheld, dropped, _ := l.stats()
	if dropped == 0 {
		t.Fatal("nothing was counted as evicted although the cap was exceeded; the counters " +
			"stopped counting when detail was dropped")
	}
	t.Logf("served=%d withheld=%d evicted=%d", served, withheld, dropped)
}

// TestPointerlessItemsAreNeverWithheld: an item RepoCortex cannot locate cannot
// be pointed at, so it is always served. No line number, no dedup.
func TestPointerlessItemsAreNeverWithheld(t *testing.T) {
	l := newSessionLedger()
	body := strings.Repeat("a line of source that would otherwise be worth withholding\n", 12)

	build := func() *Response {
		return &Response{Summary: "unlocatable", Items: []Item{
			{Kind: "file", Path: "pkg/x.go", Line: 0, Excerpt: body},
			{Kind: "file", Path: "", Line: 12, Excerpt: body},
		}}
	}
	l.apply("p", build())
	second := build()
	l.apply("p", second)

	for i, it := range second.Items {
		if it.Excerpt == "" {
			t.Errorf("item %d was withheld although it has no locatable range: %q", i, it.Elided)
		}
	}
}

// TestMeasuredTwoCallSequence is the number A23 is judged on.
//
// A realistic sequence: an agent locates a symbol, then comes back to it after
// looking at its callers — the commonest shape of a RepoCortex conversation, and
// the one that used to pay for the same body twice.
func TestMeasuredTwoCallSequence(t *testing.T) {
	f := newDedupFixture(t)
	f.index(t, dedupPath, ledgerBody("original", 14), 1, 14, dedupSymbol)

	measure := func(dedup bool) (int, int) {
		sess := f.connect(t)
		a := resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))
		if !dedup {
			// A second session has seen nothing, so its first call is exactly
			// what call two would have cost with no dedup at all.
			sess = f.connect(t)
		}
		b := resultText(callTool(t, sess, "get_symbol", map[string]any{"name": dedupSymbol}))
		return len(a), len(b)
	}

	a1, b1 := measure(false)
	a2, b2 := measure(true)

	before, after := a1+b1, a2+b2
	t.Logf("two-call sequence: before %d bytes (%d + %d), after %d bytes (%d + %d) — %d saved, %.1f%%",
		before, a1, b1, after, a2, b2, before-after, 100*float64(before-after)/float64(before))
	t.Logf("second call alone: %d -> %d bytes, %.1f%% of the duplicate response removed",
		b1, b2, 100*float64(b1-b2)/float64(b1))

	if after >= before {
		t.Fatalf("the deduped sequence is not smaller: %d vs %d", after, before)
	}
}
