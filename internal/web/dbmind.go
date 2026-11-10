package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// DBMind: which tables this repository touches, and what reads or writes each.
//
// # Nothing here is a second analysis
//
// The table inventory is registry.TableUsageReport — the exact call behind
// `cortex db`, including its Declared flag and its Uncertain count. The
// accessors are the run's own READS and WRITES edges, which is what
// `cortex db table` reads through graph.Neighbours(Incoming, READS|WRITES).
// Both sources are already loaded: the report is one aggregate query, and the
// edges are the cached structuralIndex the graph screens pay for once per run.
// A SQL scan or a traversal here would be a second place for the two answers to
// drift apart.
//
// # Why readers and writers are never one list
//
// "What touches orders" is a mildly useful question. "What WRITES orders" is
// the one asked before dropping a column, and answering it with reads mixed in
// is worse than not answering — the same reason accessEdgeKinds maps
// READ_WRITE to two edges rather than one blended kind.
//
// # Two different counts, both named
//
// registry.TableUsageReport counts STATEMENTS (rows in table_accesses); the
// edges count SYMBOLS. One function issuing four UPDATEs against one table is
// four statements and one writer, and the two numbers disagreeing is not a
// defect. Each is labelled with the population it counts, in the field name,
// because an unlabelled pair of numbers that disagree is how a reader stops
// trusting both.

