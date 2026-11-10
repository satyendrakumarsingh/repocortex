package web

import (
	"net/http"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

func (s *Server) handleMissed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.deps.Registry.CurrentRun(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if run == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"total_blindspots": 0,
			"blindspots":       []any{},
			"guidance":         "No index run found. Run 'cortex index' first.",
		})
		return
	}

	reg, ok := s.deps.Registry.(*registry.Registry)
	if !ok {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"total_blindspots": 0,
			"blindspots":       []any{},
			"guidance":         "Registry implementation does not support blindspot detection.",
		})
		return
	}

	detector := missed.NewDetector(reg, s.deps.Caps)
	opts := missed.DefaultOptions()
	opts.MaxItemsPerKind = 100

	report, err := detector.Detect(ctx, run.ID, opts)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusOK, report)
}
