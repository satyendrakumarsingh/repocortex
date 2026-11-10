package context

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// ErrEmptyTask is returned for a blank or whitespace-only task.
var ErrEmptyTask = errors.New("context: task must not be empty")

// Assemble builds the minimum sufficient context for req.Task.
//
// The pipeline, in order, each stage narrowing the last:
//
//	normalise the task    -> Intent (weighted terms, identifiers, verbs)
//	candidate retrieval   -> seeds by symbol name + files by full text
//	graph expansion       -> bounded, confidence-aware, from the seeds
//	structural completion -> owning declarations, test coverage, tables touched
//	admission             -> the gate that keeps text-only matches out
//	ranking               -> one weighted score per unit
//	compression           -> summaries first, source ranges last
//	budget                -> stop at marginal value, report what was cut
//
// runID must be the run g reads (the committed one, for a graph.Graph built
// with graph.New). Passing a different run yields a catalogue and a traversal
// that disagree; nothing here can detect that, so the caller owns it.
// Assembler owns the catalogue cache.
//
// It exists because CLAUDE.md §4.2 forbids package-level mutable state, and the
// cache used to be exactly that: a process-wide var, shared by every caller,
// invisible in every signature. The rule is right here for a concrete reason —
// a global cache is shared by tests that never asked for it, cannot be sized or
// dropped per consumer, and makes "which index is this serving?" a question the
// type system cannot answer.
//
// Ownership also lands the cache where it earns its keep. The MCP server holds
// one Assembler for its lifetime and every request reuses the catalogue; a CLI
// command builds one, answers a question and exits, where a cache was never
// going to help. Neither had to know about the other before, and that was the
// problem.
type Assembler struct {
	cache *catalogueCache
}

// NewAssembler returns an Assembler with an empty cache.
func NewAssembler() *Assembler { return &Assembler{cache: &catalogueCache{}} }

// Assemble builds a context pack, reusing this Assembler's catalogue when the
// run's content has not changed.
func (a *Assembler) Assemble(ctx context.Context, reg Registry, g Graph, runID int64, req Request) (Pack, error) {
	if a == nil || a.cache == nil {
		// A zero Assembler is usable rather than a nil-dereference: the cache is
		// an optimisation, and a caller that got construction wrong should get
		// a correct answer slowly, not a panic.
		return assemble(ctx, &catalogueCache{}, reg, g, runID, req)
	}
	return assemble(ctx, a.cache, reg, g, runID, req)
}

func assemble(ctx context.Context, cache *catalogueCache, reg Registry, g Graph, runID int64, req Request) (Pack, error) {
	if strings.TrimSpace(req.Task) == "" {
		return Pack{}, ErrEmptyTask
	}
	if reg == nil {
		return Pack{}, errors.New("context: registry must not be nil")
	}

	budget := req.Budget.withDefaults()
	tuning := DefaultTuning()
	if req.Tuning != nil {
		tuning = *req.Tuning
	}

	pack := Pack{Task: req.Task}
	intent := ParseTask(req.Task)

	if intent.Empty() {
		pack.Interpretation = "No retrievable terms in this task."
		pack.Notes = append(pack.Notes,
			"The task contained no retrievable terms once stop words and short fragments "+
				"were removed. Name a symbol, a file or a domain noun — RepoCortex matches "+
				"identifiers, not sentiment.")
		pack.TokenEstimate = pack.EstimateTokens()
		return pack, nil
	}

	// Cached per run CONTENT, not per run id: incremental indexing mutates a
	// committed run in place, so the id alone would go stale. See cache.go.
	cat, err := cache.get(ctx, reg, runID)
	if err != nil {
		return Pack{}, err
	}

	seeds := findSeeds(cat, intent, tuning.Thresholds)
	lexical, err := searchLexical(ctx, reg, intent, tuning.Thresholds)
	if err != nil {
		return Pack{}, err
	}
	// Text evidence seeds the walk too. Without this the call graph is
	// unreachable whenever the task is phrased in the user's vocabulary rather
	// than the code's — see addLexicalSeeds.
	seeds = addLexicalSeeds(cat, intent, seeds, lexical, tuning.Thresholds)

	reached, notes, err := expand(ctx, g, seeds, budget, tuning.Thresholds)
	if err != nil {
		return Pack{}, err
	}
	pack.Notes = append(pack.Notes, notes...)
	pack.Interpretation = interpret(intent, seedNames(cat, seeds), len(reached))

	b := newBuilder(cat, intent, tuning, budget, reg, runID)
	b.fromSeeds(seeds)
	b.fromGraph(reached)
	b.fromLexical(lexical)
	if err := b.fromData(ctx); err != nil {
		return Pack{}, err
	}
	b.fromTests()
	b.finishSignals(seeds)

	items, excluded, selNotes := b.choose(ctx)
	pack.Items = items
	pack.Excluded = excluded
	pack.Notes = append(pack.Notes, selNotes...)
	pack.Notes = append(pack.Notes, enforceTokenBudget(&pack, budget)...)

	if len(pack.Items) == 0 {
		pack.Notes = append(pack.Notes, fmt.Sprintf(
			"Nothing in this index matched the task: no symbol name matched %s, and no file "+
				"matched them textually, so there was nothing to expand from. Either this area "+
				"is not indexed, or the work does not exist in the repository yet.",
			quoteList(termTexts(intent))))
	}
	pack.TokenEstimate = pack.EstimateTokens()
	return pack, nil
}

