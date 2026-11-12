package registry

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Binding staged table accesses to table symbols.
//
// This is the second half of the pattern migration 0003 describes: extraction
// stages an access naming a table by string, and this pass turns it into an
// edge once the whole repository has been seen and the table's node — if there
// is one — exists.
//
// The interesting case is the one that does NOT bind. A query against a table
// no migration in this repository declares is not an error and not a gap in the
// analysis: it is a service reading a database somebody else owns, which is the
// normal shape of a microservice estate. Those accesses stay recorded and are
// reported as unbound, because "this service writes a table we do not own" is a
// question worth answering and dropping the row would make it unanswerable.

// AccessBindStats reports one binding pass.
type AccessBindStats struct {
	// Total is every staged access considered.
	Total int64
	// Bound produced an edge to a table declared in this repository.
	Bound int64
	// Unbound named a table nothing here declares — usually a table owned by
	// another service.
	Unbound int64
	// Edges is how many edges were written. Lower than Bound whenever several
	// statements in one function touch the same table the same way, which is
	// ordinary in a repository method.
	Edges int64
	// UnboundTables is a bounded, sorted sample of the table names that did not
	// bind, so the number above can be acted on.
	UnboundTables []string
}

// maxUnboundSamples bounds the retained sample.
const maxUnboundSamples = 25

// accessEdgeKind maps an access mode to the edge it justifies.
//
// READ_WRITE becomes two edges rather than one blended kind. The graph's whole
// value here is that "what writes this table" is answerable separately from
// "what reads it", and a combined kind would force every consumer to
// re-interpret it.
func accessEdgeKinds(access string) []model.EdgeKind {
	switch access {
	case "READ":
		return []model.EdgeKind{model.EdgeReads}
	case "WRITE":
		return []model.EdgeKind{model.EdgeWrites}
	case "READ_WRITE":
		return []model.EdgeKind{model.EdgeReads, model.EdgeWrites}
	case "DDL":
		// A migration DEFINES the table rather than using it. Recording it as a
		// write would put "the schema migration" in the answer to "what writes
		// this table at run time", which is not what the asker meant.
		return []model.EdgeKind{model.EdgeDependsOn}
	default:
		// UNKNOWN: the statement was recognised as SQL but not classified. It
		// touches the table somehow, and DEPENDS_ON is the honest claim.
		return []model.EdgeKind{model.EdgeDependsOn}
	}
}

