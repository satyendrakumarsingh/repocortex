package mcp

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------- session dedup
//
// NEVER RE-SERVE SOURCE THE AGENT IS ALREADY HOLDING.
//
// One conversation makes several RepoCortex calls against one index. The second
// call frequently ranks the same file into the same window as the first, so the
// same forty lines of the same file are rendered into the agent's context
// twice. The bytes are not merely redundant, they are charged twice against the
// budget in framing.go — and the budget is what keeps a result inline rather
// than spilled to a file the agent has to Read back.
//
// So a range that was already delivered in THIS session, and that is provably
// byte-identical now, is replaced by a one-line back-reference to it.
//
// THE ASYMMETRY THAT DECIDES EVERY THRESHOLD BELOW. Re-serving something the
// agent already holds costs a few hundred characters. Withholding something it
// never saw costs a whole file Read — the read itself, plus the cache write as
// that content lands in context anyway. The two errors are not comparable, so
// every threshold here leans towards SERVING, and every doubt resolves the same
// way: if it cannot be proven the agent has it, send it.
//
// Three rules follow from that, and each is load-bearing:
//
//  1. A POINTER, NEVER A BARE OMISSION. A withheld excerpt leaves an Item that
//     names the file, the lines and the fact that they were already sent.
//     Silence would read to the agent as "RepoCortex did not find it", which is a
//     worse answer than the duplication it removed.
//
//  2. PROVE-IT DEDUP ONLY. The test is a content fingerprint over the exact
//     bytes delivered, never an mtime and never the index's freshness flag. A
//     changed file produces a different fingerprint and is served in full.
//
//  3. CUT CHUNKS, NOT SLIVERS. Below minDedupLines the pointer sentence costs
//     more than the excerpt it replaces, so dedup would make the response BIGGER
//     while making it read as full of holes.
//
// PER-SESSION, NEVER PERSISTED. The state lives on the session and dies with
// it. A ledger that outlived the conversation would suppress source a NEW agent
// has never seen — the failure this mechanism exists to avoid, made permanent.

const (
	// minDedupLines is rule 3's floor: an excerpt covering fewer lines than this
	// is always served again.
	//
	// Eight, because the pointer sentence is ~115 characters and a source line
	// averages 30-40. Below eight lines the replacement is not reliably smaller
	// than what it replaces, and even when it is, the saving is a rounding error
	// against a response that now reads as full of holes. Holes cost the agent
	// judgement, and judgement is more expensive than bytes.
	minDedupLines = 8

	// minNetSaving is the whole-response floor, in rendered bytes, below which
	// no dedup is applied at all.
	//
	// It exists because the mechanism has a FIXED COST — the note explaining
	// itself, emitted once per response — that a per-item threshold cannot see.
	// A response that withholds one small excerpt pays ~270 characters for the
	// note to save ~300, which is not worth asking an agent to reason about an
	// indirection. 256 bytes is roughly one small excerpt's worth of genuine
	// saving on top of the note, and it is measured against the same renderer
	// framing.go bounds, not estimated.
	minNetSaving = 256

	// maxRangesPerSession bounds the ledger's memory. A long session touching
	// thousands of ranges must not grow without limit inside a server that may
	// hold many sessions at once.
	//
	// Eviction is SAFE BY CONSTRUCTION: an evicted entry is deleted, so the next
	// lookup misses and the range is served in full. There is no path by which
	// forgetting turns into assuming.
	maxRangesPerSession = 512

	// maxCallsRetained bounds how far back a pointer may reach.
	//
	// A back-reference to something served forty calls ago is a reference into a
	// part of the conversation that may have been compacted away, and pointing
	// at content the agent no longer holds is exactly the failure this file is
	// built to avoid. Thirty-two calls is a generous window for one task; beyond
	// it the range is served again. Any range referenced again inside the window
	// has its position refreshed, so a file the agent keeps returning to does
	// not age out from under it.
	maxCallsRetained = 32

	// maxProjectsPerSession bounds distinct projects tracked in one session. A
	// server serves one project, so this is defence against a session that
	// somehow spans several rather than an expected case; the oldest project's
	// entries are dropped whole, and dropping means re-serving.
	maxProjectsPerSession = 4

	// maxSessions bounds the number of ledgers a server retains. Each is small,
	// but a long-lived server accumulating a ledger per connection is a leak.
	// The oldest is dropped whole, and a session whose ledger was dropped simply
	// starts serving everything again.
	maxSessions = 64
)

// rangeKey identifies one delivered window of one file.
//
// The project is part of the key because a ledger must never let two indexes
// answer for each other, and the line span is part of it because "lines 40-95"
// and "lines 40-60" are different deliveries. Only an EXACT span match is
// deduped: proving that a smaller window sits inside a larger one already
// delivered would need the text kept in memory, and the saving does not justify
// either the memory or the extra way to be wrong.
type rangeKey struct {
	project string
	path    string
	start   int
	end     int
}