// builder accumulates candidates across the retrieval stages.
type builder struct {
	cat    *catalogue
	intent Intent
	tuning Tuning
	budget Budget
	reg    Registry
	runID  int64

	units map[int64]*candidate

	// content caches file text for the units that reached the data stage, so
	// SQL extraction and excerpt selection share one read per file.
	content map[int64]string

	// members records each unit's relevant symbols with how close they sit to
	// the task, so the Why string can name the ones that matter rather than the
	// ones that sort first. depth -1 marks a symbol the task named outright.
	members    map[int64][]memberNote
	memberSeen map[int64]map[int64]bool

	// keyMember is the member whose evidence the item should show: the seed the
	// task named, or failing that the shallowest symbol the traversal reached.
	keyMember map[int64]int64
	keyDepth  map[int64]int
}

func newBuilder(cat *catalogue, in Intent, t Tuning, b Budget, reg Registry, runID int64) *builder {
	return &builder{
		cat: cat, intent: in, tuning: t, budget: b, reg: reg, runID: runID,
		units:      map[int64]*candidate{},
		content:    map[int64]string{},
		members:    map[int64][]memberNote{},
		memberSeen: map[int64]map[int64]bool{},
		keyMember:  map[int64]int64{},
		keyDepth:   map[int64]int{},
	}
}

func (b *builder) unitFor(symbolID int64) *candidate {
	unitID := b.cat.unit(symbolID)
	if c := b.units[unitID]; c != nil {
		return c
	}
	sym := b.cat.symbols[unitID]
	c := &candidate{
		unitID: unitID,
		kind:   string(sym.Kind),
		name:   sym.Name,
		fqn:    sym.FQN,
		path:   b.cat.path(unitID),
		line:   sym.Range.Start.Line,
	}
	if b.cat.isTestUnit(unitID) {
		// Kind is overridden rather than left as "class" because "this is a
		// test" is the fact an agent acts on.
		c.kind = string(model.KindTest)
	}
	b.units[unitID] = c
	b.memberSeen[unitID] = map[int64]bool{}
	return c
}

// memberNote is one relevant symbol inside a unit. Depth is how far it sits
// from the task: -1 when the task named it, otherwise its traversal depth.
type memberNote struct {
	name  string
	depth int
}

// noteMember records that one symbol inside a unit carried signal, which drives
// both the member bonus and the "relevant members" clause of the Why string.
func (b *builder) noteMember(c *candidate, symbolID, depth int64) {
	if b.memberSeen[c.unitID][symbolID] {
		return
	}
	b.memberSeen[c.unitID][symbolID] = true
	c.members++
	if symbolID != c.unitID {
		b.members[c.unitID] = append(b.members[c.unitID],
			memberNote{name: b.cat.symbols[symbolID].Name, depth: int(depth)})
	}
}

func (b *builder) fromSeeds(seeds []seed) {
	// seeds arrive strongest-first, so the first seed inside a unit is the one
	// worth showing.
	for _, s := range seeds {
		c := b.unitFor(s.symbolID)
		if s.score > c.nameScore {
			c.nameScore, c.nameTerms = s.score, s.matched
		}
		b.noteMember(c, s.symbolID, -1)
		if _, ok := b.keyMember[c.unitID]; !ok {
			b.keyMember[c.unitID] = s.symbolID
			b.keyDepth[c.unitID] = -1 // -1 outranks any traversal depth
		}
	}
}

