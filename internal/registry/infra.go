package registry

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Deployment facts: what the indexed code RUNS AS.
//
// The write half stages what a manifest declared — units, their ports, the
// configuration KEY NAMES they set, the resource bounds they ask for, and the
// files that could not be read at all. The read half answers the questions that
// only become askable once those facts are nodes:
//
//	which service does this code run in     UnitsForPath
//	which endpoints ship in this container  BindInfraUnits, as CONTAINS edges
//	what does changing this key restart     UnitsForConfigKey
//
// # No values, here or anywhere below
//
// Nothing in this file has a parameter, a column or a return field that holds a
// configuration value. Read the header of migration 0009 before adding one:
// the extractor's types have no field a value fits in, and that absence is the
// enforcement rather than a filter that could later be relaxed.
//
// # Why binding is a second pass
//
// The same reason client calls are. A unit names a BUILD CONTEXT — a directory
// — and the endpoints inside that directory are declared by files that may be
// parsed long after the manifest, or, on an incremental update, not reparsed at
// all. So units are staged during indexing and joined to code afterwards, which
// also means the join survives an incremental pass that touches only a
// controller.

// InfraUnit is one thing a manifest declares can run.
//
// It carries no command line. infra.Unit has one and it is already redacted;
// see migration 0009 for why redaction is a poor first line of defence and this
// field is absent rather than empty.
type InfraUnit struct {
	// SymbolID is the graph node for this unit, or 0 when none was written.
	SymbolID int64
	Name     string
	// Type is the unit's own vocabulary: "service", "Deployment", "stage",
	// "resource:aws_db_instance".
	Type   string
	Format string
	Image  string
	// BuildContext is the source directory this unit is built from, as the
	// manifest wrote it — relative to the manifest, not to the repository root.
	// Normalisation happens at binding time; storing the normalised form would
	// mean a reader cannot find the string with grep.
	BuildContext string
	Dockerfile   string
	Final        bool
	Line         int
}

// InfraPort is one port a unit declares.
type InfraPort struct {
	Container int
	Published int
	Protocol  string
	Name      string
	// Raw carries a declaration no number could be read from — "${PORT}:80", a
	// range, a named targetPort.
	Raw  string
	Line int
}

// InfraKey is one configuration KEY NAME a deployment supplies.
//
// There is no Value field and there must never be one.
type InfraKey struct {
	// UnitID is the unit that sets this key, or 0 when the key belongs to a
	// standalone key set (ConfigMap, Secret, Helm values, Terraform variables).
	UnitID int64
	// KeySet and KeySetType name that set when UnitID is 0.
	KeySet     string
	KeySetType string
	// SymbolID is the `configuration` node this key name became.
	SymbolID int64
	Name     string
	// From is where the value comes from — literal, secret-ref, configmap-ref,
	// downward-api, host, build-arg, image. The value is not read.
	From string
	// Ref names the Secret or ConfigMap supplying it. A name, never a value.
	Ref    string
	Secret bool
	Line   int
}

// InfraEnvFrom is a whole Secret or ConfigMap injected at once.
type InfraEnvFrom struct {
	Kind     string
	Name     string
	Prefix   string
	Optional bool
	Line     int
}

// InfraLimit is one declared resource bound.
type InfraLimit struct {
	Resource string
	Request  string
	Limit    string
}

// InfraProblem is a manifest that was recognised and could not be read.
//
// It carries the stable REASON and no parser message. See migration 0009: a
// YAML error quotes the text it choked on, and a fuzz target caught it handing
// back a planted configuration value verbatim.
type InfraProblem struct {
	Format string
	Reason string
	Line   int
}

// ---------------------------------------------------------------- write path

// AddInfraUnit stages one deployment unit and returns its row id, which ports,
// keys, limits and envFrom references attach to.
func (w *Writer) AddInfraUnit(ctx context.Context, fileID int64, u InfraUnit) (int64, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if u.Name == "" {
		return 0, w.fail(errors.New("registry: infra unit name must be set"))
	}
	if err := w.ensureTx(ctx); err != nil {
		return 0, err
	}
	res, err := w.tx.ExecContext(ctx, `
		INSERT INTO infra_units
			(index_run_id, file_id, symbol_id, name, unit_type, format, image,
			 build_context, dockerfile, final, start_line)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		w.runID, fileID, nullID(u.SymbolID), u.Name, u.Type, u.Format, u.Image,
		u.BuildContext, u.Dockerfile, boolToInt(u.Final), u.Line,
	)
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: add infra unit %q: %w", u.Name, err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, w.fail(fmt.Errorf("registry: add infra unit %q: read id: %w", u.Name, err))
	}
	return id, w.rotate(ctx)
}

// AddInfraPort stages one port declaration.
func (w *Writer) AddInfraPort(ctx context.Context, unitID int64, p InfraPort) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO infra_ports
			(index_run_id, unit_id, container_port, published_port, protocol, name, raw, start_line)
		VALUES (?,?,?,?,?,?,?,?)`,
		w.runID, unitID, p.Container, p.Published, p.Protocol, p.Name, p.Raw, p.Line,
	); err != nil {
		return w.fail(fmt.Errorf("registry: add infra port on unit %d: %w", unitID, err))
	}
	return w.rotate(ctx)
}

