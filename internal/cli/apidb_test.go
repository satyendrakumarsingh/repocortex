package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// apidbFixture builds a miniature service with the shape M7 exists to answer:
// an endpoint, a handler, a repository running literal SQL, and a migration
// declaring the tables it touches.
func apidbFixture(t *testing.T) string {
	t.Helper()
	root := isolate(t)

	files := map[string]string{
		"src/CallbackController.java": `package com.pay.controller;

@RestController
@RequestMapping("/payment")
public class CallbackController {

    @PostMapping("/callback")
    public ResponseEntity<Void> handleCallback(@RequestBody CallbackRequest r) {
        return ResponseEntity.ok().build();
    }
}`,
		"src/TransactionRepository.java": `package com.pay.repository;

@Repository
public class TransactionRepository {

    public Transaction findById(String id) {
        return jdbc.queryForObject(
            "SELECT id, merchant_id, status FROM transactions WHERE id = ?",
            new TransactionMapper(), id);
    }

    public void save(Transaction txn) {
        jdbc.update("UPDATE transactions SET status = ? WHERE id = ?", txn.getStatus(), txn.getId());
        jdbc.update("INSERT INTO transaction_events (transaction_id, event_type) VALUES (?, ?)",
            txn.getId(), "CHANGED");
    }

    public void external() {
        jdbc.update("INSERT INTO audit_log (actor, action) VALUES (?, ?)", "sys", "x");
    }
}`,
		"db/V1__init.sql": `CREATE TABLE transactions (
    id          VARCHAR(64) PRIMARY KEY,
    merchant_id VARCHAR(64) NOT NULL,
    status      VARCHAR(32) NOT NULL
);

CREATE TABLE transaction_events (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id VARCHAR(64) NOT NULL,
    event_type     VARCHAR(32) NOT NULL
);`,
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

func TestAPIListsEndpointsWithHandlers(t *testing.T) {
	root := apidbFixture(t)

	out, err := run(t, root, "api")
	if err != nil {
		t.Fatalf("api: %v\n%s", err, out)
	}
	if !strings.Contains(out, "POST /payment/callback") {
		t.Errorf("endpoint missing; the class-level @RequestMapping prefix may be dropped:\n%s", out)
	}
	if !strings.Contains(out, "handleCallback") {
		t.Errorf("handler not attributed:\n%s", out)
	}
	// Coverage must be stated. "No endpoints" and "no endpoints we can read"
	// are different facts and a reader has to be able to tell them apart.
	if !strings.Contains(out, "spring") {
		t.Errorf("supported frameworks not reported:\n%s", out)
	}
}

func TestDBSeparatesReadsFromWrites(t *testing.T) {
	root := apidbFixture(t)

	out, err := run(t, root, "db")
	if err != nil {
		t.Fatalf("db: %v\n%s", err, out)
	}
	for _, want := range []string{"transactions", "transaction_events", "audit_log"} {
		if !strings.Contains(out, want) {
			t.Errorf("table %q missing:\n%s", want, out)
		}
	}
	// audit_log is queried but declared by no migration here. Dropping it would
	// make "which tables do we touch that we do not own" unanswerable.
	if !strings.Contains(out, "not declared in this repository") {
		t.Errorf("an externally-owned table was not flagged as such:\n%s", out)
	}
}

// TestDBTableNamesTheWriter is the question this milestone exists for: not
// "does anything use this table", but "what exactly writes it, and where".
func TestDBTableNamesTheWriter(t *testing.T) {
	root := apidbFixture(t)

	out, err := run(t, root, "db", "table", "transactions")
	if err != nil {
		t.Fatalf("db table: %v\n%s", err, out)
	}
	if !strings.Contains(out, "WRITTEN BY") || !strings.Contains(out, "save") {
		t.Errorf("the writing method was not named:\n%s", out)
	}
	if !strings.Contains(out, "READ BY") || !strings.Contains(out, "findById") {
		t.Errorf("the reading method was not named:\n%s", out)
	}
	// Attribution must be to the METHOD, not the class that contains it.
	if strings.Contains(out, "TransactionRepository ") {
		t.Errorf("attributed to the class rather than the method:\n%s", out)
	}
	// Every claim carries the statement that justifies it.
	if !strings.Contains(out, "UPDATE") || !strings.Contains(out, "SELECT") {
		t.Errorf("no SQL verb cited as evidence:\n%s", out)
	}
}

func TestAPIAndDBJSON(t *testing.T) {
	root := apidbFixture(t)

	for _, tc := range []struct {
		args []string
		key  string
	}{
		{[]string{"api", "--json"}, "endpoints"},
		{[]string{"db", "--json"}, "tables"},
		{[]string{"db", "table", "transactions", "--json"}, "writers"},
	} {
		out, err := run(t, root, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%v emitted non-JSON: %v\n%s", tc.args, err, out)
		}
		if _, ok := got[tc.key]; !ok {
			t.Errorf("%v: JSON missing %q", tc.args, tc.key)
		}
	}
}

// TestEmptyResultsExplainCoverage. Both commands can be silent for two very
// different reasons, and a reader must be able to tell which.
func TestEmptyResultsExplainCoverage(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, "plain.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	out, err := run(t, root, "api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "coverage limit") {
		t.Errorf("an empty API report does not distinguish 'exposes nothing' from "+
			"'uses something we cannot read':\n%s", out)
	}

	out, err = run(t, root, "db")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "coverage limit") {
		t.Errorf("an empty DB report does not state its limits:\n%s", out)
	}
}

func TestDBTableRejectsUnknownTable(t *testing.T) {
	root := apidbFixture(t)
	_, err := run(t, root, "db", "table", "nope")
	if err == nil {
		t.Fatal("an unknown table was accepted")
	}
	if !strings.Contains(err.Error(), "known tables include") {
		t.Errorf("the error should list what IS known; got %q", err)
	}
}
