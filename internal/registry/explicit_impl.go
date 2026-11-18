package registry

import (
	"context"
	"fmt"
)

// LinkExplicitImplementations records the method-level IMPLEMENTS edge a C#
// explicit interface implementation owes its interface.
//
// `void I.M() {}` in `class Impl : I` is indexed as the member `Impl.I.M`
// (internal/parse/lang_c_sharp.go; its qualified name stays `Impl.M`), so that no
// unqualified `M()` can bind to it.
// The cost is that it no longer shares a name with `I.M`, which is how the
// query layer pairs an implementation with the method it implements. This
// pass says so directly, the way Go records `Walker.Root IMPLEMENTS
// Rooter.Root`: from the explicit member to the interface's member, when the
// class has an IMPLEMENTS or EXTENDS edge to an interface of that name that
// declares a member of that name. It runs after the type-level edges exist, and
// is idempotent.
//
// The edge is as strong as the type-level edge it rests on, never `exact`
// (nothing here is compiler output). It returns the number of edges added.
func (r *Registry) LinkExplicitImplementations(ctx context.Context, runID int64) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO edges
		       (index_run_id, src_id, dst_id, kind, confidence,
		        evidence_file, evidence_line, evidence_col, source, detail)
		SELECT m.index_run_id, m.id, im.id, 'IMPLEMENTS',
		       CASE WHEN te.confidence = 'exact' THEN 'high' ELSE te.confidence END,
		       f.path, m.start_line, m.start_col, 'source_code',
		       'explicit interface implementation'
		  FROM files f
		 CROSS JOIN symbols m ON m.file_id = f.id
		 CROSS JOIN symbols o ON o.index_run_id = m.index_run_id
		       AND o.fqn = substr(m.fqn, 1, length(m.fqn) - length(substr(m.name, instr(m.name, '.') + 1)) - 1)
		 CROSS JOIN edges te ON te.src_id = o.id
		       AND te.index_run_id = m.index_run_id
		       AND te.kind IN ('IMPLEMENTS', 'EXTENDS')
		 CROSS JOIN symbols t ON t.id = te.dst_id
		       AND t.kind = 'interface'
		       AND t.name = substr(m.name, 1, instr(m.name, '.') - 1)
		 CROSS JOIN symbols im ON im.index_run_id = m.index_run_id
		       AND im.fqn = t.fqn || '.' || substr(m.name, instr(m.name, '.') + 1)
		       AND im.kind = 'method'
		 WHERE f.index_run_id = ?
		   AND f.language = 'c_sharp'
		   AND m.kind = 'method'
		   AND instr(m.name, '.') > 0
		   AND o.id <> t.id`, runID)
	if err != nil {
		return 0, fmt.Errorf("registry: link explicit implementations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