// AddInfraKey stages one configuration key NAME.
func (w *Writer) AddInfraKey(ctx context.Context, fileID int64, k InfraKey) error {
	if w.failed != nil {
		return w.failed
	}
	if k.Name == "" {
		return w.fail(errors.New("registry: infra key name must be set"))
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO infra_keys
			(index_run_id, file_id, unit_id, key_set, key_set_type, symbol_id,
			 name, from_source, ref, secret, start_line)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		w.runID, fileID, nullID(k.UnitID), k.KeySet, k.KeySetType, nullID(k.SymbolID),
		k.Name, k.From, k.Ref, boolToInt(k.Secret), k.Line,
	); err != nil {
		return w.fail(fmt.Errorf("registry: add infra key %q: %w", k.Name, err))
	}
	return w.rotate(ctx)
}

// AddInfraEnvFrom stages a whole-object environment injection.
func (w *Writer) AddInfraEnvFrom(ctx context.Context, unitID int64, r InfraEnvFrom) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO infra_env_from (index_run_id, unit_id, kind, name, prefix, optional, start_line)
		VALUES (?,?,?,?,?,?,?)`,
		w.runID, unitID, r.Kind, r.Name, r.Prefix, boolToInt(r.Optional), r.Line,
	); err != nil {
		return w.fail(fmt.Errorf("registry: add infra envFrom %s/%s: %w", r.Kind, r.Name, err))
	}
	return w.rotate(ctx)
}

// AddInfraLimit stages one declared resource bound.
func (w *Writer) AddInfraLimit(ctx context.Context, unitID int64, l InfraLimit) error {
	if w.failed != nil {
		return w.failed
	}
	if l.Resource == "" {
		return w.fail(errors.New("registry: infra limit must name a resource"))
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO infra_limits (index_run_id, unit_id, resource, request_qty, limit_qty)
		VALUES (?,?,?,?,?)`,
		w.runID, unitID, l.Resource, l.Request, l.Limit,
	); err != nil {
		return w.fail(fmt.Errorf("registry: add infra limit %q: %w", l.Resource, err))
	}
	return w.rotate(ctx)
}

// AddInfraProblem records a manifest that could not be read.
//
// This is the write that makes silence honest, so it is deliberately as
// unconditional as the ones above: there is no threshold below which a
// manifest we failed to parse stops being worth recording.
func (w *Writer) AddInfraProblem(ctx context.Context, fileID int64, p InfraProblem) error {
	if w.failed != nil {
		return w.failed
	}
	if p.Reason == "" {
		return w.fail(errors.New("registry: infra problem reason must be set"))
	}
	if err := w.ensureTx(ctx); err != nil {
		return err
	}
	if _, err := w.tx.ExecContext(ctx, `
		INSERT INTO infra_problems (index_run_id, file_id, format, reason, start_line)
		VALUES (?,?,?,?,?)`,
		w.runID, fileID, p.Format, p.Reason, p.Line,
	); err != nil {
		return w.fail(fmt.Errorf("registry: add infra problem %q: %w", p.Reason, err))
	}
	return w.rotate(ctx)
}

// nullID turns a zero id into SQL NULL.
//
// Zero is not a row id, and storing it would fabricate a reference to a symbol
// that does not exist — which ON DELETE SET NULL then could not clean up,
// because there is nothing for it to match.
func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// ---------------------------------------------------------------- the join

// UnitBindStats reports one pass binding deployment units to the code they run.
type UnitBindStats struct {
	// Units is every staged unit in the run.
	Units int64
	// WithContext is how many of them name a build context at all. The rest are
	// Kubernetes containers, Terraform resources and Service objects, which say
	// what runs without saying which directory it was built from — a coverage
	// fact, not a failure.
	WithContext int64
	// Endpoints is every endpoint node considered.
	Endpoints int64
	// Bound matched exactly one unit.
	Bound int64
	// Ambiguous matched more than one unit at the same specificity and was
	// therefore refused. Two services built from the SAME directory is a real
	// pattern (an api and a worker sharing a codebase), and picking one would
	// be an invented dependency.
	Ambiguous int64
	// Edges is how many CONTAINS edges landed, which is lower than Bound
	// whenever two endpoints in one directory belong to the same unit and the
	// same evidence line.
	Edges int64
	// EmptyContexts is a bounded, sorted sample of build contexts that
	// contained no indexed file at all. Usually a path outside the repository
	// or one the walker excludes; worth naming, because a context that matches
	// nothing silently produces no edges and looks exactly like success.
	EmptyContexts []string
}