// servedRange is what the session already holds for one key.
type servedRange struct {
	// fingerprint is over the EXACT bytes delivered, after redaction and after
	// every bound in framing.go. Fingerprinting what we intended to send rather
	// than what we sent would let a clipped excerpt masquerade as a full one.
	fingerprint [32]byte

	// call is the sequence number of the call that last delivered or
	// re-affirmed this range, for the maxCallsRetained window.
	call int64
}

// sessionLedger records what one conversation already holds.
//
// Guarded by its own mutex rather than the server's: an MCP server handles
// concurrent tool calls, and two calls in one session legitimately race to
// record the same range. The loser of that race records a second, identical
// entry — which is idempotent — so concurrency costs a duplicated write and
// never a wrong answer.
type sessionLedger struct {
	mu    sync.Mutex
	calls int64

	seen  map[rangeKey]servedRange
	order []rangeKey // insertion order, for eviction

	// projects is the FIFO of distinct projects seen, capped.
	projects []string

	// Counters keep counting across eviction. They exist so a maintainer can
	// tell "we withheld nothing because nothing repeated" from "we withheld
	// nothing because the ledger kept throwing entries away".
	served   int64
	withheld int64
	evicted  int64
	saved    int64
}

func newSessionLedger() *sessionLedger {
	return &sessionLedger{seen: make(map[rangeKey]servedRange)}
}

// stats reports the counters. For tests and for future telemetry; the numbers
// are the only way to see this mechanism working from outside.
func (l *sessionLedger) stats() (served, withheld, evicted, saved int64) {
	if l == nil {
		return 0, 0, 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.served, l.withheld, l.evicted, l.saved
}

// elidedPointer is rule 1: the sentence that replaces a withheld excerpt.
//
// It names the span and says why it is absent, on the item's own provenance
// line — which already carries the path — so the agent can find what it is
// pointed at without a second lookup. It deliberately does NOT repeat the path:
// the line above it is the path, and paths run to 240 bytes.
func elidedPointer(k rangeKey) string {
	return fmt.Sprintf("source withheld: lines %d-%d of this file were already "+
		"served in this session and are unchanged — reuse that copy", k.start, k.end)
}

// dedupNote explains the mechanism once per response.
//
// Once, not per item: an agent needs to be told what a back-reference means and
// where to look, and being told n times is n-1 wasted explanations. Its length
// is charged against the saving before any dedup is applied.
func dedupNote(n int) string {
	return fmt.Sprintf("%d source excerpt(s) below are withheld because THIS session already "+
		"returned those exact lines and they have not changed; each is marked with the lines "+
		"it covers. Reuse the copy from earlier in this conversation rather than reading the "+
		"file. Content is only ever withheld after proving it is byte-identical.", n)
}

// excerptLines counts the lines an excerpt covers, ignoring a trailing newline.
func excerptLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n") + 1
	if strings.HasSuffix(s, "\n") {
		n--
	}
	return n
}

// pointerCost is the rendered cost of the back-reference, mirroring RenderText.
func pointerCost(p string) int { return len("  [") + len(p) + len("]") }

// apply is the whole mechanism: withhold what is proven duplicate, record what
// is served.
//
// IT RUNS AFTER Bound, AND THAT ORDER IS THE POINT. The ledger must record what
// the agent RECEIVED, not what a tool proposed — an excerpt that Bound dropped
// or clipped never reached the agent, and recording it would make a later call
// point at content that does not exist in the conversation. Running after Bound
// also means this can only shrink an already-legal response: every withheld
// item is required to save strictly more than its pointer costs, and the note
// is charged against the total before anything is applied, so the rendered
// length falls. The cap guarantee in framing.go is untouched.
//
// A nil ledger — an unknown or absent session — serves everything. That is the
// safe direction, and it is what makes a fresh session behave like one.
func (l *sessionLedger) apply(project string, r *Response) {
	if l == nil || r == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.calls++
	call := l.calls
	l.pruneByCall()

	type candidate struct {
		idx    int
		key    rangeKey
		fp     [32]byte
		saving int
	}
	var withhold, record []candidate

	for i := range r.Items {
		it := r.Items[i]
		// No excerpt, no path or no line means nothing to prove and nothing to
		// point at. An item RepoCortex cannot locate is never withheld.
		if it.Excerpt == "" || it.Path == "" || it.Line <= 0 {
			continue
		}

		lines := excerptLines(it.Excerpt)
		key := rangeKey{project: project, path: it.Path, start: it.Line, end: it.Line + lines - 1}
		fp := sha256.Sum256([]byte(it.Path + "\x00" + it.Excerpt))

		prev, ok := l.seen[key]
		switch {
		case !ok || prev.fingerprint != fp:
			// Never served, or served and since changed. Rule 2: serve it.
			record = append(record, candidate{idx: i, key: key, fp: fp})
		case lines < minDedupLines:
			// Rule 3: proven duplicate, but too small to be worth a pointer.
			// Re-affirm it so the window does not forget a file in active use.
			record = append(record, candidate{idx: i, key: key, fp: fp})
		default:
			saving := evidenceBlockLen(it) - pointerCost(elidedPointer(key))
			if saving <= 0 {
				// The pointer is not smaller than what it replaces. Serving is
				// both cheaper and clearer.
				record = append(record, candidate{idx: i, key: key, fp: fp})
				continue
			}
			withhold = append(withhold, candidate{idx: i, key: key, fp: fp, saving: saving})
		}
	}

	// All or nothing, measured against the renderer. Charging the note here —
	// before anything is withheld — is what keeps a response from growing in the
	// name of shrinking, and keeps the decision deterministic given the ledger.
	if len(withhold) > 0 {
		net := 0
		for _, c := range withhold {
			net += c.saving
		}
		net -= len("note: ") + len(dedupNote(len(withhold))) + 1
		if net < minNetSaving {
			record = append(record, withhold...)
			withhold = nil
		}
	}

	for _, c := range withhold {
		r.Items[c.idx].Excerpt = ""
		r.Items[c.idx].Elided = elidedPointer(c.key)
		l.withheld++
		l.saved += int64(c.saving)
		l.put(c.key, servedRange{fingerprint: c.fp, call: call}, false)
	}
	if len(withhold) > 0 {
		r.Notes = appendOnce(r.Notes, dedupNote(len(withhold)))
	}
	for _, c := range record {
		l.put(c.key, servedRange{fingerprint: c.fp, call: call}, true)
	}
}

