package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/planchange"
)

func (s *Server) handlePlanChange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))
	if symbol == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "missing required parameter ?symbol=<name>",
		})
		return
	}

	run, err := s.deps.Registry.CurrentRun(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if run == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"error": "No index run found. Run 'cortex index' first.",
		})
		return
	}

	planner := planchange.NewPlanner(s.deps.Caps)
	plan, err := planner.Plan(ctx, run.ID, symbol, planchange.DefaultOptions())
	if err != nil {
		var amb *capability.AmbiguousTargetError
		switch {
		case errors.As(err, &amb):
			s.writeJSON(w, http.StatusOK, map[string]any{
				"status":     "ambiguous",
				"error":      fmt.Sprintf("%q matches %d definitions. Choose a candidate or re-ask with its fully-qualified name.", symbol, len(amb.Candidates)),
				"candidates": candidatesOf(amb.Candidates),
			})
			return
		case errors.Is(err, capability.ErrTargetNotFound):
			s.writeJSON(w, http.StatusOK, map[string]any{
				"status": "not_found",
				"error":  fmt.Sprintf("No symbol named %q found in this index. Check spelling or try a fully-qualified name.", symbol),
			})
			return
		default:
			s.fail(w, r, err)
			return
		}
	}

	s.writeJSON(w, http.StatusOK, plan)
}