func (b *builder) fromGraph(reached map[int64]reach) {
	// Sorted iteration: which member wins a tie must not depend on map order.
	ids := make([]int64, 0, len(reached))
	for id := range reached {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if _, known := b.cat.symbols[id]; !known {
			// A traversal only returns symbols from its run, so this happens
			// only when runID and the graph's run disagree. Skipping beats
			// emitting an item with an empty name.
			continue
		}
		r := reached[id]
		c := b.unitFor(id)
		if p := proximity(r.depth); p > c.proximity {
			c.proximity, c.depth = p, r.depth
			// A root's confidence is exact by graph.Traverse's convention — "a
			// root is an input, not an inference" — but that says nothing about
			// how we came to include it, which was a name match. Copying it
			// would put confidence=exact on an item no compiler ever saw, and
			// invariant 2 says exact means a compiler proved it. Only the
			// confidence of an actual traversed path is carried forward.
			if r.depth > 0 {
				c.pathConf = r.conf
			}
		}
		if d, ok := b.keyDepth[c.unitID]; !ok || (d >= 0 && r.depth < d) {
			b.keyMember[c.unitID], b.keyDepth[c.unitID] = id, r.depth
		}
		b.noteMember(c, id, int64(r.depth))
	}
}

func (b *builder) fromLexical(hits map[int64]*lexicalHit) {
	// Normalise bm25 against the best score in THIS result set. bm25 is not
	// comparable across queries or corpora, so an absolute threshold would
	// behave differently on every repository; a ratio to the best hit is at
	// least meaningful within one task.
	var best float64
	for _, h := range hits {
		if h.best < best {
			best = h.best
		}
	}
	total := b.intent.TotalWeight()

	fileIDs := make([]int64, 0, len(hits))
	for id := range hits {
		fileIDs = append(fileIDs, id)
	}
	sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })

	for _, fileID := range fileIDs {
		h := hits[fileID]

		// Coverage of the task's intent, scaled by how strongly FTS ranked the
		// file. Coverage dominates deliberately: matching two of four task
		// words says more than matching one of them very often, which is
		// exactly how the decoy file outranks the real one on raw bm25.
		coverage := 0.0
		if total > 0 {
			coverage = h.weight / total
		}
		strength := 1.0
		if best < 0 {
			strength = h.best / best
		}
		score := coverage * (0.5 + 0.5*strength)

		units := b.cat.unitsByFile[fileID]
		for _, unitID := range units {
			c := b.unitFor(unitID)
			c.lexical, c.lexTerms = score, h.terms
		}
		if len(units) == 0 {
			// A file with no symbols — a README, a config file, a language with
			// no parser. It still answers questions, so it becomes a file-level
			// candidate rather than vanishing.
			b.addFileCandidate(fileID, score, h.terms)
		}
	}
}

// syntheticFileBase and syntheticTableBase offset file- and table-level
// candidates out of the symbol id space. SQLite rowids are always positive, so
// a synthetic unit can never shadow a real symbol, and the two ranges cannot
// collide with each other.
const (
	syntheticFileBase  = int64(-1)
	syntheticTableBase = int64(-1_000_000_000)
)

func (b *builder) addFileCandidate(fileID int64, lexical float64, terms []string) {
	f := b.cat.files[fileID]
	if f.Path == "" {
		return
	}
	id := syntheticFileBase - fileID
	if _, ok := b.units[id]; ok {
		return
	}
	b.units[id] = &candidate{
		unitID:   id,
		kind:     string(model.KindFile),
		name:     f.Path,
		path:     f.Path,
		lexical:  lexical,
		lexTerms: terms,
	}
	b.memberSeen[id] = map[int64]bool{}
}

// maxExcerptWindow is how many lines of a file are read at once. Large enough
// for any realistic source file, small enough that a generated half-million-line
// file cannot be pulled into memory whole.
const maxExcerptWindow = 20000

