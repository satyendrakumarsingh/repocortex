package registry

import (
	"container/list"
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// candidateChunk is how many names go into one IN (...) lookup.
//
// SQLite's default parameter limit is 32,766 and a file rarely names more than
// a few hundred distinct symbols, so this only binds on generated code. Kept
// well under the limit because the cost of an extra round trip is trivial next
// to the cost of a failed statement.
const candidateChunk = 512

// DefaultCandidateBudget bounds how many candidate rows a CandidateCache holds.
//
// This is the constant that makes resolution's memory flat in repository size.
// The previous design loaded EVERY symbol in the run into a map before
// resolving anything, so peak memory tracked the repository: a 25,000-file tree
// held 725,000 entries, and resolution's RSS grew 6.96x between 1,000 and
// 25,000 files.
//
// The budget is a trade. Set low (20,000 rows, 24 MB of heap on a 25,000-file
// tree) it cost nothing on a repository whose names are rare. On a Java tree
// the same budget thrashed: 232,000 of 509,000 lookups on the 8,800-file
// server module of Elasticsearch missed, because the names a page of files
// asks for (`get`, `of`, `toString`, each with up to 1,000 definitions)
// overflow it before the page is resolved, so every file fetched its names
// again. At 500,000 rows (about 90 MB) that module's whole working set of
// 119,000 rows stays resident, lookups that reach the database fell from
// 7,791 to 17, and memory stayed flat in repository size because eviction
// still bounds it.
const DefaultCandidateBudget = 500_000

// DefaultMaxInternPaths bounds how many distinct file paths the interning map holds
// before being cleared to prevent unbounded memory growth in long-running sessions.
const DefaultMaxInternPaths = 5_000

// DefaultMaxInternKinds bounds how many distinct symbol kinds the interning map holds.
const DefaultMaxInternKinds = 500

// DefaultPerNameLimit caps rows read for a single name when the caller does not
// state its own ceiling. Deliberately generous: a caller that considers more
// candidates than this would silently get fewer, so the default is only a
// backstop for callers that have no ceiling of their own.
const DefaultPerNameLimit = 1_000

// CandidateCache answers "what could this name refer to" for one index run,
// reading from the database on demand instead of preloading the run.
//
// It exists because the resolver only ever asks about names that appear in the
// file it is currently resolving. Loading the whole run to answer questions
// about a few hundred names is what made memory scale with the repository, and
// on a 5,000-file tree 69% of the symbols loaded had a name that no reference
// in the run ever mentioned.
//
// A cache is not an optimisation here, it is what keeps the design viable: a
// name shared by thousands of symbols would otherwise be re-read once per file
// that mentions it. Eviction is LRU and bounded by candidate ROWS rather than
// by name count, because one name can own thousands of rows and a per-name
// bound would not bound memory at all.
//
// Not safe for concurrent use. Resolution is a single pass over files, and
// giving this a mutex would suggest otherwise.
type CandidateCache struct {
	reg      *Registry
	runID    int64
	budget   int
	perName  int
	maxPaths int
	maxKinds int

	// Interning pools. A file path is shared by every symbol in that file and
	// a kind by most of the run, so holding one copy of each keeps a cached
	// entry to its scalars. Bounded by DefaultMaxInternPaths / DefaultMaxInternKinds
	// to prevent unbounded growth in long-running sessions.
	paths map[string]string
	kinds map[string]string

	entries map[string]*list.Element
	lru     *list.List // front = most recently used
	rows    int

	// files is the run's file list, read once on first use by the guided
	// lookups. See guided.go.
	files []FileRec

	// spellings holds, per language, each lower-cased name with the distinct
	// spellings the run defines it under. Read once on first use. See Spellings.
	spellings map[string]map[string][]string

	// directives holds, per language, each file's export and part rows. Read
	// once on first use. See Directives.
	directives map[string]map[string]FileDirectives

	// importGraphs holds, per language, each file's import paths. Read once on
	// first use. See ImportGraph.
	importGraphs map[string]map[string][]string

	hits, misses, evictions int
}

// cacheEntry is one name's candidates, held so the LRU list can find its map
// key on eviction.
type cacheEntry struct {
	name  string
	cands []SymbolIndexEntry
}

// NewCandidateCache returns a lookup over runID holding at most budget
// candidate rows, reading at most perName rows for any single name.
//
// perName is what makes the working set independent of repository size. The
// caller passes its own ceiling plus one — the resolver abandons a name that
// has more candidates than it will consider, and it decides that from the COUNT
// alone, so one row past the ceiling is all the information the extra rows
// carry. Without it a name shared by 5,000 symbols reads 5,000 rows to
// establish a fact that 9 rows establish, and that bucket grows with the
// repository, which is the growth this type exists to remove.
//
// A budget or perName of zero or less uses the defaults.
func NewCandidateCache(reg *Registry, runID int64, budget, perName int) *CandidateCache {
	if budget <= 0 {
		budget = DefaultCandidateBudget
	}
	if perName <= 0 {
		perName = DefaultPerNameLimit
	}
	return &CandidateCache{
		reg:      reg,
		runID:    runID,
		budget:   budget,
		perName:  perName,
		maxPaths: DefaultMaxInternPaths,
		maxKinds: DefaultMaxInternKinds,
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
		paths:    make(map[string]string, 1024),
		kinds:    make(map[string]string, 32),
	}
}

// internPath returns the pooled copy of path, adding it if absent. If the pool
// reaches maxPaths (or DefaultMaxInternPaths), it is reset so long-running sessions
// do not leak memory.
func (c *CandidateCache) internPath(s string) string {
	if got, ok := c.paths[s]; ok {
		return got
	}
	limit := c.maxPaths
	if limit <= 0 {
		limit = DefaultMaxInternPaths
	}
	if len(c.paths) >= limit {
		c.paths = make(map[string]string, min(limit, 1024))
	}
	c.paths[s] = s
	return s
}

// internKind returns the pooled copy of kind, adding it if absent. If the pool
// reaches maxKinds (or DefaultMaxInternKinds), it is reset so long-running sessions
// do not leak memory.
func (c *CandidateCache) internKind(s string) string {
	if got, ok := c.kinds[s]; ok {
		return got
	}
	limit := c.maxKinds
	if limit <= 0 {
		limit = DefaultMaxInternKinds
	}
	if len(c.kinds) >= limit {
		c.kinds = make(map[string]string, min(limit, 32))
	}
	c.kinds[s] = s
	return s
}

// Candidates returns the symbols each name could refer to.
//
// Names absent from the result have no candidates; the caller must treat a
// missing key and an empty slice identically, exactly as it did when the whole
// run was one map.
//
// The returned slices are owned by the cache and must not be modified.
func (c *CandidateCache) Candidates(ctx context.Context, names []string) (map[string][]SymbolIndexEntry, error) {
	out := make(map[string][]SymbolIndexEntry, len(names))

	var missing []string
	for _, n := range names {
		if el, ok := c.entries[n]; ok {
			c.lru.MoveToFront(el)
			c.hits++
			if cands := el.Value.(*cacheEntry).cands; len(cands) > 0 {
				out[n] = cands
			}
			continue
		}
		c.misses++
		missing = append(missing, n)
	}
	if len(missing) == 0 {
		return out, nil
	}

	fetched, err := c.fetch(ctx, missing)
	if err != nil {
		return nil, err
	}
	// Every miss is admitted, including the ones that found nothing: "this name
	// resolves to nothing" is an answer worth caching, and on a repository that
	// leans on third-party code it is the most common one.
	for _, n := range missing {
		cands := fetched[n]
		c.admit(n, cands)
		if len(cands) > 0 {
			out[n] = cands
		}
	}
	return out, nil
}

// admit inserts one name's candidates and evicts until the budget holds.
func (c *CandidateCache) admit(name string, cands []SymbolIndexEntry) {
	el := c.lru.PushFront(&cacheEntry{name: name, cands: cands})
	c.entries[name] = el
	c.rows += len(cands)

	// A single name whose candidates exceed the whole budget is kept anyway:
	// evicting it would leave the cache empty and the next lookup would fetch
	// it again immediately. One oversized bucket is bounded by the repository's
	// worst name collision, not by repository size, so it does not reintroduce
	// the growth this cache exists to remove.
	for c.rows > c.budget && c.lru.Len() > 1 {
		back := c.lru.Back()
		if back == nil {
			break
		}
		ent := c.lru.Remove(back).(*cacheEntry)
		delete(c.entries, ent.name)
		c.rows -= len(ent.cands)
		c.evictions++
	}
}

// fetch reads candidates for names straight from the run.
func (c *CandidateCache) fetch(ctx context.Context, names []string) (map[string][]SymbolIndexEntry, error) {
	out := make(map[string][]SymbolIndexEntry, len(names))

	for start := 0; start < len(names); start += candidateChunk {
		end := min(start+candidateChunk, len(names))
		chunk := names[start:end]

		args := make([]any, 0, len(chunk)+1)
		args = append(args, c.runID)
		for _, n := range chunk {
			args = append(args, n)
		}

		// ROW_NUMBER caps each name's rows inside SQLite rather than reading
		// them all and discarding the tail in Go. ORDER BY s.id inside the
		// partition reproduces the order the preloaded index happened to have:
		// that index came from a table scan with no ORDER BY, so its buckets
		// arrived in rowid order, and the resolver's single-candidate tiers
		// read cands[0] directly. Leaving the order to the planner would make
		// resolution's output depend on which index it chose.
		q := `SELECT id, name, fqn, kind, file_id, path, exported, param_count, language, return_type, flags FROM (
		        SELECT s.id AS id, s.name AS name, s.fqn AS fqn, s.kind AS kind,
		               s.file_id AS file_id, f.path AS path, s.exported AS exported,
		               s.param_count AS param_count, f.language AS language,
		               s.return_type AS return_type, s.flags AS flags,
		               ROW_NUMBER() OVER (PARTITION BY s.name ORDER BY s.id) AS rn
		          FROM symbols s
		          JOIN files f ON f.id = s.file_id
		         WHERE s.index_run_id = ? AND s.name IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `))
		       WHERE rn <= ?
		       ORDER BY name, id`
		args = append(args, c.perName)

		rows, err := c.reg.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("registry: load candidates: %w", err)
		}
		for rows.Next() {
			var e SymbolIndexEntry
			var exported, paramCount, flags int
			if err := rows.Scan(&e.ID, &e.Name, &e.FQN, &e.Kind, &e.FileID, &e.FilePath,
				&exported, &paramCount, &e.Language, &e.ReturnType, &flags); err != nil {
				_ = rows.Close()
				return nil, err
			}
			e.Exported = exported == 1
			e.setFlags(flags)
			e.ParamCount = model.Arity(paramCount)
			e.FilePath = c.internPath(e.FilePath)
			e.Kind = c.internKind(e.Kind)
			e.Language = c.internKind(e.Language)
			e.ReturnType = c.internKind(e.ReturnType)
			// The name is this bucket's key; reuse that copy rather than
			// keeping one more per candidate.
			if bucket, ok := out[e.Name]; ok {
				e.Name = bucket[0].Name
				out[e.Name] = append(bucket, e)
				continue
			}
			out[e.Name] = []SymbolIndexEntry{e}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// maxSpellings bounds the names one language's spelling table holds. A
// language whose names are case-insensitive is a scripting language, and a
// repository of one has thousands of definitions, not millions; past the bound
// the table is cut at the names that sort first and the rest keep exact-case
// lookup, which is what they had.
const maxSpellings = 200_000

// Spellings returns the names a language's definitions are written under,
// grouped by lower-cased name and each group sorted, for the languages that
// do not tell `Write-Log` from `write-log`. It is read on first use and kept
// for the life of the cache: a run's symbols do not change under a resolver,
// and asking per name would scan the run for every command a script calls,
// most of which (the shell's own) the repository does not define at all.
func (c *CandidateCache) Spellings(ctx context.Context, language string) (map[string][]string, error) {
	if got, ok := c.spellings[language]; ok {
		return got, nil
	}
	rows, err := c.reg.db.QueryContext(ctx,
		`SELECT DISTINCT s.name FROM symbols s JOIN files f ON f.id = s.file_id
		  WHERE s.index_run_id = ? AND f.language = ? ORDER BY s.name LIMIT ?`,
		c.runID, language, maxSpellings)
	if err != nil {
		return nil, fmt.Errorf("registry: load spellings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		out[key] = append(out[key], name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if c.spellings == nil {
		c.spellings = map[string]map[string][]string{}
	}
	c.spellings[language] = out
	return out, nil
}

// CandidateStats reports cache behaviour. For tests and diagnostics; resolution
// does not read it.
type CandidateStats struct {
	Hits, Misses, Evictions, Rows int
	Paths, Kinds                  int
}

// Stats returns a snapshot of cache behaviour.
func (c *CandidateCache) Stats() CandidateStats {
	return CandidateStats{
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evictions,
		Rows:      c.rows,
		Paths:     len(c.paths),
		Kinds:     len(c.kinds),
	}
}
