package context

import (
	"context"
	"strings"
	"testing"
)

func TestExtractTablesFromEmbeddedSQL(t *testing.T) {
	const java = `
public void save(Transaction txn) {
    jdbc.update(
        "UPDATE transactions SET status = ?, updated_at = now() WHERE id = ?",
        txn.getStatus(), txn.getId());
    jdbc.update(
        "INSERT INTO transaction_events (transaction_id, event_type) VALUES (?, ?)",
        txn.getId(), "STATUS_CHANGED");
}
public Transaction findById(String id) {
    return jdbc.queryForObject("SELECT id, status FROM transactions WHERE id = ?", id);
}`

	got := map[string]bool{}
	for _, ref := range extractTables("java", java) {
		got[ref.Name] = ref.Write
		if ref.Line <= 0 {
			t.Errorf("table %q has no line", ref.Name)
		}
	}
	if write, ok := got["transactions"]; !ok || !write {
		t.Errorf("transactions: got (write=%v, found=%v), want written", write, ok)
	}
	if write, ok := got["transaction_events"]; !ok || !write {
		t.Errorf("transaction_events: got (write=%v, found=%v), want written", write, ok)
	}
	if len(got) != 2 {
		t.Errorf("extracted %v, want exactly the two tables", got)
	}
}

// TestExtractTablesIgnoresHostLanguageSyntax is why the scan is restricted to
// string literals. `for ... range`, Python's `from x import y` and Java's
// `update(...)` all contain words this extractor looks for; none of them is SQL.
func TestExtractTablesIgnoresHostLanguageSyntax(t *testing.T) {
	const notSQL = `
from collections import OrderedDict
for row in rows:
    update(row)
    analytics.aggregate("campaign_clicks")
    writer.write(path)
`
	if got := extractTables("python", notSQL); len(got) != 0 {
		t.Errorf("extracted %+v from host-language syntax, want nothing", got)
	}
}

func TestExtractTablesFromMigration(t *testing.T) {
	const migration = `
CREATE TABLE transactions (
    id VARCHAR(64) PRIMARY KEY
);
CREATE INDEX idx_transactions_status ON transactions (status);
CREATE TABLE transaction_events (
    transaction_id VARCHAR(64) NOT NULL REFERENCES transactions(id)
);`

	got := map[string]bool{}
	for _, ref := range extractTables("sql", migration) {
		got[ref.Name] = true
	}
	if !got["transactions"] || !got["transaction_events"] {
		t.Errorf("extracted %v, want both tables", got)
	}
	// CREATE INDEX names an index, not a table, and must not become an item.
	if got["idx_transactions_status"] {
		t.Error("an index name was reported as a table")
	}
}

// TestTablesCarryTheirRelationship checks the fact that actually matters for an
// idempotency task: which side of the flow WRITES.
func TestTablesCarryTheirRelationship(t *testing.T) {
	reg, g, runID := paymentFixture(t)

	pack, err := NewAssembler().Assemble(context.Background(), reg, g, runID, Request{Task: goldenTask})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range pack.Items {
		if it.Kind != "table" {
			continue
		}
		if !contains([]string{"transactions", "transaction_events"}, it.Name) {
			t.Errorf("unexpected table %q in the pack", it.Name)
		}
		if !strings.Contains(it.Why, "writes") && !strings.Contains(it.Why, "TransactionRepository") {
			t.Errorf("table %s does not say who touches it: %s", it.Name, it.Why)
		}
		if it.Confidence == "" {
			t.Errorf("table %s carries no confidence; SQL-literal extraction is a heuristic and must say so", it.Name)
		}
	}
}
