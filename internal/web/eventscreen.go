package web

import (
	"context"
	"net/http"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// EventReader supplies the event bus topology for the portal.
type EventReader interface {
	EventTopologyReport(ctx context.Context, runID int64) ([]registry.EventTopology, error)
}

// EventsPayload is what GET /api/events returns.
type EventsPayload struct {
	Total    int                      `json:"total"`
	Topics   []registry.EventTopology `json:"topics"`
	Guidance string                   `json:"guidance"`
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.deps.Registry.CurrentRun(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if run == nil {
		s.writeJSON(w, http.StatusOK, EventsPayload{
			Total:    0,
			Topics:   []registry.EventTopology{},
			Guidance: "No index run found. Run 'repocortex index' to index the repository.",
		})
		return
	}

	er, ok := s.deps.Registry.(EventReader)
	if !ok {
		s.writeJSON(w, http.StatusOK, EventsPayload{
			Total:    0,
			Topics:   []registry.EventTopology{},
			Guidance: "Event topology is not supported by this registry.",
		})
		return
	}

	report, err := er.EventTopologyReport(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	if report == nil {
		report = []registry.EventTopology{}
	}

	guidance := "Discovered message broker topics and queues across Kafka, RabbitMQ, AWS SQS/SNS, and GCP Pub/Sub."
	if len(report) == 0 {
		guidance = "No message broker publishers or consumers discovered in this repository."
	}

	s.writeJSON(w, http.StatusOK, EventsPayload{
		Total:    len(report),
		Topics:   report,
		Guidance: guidance,
	})
}