// BindInfraUnits joins deployment units to the code they run.
//
// The evidence is a build context: `build: ./services/api` in a compose file
// says that everything under services/api is what ends up in that service's
// image. That is the only statement in a repository that connects a running
// service to source, and it is stated by the manifest rather than inferred, so
// it is worth an edge.
//
// # What is bound, and why not everything
//
// Endpoints, not every symbol. A CONTAINS edge from a unit to each of the
// 40,000 functions under its build context would be technically true, would
// dominate the edge table, and would make every blast-radius traversal pass
// through a hub node that connects to nearly everything. Endpoints are the
// public surface of a service — "which endpoints ship in this container" is the
// question people actually ask — and the general form ("which service does this
// FILE run in") is answered by UnitsForPath without materialising anything.
//
// # Why medium confidence and not high
//
// The manifest proves the directory. It does not prove that a given file inside
// it reaches the image: a Dockerfile can COPY a subset, a .dockerignore can
// exclude half the tree, and a multi-stage build can discard the stage a file
// was compiled in. Directory containment is strong evidence and not proof, and
// the confidence model exists so that difference survives to the caller.
//
// Idempotent, like every other binding pass: re-running it writes only the
// edges that are missing.
func (r *Registry) BindInfraUnits(ctx context.Context, runID int64) (UnitBindStats, error) {
	var st UnitBindStats

	units, err := r.unitContexts(ctx, runID, &st)
	if err != nil {
		return st, err
	}
	endpoints, err := r.endpointLocations(ctx, runID)
	if err != nil {
		return st, err
	}
	st.Endpoints = int64(len(endpoints))

	// used is indexed by position in units, never keyed by a map, because the
	// sample below is derived from it and a map's iteration order would make
	// two runs of identical source disagree about which contexts to report.
	used := make([]bool, len(units))

	var edges []model.Edge
	for _, ep := range endpoints {
		idx, ambiguous := matchUnitForPath(units, ep.path)
		if ambiguous {
			st.Ambiguous++
			continue
		}
		if idx < 0 {
			continue
		}
		used[idx] = true
		u := units[idx]
		st.Bound++
		if u.symbolID == 0 || ep.symbolID == 0 {
			// A staged unit whose node was not written. Counted as bound —
			// the join succeeded — but there is no node for an edge to leave
			// from, and inventing one would be worse than the gap.
			continue
		}
		edges = append(edges, model.Edge{
			SrcID: u.symbolID, DstID: ep.symbolID,
			Kind:       model.EdgeContains,
			Confidence: model.ConfidenceMedium,
			Evidence: model.Evidence{
				FilePath: u.manifest,
				Range:    model.Range{Start: model.Position{Line: u.line}},
				Source:   model.SourceConfig,
				Detail:   "build context " + u.context,
			},
		})
	}

	written, err := r.AddResolvedEdges(ctx, runID, edges)
	if err != nil {
		return st, err
	}
	st.Edges = int64(written)

	for i, u := range units {
		if used[i] || len(st.EmptyContexts) >= maxUnboundSamples {
			continue
		}
		st.EmptyContexts = append(st.EmptyContexts, u.manifest+": "+u.context)
	}
	sortStrings(st.EmptyContexts)
	return st, nil
}

// unitContext is one staged unit reduced to what the join needs.
type unitContext struct {
	symbolID int64
	manifest string
	name     string
	line     int
	// context is the build context normalised to a repository-relative
	// directory, with no trailing slash. "" means the whole repository, which
	// is what `build: .` on a compose file at the root means.
	context string
	// depth is how many segments context has, and is the specificity used to
	// pick between nested contexts. Precomputed because it is compared once per
	// endpoint per unit.
	depth int
}

// unitContexts loads the units that name a build context, normalised and
// ordered.
//
// The ORDER BY is total over content, not over rowid. That is the property the
// determinism gate depends on: this list feeds a longest-match rule whose
// tie-break is positional, and an unordered read feeding a positional rule is
// exactly the bug this codebase has already paid for once.
func (r *Registry) unitContexts(ctx context.Context, runID int64, st *UnitBindStats) ([]unitContext, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, u.name, u.build_context, u.start_line, COALESCE(u.symbol_id, 0)
		  FROM infra_units u
		  JOIN files f ON f.id = u.file_id
		 WHERE u.index_run_id = ?
		 ORDER BY f.path, u.start_line, u.name, u.build_context, u.id`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: read infra units: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []unitContext
	for rows.Next() {
		var (
			manifest, name, buildCtx string
			line                     int
			symbolID                 int64
		)
		if err := rows.Scan(&manifest, &name, &buildCtx, &line, &symbolID); err != nil {
			return nil, err
		}
		st.Units++
		if buildCtx == "" {
			continue
		}
		st.WithContext++
		dir, ok := repoRelativeContext(manifest, buildCtx)
		if !ok {
			// A context that resolves outside the repository — `build: ../..`
			// in a compose file that lives in a submodule. Real, and nothing
			// in this index can be joined to it, so it produces no unit here
			// rather than a match against the root.
			continue
		}
		out = append(out, unitContext{
			symbolID: symbolID, manifest: manifest, name: name, line: line,
			context: dir, depth: contextDepth(dir),
		})
	}
	return out, rows.Err()
}

// repoRelativeContext resolves a manifest-relative build context against the
// manifest's own directory.
//
// `build: ./api` in deploy/docker-compose.yml means deploy/api, not api. Getting
// this wrong does not fail loudly: it produces a context that matches no file,
// or worse, one that matches a different team's directory.
//
// ok is false when the result escapes the repository root, which cannot be
// joined against an index of files inside it.
func repoRelativeContext(manifestPath, buildContext string) (string, bool) {
	c := strings.TrimSpace(strings.ReplaceAll(buildContext, "\\", "/"))
	if c == "" {
		return "", false
	}
	// A URL context (`build: https://github.com/...#branch`) builds from
	// somewhere else entirely. Nothing in this repository corresponds to it.
	if strings.Contains(c, "://") {
		return "", false
	}
	dir := path.Dir(manifestPath)
	if dir == "." || dir == "/" {
		dir = ""
	}
	joined := path.Clean(path.Join(dir, c))
	switch {
	case joined == "." || joined == "/":
		return "", true // the repository root
	case joined == ".." || strings.HasPrefix(joined, "../"):
		return "", false
	}
	return strings.TrimPrefix(joined, "./"), true
}

