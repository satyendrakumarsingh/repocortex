package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestEventTopologyPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Multi-service repository with publisher and consumer
	tree := map[string]string{
		"services/order/publisher.go": `package order

func PublishOrderCreated() error {
	msg := &sarama.ProducerMessage{
		Topic: "orders.created",
	}
	return nil
}
`,
		"services/notifications/consumer.go": `package notifications

func HandleOrders() {
	consumer.subscribe(&kafka.Consumer{
		Topic: "orders.created",
	})
}
`,
		"services/billing/payment.go": `package billing

func ProcessPayment() {
	// ordinary function with no event bindings
}
`,
	}
	writeTree(t, root, tree)

	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))

	// 1. Verify EventTopologyReport
	report, err := reg.EventTopologyReport(ctx, runID)
	if err != nil {
		t.Fatalf("EventTopologyReport: %v", err)
	}

	if len(report) != 1 {
		t.Fatalf("expected 1 unified event topic, got %d", len(report))
	}

	top := report[0]
	if top.Topic != "orders.created" {
		t.Errorf("expected topic 'orders.created', got %q", top.Topic)
	}
	if top.Broker != "kafka" {
		t.Errorf("expected broker 'kafka', got %q", top.Broker)
	}
	if len(top.Producers) != 1 {
		t.Fatalf("expected 1 producer, got %d: %+v", len(top.Producers), top.Producers)
	}
	if len(top.Consumers) != 1 {
		t.Fatalf("expected 1 consumer, got %d: %+v", len(top.Consumers), top.Consumers)
	}

	if top.Producers[0].SymbolName != "PublishOrderCreated" {
		t.Errorf("expected producer function 'PublishOrderCreated', got %q", top.Producers[0].SymbolName)
	}
	if top.Consumers[0].SymbolName != "HandleOrders" {
		t.Errorf("expected consumer function 'HandleOrders', got %q", top.Consumers[0].SymbolName)
	}

	// 2. Verify graph edges: exactly one PUBLISHES edge and one CONSUMES edge
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}

	var pubEdges, conEdges []model.Edge
	for _, e := range edges {
		if e.Kind == model.EdgePublishes {
			pubEdges = append(pubEdges, e)
		}
		if e.Kind == model.EdgeConsumes {
			conEdges = append(conEdges, e)
		}
	}

	if len(pubEdges) != 1 {
		t.Errorf("expected 1 PUBLISHES edge, got %d", len(pubEdges))
	}
	if len(conEdges) != 1 {
		t.Errorf("expected 1 CONSUMES edge, got %d", len(conEdges))
	}

	// 3. Verify that both edges point to the SAME unified canonical DstID
	if len(pubEdges) > 0 && len(conEdges) > 0 {
		if pubEdges[0].DstID != conEdges[0].DstID {
			t.Errorf("expected unified event symbol ID, got pub DstID=%d vs con DstID=%d",
				pubEdges[0].DstID, conEdges[0].DstID)
		}
		if pubEdges[0].DstID != top.SymbolID {
			t.Errorf("expected edge DstID to match report SymbolID %d, got %d",
				top.SymbolID, pubEdges[0].DstID)
		}
	}
}

func TestEventTopologyPipelineNegativeEmpty(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	tree := map[string]string{
		"main.go": `package main
func main() {}
`,
	}
	writeTree(t, root, tree)

	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))

	report, err := reg.EventTopologyReport(ctx, runID)
	if err != nil {
		t.Fatalf("EventTopologyReport: %v", err)
	}

	if len(report) != 0 {
		t.Errorf("expected 0 event topics, got %d", len(report))
	}
}