// put records a range, evicting to stay inside the caps. Caller holds l.mu.
func (l *sessionLedger) put(k rangeKey, v servedRange, counted bool) {
	if _, exists := l.seen[k]; !exists {
		l.order = append(l.order, k)
		l.noteProject(k.project)
	}
	l.seen[k] = v
	if counted {
		l.served++
	}
	l.evictOverflow()
}

// noteProject caps distinct projects, dropping the oldest whole. Caller holds
// l.mu.
func (l *sessionLedger) noteProject(project string) {
	for _, p := range l.projects {
		if p == project {
			return
		}
	}
	l.projects = append(l.projects, project)
	for len(l.projects) > maxProjectsPerSession {
		oldest := l.projects[0]
		l.projects = l.projects[1:]
		for k := range l.seen {
			if k.project == oldest {
				delete(l.seen, k)
				l.evicted++
			}
		}
	}
}

// evictOverflow drops the oldest ranges until the ledger is inside its cap.
// Caller holds l.mu.
func (l *sessionLedger) evictOverflow() {
	for len(l.seen) > maxRangesPerSession && len(l.order) > 0 {
		k := l.order[0]
		l.order = l.order[1:]
		if _, ok := l.seen[k]; ok {
			delete(l.seen, k)
			l.evicted++
		}
	}
}

// pruneByCall drops ranges older than the call window. Caller holds l.mu.
func (l *sessionLedger) pruneByCall() {
	cutoff := l.calls - maxCallsRetained
	if cutoff <= 0 {
		return
	}
	for k, v := range l.seen {
		if v.call <= cutoff {
			delete(l.seen, k)
			l.evicted++
		}
	}
}

// ledgerBook holds one ledger per live session.
//
// Keyed by the session POINTER rather than by ServerSession.ID(), because ID()
// is empty for every stdio session — the transport RepoCortex actually ships on.
// Keying on an empty string would merge unrelated conversations into one
// ledger, and the failure mode of that is the one thing this file must never
// do: withhold source from an agent that never saw it. Holding the pointer as a
// key also keeps the session reachable, so its address cannot be recycled
// underneath us while an entry exists.
type ledgerBook struct {
	mu    sync.Mutex
	byID  map[*sdk.ServerSession]*sessionLedger
	order []*sdk.ServerSession
}

func newLedgerBook() *ledgerBook {
	return &ledgerBook{byID: make(map[*sdk.ServerSession]*sessionLedger)}
}

// forSession returns the ledger for a session, creating it on first use.
//
// A nil session — a direct handler call, or a transport that does not supply
// one — gets a nil ledger, and a nil ledger serves everything. Unknown session
// means fresh session, always.
func (b *ledgerBook) forSession(ss *sdk.ServerSession) *sessionLedger {
	if b == nil || ss == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if l, ok := b.byID[ss]; ok {
		return l
	}
	l := newSessionLedger()
	b.byID[ss] = l
	b.order = append(b.order, ss)
	for len(b.order) > maxSessions {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.byID, oldest)
	}
	return l
}

// sessionOf extracts the session from a tool request, tolerating a nil request
// so handlers stay callable directly in tests.
func sessionOf(req *sdk.CallToolRequest) *sdk.ServerSession {
	if req == nil {
		return nil
	}
	return req.Session
}