func contextDepth(dir string) int {
	if dir == "" {
		return 0
	}
	return strings.Count(dir, "/") + 1
}

// endpointLocation is one endpoint node and the file that declares it.
type endpointLocation struct {
	symbolID int64
	path     string
}

// endpointLocations reads the run's endpoint nodes in a stable order.
func (r *Registry) endpointLocations(ctx context.Context, runID int64) ([]endpointLocation, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, f.path
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = ?
		 ORDER BY f.path, s.fqn, s.start_line, s.id`,
		runID, string(model.KindEndpoint))
	if err != nil {
		return nil, fmt.Errorf("registry: read endpoint locations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []endpointLocation
	for rows.Next() {
		var e endpointLocation
		if err := rows.Scan(&e.symbolID, &e.path); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// matchUnitForPath picks the unit whose build context most specifically
// contains a file.
//
// Longest context wins, and that is a rule rather than a heuristic: a monorepo
// with `build: .` at the root and `build: ./services/api` beneath it means the
// api service, and nothing else, is built from services/api. The root context
// still contains the file, but the nested one is the more specific true
// statement and every build system reads it that way.
//
// Two units at the SAME depth both containing the file is a genuine ambiguity —
// an api and a worker built from one directory — and it refuses. A wrong edge
// here says a change restarts a service it does not, which is the failure this
// whole package is shaped to avoid; a refusal is a visible gap.
func matchUnitForPath(units []unitContext, filePath string) (idx int, ambiguous bool) {
	best, bestDepth, ties := -1, -1, 0
	for i, u := range units {
		if !contextContains(u.context, filePath) {
			continue
		}
		switch {
		case u.depth > bestDepth:
			best, bestDepth, ties = i, u.depth, 1
		case u.depth == bestDepth:
			// Two rows for the same unit are not an ambiguity. A compose file
			// listing api twice cannot happen, but the same service appearing
			// in docker-compose.yml and docker-compose.prod.yml can, and those
			// are separate nodes describing one deployment.
			if units[i].manifest != units[best].manifest || units[i].name != units[best].name {
				ties++
			}
		}
	}
	if best < 0 {
		return -1, false
	}
	if ties > 1 {
		return -1, true
	}
	return best, false
}

// contextContains reports whether a repository-relative directory contains a
// file path.
//
// The separator check is what stops `services/api` from matching
// `services/api-gateway/main.go`, which is a prefix match and a wrong answer.
func contextContains(dir, filePath string) bool {
	if dir == "" {
		return true
	}
	return strings.HasPrefix(filePath, dir+"/")
}

// ---------------------------------------------------------------- read path

// UnitMatch names the deployment unit a file runs in.
type UnitMatch struct {
	SymbolID int64
	Name     string
	Type     string
	Format   string
	Image    string
	// Manifest and Line are where to look to verify or change it.
	Manifest string
	Line     int
	// BuildContext is the repository-relative directory that matched.
	BuildContext string
	// Ambiguous is set when more than one unit is built from the same
	// directory, in which case every candidate is returned rather than one
	// being chosen. The honest answer to "which service does this run in" is
	// sometimes "these two".
	Ambiguous bool
}

// UnitsForPath answers "which service does this code run in".
//
// The question no line of source can answer, and the reason this whole feature
// exists. Returns the most specific unit whose build context contains the file;
// when two units are built from the same directory it returns both, flagged, so
// a caller reports the ambiguity instead of a coin flip.
//
// Empty is a real answer: a repository whose manifests never name a build
// context — pure Kubernetes, images built elsewhere in CI — cannot connect its
// services to its code, and saying so is better than guessing.
func (r *Registry) UnitsForPath(ctx context.Context, runID int64, filePath string) ([]UnitMatch, error) {
	if filePath == "" {
		return nil, errors.New("registry: UnitsForPath: file path must be set")
	}
	var st UnitBindStats
	units, err := r.unitContexts(ctx, runID, &st)
	if err != nil {
		return nil, err
	}

	// Collect every containing unit, then keep only those at the deepest
	// context. Two passes over a slice, rather than one pass with a map keyed
	// by depth — the map would be ranged to build the result, and that is the
	// shape of the non-determinism this file is careful about.
	best := -1
	for _, u := range units {
		if contextContains(u.context, filePath) && u.depth > best {
			best = u.depth
		}
	}
	if best < 0 {
		return nil, nil
	}

	var matches []unitContext
	for _, u := range units {
		if u.depth == best && contextContains(u.context, filePath) {
			matches = append(matches, u)
		}
	}
	out := make([]UnitMatch, 0, len(matches))
	for _, u := range matches {
		out = append(out, UnitMatch{
			SymbolID: u.symbolID, Name: u.name, Manifest: u.manifest,
			Line: u.line, BuildContext: u.context, Ambiguous: len(matches) > 1,
		})
	}
	if err := r.decorateUnitMatches(ctx, runID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// decorateUnitMatches fills in the columns the context query does not read.
//
// A second small query rather than widening unitContexts, because that one runs
// once per endpoint in the binding pass and carrying four unused strings
// through it would cost the hot path to save a rare one.
func (r *Registry) decorateUnitMatches(ctx context.Context, runID int64, matches []UnitMatch) error {
	if len(matches) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, u.name, u.start_line, u.unit_type, u.format, u.image
		  FROM infra_units u
		  JOIN files f ON f.id = u.file_id
		 WHERE u.index_run_id = ?
		 ORDER BY f.path, u.start_line, u.name, u.id`, runID)
	if err != nil {
		return fmt.Errorf("registry: decorate unit matches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type key struct {
		path string
		name string
		line int
	}
	detail := map[key][3]string{}
	for rows.Next() {
		var k key
		var typ, format, image string
		if err := rows.Scan(&k.path, &k.name, &k.line, &typ, &format, &image); err != nil {
			return err
		}
		if _, dup := detail[k]; !dup {
			detail[k] = [3]string{typ, format, image}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Ranged over the SLICE, not the map: the map is only ever looked up.
	for i := range matches {
		d := detail[key{matches[i].Manifest, matches[i].Name, matches[i].Line}]
		matches[i].Type, matches[i].Format, matches[i].Image = d[0], d[1], d[2]
	}
	return nil
}

// ConfigKeyImpact is what one configuration key name touches at run time.
type ConfigKeyImpact struct {
	Key string
	// Units are the deployment units that SET this key. Changing the key's
	// value restarts these.
	Units []UnitMatch
	// Secret is true when the key is supplied by a Secret or its name says
	// credential. No value was read to decide it.
	Secret bool
	// Sources are the distinct places the value comes from — literal,
	// secret-ref, configmap-ref — sorted. Two units taking one key from
	// different places is worth seeing.
	Sources []string
}

// UnitsForConfigKey answers "what does changing this config key restart".
//
// Matching is on the key NAME as the manifest wrote it, case-sensitively first
// and then case-insensitively, because a manifest and the code that reads it
// agree on spelling far more often than a properties file and an environment
// variable do. The looser cross-spelling folding that matchConfigKey performs
// is deliberately NOT applied here: that mapping exists to connect a Spring
// property to an environment variable, and applying it to a restart claim would
// let `database.url` in a values file assert that changing it restarts a
// workload that actually sets DATABASE_URL from somewhere else.
func (r *Registry) UnitsForConfigKey(ctx context.Context, runID int64, key string) (ConfigKeyImpact, error) {
	impact := ConfigKeyImpact{Key: key}
	if key == "" {
		return impact, errors.New("registry: UnitsForConfigKey: key name must be set")
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, u.name, u.unit_type, u.format, u.image, u.start_line,
		       u.build_context, k.from_source, k.secret
		  FROM infra_keys k
		  JOIN infra_units u ON u.id = k.unit_id
		  JOIN files f       ON f.id = u.file_id
		 WHERE k.index_run_id = ? AND (k.name = ? OR LOWER(k.name) = LOWER(?))
		 ORDER BY f.path, u.start_line, u.name, k.from_source, k.id`, runID, key, key)
	if err != nil {
		return impact, fmt.Errorf("registry: units for config key %q: %w", key, err)
	}
	defer func() { _ = rows.Close() }()

	seenSource := map[string]struct{}{}
	for rows.Next() {
		var m UnitMatch
		var from string
		var secret int
		if err := rows.Scan(&m.Manifest, &m.Name, &m.Type, &m.Format, &m.Image,
			&m.Line, &m.BuildContext, &from, &secret); err != nil {
			return impact, err
		}
		impact.Units = append(impact.Units, m)
		if secret == 1 {
			impact.Secret = true
		}
		if from != "" {
			if _, dup := seenSource[from]; !dup {
				seenSource[from] = struct{}{}
				// Appended from the ROW, in the query's order, so the map is
				// only ever a membership test and never an iteration source.
				impact.Sources = append(impact.Sources, from)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return impact, err
	}
	sortStrings(impact.Sources)
	return impact, nil
}

// InfraFile is one manifest as the index holds it.
//
// Shaped per file rather than as a flat list of units because that is how a
// deployment is read and changed: the question is "what does this compose file
// declare", and a report that interleaved three manifests would be unusable.
type InfraFile struct {
	Path     string
	Format   string
	Units    []InfraUnitRecord
	KeySets  []InfraKeySetRecord
	Problems []InfraProblem
}

// InfraUnitRecord is one persisted unit with everything attached to it.
type InfraUnitRecord struct {
	InfraUnit
	Ports   []InfraPort
	Keys    []InfraKey
	EnvFrom []InfraEnvFrom
	Limits  []InfraLimit
}

// InfraKeySetRecord is a ConfigMap, Secret, values file or variable block.
type InfraKeySetRecord struct {
	Name string
	Type string
	Line int
	Keys []InfraKey
}

// InfraReport reads every deployment fact in a run, grouped by manifest.
//
// Problems are part of the report and not an error return, for the reason
// migration 0009 gives: a manifest we could not read is data. A caller that
// received units and no problems would report an inventory as complete when it
// is not.
func (r *Registry) InfraReport(ctx context.Context, runID int64) ([]InfraFile, error) {
	files, order, err := r.infraUnitsByFile(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := r.attachInfraKeys(ctx, runID, files, &order); err != nil {
		return nil, err
	}
	if err := r.attachInfraProblems(ctx, runID, files, &order); err != nil {
		return nil, err
	}

	out := make([]InfraFile, 0, len(order))
	for _, p := range order {
		out = append(out, *files[p])
	}
	// order is built in first-seen order from queries that are themselves
	// ordered by path, so this sort is already satisfied; it is here because
	// the guarantee must not depend on three queries agreeing.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// infraUnitsByFile loads units, ports, limits and envFrom, grouped by manifest.
func (r *Registry) infraUnitsByFile(ctx context.Context, runID int64) (map[string]*InfraFile, []string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT u.id, f.path, u.format, u.name, u.unit_type, u.image,
		       u.build_context, u.dockerfile, u.final, u.start_line,
		       COALESCE(u.symbol_id, 0)
		  FROM infra_units u
		  JOIN files f ON f.id = u.file_id
		 WHERE u.index_run_id = ?
		 ORDER BY f.path, u.start_line, u.name, u.id`, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("registry: infra report: units: %w", err)
	}
	defer func() { _ = rows.Close() }()

	files := map[string]*InfraFile{}
	var order []string
	// unitAt maps a unit row id to where it landed, so ports, limits and
	// envFrom rows can be attached without a second lookup by name.
	unitAt := map[int64]unitSlot{}

	for rows.Next() {
		var (
			id       int64
			p        InfraUnitRecord
			filePath string
			format   string
			final    int
		)
		if err := rows.Scan(&id, &filePath, &format, &p.Name, &p.Type, &p.Image,
			&p.BuildContext, &p.Dockerfile, &final, &p.Line, &p.SymbolID); err != nil {
			return nil, nil, err
		}
		p.Final = final == 1
		p.Format = format
		f := files[filePath]
		if f == nil {
			f = &InfraFile{Path: filePath, Format: format}
			files[filePath] = f
			order = append(order, filePath)
		}
		f.Units = append(f.Units, p)
		unitAt[id] = unitSlot{path: filePath, index: len(f.Units) - 1}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if err := r.attachInfraPorts(ctx, runID, files, unitAt); err != nil {
		return nil, nil, err
	}
	if err := r.attachInfraLimits(ctx, runID, files, unitAt); err != nil {
		return nil, nil, err
	}
	if err := r.attachInfraEnvFrom(ctx, runID, files, unitAt); err != nil {
		return nil, nil, err
	}
	return files, order, nil
}

// unitSlot is where a staged unit landed in the per-file report.
type unitSlot struct {
	path  string
	index int
}

// unitRef resolves a staged unit id to the record it was loaded into.
func unitRef(files map[string]*InfraFile, unitAt map[int64]unitSlot, id int64) *InfraUnitRecord {
	loc, ok := unitAt[id]
	if !ok {
		return nil
	}
	f := files[loc.path]
	if f == nil || loc.index < 0 || loc.index >= len(f.Units) {
		return nil
	}
	return &f.Units[loc.index]
}

func (r *Registry) attachInfraPorts(
	ctx context.Context, runID int64,
	files map[string]*InfraFile, unitAt map[int64]unitSlot,
) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT p.unit_id, p.container_port, p.published_port, p.protocol, p.name, p.raw, p.start_line
		  FROM infra_ports p
		  JOIN infra_units u ON u.id = p.unit_id
		  JOIN files f       ON f.id = u.file_id
		 WHERE p.index_run_id = ?
		 ORDER BY f.path, u.start_line, u.name, p.start_line,
		          p.container_port, p.published_port, p.name, p.id`, runID)
	if err != nil {
		return fmt.Errorf("registry: infra report: ports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var unitID int64
		var p InfraPort
		if err := rows.Scan(&unitID, &p.Container, &p.Published, &p.Protocol,
			&p.Name, &p.Raw, &p.Line); err != nil {
			return err
		}
		if u := unitRef(files, unitAt, unitID); u != nil {
			u.Ports = append(u.Ports, p)
		}
	}
	return rows.Err()
}

func (r *Registry) attachInfraLimits(
	ctx context.Context, runID int64,
	files map[string]*InfraFile, unitAt map[int64]unitSlot,
) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT l.unit_id, l.resource, l.request_qty, l.limit_qty
		  FROM infra_limits l
		  JOIN infra_units u ON u.id = l.unit_id
		  JOIN files f       ON f.id = u.file_id
		 WHERE l.index_run_id = ?
		 ORDER BY f.path, u.start_line, u.name, l.resource, l.id`, runID)
	if err != nil {
		return fmt.Errorf("registry: infra report: limits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var unitID int64
		var l InfraLimit
		if err := rows.Scan(&unitID, &l.Resource, &l.Request, &l.Limit); err != nil {
			return err
		}
		if u := unitRef(files, unitAt, unitID); u != nil {
			u.Limits = append(u.Limits, l)
		}
	}
	return rows.Err()
}

func (r *Registry) attachInfraEnvFrom(
	ctx context.Context, runID int64,
	files map[string]*InfraFile, unitAt map[int64]unitSlot,
) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.unit_id, e.kind, e.name, e.prefix, e.optional, e.start_line
		  FROM infra_env_from e
		  JOIN infra_units u ON u.id = e.unit_id
		  JOIN files f       ON f.id = u.file_id
		 WHERE e.index_run_id = ?
		 ORDER BY f.path, u.start_line, u.name, e.kind, e.name, e.id`, runID)
	if err != nil {
		return fmt.Errorf("registry: infra report: envFrom: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var unitID int64
		var e InfraEnvFrom
		var optional int
		if err := rows.Scan(&unitID, &e.Kind, &e.Name, &e.Prefix, &optional, &e.Line); err != nil {
			return err
		}
		e.Optional = optional == 1
		if u := unitRef(files, unitAt, unitID); u != nil {
			u.EnvFrom = append(u.EnvFrom, e)
		}
	}
	return rows.Err()
}

// attachInfraKeys hangs key names off their unit, or off a key set when they
// belong to one.
//
// Keys are matched to units by (manifest, unit name, unit line) rather than by
// row id, because this query is ordered by manifest for determinism and joining
// through infra_units already gives it those three columns. A key whose unit
// cannot be found is dropped rather than promoted to a key set: a key with no
// owner is a defect in the write path, and inventing a container for it would
// hide the defect behind plausible output.
func (r *Registry) attachInfraKeys(
	ctx context.Context, runID int64,
	files map[string]*InfraFile, order *[]string,
) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, COALESCE(u.name,''), COALESCE(u.start_line,0),
		       COALESCE(k.unit_id,0), k.key_set, k.key_set_type,
		       k.name, k.from_source, k.ref, k.secret, k.start_line,
		       COALESCE(k.symbol_id,0)
		  FROM infra_keys k
		  JOIN files f            ON f.id = k.file_id
		  LEFT JOIN infra_units u ON u.id = k.unit_id
		 WHERE k.index_run_id = ?
		 ORDER BY f.path, k.key_set, COALESCE(u.start_line,0), COALESCE(u.name,''),
		          k.start_line, k.name, k.id`, runID)
	if err != nil {
		return fmt.Errorf("registry: infra report: keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			filePath, unitName string
			unitLine           int
			unitID             int64
			k                  InfraKey
			secret             int
		)
		if err := rows.Scan(&filePath, &unitName, &unitLine, &unitID,
			&k.KeySet, &k.KeySetType, &k.Name, &k.From, &k.Ref, &secret,
			&k.Line, &k.SymbolID); err != nil {
			return err
		}
		k.Secret = secret == 1
		k.UnitID = unitID

		f := files[filePath]
		if f == nil {
			// A manifest that declared key sets and no units at all — a bare
			// ConfigMap file, a values.yaml. Real and common.
			f = &InfraFile{Path: filePath}
			files[filePath] = f
			*order = append(*order, filePath)
		}
		if unitID != 0 {
			if u := findUnit(f, unitName, unitLine); u != nil {
				u.Keys = append(u.Keys, k)
			}
			continue
		}
		f.KeySets = appendKeySetKey(f.KeySets, k)
	}
	return rows.Err()
}

