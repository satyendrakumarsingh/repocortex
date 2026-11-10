package registry

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Staging outbound HTTP calls, and binding them to the endpoints they consume.
//
// This is the second half of the pattern migration 0007 describes, and the same
// pattern migration 0003 established for table accesses: extraction stages a
// call naming a target by string, and this pass turns it into a CONSUMES edge
// once the whole repository has been seen and the route that answers it — if
// this repository declares one — exists as a node.
//
// # Why binding is conservative to the point of refusing
//
// The failure mode being avoided is not "too few edges". It is an impact report
// that says a component depends on an endpoint it never calls. A caller acting
// on that edits the wrong handler, or ships a breaking change believing nothing
// consumes the route. So the rule throughout is: when the evidence admits more
// than one answer, record that it did and produce nothing. An unbound call is a
// visible gap; a wrong edge is an invisible lie.
//
// # Why the match key is derived and never stored in place of the path
//
// Matching needs `/API/Orders/42?page=2` and `GET /api/orders/{id}` to meet.
// Displaying needs the path the developer actually wrote, because that is the
// string they will grep for. Those are different jobs, so canonicalisation
// happens here, in memory, and the stored path stays as written.

// ClientCall is one outbound HTTP request, staged for binding.
//
// Staged rather than resolved inline for the reason migration 0007 records: the
// route it targets may be declared by a file parsed later in the same run, or,
// on an incremental update, not reparsed at all.
type ClientCall struct {
	// Method is the verb as written, uppercased. Empty means the syntax stated
	// none; it is treated as ANY when matching, never guessed at.
	Method string
	// Path is the literal part of the target, exactly as written. For a dynamic
	// target it is the constant prefix, which may be empty.
	Path string
	// Client names the convention that matched (fetch, axios, go-net-http).
	Client string
	// SrcID is the declaration containing the call, or 0 when the call sits
	// outside every declaration.
	SrcID int64
	Line  int
	// Dynamic marks a target assembled at run time rather than written out.
	Dynamic bool
	// Confidence is the extractor's confidence in the CALL. The bound edge's
	// confidence descends from it and is never stronger.
	Confidence model.Confidence
}