// fromData reads the source of units that already carry structural signal and
// records the tables they touch.
//
// Content is fetched only for those units, and only up to the file budget, so
// this is a bounded number of indexed reads rather than a scan of the
// repository. It is also the only place source text is read; the excerpt stage
// reuses the same cache.
//
// Reading a file merely because a word appeared in it would put the decoy back
// in through the side door, so lexical-only candidates are never opened.
func (b *builder) fromData(ctx context.Context) error {
	type ranked struct {
		unitID int64
		score  float64
	}
	var pre []ranked
	for _, id := range b.sortedUnits() {
		c := b.units[id]
		if id < 0 || (c.nameScore == 0 && c.proximity == 0) {
			continue
		}
		pre = append(pre, ranked{id, c.nameScore*b.tuning.SymbolNameMatch + c.proximity*b.tuning.GraphProximity})
	}
	sort.Slice(pre, func(i, j int) bool {
		if pre[i].score != pre[j].score {
			return pre[i].score > pre[j].score
		}
		return pre[i].unitID < pre[j].unitID
	})

	seenFile := map[int64]bool{}
	tables := map[string]*tableRef{}
	owner := map[string]*candidate{}

	for _, r := range pre {
		c := b.units[r.unitID]
		fileID := b.cat.symbols[r.unitID].FileID
		if seenFile[fileID] {
			continue
		}
		if len(seenFile) >= b.budget.MaxFiles {
			break
		}
		seenFile[fileID] = true

		text, err := b.reg.FileExcerpt(ctx, b.runID, c.path, 1, maxExcerptWindow)
		if err != nil {
			// A file with no FTS row (skipped as binary or oversized) is normal
			// and must not fail the request.
			continue
		}
		b.content[fileID] = text

		for _, t := range extractTables(b.cat.files[fileID].Language, text) {
			c.tables = append(c.tables, t)
			key := strings.ToLower(t.Name)
			if prev, ok := tables[key]; ok {
				prev.Write = prev.Write || t.Write
				continue
			}
			ref := t
			tables[key] = &ref
			owner[key] = c
		}
	}

	for _, key := range sortedKeys(tables) {
		b.addTableCandidate(*tables[key], owner[key])
	}
	return nil
}

// addTableCandidate turns a table reference into a unit of context.
//
// When the run holds a symbol of the same name defined in a SQL file, the
// migration that creates the table becomes the item's location and the
// relationship is reported as MEDIUM confidence: two independent sources — a
// statement in application code and a definition in a migration — agree. With
// only the string literal it stays LOW and points at the code that issued the
// statement, which is honest about what was actually observed.
func (b *builder) addTableCandidate(t tableRef, ownerUnit *candidate) {
	name := strings.ToLower(t.Name)

	unitID := int64(0)
	path, line := ownerUnit.path, t.Line
	conf := model.ConfidenceLow

	for _, symID := range b.cat.byName[name] {
		s := b.cat.symbols[symID]
		if !strings.EqualFold(b.cat.files[s.FileID].Language, "sql") {
			continue
		}
		unitID, path, line = symID, b.cat.path(symID), s.Range.Start.Line
		conf = model.ConfidenceMedium
		break
	}
	if unitID == 0 {
		// No migration in the index. Key the synthetic id off the table name so
		// two references to the same table converge on one item.
		unitID = syntheticTableBase - int64(len(b.units))
	}
	if existing, ok := b.units[unitID]; ok && existing.tableOwner != "" {
		return
	}

	relation := ownerUnit.name
	if t.Write {
		relation = ownerUnit.name + " (writes)"
	}
	b.units[unitID] = &candidate{
		unitID:     unitID,
		kind:       string(model.KindTable),
		name:       name,
		path:       path,
		line:       line,
		tableOwner: relation,
		relates:    conf,
		tables:     []tableRef{t},
	}
	b.memberSeen[unitID] = map[int64]bool{}
}

// fromTests pulls in the tests that cover what is already included.
//
// The TestRelation weight applies ONLY to units nothing else admitted. A test
// the task named outright, or one the call graph already reached, is accounted
// for by those stronger signals; stacking the test bonus on top would rank a
// test above the code it covers, which is not the order to read them in.
func (b *builder) fromTests() {
	subjects := map[string]bool{}
	for _, id := range b.sortedUnits() {
		if c := b.units[id]; c.nameScore > 0 || c.proximity > 0 {
			subjects[c.name] = true
		}
	}
	if len(subjects) == 0 {
		return
	}

	// Driven by subject rather than by a walk over every symbol in the run: the
	// question is "which tests cover THESE names", and the catalogue answers it
	// by key. Subjects are visited in sorted order so the candidate set is built
	// in the same sequence on every run.
	for _, subject := range sortedKeys(subjects) {
		for _, symID := range b.cat.testsBySubject[subject] {
			c := b.unitFor(symID)
			if c.nameScore > 0 || c.proximity > 0 {
				continue
			}
			c.testFor = subject
			c.relates = model.ConfidenceLow
		}
	}
}