// BindTableAccesses resolves staged accesses into edges.
//
// Idempotent: re-running it after new files are indexed rebinds everything and
// writes only edges that are missing, so it is safe to call from both a full
// index and an incremental update.
func (r *Registry) BindTableAccesses(ctx context.Context, runID int64) (AccessBindStats, error) {
	var st AccessBindStats

	tables, err := r.tableSymbols(ctx, runID)
	if err != nil {
		return st, err
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT id, table_name, access, verb, COALESCE(src_symbol_id, 0),
		       start_line, certain, file_id
		  FROM table_accesses
		 WHERE index_run_id = ?`, runID)
	if err != nil {
		return st, fmt.Errorf("registry: read table accesses: %w", err)
	}
	defer func() { _ = rows.Close() }()

	filePaths, err := r.filePaths(ctx, runID)
	if err != nil {
		return st, err
	}

	var (
		bindings []accessBinding
		edges    []model.Edge
		unbound  = map[string]struct{}{}
	)

	for rows.Next() {
		var (
			id, srcID, fileID int64
			table, access     string
			verb              string
			line, certain     int
		)
		if err := rows.Scan(&id, &table, &access, &verb, &srcID, &line, &certain, &fileID); err != nil {
			return st, err
		}
		st.Total++

		tableID, ok := tables[strings.ToLower(table)]
		if !ok {
			st.Unbound++
			if len(unbound) < maxUnboundSamples {
				unbound[strings.ToLower(table)] = struct{}{}
			}
			bindings = append(bindings, accessBinding{id: id, boundTo: 0})
			continue
		}
		st.Bound++
		bindings = append(bindings, accessBinding{id: id, boundTo: tableID})

		if srcID == 0 {
			// The statement sits outside every definition — a migration's top
			// level. The table node exists and is bound; there is simply no
			// source symbol for an edge to start from.
			continue
		}

		conf := model.ConfidenceHigh
		detail := verb + " (literal SQL)"
		if certain == 0 {
			// A run-time fragment. The table is real and the verb is real; what
			// is uncertain is whether this exact statement ever executes as
			// written, so the claim is weakened rather than dropped.
			conf = model.ConfidenceLow
			detail = verb + " (dynamically assembled SQL)"
		}

		for _, kind := range accessEdgeKinds(access) {
			edges = append(edges, model.Edge{
				SrcID: srcID, DstID: tableID, Kind: kind, Confidence: conf,
				Evidence: model.Evidence{
					FilePath: filePaths[fileID],
					Range:    model.Range{Start: model.Position{Line: line}},
					Source:   model.SourceSourceCode,
					Detail:   detail,
				},
			})
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	written, err := r.AddResolvedEdges(ctx, runID, edges)
	if err != nil {
		return st, err
	}
	// What LANDED, not what was built. Several statements in one method against
	// one table produce identical edges, and INSERT OR IGNORE drops the
	// repeats — reporting len(edges) counted them anyway.
	st.Edges = int64(written)

	if err := r.markBindings(ctx, bindings); err != nil {
		return st, err
	}

	for t := range unbound {
		st.UnboundTables = append(st.UnboundTables, t)
	}
	sortStrings(st.UnboundTables)
	return st, nil
}

// tableSymbols indexes the run's declared tables by name.
func (r *Registry) tableSymbols(ctx context.Context, runID int64) (map[string]int64, error) {
	// ORDER BY is load-bearing, not tidiness. "First declaration wins" needs a
	// definition of first, and an unordered SELECT does not supply one: row order
	// follows the query plan over ids the PARALLEL indexing pass assigned, so
	// which file won varied between runs of the same tree.
	//
	// Measured on this repository: `table_accesses` is created in
	// 0003_data_access.sql and named again by a CREATE INDEX in
	// 0005_fk_indexes.sql, so two symbols carry the name. Runs disagreed about
	// which one every READS and WRITES edge pointed at — 40+ edges moving between
	// two otherwise identical runs, and two table nodes alternately orphaned.
	// That is a breach of the determinism guarantee the whole product rests on:
	// a re-index produced a different graph from the same bytes.
	//
	// Ordering by path then line makes "first" mean first in the migration
	// sequence, which is where the CREATE TABLE lives — so the canonical node is
	// also the correct one. `id` is the final tiebreak so the result is total
	// even for two declarations at one location.
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = ?
		 ORDER BY f.path, s.start_line, s.id`,
		runID, string(model.KindTable))
	if err != nil {
		return nil, fmt.Errorf("registry: read table symbols: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		// First declaration wins. A table created in one migration and altered
		// in three later ones must be ONE node, or every question about it
		// splits four ways.
		if _, ok := out[strings.ToLower(name)]; !ok {
			out[strings.ToLower(name)] = id
		}
	}
	return out, rows.Err()
}

func (r *Registry) filePaths(ctx context.Context, runID int64) (map[int64]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, path FROM files WHERE index_run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: read file paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		out[id] = path
	}
	return out, rows.Err()
}

// accessBinding is one staged access and the table it resolved to (0 = none).
type accessBinding struct {
	id      int64
	boundTo int64
}

func (r *Registry) markBindings(ctx context.Context, bindings []accessBinding) error {
	if len(bindings) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: mark bindings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE table_accesses SET bound_symbol_id = ?, binding = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("registry: mark bindings: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, b := range bindings {
		var target any
		status := "unbound"
		if b.boundTo > 0 {
			target = b.boundTo
			status = "bound"
		}
		if _, err := stmt.ExecContext(ctx, target, status, b.id); err != nil {
			return fmt.Errorf("registry: mark binding %d: %w", b.id, err)
		}
	}
	return tx.Commit()
}

// TableUsage is one table and how the repository uses it.
type TableUsage struct {
	SymbolID int64
	Table    string
	// Declared is false for a table only ever named by a query — one this
	// repository uses but does not own.
	Declared  bool
	Readers   int
	Writers   int
	Accesses  int
	Uncertain int
}