// TableAccessor is one symbol that reads or writes a table.
type TableAccessor struct {
	SymbolID int64  `json:"symbol_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Path     string `json:"file_path,omitempty"`

	// Access is READS or WRITES, the edge kind that put this row here, so the
	// claim is legible without reading the field it arrived in.
	Access string `json:"access"`

	// Confidence is the STRONGEST tier among the edges from this symbol to this
	// table for this access. Unlike a blast-radius facet — where a chain is
	// only as good as its weakest hop — this is a single recorded edge, so
	// picking the weakest of several independent statements would understate a
	// proven write because another statement in the same function was a guess.
	Confidence string `json:"confidence"`

	// Statements is how many recorded edges (distinct SQL sites) link this
	// symbol to this table this way.
	Statements int `json:"statements"`

	// Evidence is the source location of the edge that set Confidence.
	Evidence Evidence `json:"evidence"`
}

// TableRow is one table and everything this index knows about it.
type TableRow struct {
	Table string `json:"table"`

	// SymbolID is the table's node, when a migration in this repository
	// declares it. Zero for a table only ever named by a query.
	SymbolID int64 `json:"symbol_id,omitempty"`

	// Declared is false for a table this repository uses but does not own. Kept
	// and reported rather than dropped: "which tables do we touch that we do
	// not own" is the question that becomes unanswerable if it is filtered out.
	Declared bool `json:"declared_here"`

	// DeclaredAt is the migration or schema file that declares it.
	DeclaredAt Evidence `json:"declared_at,omitempty"`

	// The STATEMENT counts, from registry.TableUsageReport — the same figures
	// `cortex db` prints. Whole-index, unaffected by the confidence floor,
	// because a staged access exists whether or not it bound to a table node.
	ReadStatements      int `json:"read_statements"`
	WriteStatements     int `json:"write_statements"`
	TotalStatements     int `json:"total_statements"`
	UncertainStatements int `json:"uncertain_statements"`

	// The SYMBOL counts and rows, from the run's READS / WRITES edges at or
	// above the floor. Different population from the statement counts above —
	// see the file comment.
	SymbolsReading int `json:"symbols_reading"`
	SymbolsWriting int `json:"symbols_writing"`

	ReadBy    []TableAccessor `json:"read_by"`
	WrittenBy []TableAccessor `json:"written_by"`

	// Caveat states what this row does NOT establish, in the cases where an
	// empty accessor list would otherwise read as a finding about the code.
	Caveat string `json:"caveat,omitempty"`
}

// DBCounts is the headline arithmetic.
type DBCounts struct {
	Tables   int `json:"tables"`
	Declared int `json:"declared_here"`
	External int `json:"not_declared_here"`

	WithWriter     int `json:"with_a_recorded_writer"`
	WithReader     int `json:"with_a_recorded_reader"`
	WithNoAccessor int `json:"with_no_recorded_accessor"`

	UncertainStatements int `json:"uncertain_statements"`
}

// DBFilters echoes the bounds actually applied.
type DBFilters struct {
	MinConfidence string `json:"min_confidence"`
	MaxAccessors  int    `json:"max_accessors_per_table"`
}

// DBResponse is the whole DBMind screen.
type DBResponse struct {
	Meta   Meta   `json:"meta"`
	Legend []Tier `json:"confidence_legend"`

	Filters DBFilters `json:"filters"`

	Tables []TableRow `json:"tables"`
	Counts DBCounts   `json:"counts"`

	// Coverage names what a static SQL read cannot see, so an empty screen is
	// never read as "this service touches no database".
	Coverage string `json:"coverage_limit"`

	Help *Guidance  `json:"guidance,omitempty"`
	Next NextAction `json:"next_action"`

	Omissions Omissions `json:"omissions"`
}

// maxAccessorsPerTable bounds one table's reader or writer list.
//
// A table read by four hundred functions is a fact worth stating as a COUNT;
// four hundred rows on one card is not something a reader consumes. The
// remainder is recorded like every other bound in this portal.
const maxAccessorsPerTable = 25

// dbCoverageLimit is the sentence every DBMind answer carries, empty or not.
const dbCoverageLimit = "SQL is recognised in .sql files and in string literals in source. Queries built " +
	"by an ORM's fluent API, or assembled far from where they run, are not visible to a static read — " +
	"that is a limit of this index, not a finding about your code."

func (s *Server) handleDB(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	meta, run, ok, err := s.run(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	floor := confidenceParam(r, "min_confidence", DefaultFloor)
	resp := DBResponse{
		Meta: meta, Legend: Legend(), Tables: []TableRow{},
		Filters:  DBFilters{MinConfidence: string(floor), MaxAccessors: maxAccessorsPerTable},
		Coverage: dbCoverageLimit,
	}
	if !ok {
		resp.Help = meta.Help
		resp.Next = NextAction{
			Label: "Index this repository", Command: "cortex index",
			Detail: "Table access is extracted during indexing; there is nothing committed to read.",
		}
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	usage, err := s.deps.Registry.TableUsageReport(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	idx, err := s.structural(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var om Omissions
	rows, hiddenEdges := idx.tableRows(usage, floor)

	for i := range rows {
		row := &rows[i]
		row.ReadBy = capped(row.ReadBy, maxAccessorsPerTable, &om, ReasonRowBudget,
			fmt.Sprintf("%s: readers beyond the first %d (ordered by statements, then name) are not "+
				"listed. symbols_reading is the full figure.", row.Table, maxAccessorsPerTable),
			"cortex db table "+row.Table)
		row.WrittenBy = capped(row.WrittenBy, maxAccessorsPerTable, &om, ReasonRowBudget,
			fmt.Sprintf("%s: writers beyond the first %d are not listed. symbols_writing is the "+
				"full figure.", row.Table, maxAccessorsPerTable),
			"cortex db table "+row.Table)
		if row.ReadBy == nil {
			row.ReadBy = []TableAccessor{}
		}
		if row.WrittenBy == nil {
			row.WrittenBy = []TableAccessor{}
		}
	}

	resp.Counts = dbCounts(rows)
	resp.Tables = capped(rows, maxRows, &om, ReasonRowBudget,
		fmt.Sprintf("Tables beyond the first %d (ordered by total statements, then name) are not "+
			"listed. counts.tables is the full figure.", maxRows),
		"cortex db")
	if resp.Tables == nil {
		resp.Tables = []TableRow{}
	}

	om.Hide(ReasonBelowConfidenceFloor, hiddenEdges,
		fmt.Sprintf("READS / WRITES relationships between a symbol and a table recorded weaker than %s. "+
			"Each is a symbol this screen does not name as a reader or a writer, so a table showing no "+
			"writer may still be written through one of them.", tierWord(floor)),
		"set the standard of proof to `low`")

	switch {
	case len(rows) == 0:
		resp.Help = &Guidance{
			Title:  "This index recorded no database access",
			Detail: dbCoverageLimit + " An empty screen here means no SQL statement was recognised, not that this repository has no database.",
			Steps: []string{
				"cortex index — table access is extracted during indexing, so an index built before the SQL recogniser landed will show nothing",
				"cortex db — the same report in the terminal, with the coverage note",
				"Check the overview's blind spots: a language whose string literals were never parsed cannot yield a query",
			},
		}
		resp.Next = NextAction{
			Label:   "Confirm this is a repository with no SQL",
			Detail:  "If it queries a database through an ORM's fluent API, this screen is silent by construction rather than by finding.",
			Command: "cortex db",
			Screen:  "overview#blind-spots",
		}
	case resp.Counts.WithNoAccessor == resp.Counts.Tables:
		resp.Next = NextAction{
			Label: "Bind table accesses to their callers",
			Detail: fmt.Sprintf("All %d table(s) are known by name and none has a recorded reader or writer, "+
				"so this screen can say what exists but not who touches it.", resp.Counts.Tables),
			Command: "cortex resolve",
		}
	default:
		resp.Next = NextAction{
			Label: "Check the writers before changing a column",
			Detail: fmt.Sprintf("%d of %d table(s) have a recorded writer at %s or better. A table with none "+
				"is a table whose writers this index did not find — not one nothing writes.",
				resp.Counts.WithWriter, resp.Counts.Tables, tierWord(floor)),
			Screen: "blast",
		}
	}

	resp.Omissions = om
	s.writeJSON(w, http.StatusOK, resp)
}

func dbCounts(rows []TableRow) DBCounts {
	var c DBCounts
	c.Tables = len(rows)
	for _, t := range rows {
		if t.Declared {
			c.Declared++
		} else {
			c.External++
		}
		if t.SymbolsWriting > 0 {
			c.WithWriter++
		}
		if t.SymbolsReading > 0 {
			c.WithReader++
		}
		if t.SymbolsReading == 0 && t.SymbolsWriting == 0 {
			c.WithNoAccessor++
		}
		c.UncertainStatements += t.UncertainStatements
	}
	return c
}

// tableRows merges the statement-level report with the run's READS / WRITES
// edges.
//
// One linear scan over the cached compact edges, the same shape blast.go's
// facets uses and for the same reason: a WRITES edge from a symbol to a table
// IS the fact, and traversing past it would be this layer inventing a
// relationship the index did not record.
//
// The returned int is how many qualifying edges fell below the floor.
func (idx *structuralIndex) tableRows(usage []registry.TableUsage, floor model.Confidence) ([]TableRow, int) {
	// Table nodes, by symbol id and by name. A table declared in this
	// repository has a symbol; one only ever named by a query does not.
	tableByID := map[int64]string{}
	for id, sym := range idx.syms {
		if sym.Kind != model.KindTable {
			continue
		}
		tableByID[id] = strings.ToLower(sym.Name)
	}

	rows := map[string]*TableRow{}
	get := func(name string) *TableRow {
		row, ok := rows[name]
		if !ok {
			row = &TableRow{Table: name}
			rows[name] = row
		}
		return row
	}

	for _, u := range usage {
		row := get(strings.ToLower(u.Table))
		row.Declared = u.Declared
		row.ReadStatements = u.Readers
		row.WriteStatements = u.Writers
		row.TotalStatements = u.Accesses
		row.UncertainStatements = u.Uncertain
		if u.SymbolID > 0 {
			row.SymbolID = u.SymbolID
		}
	}
	// A table node with no staged access still belongs in the inventory: a
	// migration declared it, and "declared and never queried here" is a
	// finding, not a reason to omit the row.
	for id, name := range tableByID {
		row := get(name)
		row.Declared = true
		row.SymbolID = id
	}
	for _, row := range rows {
		if row.SymbolID > 0 {
			row.DeclaredAt = Evidence{FilePath: idx.symbolPath(row.SymbolID)}
		}
	}

	type accKey struct {
		table  string
		symbol int64
		access model.EdgeKind
	}
	acc := map[accKey]*TableAccessor{}
	hidden := 0

	for i := range idx.edges {
		e := idx.edges[i]
		if e.Kind != model.EdgeReads && e.Kind != model.EdgeWrites {
			continue
		}
		table, ok := tableByID[e.Dst]
		if !ok {
			// A READS/WRITES edge whose destination is not a table node is not
			// this screen's claim to make.
			continue
		}
		if !e.Confidence.AtLeast(floor) {
			hidden++
			continue
		}
		k := accKey{table: table, symbol: e.Src, access: e.Kind}
		a, seen := acc[k]
		if !seen {
			sym := idx.syms[e.Src]
			name := sym.Name
			if name == "" {
				name = fmt.Sprintf("symbol#%d", e.Src)
			}
			a = &TableAccessor{
				SymbolID: e.Src, Name: name, Kind: string(sym.Kind),
				Path: idx.symbolPath(e.Src), Access: string(e.Kind),
				Confidence: tierOf(e.Confidence),
				Evidence:   Evidence{FilePath: e.File, Line: int(e.Line)},
			}
			acc[k] = a
		}
		a.Statements++
		// Strongest wins: several independent statements, and the best-proven
		// one is what the reader should be sent to check first.
		if e.Confidence.Rank() > model.Confidence(a.Confidence).Rank() {
			a.Confidence = tierOf(e.Confidence)
			a.Evidence = Evidence{FilePath: e.File, Line: int(e.Line)}
		}
	}

	for k, a := range acc {
		row := get(k.table)
		if k.access == model.EdgeWrites {
			row.WrittenBy = append(row.WrittenBy, *a)
			row.SymbolsWriting++
			continue
		}
		row.ReadBy = append(row.ReadBy, *a)
		row.SymbolsReading++
	}

	out := make([]TableRow, 0, len(rows))
	for _, row := range rows {
		sortAccessors(row.ReadBy)
		sortAccessors(row.WrittenBy)
		row.Caveat = tableCaveat(*row, floor)
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalStatements != out[j].TotalStatements {
			return out[i].TotalStatements > out[j].TotalStatements
		}
		return out[i].Table < out[j].Table
	})
	return out, hidden
}

func sortAccessors(in []TableAccessor) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Statements != in[j].Statements {
			return in[i].Statements > in[j].Statements
		}
		if in[i].Name != in[j].Name {
			return in[i].Name < in[j].Name
		}
		return in[i].SymbolID < in[j].SymbolID
	})
}

// tableCaveat states what a row does not establish.
//
// An empty accessor list is the shape most easily misread as a finding, so the
// two ways it arises are named separately: the table is not declared here (the
// accesses exist but hang off no node), or it is declared and nothing bound to
// it. Neither is "nothing uses this table".
func tableCaveat(row TableRow, floor model.Confidence) string {
	switch {
	case !row.Declared && row.SymbolsReading == 0 && row.SymbolsWriting == 0:
		return fmt.Sprintf("No migration in this repository declares %s, so its %d recorded statement(s) "+
			"are not attached to a table node and no reader or writer can be listed. Most likely a table "+
			"another service owns.", row.Table, row.TotalStatements)
	case row.SymbolsReading == 0 && row.SymbolsWriting == 0:
		return fmt.Sprintf("No symbol reaches %s through a READS or WRITES edge at %s or better. That is "+
			"the absence of a recorded relationship, not evidence that nothing touches this table.",
			row.Table, tierWord(floor))
	case row.SymbolsWriting == 0:
		return fmt.Sprintf("No recorded writer at %s or better. Before concluding %s is read-only, check "+
			"what this view can't see for writes hidden below the standard of proof.", tierWord(floor), row.Table)
	default:
		return ""
	}
}