// finishSignals computes the modifier signals, which need the full candidate
// set before they mean anything, and scores every candidate.
func (b *builder) finishSignals(seeds []seed) {
	var seedPaths []string
	for _, s := range seeds {
		if p := b.cat.path(s.symbolID); !contains(seedPaths, p) {
			seedPaths = append(seedPaths, p)
		}
	}
	sort.Strings(seedPaths)
	span := b.cat.maxMod - b.cat.minMod

	for _, id := range b.sortedUnits() {
		c := b.units[id]
		for _, p := range seedPaths {
			if a := affinity(c.path, p); a > c.affinity {
				c.affinity = a
			}
		}
		if span > 0 && id > 0 {
			if f, ok := b.cat.files[b.cat.symbols[id].FileID]; ok {
				c.recency = float64(f.ModTimeUnix-b.cat.minMod) / float64(span)
			}
		}
		if c.relates == "" {
			switch {
			case c.pathConf != "":
				c.relates = c.pathConf
			case c.nameScore > 0:
				// The task naming a symbol says a lot about intent, but it is
				// still a name match. Nothing short of a compiler earns exact.
				c.relates = model.ConfidenceHigh
			default:
				c.relates = model.ConfidenceLow
			}
		}
		c.computeScore(b.tuning.Weights)
	}
}