func findUnit(f *InfraFile, name string, line int) *InfraUnitRecord {
	for i := range f.Units {
		if f.Units[i].Name == name && f.Units[i].Line == line {
			return &f.Units[i]
		}
	}
	return nil
}

// appendKeySetKey files a key under its set, creating the set on first sight.
//
// Linear scan rather than a map, and deliberately: a manifest declares a
// handful of key sets, the slice preserves the query's order (which is total
// and therefore deterministic), and a map would have to be sorted back into a
// slice at the end anyway.
func appendKeySetKey(sets []InfraKeySetRecord, k InfraKey) []InfraKeySetRecord {
	for i := range sets {
		if sets[i].Name == k.KeySet && sets[i].Type == k.KeySetType {
			sets[i].Keys = append(sets[i].Keys, k)
			return sets
		}
	}
	return append(sets, InfraKeySetRecord{
		Name: k.KeySet, Type: k.KeySetType, Line: k.Line, Keys: []InfraKey{k},
	})
}

func (r *Registry) attachInfraProblems(
	ctx context.Context, runID int64,
	files map[string]*InfraFile, order *[]string,
) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT f.path, p.format, p.reason, p.start_line
		  FROM infra_problems p
		  JOIN files f ON f.id = p.file_id
		 WHERE p.index_run_id = ?
		 ORDER BY f.path, p.start_line, p.reason, p.id`, runID)
	if err != nil {
		return fmt.Errorf("registry: infra report: problems: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var filePath string
		var p InfraProblem
		if err := rows.Scan(&filePath, &p.Format, &p.Reason, &p.Line); err != nil {
			return err
		}
		f := files[filePath]
		if f == nil {
			f = &InfraFile{Path: filePath, Format: p.Format}
			files[filePath] = f
			*order = append(*order, filePath)
		}
		if f.Format == "" {
			f.Format = p.Format
		}
		f.Problems = append(f.Problems, p)
	}
	return rows.Err()
}

// ReplaceFileInfra re-stages one manifest's deployment facts after an
// incremental replace.
//
// Needed for the reason ReplaceFileAnalysis and ReplaceFileCalls are: the
// incremental path writes through ReplaceFile rather than a Writer, ON DELETE
// CASCADE has already taken this file's infra rows with its old file row, and
// without this, editing a compose file would silently delete every service it
// declares. That erosion is invisible — an empty result and a correct empty
// result look identical — and it is the exact defect that made `cortex api`
// report zero endpoints during a watch session.
//
// Insert-only, like its siblings: the caller has just replaced the file, so
// there is nothing left to delete.
func (r *Registry) ReplaceFileInfra(
	ctx context.Context,
	runID, fileID int64,
	units []InfraUnitRecord,
	keySets []InfraKeySetRecord,
	problems []InfraProblem,
) error {
	if len(units) == 0 && len(keySets) == 0 && len(problems) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: replace file infra: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, u := range units {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO infra_units
				(index_run_id, file_id, symbol_id, name, unit_type, format, image,
				 build_context, dockerfile, final, start_line)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			runID, fileID, nullID(u.SymbolID), u.Name, u.Type, u.Format, u.Image,
			u.BuildContext, u.Dockerfile, boolToInt(u.Final), u.Line)
		if err != nil {
			return fmt.Errorf("registry: re-add infra unit %q: %w", u.Name, err)
		}
		unitID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("registry: re-add infra unit %q: read id: %w", u.Name, err)
		}
		for _, p := range u.Ports {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO infra_ports
					(index_run_id, unit_id, container_port, published_port, protocol, name, raw, start_line)
				VALUES (?,?,?,?,?,?,?,?)`,
				runID, unitID, p.Container, p.Published, p.Protocol, p.Name, p.Raw, p.Line); err != nil {
				return fmt.Errorf("registry: re-add infra port on %q: %w", u.Name, err)
			}
		}
		for _, l := range u.Limits {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO infra_limits (index_run_id, unit_id, resource, request_qty, limit_qty)
				VALUES (?,?,?,?,?)`,
				runID, unitID, l.Resource, l.Request, l.Limit); err != nil {
				return fmt.Errorf("registry: re-add infra limit on %q: %w", u.Name, err)
			}
		}
		for _, e := range u.EnvFrom {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO infra_env_from (index_run_id, unit_id, kind, name, prefix, optional, start_line)
				VALUES (?,?,?,?,?,?,?)`,
				runID, unitID, e.Kind, e.Name, e.Prefix, boolToInt(e.Optional), e.Line); err != nil {
				return fmt.Errorf("registry: re-add infra envFrom on %q: %w", u.Name, err)
			}
		}
		for _, k := range u.Keys {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO infra_keys
					(index_run_id, file_id, unit_id, key_set, key_set_type, symbol_id,
					 name, from_source, ref, secret, start_line)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				runID, fileID, unitID, "", "", nullID(k.SymbolID),
				k.Name, k.From, k.Ref, boolToInt(k.Secret), k.Line); err != nil {
				return fmt.Errorf("registry: re-add infra key %q: %w", k.Name, err)
			}
		}
	}

	for _, ks := range keySets {
		for _, k := range ks.Keys {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO infra_keys
					(index_run_id, file_id, unit_id, key_set, key_set_type, symbol_id,
					 name, from_source, ref, secret, start_line)
				VALUES (?,?,NULL,?,?,?,?,?,?,?,?)`,
				runID, fileID, ks.Name, ks.Type, nullID(k.SymbolID),
				k.Name, k.From, k.Ref, boolToInt(k.Secret), k.Line); err != nil {
				return fmt.Errorf("registry: re-add infra key %q: %w", k.Name, err)
			}
		}
	}

	for _, p := range problems {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO infra_problems (index_run_id, file_id, format, reason, start_line)
			VALUES (?,?,?,?,?)`,
			runID, fileID, p.Format, p.Reason, p.Line); err != nil {
			return fmt.Errorf("registry: re-add infra problem %q: %w", p.Reason, err)
		}
	}
	return tx.Commit()
}