// TableUsageReport summarises every table the run knows about.
//
// Reads the staged accesses rather than the edges, deliberately: an access that
// bound to no table has no edge, and those are exactly the rows that answer
// "which databases does this service touch that it does not own".
func (r *Registry) TableUsageReport(ctx context.Context, runID int64) ([]TableUsage, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.table_name,
		       COALESCE(a.bound_symbol_id, 0),
		       SUM(CASE WHEN a.access IN ('READ','READ_WRITE') THEN 1 ELSE 0 END),
		       SUM(CASE WHEN a.access IN ('WRITE','READ_WRITE') THEN 1 ELSE 0 END),
		       COUNT(*),
		       SUM(CASE WHEN a.certain = 0 THEN 1 ELSE 0 END)
		  FROM table_accesses a
		 WHERE a.index_run_id = ?
		 GROUP BY a.table_name, a.bound_symbol_id
		 ORDER BY COUNT(*) DESC, a.table_name`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: table usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TableUsage
	for rows.Next() {
		var u TableUsage
		var boundID sql.NullInt64
		if err := rows.Scan(&u.Table, &boundID, &u.Readers, &u.Writers, &u.Accesses, &u.Uncertain); err != nil {
			return nil, err
		}
		if boundID.Valid && boundID.Int64 > 0 {
			u.SymbolID = boundID.Int64
			u.Declared = true
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Endpoints lists the run's HTTP endpoints with the handler each exposes.
type Endpoint struct {
	SymbolID  int64
	Signature string
	FilePath  string
	Line      int
	// Handler is empty when the framework's syntax did not reveal one. The
	// endpoint is still real; only its target is unknown.
	Handler     string
	HandlerID   int64
	HandlerPath string
	Detail      string
}

// EndpointReport lists every endpoint and its handler.
func (r *Registry) EndpointReport(ctx context.Context, runID int64) ([]Endpoint, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.name, ef.path, e.start_line,
		       COALESCE(h.name, ''), COALESCE(h.id, 0), COALESCE(hf.path, ''),
		       COALESCE(edge.detail, '')
		  FROM symbols e
		  JOIN files   ef   ON ef.id = e.file_id
		  LEFT JOIN edges   edge ON edge.src_id = e.id AND edge.kind = ?
		  LEFT JOIN symbols h    ON h.id = edge.dst_id
		  LEFT JOIN files   hf   ON hf.id = h.file_id
		 WHERE e.index_run_id = ? AND e.kind = ?
		 ORDER BY ef.path, e.start_line`,
		string(model.EdgeExposes), runID, string(model.KindEndpoint))
	if err != nil {
		return nil, fmt.Errorf("registry: endpoint report: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.SymbolID, &e.Signature, &e.FilePath, &e.Line,
			&e.Handler, &e.HandlerID, &e.HandlerPath, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ReplaceFileAnalysis writes one file's staged analysis rows.
//
// Needed because the incremental path writes through ReplaceFile rather than
// through a Writer, and Writer refuses a committed run by design. ReplaceFile
// deletes and re-inserts the file row, so ON DELETE CASCADE has already removed
// the previous table_accesses and config_usages — this puts the new ones back.
//
// Without it, re-indexing one file ERASED its analysis facts: a controller lost
// its endpoints and its table writes, `repocortex api` went from one endpoint to
// zero, and nothing reported it, because an empty result and a correct empty
// result are indistinguishable. With `repocortex watch` running, the whole
// analysis layer eroded over a working session.
//
// Insert-only, deliberately: the caller has just replaced the file, so there is
// nothing left to delete, and issuing a DELETE anyway would invite someone to
// call this without replacing first and get a silently different result.
func (r *Registry) ReplaceFileAnalysis(
	ctx context.Context,
	runID, fileID int64,
	accesses []TableAccess,
	usages []ConfigUsage,
) error {
	if len(accesses) == 0 && len(usages) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: replace file analysis: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, a := range accesses {
		var src any
		if a.SrcID > 0 {
			src = a.SrcID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO table_accesses
				(index_run_id, file_id, table_name, access, verb, src_symbol_id, start_line, certain, binding)
			VALUES (?,?,?,?,?,?,?,?, 'pending')`,
			runID, fileID, a.Table, a.Access, a.Verb, src, a.Line, boolToInt(a.Certain),
		); err != nil {
			return fmt.Errorf("registry: re-add table access %q: %w", a.Table, err)
		}
	}
	for _, u := range usages {
		var src any
		if u.SrcID > 0 {
			src = u.SrcID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO config_usages
				(index_run_id, file_id, key_name, mechanism, src_symbol_id, start_line, binding)
			VALUES (?,?,?,?,?,?, 'pending')`,
			runID, fileID, u.Key, u.Mechanism, src, u.Line,
		); err != nil {
			return fmt.Errorf("registry: re-add config usage %q: %w", u.Key, err)
		}
	}
	return tx.Commit()
}
