package registry

import (
	"context"
	"fmt"
	"path"
)

// OwnPackage is a package the repository declares for itself: an R package's
// DESCRIPTION, as the directory it sits in and the name its Package: field
// gives.
type OwnPackage struct {
	Name string
	Dir  string
}

// OwnPackages lists the packages the run's repository declares, ordered by
// path so that which of two same-named ones is read first never depends on the
// order files were indexed in.
func (r *Registry) OwnPackages(ctx context.Context, runID int64) ([]OwnPackage, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT s.name, f.path FROM symbols s JOIN files f ON f.id = s.file_id
		  WHERE s.index_run_id = ? AND s.kind = 'package' AND s.fqn = 'rpackage:' || s.name
		  ORDER BY f.path, s.name`, runID)
	if err != nil {
		return nil, fmt.Errorf("registry: own packages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OwnPackage
	for rows.Next() {
		var name, p string
		if err := rows.Scan(&name, &p); err != nil {
			return nil, err
		}
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		out = append(out, OwnPackage{Name: name, Dir: dir})
	}
	return out, rows.Err()
}
