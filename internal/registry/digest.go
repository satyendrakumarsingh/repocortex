package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
)

// digestVersion prefixes every digest.
//
// It is part of the hashed input, so changing what the digest covers changes
// every digest — an old value can never be silently compared against a new
// scheme and appear to match. Bump it whenever the field set below changes.
const digestVersion = "github.com/satyendrakumarsingh/repocortex/registry-digest/v1"

// RegistryDigest returns a stable sha256 over the content of one run.
//
// This is the determinism gate. Indexing the same tree twice must produce the
// same digest; if it does not, something in the pipeline depends on goroutine
// scheduling, map iteration order, or filesystem walk order, and the index is
// not reproducible. That class of bug is close to undebuggable once it reaches
// a user, so the digest is designed to catch it cheaply.
//
// Three properties make it work:
//
//   - Order independence. Rows are read in an explicit, total ORDER BY over
//     content columns, not by rowid, so the order rows happened to be inserted
//     in cannot reach the hash.
//
//   - No synthetic identity. Autoincrement ids are excluded outright. Edges
//     reference their endpoints by (file path, fqn, start line, start col) —
//     the symbol's content identity — rather than by src_id/dst_id, which are
//     allocation artefacts that legitimately differ between two identical runs.
//
//   - No timestamps. started/finished are per-run by definition, and file
//     mod_time_unix differs after a fresh clone of the same commit. Content is
//     covered by content_hash and size instead.
//
// Fields are hashed length-prefixed rather than joined with a separator, so no
// value containing the separator can be crafted to collide with a different
// row (a doc comment is attacker-controlled content in a hostile repository).
func (r *Registry) RegistryDigest(ctx context.Context, runID int64) (string, error) {
	return digest(ctx, r.db, runID)
}

