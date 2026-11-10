package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func eventsFixture(t *testing.T) string {
	t.Helper()
	root := isolate(t)

	files := map[string]string{
		"producer.go": `package main

func EmitEvents() {
	msg := &sarama.ProducerMessage{
		Topic: "user.signups",
	}
}
`,
		"consumer.go": `package main

func HandleEvents() {
	consumer.subscribe(&kafka.Consumer{
		Topic: "user.signups",
	})
}
`,
	}

	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return root
}

func TestEventsCmd(t *testing.T) {
	root := eventsFixture(t)

	// 1. Text output
	out, err := run(t, root, "events")
	if err != nil {
		t.Fatalf("events: %v\n%s", err, out)
	}
	if !strings.Contains(out, "user.signups") {
		t.Errorf("missing topic in output:\n%s", out)
	}
	if !strings.Contains(out, "kafka") {
		t.Errorf("missing broker in output:\n%s", out)
	}

	// 2. JSON output
	jsonOut, err := run(t, root, "events", "--json")
	if err != nil {
		t.Fatalf("events --json: %v\n%s", err, jsonOut)
	}
	var data []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &data); err != nil {
		t.Fatalf("failed to parse json output: %v\n%s", err, jsonOut)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 event topic, got %d", len(data))
	}
	if data[0]["topic"] != "user.signups" {
		t.Errorf("expected topic user.signups, got %v", data[0]["topic"])
	}

	// 3. Topic details
	topicOut, err := run(t, root, "events", "topic", "user.signups")
	if err != nil {
		t.Fatalf("events topic user.signups: %v\n%s", err, topicOut)
	}
	if !strings.Contains(topicOut, "EmitEvents") {
		t.Errorf("missing producer EmitEvents in topic details:\n%s", topicOut)
	}
	if !strings.Contains(topicOut, "HandleEvents") {
		t.Errorf("missing consumer HandleEvents in topic details:\n%s", topicOut)
	}

	// 4. Topic details JSON
	topicJSON, err := run(t, root, "events", "topic", "user.signups", "--json")
	if err != nil {
		t.Fatalf("events topic user.signups --json: %v\n%s", err, topicJSON)
	}
	var single map[string]any
	if err := json.Unmarshal([]byte(topicJSON), &single); err != nil {
		t.Fatalf("failed to parse topic json: %v\n%s", err, topicJSON)
	}
	if single["topic"] != "user.signups" {
		t.Errorf("expected topic user.signups, got %v", single["topic"])
	}

	// 5. Negative case: Non-existent topic
	_, err = run(t, root, "events", "topic", "nonexistent_topic")
	if err == nil {
		t.Fatal("expected error on nonexistent topic, got nil")
	}
}

func TestEventsCmdNegativeEmpty(t *testing.T) {
	root := isolate(t)
	full := filepath.Join(root, "main.go")
	if err := os.WriteFile(full, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	out, err := run(t, root, "events")
	if err != nil {
		t.Fatalf("events on empty: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No event bus publishers or consumers found") {
		t.Errorf("expected no events found message, got:\n%s", out)
	}
}