func (b *builder) sortedUnits() []int64 {
	out := make([]int64, 0, len(b.units))
	for id := range b.units {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// choose applies admission, ranking, the marginal-value cut and the budget.
func (b *builder) choose(ctx context.Context) ([]Item, []Exclusion, []string) {
	var admitted, rejected []*candidate
	for _, id := range b.sortedUnits() {
		c := b.units[id]
		c.via = admit(c, b.tuning.Thresholds)
		if c.via == admitNone || c.score <= 0 {
			rejected = append(rejected, c)
			continue
		}
		admitted = append(admitted, c)
	}
	sort.Slice(admitted, func(i, j int) bool { return less(admitted[i], admitted[j]) })

	var notes []string
	var excluded []Exclusion

	// Marginal-value cut, applied before the budget. Stopping because the next
	// item adds nothing is a better answer than stopping because a counter
	// filled up, and it is the difference between "here is what matters" and
	// "here are twenty files".
	floor := b.tuning.AbsoluteFloor
	if len(admitted) > 0 {
		if rel := admitted[0].score * b.tuning.MarginalFraction; rel > floor {
			floor = rel
		}
	}
	kept := make([]*candidate, 0, len(admitted))
	thin := 0
	for _, c := range admitted {
		if c.score < floor {
			thin++
			excluded = append(excluded, Exclusion{
				Name: c.name, Path: c.path,
				Reason: fmt.Sprintf("relevance %.2f is below this task's cut-off of %.2f "+
					"(%s), so it would cost more context than it explains", c.score, floor, c.via),
			})
			continue
		}
		kept = append(kept, c)
	}
	if thin > 0 {
		// "see excluded" was a promise the pack does not keep: the exclusion
		// list is capped at MaxExclusions, so a note claiming 661 candidates
		// were listed sat above a list of 12. A sample is useful; a sample
		// described as the whole set is a false statement about the answer.
		notes = append(notes, fmt.Sprintf(
			"%s scored below the marginal-value cut-off and were left out. The excluded "+
				"list below is a sample of at most %d of them, not the full set.",
			plural(thin, "further candidate"), b.tuning.MaxExclusions))
	}

	// The body cliff is applied HERE: over every candidate that survived the
	// marginal-value cut, before MaxSymbols or MaxFiles have removed anything.
	// Applied after the caps, the caps fill with cliff-bound items and the
	// envelope they free is never handed to the items behind them.
	markBodyCliff(kept, b.tuning.Weights, b.tuning.Thresholds)

	// Near-misses are worth naming; the rest of the repository is not.
	sort.Slice(rejected, func(i, j int) bool { return less(rejected[i], rejected[j]) })
	for _, c := range rejected {
		if len(excluded) >= b.tuning.MaxExclusions {
			break
		}
		if c.lexical == 0 && c.nameScore == 0 {
			continue
		}
		excluded = append(excluded, Exclusion{
			Name: c.name, Path: c.path,
			Reason: rejectionReason(c, b.tuning.Thresholds),
		})
	}

	items, budgetNotes := b.fit(ctx, kept, &excluded)
	notes = append(notes, budgetNotes...)
	if len(excluded) > b.tuning.MaxExclusions {
		excluded = excluded[:b.tuning.MaxExclusions]
	}
	return items, excluded, notes
}

// less orders candidates for presentation: strongest first, then by location so
// two identically-scored items never swap between runs.
func less(a, c *candidate) bool {
	switch {
	case a.score != c.score:
		return a.score > c.score
	case a.path != c.path:
		return a.path < c.path
	case a.line != c.line:
		return a.line < c.line
	default:
		return a.name < c.name
	}
}

// fit turns ranked candidates into items under the budget.
//
// Order of operations is the compression strategy: every item gets its summary
// first, and only then is the source envelope divided among them. Source is the
// most expensive thing in a pack and the easiest for an agent to fetch itself,
// so it is added last and cut first.
//
// The division is by weight, not by count — see budget.go. The item caps run
// first so that no part of the envelope is granted to an item the caps then
// remove; the CLIFF has already run, before those caps, for the opposite reason.
func (b *builder) fit(ctx context.Context, ranked []*candidate, excluded *[]Exclusion) ([]Item, []string) {
	var notes []string
	files := map[string]bool{}
	dropped := 0

	chosen := make([]*candidate, 0, len(ranked))
	for _, c := range ranked {
		switch {
		case len(chosen) >= b.budget.MaxSymbols:
			dropped++
			b.recordBudgetExclusion(excluded, c, "item budget")
			continue
		case !files[c.path] && len(files) >= b.budget.MaxFiles:
			dropped++
			b.recordBudgetExclusion(excluded, c, "file budget")
			continue
		}
		files[c.path] = true
		chosen = append(chosen, c)
	}

	envelope := bodyEnvelope(b.budget, b.tuning.Thresholds)
	for i, g := range allocateBodies(chosen, b.tuning.Thresholds, envelope) {
		chosen[i].bodyChars = g
	}

	items := make([]Item, 0, len(chosen))
	for _, c := range chosen {
		items = append(items, Item{
			Kind:       c.kind,
			Name:       c.name,
			FQN:        c.fqn,
			Path:       c.path,
			Line:       c.line,
			Relevance:  round2(c.score),
			Why:        b.why(c),
			Confidence: string(c.relates),
		})
	}

	// items and chosen are parallel, which is also why the excerpt stage no
	// longer needs a path+name map to find a candidate again — one fewer place
	// for two items from the same file to collide.
	b.addExcerpts(ctx, items, chosen)
	notes = append(notes, b.allocationNote(items, chosen, envelope)...)

	if dropped > 0 {
		notes = append(notes, fmt.Sprintf(
			"Truncated: %s did not fit the budget (at most %d items, %d files, ~%d tokens). "+
				"At most %d of them appear in the excluded list below; narrow the task or "+
				"raise the budget to see the rest.",
			plural(dropped, "further item"), b.budget.MaxSymbols, b.budget.MaxFiles,
			b.budget.MaxTokensEstimate, b.tuning.MaxExclusions))
	}
	return items, notes
}

// enforceTokenBudget is the last bound applied, because it is the only one that
// depends on the assembled text.
//
// Excerpts go first and whole items only afterwards. That order IS the
// compression policy: a source range costs twenty lines and tells an agent
// something it can fetch itself in one call, while the item's summary costs one
// line and tells it where to look. Dropping the summary to keep the source
// would be the wrong trade every time.
//
// One item always survives. "Nothing fits" is a worse answer than the single
// most relevant thing plus a note saying the rest was cut.
func enforceTokenBudget(p *Pack, b Budget) []string {
	var droppedExcerpts, droppedItems int

	for p.ContentTokens() > b.MaxTokensEstimate {
		if i := lastWithExcerpt(p.Items); i >= 0 {
			p.Items[i].Excerpt = ""
			droppedExcerpts++
			continue
		}
		if len(p.Items) <= 1 {
			break
		}
		last := p.Items[len(p.Items)-1]
		p.Items = p.Items[:len(p.Items)-1]
		droppedItems++
		p.Excluded = append(p.Excluded, Exclusion{
			Name: last.Name, Path: last.Path,
			Reason: fmt.Sprintf("relevant (%.2f) but dropped to stay inside the ~%d-token budget",
				last.Relevance, b.MaxTokensEstimate),
		})
	}

	if droppedExcerpts == 0 && droppedItems == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"Truncated to the ~%d-token content budget: %s and %s removed. "+
			"Source was dropped before summaries; fetch it with the paths and lines above.",
		b.MaxTokensEstimate, plural(droppedExcerpts, "source excerpt"), plural(droppedItems, "item"))}
}

func lastWithExcerpt(items []Item) int {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Excerpt != "" {
			return i
		}
	}
	return -1
}