// AddClientCall stages one outbound HTTP call.
func (w *Writer) AddClientCall(ctx context.Context, fileID int64, c ClientCall) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	var src any
	if c.SrcID > 0 {
		src = c.SrcID
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO client_calls
			(index_run_id, file_id, method, path, client, src_symbol_id, start_line, dynamic, confidence, binding)
		VALUES (?,?,?,?,?,?,?,?,?, 'pending')`,
		w.runID, fileID, c.Method, c.Path, c.Client, src, c.Line,
		boolToInt(c.Dynamic), string(stageConfidence(c.Confidence)),
	); err != nil {
		return w.fail(fmt.Errorf("registry: add client call %q: %w", c.Path, err))
	}
	return w.rotate(ctx)
}

// stageConfidence normalises what the extractor claimed.
//
// exact is refused rather than stored: it means "a compiler proved it", and a
// text scan for `fetch(` proves nothing of the kind. model.Edge.Validate would
// reject the resulting edge anyway; catching it here means the wrong claim
// never reaches disk.
func stageConfidence(c model.Confidence) model.Confidence {
	switch c {
	case model.ConfidenceHigh, model.ConfidenceMedium, model.ConfidenceLow:
		return c
	case model.ConfidenceExact:
		return model.ConfidenceHigh
	default:
		return model.ConfidenceLow
	}
}

// CallBindStats reports one binding pass over staged calls.
type CallBindStats struct {
	// Total is every staged call considered.
	Total int64
	// Bound matched exactly one endpoint declared in this repository.
	Bound int64
	// Unbound matched none, or matched ambiguously. Both are recorded facts:
	// the first usually means a call into another service, the second means
	// this repository declares routes that a single call cannot be told apart.
	Unbound int64
	// Ambiguous is the subset of Unbound where more than one route qualified.
	// Separated because the two need different actions — one is expected, the
	// other is a route table worth looking at.
	Ambiguous int64
	// Dynamic counts calls whose target was assembled at run time. They are
	// staged for the coverage denominator; one with no literal prefix at all is
	// never bound.
	Dynamic int64
	// Edges is how many CONSUMES edges landed. Lower than Bound whenever one
	// declaration calls the same endpoint twice, which is ordinary.
	Edges int64
	// UnboundPaths is a bounded, sorted sample of targets that did not bind, so
	// the number above can be acted on.
	UnboundPaths []string
}

// BindClientCalls resolves staged calls into CONSUMES edges.
//
// Idempotent: re-running it after more files are indexed rebinds everything and
// writes only the edges that are missing, so it is safe to call from both a
// full index and an incremental update — exactly as BindTableAccesses is.
func (r *Registry) BindClientCalls(ctx context.Context, runID int64) (CallBindStats, error) {
	var st CallBindStats

	routes, err := r.endpointRoutes(ctx, runID)
	if err != nil {
		return st, err
	}
	filePaths, err := r.filePaths(ctx, runID)
	if err != nil {
		return st, err
	}

	// Explicit total ORDER BY. The binding decision itself never depends on
	// order — an ambiguity is refused rather than resolved by whoever came
	// first — but the edge batch and the unbound sample do, and determinism is
	// load-bearing here (see digest.go).
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, file_id, method, path, COALESCE(src_symbol_id, 0),
		       start_line, dynamic, confidence
		  FROM client_calls
		 WHERE index_run_id = ?
		 ORDER BY file_id, start_line, path, id`, runID)
	if err != nil {
		return st, fmt.Errorf("registry: read client calls: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		bindings []accessBinding
		edges    []model.Edge
		unbound  = map[string]struct{}{}
	)

	for rows.Next() {
		var (
			id, fileID, srcID int64
			method, path      string
			line, dynamic     int
			conf              string
		)
		if err := rows.Scan(&id, &fileID, &method, &path, &srcID, &line, &dynamic, &conf); err != nil {
			return st, err
		}
		st.Total++
		if dynamic == 1 {
			st.Dynamic++
		}

		m := matchCall(routes, method, path, dynamic == 1, stageConfidence(model.Confidence(conf)))
		if !m.bound {
			st.Unbound++
			if m.ambiguous {
				st.Ambiguous++
			}
			if len(unbound) < maxUnboundSamples {
				unbound[callLabel(method, path, dynamic == 1)] = struct{}{}
			}
			bindings = append(bindings, accessBinding{id: id, boundTo: 0})
			continue
		}
		st.Bound++
		bindings = append(bindings, accessBinding{id: id, boundTo: m.endpointID})

		if srcID == 0 {
			// The call sits outside every declaration — a module-level constant
			// or a top-level prefetch. The endpoint is identified and the call
			// is bound; there is simply no source node for an edge to leave
			// from, and inventing the enclosing file as the caller would put a
			// file in the answer to "which function calls this endpoint".
			continue
		}

		edges = append(edges, model.Edge{
			SrcID: srcID, DstID: m.endpointID,
			Kind:       model.EdgeConsumes,
			Confidence: m.confidence,
			Evidence: model.Evidence{
				FilePath: filePaths[fileID],
				Range:    model.Range{Start: model.Position{Line: line}},
				Source:   model.SourceSourceCode,
				Detail:   m.detail,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	written, err := r.AddResolvedEdges(ctx, runID, edges)
	if err != nil {
		return st, err
	}
	// What LANDED, not what was built: one component calling one endpoint from
	// two lines of the same function produces identical edges, and INSERT OR
	// IGNORE drops the repeat.
	st.Edges = int64(written)

	if err := r.markCallBindings(ctx, bindings); err != nil {
		return st, err
	}

	for p := range unbound {
		st.UnboundPaths = append(st.UnboundPaths, p)
	}
	sortStrings(st.UnboundPaths)
	return st, nil
}

// callLabel renders a staged call for the unbound sample the way a person
// writes it in a ticket, with the unresolved remainder named rather than
// smoothed over.
func callLabel(method, path string, dynamic bool) string {
	m := method
	if m == "" {
		m = "ANY"
	}
	if dynamic {
		path += "<unresolved>"
	}
	return m + " " + path
}

// routeCandidate is one endpoint node, pre-canonicalised for matching.
type routeCandidate struct {
	id     int64
	method string   // lowercased, "" means ANY
	segs   []string // canonical path segments, "*" for a parameter
}

// endpointRoutes loads the run's endpoint nodes in a stable order.
//
// The endpoint's name is its signature — "GET /api/orders/{id}", or
// "ANY /health" where the framework stated no verb — which is what
// api.Route.Signature produced when the node was written.
func (r *Registry) endpointRoutes(ctx context.Context, runID int64) ([]routeCandidate, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = ?
		 ORDER BY f.path, s.start_line, s.name, s.id`,
		runID, string(model.KindEndpoint))
	if err != nil {
		return nil, fmt.Errorf("registry: read endpoint symbols: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []routeCandidate
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		method, path := splitSignature(name)
		out = append(out, routeCandidate{
			id:     id,
			method: canonMethod(method),
			segs:   canonSegments(path),
		})
	}
	return out, rows.Err()
}

// splitSignature separates "GET /api/orders/{id}" into verb and path.
//
// A signature with no space is treated as a bare path: an endpoint node written
// by a future extractor that does not spell the verb must still be matchable,
// and reading its whole name as a verb would make it match nothing.
func splitSignature(sig string) (method, path string) {
	sig = strings.TrimSpace(sig)
	if i := strings.IndexByte(sig, ' '); i >= 0 {
		return sig[:i], strings.TrimSpace(sig[i+1:])
	}
	if strings.HasPrefix(sig, "/") {
		return "", sig
	}
	return sig, ""
}

// canonMethod folds the two spellings of "no verb stated" into one.
func canonMethod(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "any" {
		return ""
	}
	return m
}

// methodsMatch reports whether two verbs can describe the same request. An
// empty verb is ANY on either side: the extractor could not read it, which is
// not evidence that it differs.
func methodsMatch(a, b string) bool { return a == "" || b == "" || a == b }

// canonSegments reduces a path to the segments matching compares.
//
// Scheme and host go (a call to https://api.example.com/v1/orders and a local
// route /v1/orders are the same route when the host is this service, and the
// host is not knowable from the text either way). Query and fragment go — they
// are arguments, not identity. Case goes. A trailing slash goes, because
// /orders/ and /orders are one route to every framework here. Every server
// parameter spelling collapses to a single wildcard, so {id}, :id, <int:id> and
// * are one concept rather than four.
func canonSegments(p string) []string {
	p = strings.TrimSpace(p)
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			p = rest[j:]
		} else {
			p = "/"
		}
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	p = strings.ToLower(strings.Trim(p, "/"))
	if p == "" {
		return nil
	}
	raw := strings.Split(p, "/")
	segs := make([]string, 0, len(raw))
	for _, s := range raw {
		if s == "" {
			// A doubled slash is a typo, not a segment. Treating it as one
			// would make /api//orders unmatchable against /api/orders.
			continue
		}
		if isParamSegment(s) {
			s = "*"
		}
		segs = append(segs, s)
	}
	return segs
}

// isParamSegment recognises every server-side parameter spelling in use across
// the frameworks route extraction covers.
//
//	{id}      Spring, Express 5, FastAPI, .NET
//	{id:\d+}  Spring with a regex constraint
//	:id       Express 4, chi, gin
//	<int:id>  Flask
//	*, **     catch-alls
func isParamSegment(s string) bool {
	switch {
	case s == "*" || s == "**":
		return true
	case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
		return true
	case strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"):
		return true
	case strings.HasPrefix(s, ":") && len(s) > 1:
		return true
	case strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"):
		// Next.js file-system routing, which reaches here through a path
		// written in a client call rather than through route extraction.
		return true
	default:
		return false
	}
}

// callMatch is one call's binding decision.
type callMatch struct {
	bound      bool
	ambiguous  bool
	endpointID int64
	confidence model.Confidence
	detail     string
}

// matchCall decides which endpoint, if any, a staged call consumes.
//
// The evidence is graded, and the confidence of the resulting edge descends
// with it:
//
//   - A fully literal path matching a route with no parameters is the strongest
//     claim available from source text, and keeps the call's own confidence.
//   - A match that needed a wildcard to absorb a segment drops one tier. The
//     route is right; which resource the call names is not proven, and a
//     parameterised route is by construction reachable from many callers.
//   - A dynamic call matched only by its literal prefix drops to low. All that
//     is known is that the target starts where the route starts.
//
// Ambiguity refuses rather than picks. The one exception is specificity: a
// literal route and a parameterised one both matching /api/orders/summary are
// not two answers, because every framework here routes that request to the
// literal one. Choosing it is following the framework's own rule, not guessing
// between candidates — and within a tier, two qualifying routes still bind
// nothing.
func matchCall(
	routes []routeCandidate,
	method, path string,
	dynamic bool,
	conf model.Confidence,
) callMatch {
	m := canonMethod(method)
	segs := canonSegments(path)

	if dynamic {
		return matchByPrefix(routes, m, path, segs)
	}
	if len(segs) == 0 {
		// A literal call to "/" or to nothing. The root is a real route, but a
		// bare "/" is also what a mis-read target degrades to, and binding
		// every unreadable call to the root endpoint would be the invented
		// dependency this package exists to avoid.
		return callMatch{}
	}

	var exact, wild []routeCandidate
	for _, rt := range routes {
		if !methodsMatch(m, rt.method) {
			continue
		}
		used, ok := segmentsMatch(rt.segs, segs)
		if !ok {
			continue
		}
		if used == 0 {
			exact = append(exact, rt)
		} else {
			wild = append(wild, rt)
		}
	}

	if len(exact) == 1 {
		return callMatch{
			bound: true, endpointID: exact[0].id,
			// Nothing was absorbed and nothing was assumed, so the edge is
			// exactly as strong as the extraction that found the call — no
			// stronger, which is why the call's own confidence is carried
			// rather than a constant.
			confidence: conf,
			detail:     "literal path match",
		}
	}
	if len(exact) > 1 {
		return callMatch{ambiguous: true}
	}
	if len(wild) == 1 {
		return callMatch{
			bound: true, endpointID: wild[0].id,
			confidence: demote(conf),
			detail:     "path parameter match",
		}
	}
	if len(wild) > 1 {
		return callMatch{ambiguous: true}
	}
	return callMatch{}
}

// demote weakens a confidence by exactly one tier, floored at low.
//
// One tier, not a jump to low: a wildcard match is weaker evidence than a
// literal one, but it is not the same as no evidence about the path at all,
// and collapsing the two would make the ranking that thresholds exist to read
// (Confidence.AtLeast) meaningless.
func demote(c model.Confidence) model.Confidence {
	switch c {
	case model.ConfidenceExact, model.ConfidenceHigh:
		return model.ConfidenceMedium
	default:
		return model.ConfidenceLow
	}
}

// segmentsMatch compares a route's segments against a call's, and reports how
// many wildcards were spent.
//
// Segment count is compared first because it is the cheap discriminator and
// because a wildcard absorbs exactly ONE segment. A catch-all that swallowed
// the remainder would make /api/* match every call the repository makes, which
// is how a route table with one wildcard produces an impact report where
// everything depends on everything.
func segmentsMatch(route, call []string) (wildcards int, ok bool) {
	if len(route) != len(call) {
		return 0, false
	}
	for i, rs := range route {
		if rs == "*" {
			wildcards++
			continue
		}
		if rs != call[i] {
			return 0, false
		}
	}
	return wildcards, true
}

// matchByPrefix handles a call whose target was assembled at run time.
//
// Only whole literal segments are trusted. `/api/user${id}` yields the literal
// prefix "/api/user", whose last segment is half a word — matching it against
// the route /api/user would bind a call that actually requests /api/users/7. So
// a prefix that does not end at a segment boundary loses its final segment.
//
// A single surviving candidate binds at low confidence. Anything else — no
// candidate, several candidates, or no literal prefix at all — stages the call
// without binding it. It still counts: a dynamic call is a real outbound
// request, and dropping it would shrink the denominator of "how many of this
// component's calls did we resolve".
func matchByPrefix(routes []routeCandidate, method, rawPath string, segs []string) callMatch {
	if len(segs) > 0 && !strings.HasSuffix(strings.TrimSpace(rawPath), "/") {
		segs = segs[:len(segs)-1]
	}
	if len(segs) == 0 {
		return callMatch{}
	}

	var hits []routeCandidate
	for _, rt := range routes {
		if !methodsMatch(method, rt.method) {
			continue
		}
		if len(rt.segs) < len(segs) {
			continue
		}
		ok := true
		for i, s := range segs {
			if rt.segs[i] != "*" && rt.segs[i] != s {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, rt)
		}
	}
	if len(hits) == 1 {
		return callMatch{
			bound: true, endpointID: hits[0].id,
			confidence: model.ConfidenceLow,
			detail:     "literal prefix, single candidate route",
		}
	}
	return callMatch{ambiguous: len(hits) > 1}
}

// markCallBindings records each call's outcome.
//
// Mirrors markBindings for table accesses rather than sharing it: the two write
// different tables, and parameterising the table name into the UPDATE would put
// a SQL identifier in a variable for the sake of six saved lines.
func (r *Registry) markCallBindings(ctx context.Context, bindings []accessBinding) error {
	if len(bindings) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: mark call bindings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE client_calls SET bound_symbol_id = ?, binding = ? WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("registry: mark call bindings: prepare: %w", err)
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
			return fmt.Errorf("registry: mark call binding %d: %w", b.id, err)
		}
	}
	return tx.Commit()
}

// ClientCallUsage is one staged call and what it resolved to, for reporting.
type ClientCallUsage struct {
	CallID     int64
	FilePath   string
	Line       int
	Method     string
	Path       string
	Client     string
	Dynamic    bool
	Caller     string
	EndpointID int64
	Endpoint   string
	Binding    string
}

// ClientCallReport lists every staged call and its binding outcome.
//
// Reads the staged rows rather than the edges, deliberately and for the reason
// TableUsageReport does: a call that bound to nothing has no edge, and those
// are exactly the rows that answer "which APIs does this repository consume
// that it does not own".
func (r *Registry) ClientCallReport(ctx context.Context, runID int64) ([]ClientCallUsage, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, f.path, c.start_line, c.method, c.path, c.client, c.dynamic,
		       COALESCE(src.name, ''), COALESCE(c.bound_symbol_id, 0),
		       COALESCE(ep.name, ''), c.binding
		  FROM client_calls c
		  JOIN files f        ON f.id = c.file_id
		  LEFT JOIN symbols src ON src.id = c.src_symbol_id
		  LEFT JOIN symbols ep  ON ep.id = c.bound_symbol_id
		 WHERE c.index_run_id = ?
		 ORDER BY f.path, c.start_line, c.path, c.id`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: client call report: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ClientCallUsage
	for rows.Next() {
		var u ClientCallUsage
		var dynamic int
		var endpointID sql.NullInt64
		if err := rows.Scan(&u.CallID, &u.FilePath, &u.Line, &u.Method, &u.Path,
			&u.Client, &dynamic, &u.Caller, &endpointID, &u.Endpoint, &u.Binding); err != nil {
			return nil, err
		}
		u.Dynamic = dynamic == 1
		if endpointID.Valid {
			u.EndpointID = endpointID.Int64
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ReplaceFileCalls re-stages one file's client calls after an incremental
// replace.
//
// Needed for the reason ReplaceFileAnalysis is: the incremental path writes
// through ReplaceFile rather than a Writer, ON DELETE CASCADE has already taken
// the file's previous client_calls with its old file row, and without this a
// re-indexed component would silently lose every outbound call it makes —
// exactly the erosion that made `cortex api` report zero endpoints during a
// watch session.
func (r *Registry) ReplaceFileCalls(ctx context.Context, runID, fileID int64, calls []ClientCall) error {
	if len(calls) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: replace file calls: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, c := range calls {
		var src any
		if c.SrcID > 0 {
			src = c.SrcID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO client_calls
				(index_run_id, file_id, method, path, client, src_symbol_id, start_line, dynamic, confidence, binding)
			VALUES (?,?,?,?,?,?,?,?,?, 'pending')`,
			runID, fileID, c.Method, c.Path, c.Client, src, c.Line,
			boolToInt(c.Dynamic), string(stageConfidence(c.Confidence)),
		); err != nil {
			return fmt.Errorf("registry: re-add client call %q: %w", c.Path, err)
		}
	}
	return tx.Commit()
}