// CommittedDigest returns the digest recorded for the currently committed run.
func (r *Registry) CommittedDigest(ctx context.Context) (string, error) {
	var dg sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT registry_digest FROM index_runs WHERE committed = 1`).Scan(&dg)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoCommittedRun
	}
	if err != nil {
		return "", fmt.Errorf("registry: read committed digest: %w", err)
	}
	return dg.String, nil
}

func digest(ctx context.Context, q queryer, runID int64) (string, error) {
	h := sha256.New()
	hashStr(h, digestVersion)

	if err := digestFiles(ctx, q, runID, h); err != nil {
		return "", err
	}
	if err := digestSymbols(ctx, q, runID, h); err != nil {
		return "", err
	}
	if err := digestEdges(ctx, q, runID, h); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Each ORDER BY below sorts on *every* field that is subsequently hashed, in
// the same sequence.
//
// A shorter key would be enough today — migration 0001's UNIQUE constraints
// already make (path), (path, fqn, start_line) and the edge tuple unique — but
// relying on that would make the determinism gate quietly dependent on a
// constraint declared in another package. Sorting the full tuple means the only
// rows a query may return in an arbitrary relative order are rows that are
// byte-identical in every hashed field, and those hash the same whichever order
// they arrive in. Determinism then holds unconditionally. Measured cost of the
// longer sort keys: under 1% of digest time, which is itself a fraction of a
// commit.
//
// Nullable columns are wrapped in the same COALESCE in the ORDER BY as in the
// SELECT. Sorting the raw column while hashing the coalesced one would break
// the argument above: SQLite orders NULL before '', so a run that recorded a
// missing doc comment as NULL and one that recorded it as '' could order the
// same content differently while hashing each row identically.
//
// Endpoint ids are replaced throughout by the endpoint's content identity
// (file path, fqn, position), because src_id/dst_id are allocation artefacts.
// TEXT columns here carry no COLLATE clause, so SQLite compares them byte-wise:
// stable across platforms and locales.

func digestFiles(ctx context.Context, q queryer, runID int64, h hash.Hash) error {
	rows, err := q.QueryContext(ctx, `
		SELECT path, language, content_hash, size_bytes, parse_status, COALESCE(parse_error,'')
		  FROM files
		 WHERE index_run_id = ?
		 ORDER BY path, language, content_hash, size_bytes, parse_status, COALESCE(parse_error,'')`, runID)
	if err != nil {
		return fmt.Errorf("registry: digest files: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var path, lang, chash, status, perr string
		var size int64
		if err := rows.Scan(&path, &lang, &chash, &size, &status, &perr); err != nil {
			return fmt.Errorf("registry: digest files: scan: %w", err)
		}
		hashStr(h, "F")
		hashStr(h, path)
		hashStr(h, lang)
		hashStr(h, chash)
		hashInt(h, size)
		hashStr(h, status)
		hashStr(h, perr)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("registry: digest files: %w", err)
	}
	return nil
}

func digestSymbols(ctx context.Context, q queryer, runID int64, h hash.Hash) error {
	rows, err := q.QueryContext(ctx, `
		SELECT f.path, s.fqn, s.start_line, s.start_col,
		       s.kind, s.name, s.end_line, s.end_col,
		       COALESCE(s.signature,''), COALESCE(s.doc,''), s.exported, s.confidence, s.source
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ?
		 ORDER BY f.path, s.fqn, s.start_line, s.start_col,
		          s.kind, s.name, s.end_line, s.end_col,
		          COALESCE(s.signature,''), COALESCE(s.doc,''),
		          s.exported, s.confidence, s.source`, runID)
	if err != nil {
		return fmt.Errorf("registry: digest symbols: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var path, fqn, kind, name, sig, doc, conf, src string
		var startLine, startCol, endLine, endCol, exported int64
		if err := rows.Scan(&path, &fqn, &startLine, &startCol, &kind, &name,
			&endLine, &endCol, &sig, &doc, &exported, &conf, &src); err != nil {
			return fmt.Errorf("registry: digest symbols: scan: %w", err)
		}
		hashStr(h, "S")
		hashStr(h, path)
		hashStr(h, fqn)
		hashInt(h, startLine)
		hashInt(h, startCol)
		hashStr(h, kind)
		hashStr(h, name)
		hashInt(h, endLine)
		hashInt(h, endCol)
		hashStr(h, sig)
		hashStr(h, doc)
		hashInt(h, exported)
		hashStr(h, conf)
		hashStr(h, src)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("registry: digest symbols: %w", err)
	}
	return nil
}

func digestEdges(ctx context.Context, q queryer, runID int64, h hash.Hash) error {
	rows, err := q.QueryContext(ctx, `
		SELECT sf.path, ss.fqn, ss.start_line, ss.start_col,
		       df.path, ds.fqn, ds.start_line, ds.start_col,
		       e.kind, e.confidence, e.evidence_file, e.evidence_line, e.evidence_col,
		       e.source, COALESCE(e.detail,'')
		  FROM edges e
		  JOIN symbols ss ON ss.id = e.src_id
		  JOIN files   sf ON sf.id = ss.file_id
		  JOIN symbols ds ON ds.id = e.dst_id
		  JOIN files   df ON df.id = ds.file_id
		 WHERE e.index_run_id = ?
		 ORDER BY sf.path, ss.fqn, ss.start_line, ss.start_col,
		          df.path, ds.fqn, ds.start_line, ds.start_col,
		          e.kind, e.confidence, e.evidence_file, e.evidence_line, e.evidence_col,
		          e.source, COALESCE(e.detail,'')`, runID)
	if err != nil {
		return fmt.Errorf("registry: digest edges: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var srcPath, srcFQN, dstPath, dstFQN, kind, conf, evFile, src, detail string
		var srcLine, srcCol, dstLine, dstCol, evLine, evCol int64
		if err := rows.Scan(&srcPath, &srcFQN, &srcLine, &srcCol,
			&dstPath, &dstFQN, &dstLine, &dstCol,
			&kind, &conf, &evFile, &evLine, &evCol, &src, &detail); err != nil {
			return fmt.Errorf("registry: digest edges: scan: %w", err)
		}
		hashStr(h, "E")
		hashStr(h, srcPath)
		hashStr(h, srcFQN)
		hashInt(h, srcLine)
		hashInt(h, srcCol)
		hashStr(h, dstPath)
		hashStr(h, dstFQN)
		hashInt(h, dstLine)
		hashInt(h, dstCol)
		hashStr(h, kind)
		hashStr(h, conf)
		hashStr(h, evFile)
		hashInt(h, evLine)
		hashInt(h, evCol)
		hashStr(h, src)
		hashStr(h, detail)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("registry: digest edges: %w", err)
	}
	return nil
}

// hashStr writes a length-prefixed string. The prefix is what makes the
// encoding injective: without it ("ab","c") and ("a","bc") hash identically,
// and a crafted symbol name could make two different graphs share a digest.
func hashStr(h hash.Hash, s string) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(len(s)))
	_, _ = h.Write(buf[:n])
	_, _ = h.Write([]byte(s))
}

func hashInt(h hash.Hash, v int64) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint(buf[:], v)
	_, _ = h.Write(buf[:n])
}