// allocationNote states how the source envelope was divided, and is emitted
// only when the division actually withheld something.
//
// The per-item reason is on the item itself (see why); this note exists so the
// reader learns the RULE once rather than inferring it from several items that
// each explain their own case.
func (b *builder) allocationNote(items []Item, chosen []*candidate, envelope int) []string {
	withheld, carrying := 0, 0
	for i, c := range chosen {
		if items[i].Excerpt != "" {
			carrying++
		}
		if c.withheld != "" {
			withheld++
		}
	}
	if withheld == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"Source was allocated by relevance, not evenly: %s of %s carry an excerpt inside a "+
			"~%d-character source budget, and %s named without it. Each of those says why in "+
			"its own reason; the paths and lines above fetch the rest.",
		plural(carrying, "item"), plural(len(items), "item"), envelope,
		plural(withheld, "item"))}
}

func (b *builder) recordBudgetExclusion(excluded *[]Exclusion, c *candidate, which string) {
	*excluded = append(*excluded, Exclusion{
		Name: c.name, Path: c.path,
		Reason: fmt.Sprintf("relevant (%.2f, %s) but dropped by the %s", c.score, c.via, which),
	})
}

// addExcerpts attaches source to each item, up to the characters its relevance
// won it. items and chosen are parallel.
//
// An item with a grant of zero gets nothing: either the cliff withheld it, or
// the envelope did not reach it, and in both cases the candidate carries the
// reason for why to state.
func (b *builder) addExcerpts(ctx context.Context, items []Item, chosen []*candidate) {
	remaining := b.budget.MaxLines
	for i := range items {
		if remaining <= 0 {
			return
		}
		c := chosen[i]
		if c.bodyChars <= 0 || !c.canShowSource() {
			continue
		}
		fileID := b.cat.symbols[c.unitID].FileID
		text, ok := b.content[fileID]
		if !ok {
			var err error
			if text, err = b.reg.FileExcerpt(ctx, b.runID, c.path, 1, maxExcerptWindow); err != nil {
				continue
			}
			b.content[fileID] = text
		}

		start, lines := b.excerptRange(c)
		if lines > remaining {
			lines = remaining
		}
		if lines <= 0 {
			continue
		}
		excerpt := clipToChars(sliceLines(text, start, lines), c.bodyChars)
		if excerpt == "" {
			continue
		}
		items[i].Excerpt = excerpt
		remaining -= itemLines(items[i])
	}
}

// clipToChars trims an excerpt to the characters its item was granted.
//
// Whole lines, because half a line of Java is not source an agent can read. When
// not even the first line fits, that one line is cut at a rune boundary and
// marked with an ellipsis — a clipped line is still evidence of what is there,
// and the mark keeps it from being mistaken for the real text.
func clipToChars(excerpt string, limit int) string {
	if limit <= 0 || len(excerpt) <= limit {
		return excerpt
	}
	lines := strings.Split(excerpt, "\n")
	total, kept := 0, 0
	for _, ln := range lines {
		cost := len(ln)
		if kept > 0 {
			cost++ // the newline that joins it
		}
		if total+cost > limit {
			break
		}
		total += cost
		kept++
	}
	if kept > 0 {
		return strings.Join(lines[:kept], "\n")
	}

	first := lines[0]
	cut := limit - 1
	for cut > 0 && !utf8.RuneStart(first[cut]) {
		cut--
	}
	if cut <= 0 {
		return ""
	}
	return first[:cut] + "…"
}

// excerptRange picks WHERE the window starts: at the member that carried the
// signal, not at the top of the file. An agent already has the declaration line
// from Path and Line; what it does not have is the body that has to change.
//
// How much of that window ships is the allocator's decision, not this
// function's, so the window runs from the key member to the end of the unit it
// sits in and the char grant cuts it back. Returning the key member's own span
// instead — which is what this did — capped the strongest item in the golden
// pack at 29 characters, because the member that carried its signal was a
// one-line route annotation. It had won 764 characters of budget and could not
// spend them: allocation by relevance means nothing if the window cannot grow to
// hold what relevance bought.
func (b *builder) excerptRange(c *candidate) (start, lines int) {
	target := c.unitID
	if key, ok := b.keyMember[c.unitID]; ok && key > 0 {
		target = key
	}
	sym, ok := b.cat.symbols[target]
	if !ok {
		return c.line, b.tuning.ExcerptLines
	}
	start = sym.Range.Start.Line
	end := sym.Range.End.Line
	if unit, ok := b.cat.symbols[c.unitID]; ok && unit.Range.End.Line > end {
		end = unit.Range.End.Line
	}
	span := end - start + 1
	if span < 1 || span > b.tuning.ExcerptLines {
		span = b.tuning.ExcerptLines
	}
	return start, span
}

func sliceLines(text string, start, count int) string {
	all := strings.Split(text, "\n")
	if start < 1 {
		start = 1
	}
	if start > len(all) {
		return ""
	}
	end := start - 1 + count
	if end > len(all) {
		end = len(all)
	}
	return strings.Join(all[start-1:end], "\n")
}

// why builds the item's justification. It is never empty: the admission branch
// always contributes a clause, and the fallback covers the impossible case
// rather than letting a silent regression ship an unexplained item.
func (b *builder) why(c *candidate) string {
	var parts []string

	switch c.via {
	case admitNamed:
		parts = append(parts, "named by the task (matches "+quoteList(c.nameTerms)+")")
	case admitGraph:
		parts = append(parts, fmt.Sprintf("%s from what the task names, through the call graph (path confidence %s)",
			plural(c.depth, "hop"), c.pathConf))
	case admitTest:
		parts = append(parts, "regression coverage for "+c.testFor+
			", matched by test naming convention — no call edge between them was resolved")
	case admitData:
		parts = append(parts, "persistence touched by "+c.tableOwner)
	case admitLexical:
		parts = append(parts, "matches "+quoteList(c.lexTerms)+" textually")
	}

	if c.via == admitNamed && c.depth > 0 {
		parts = append(parts, fmt.Sprintf("%s from another task symbol", plural(c.depth, "hop")))
	}
	if names := b.relevantMembers(c); len(names) > 0 && c.via != admitData {
		parts = append(parts, "relevant members: "+strings.Join(names, ", "))
	}
	if len(c.tables) > 0 && c.via != admitData {
		parts = append(parts, tableList(c.tables))
	}
	if c.via != admitLexical && len(c.lexTerms) > 0 {
		parts = append(parts, "text also matches "+quoteList(c.lexTerms))
	}
	// A body the allocator withheld is reported on the item that lost it. The
	// item is still NAMED — an agent can open the file itself — and "we chose not
	// to spend your budget here, for this reason" is a different product from an
	// item that silently arrived without source.
	if c.withheld != "" {
		parts = append(parts, c.withheld)
	}
	if len(parts) == 0 {
		return "included by relevance to the task"
	}
	return strings.Join(parts, "; ")
}

// relevantMembers names the members worth mentioning, nearest to the task
// first. Ordering by proximity rather than alphabetically is what puts
// updateStatus in front of a constructor that merely shares the class.
func (b *builder) relevantMembers(c *candidate) []string {
	const shown = 3

	notes := append([]memberNote(nil), b.members[c.unitID]...)
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].depth != notes[j].depth {
			return notes[i].depth < notes[j].depth
		}
		return notes[i].name < notes[j].name
	})

	out := make([]string, 0, shown+1)
	for i, n := range notes {
		if i == shown {
			out = append(out, "+"+strconv.Itoa(len(notes)-shown)+" more")
			break
		}
		out = append(out, n.name)
	}
	return out
}

func tableList(ts []tableRef) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		verb := "reads"
		if t.Write {
			verb = "writes"
		}
		out = append(out, verb+" "+t.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func seedNames(cat *catalogue, seeds []seed) []string {
	var out []string
	for _, s := range seeds {
		if n := cat.symbols[s.symbolID].Name; !contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// interpret states RepoCortex's reading of the task back to the caller.
//
// Almost every bad pack comes from a misread task rather than from bad ranking,
// so this line exists to make that diagnosable in one glance instead of one
// debugging session.
func interpret(in Intent, seeds []string, reached int) string {
	var b strings.Builder
	b.WriteString("Concepts: " + strings.Join(termTexts(in), ", "))
	if len(in.Verbs) > 0 {
		b.WriteString("; action: " + strings.Join(in.Verbs, ", "))
	}
	if len(seeds) == 0 {
		b.WriteString(". No symbol in this index is named after any of them")
		return b.String()
	}
	shown := seeds
	if len(shown) > 5 {
		shown = append(shown[:5:5], "…")
	}
	fmt.Fprintf(&b, ". Anchored on %s named by the task (%s); expanded to %s in the call graph",
		plural(len(seeds), "symbol"), strings.Join(shown, ", "),
		plural(reached, "reachable symbol"))
	return b.String()
}

func termTexts(in Intent) []string {
	out := make([]string, 0, len(in.Terms))
	for _, t := range in.Terms {
		out = append(out, t.Text)
	}
	return out
}

// round2 keeps relevance to two decimals: it is an ordering device, and
// printing sixteen digits of float noise invites callers to read meaning into
// differences that are not there.
func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
