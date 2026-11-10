package analyze

import (
	"os"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/db"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// The fixture chain this whole milestone exists to answer:
//
//	POST /payment/callback -> handleCallback -> updateStatus
//	  -> TransactionRepository.findById -> SELECT transactions   (READ)
//	  -> TransactionRepository.save     -> UPDATE transactions   (WRITE)
//	                                    -> INSERT transaction_events (WRITE)

func TestFileAttributesSQLToTheEnclosingMethod(t *testing.T) {
	src := []byte(`package com.pay.repository;

@Repository
public class TransactionRepository {

    public Transaction findById(String transactionId) {
        return jdbc.queryForObject(
            "SELECT id, merchant_id, status, amount FROM transactions WHERE id = ?",
            new TransactionMapper(), transactionId);
    }

    public void save(Transaction txn) {
        jdbc.update(
            "UPDATE transactions SET status = ?, updated_at = now() WHERE id = ?",
            txn.getStatus(), txn.getId());
        jdbc.update(
            "INSERT INTO transaction_events (transaction_id, event_type) VALUES (?, ?)",
            txn.getId(), "STATUS_CHANGED");
    }
}`)

	symbols := []model.Symbol{
		{Kind: model.KindClass, Name: "TransactionRepository", FQN: "repo:TransactionRepository",
			Range: rng(4, 25)},
		{Kind: model.KindMethod, Name: "findById", FQN: "repo:findById", Range: rng(6, 10)},
		{Kind: model.KindMethod, Name: "save", FQN: "repo:save", Range: rng(12, 20)},
	}

	res := File("TransactionRepository.java", src, symbols)

	// Attribution to the innermost enclosing definition is the whole point.
	// "TransactionRepository writes transactions" hides which of its methods
	// does; the class is true but useless.
	assertAccess(t, res.Accesses, "transactions", db.AccessRead, "repo:findById")
	assertAccess(t, res.Accesses, "transactions", db.AccessWrite, "repo:save")
	assertAccess(t, res.Accesses, "transaction_events", db.AccessWrite, "repo:save")

	for _, a := range res.Accesses {
		if a.SrcSymbolFQN == "repo:TransactionRepository" {
			t.Errorf("%s %s attributed to the class, not the method that runs it", a.Verb, a.Table)
		}
	}
}

func TestFileDeclaresTablesAndColumnsFromMigrations(t *testing.T) {
	const path = "../../fixtures/payment-platform/src/main/resources/db/migration/V1__create_transactions.sql"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	res := File(path, src, nil)

	tables := map[string]bool{}
	columns := map[string]bool{}
	for _, s := range res.Symbols {
		switch s.Kind {
		case model.KindTable:
			tables[s.Name] = true
		case model.KindColumn:
			columns[s.Name] = true
		}
	}
	for _, want := range []string{"transactions", "transaction_events"} {
		if !tables[want] {
			t.Errorf("migration did not declare table %q; got %v", want, tables)
		}
	}
	for _, want := range []string{"id", "merchant_id", "status", "amount", "created_at", "event_type"} {
		if !columns[want] {
			t.Errorf("migration did not declare column %q; got %v", want, columns)
		}
	}
	// CREATE INDEX names its table but declares no new one.
	if tables["idx_transactions_merchant"] {
		t.Error("an index name was declared as a table")
	}
}

func TestFileExtractsEndpointsAsSymbols(t *testing.T) {
	src := []byte(`@RestController
@RequestMapping("/payment")
public class CallbackController {
    @PostMapping("/callback")
    public ResponseEntity<Void> handleCallback(@RequestBody CallbackRequest r) { return null; }
}`)
	res := File("CallbackController.java", src, nil)

	var found bool
	for _, s := range res.Symbols {
		if s.Kind == model.KindEndpoint && s.Name == "POST /payment/callback" {
			found = true
			if !s.Exported {
				t.Error("an endpoint is the public face of a service and must be exported")
			}
			if s.Confidence == model.ConfidenceExact {
				t.Error("no compiler proved this route; it must not claim exact")
			}
		}
	}
	if !found {
		t.Fatalf("endpoint symbol missing: %v", symbolNames(res.Symbols))
	}
	if len(res.Exposures) != 1 || res.Exposures[0].HandlerName != "handleCallback" {
		t.Errorf("exposure = %+v, want one naming handleCallback", res.Exposures)
	}
}

// TestSQLFileStatementsSurviveSemicolonsInStrings. Splitting on a semicolon
// inside a literal leaves two fragments, neither of which parses, and the table
// disappears with no error anywhere.
func TestSQLFileStatementsSurviveSemicolonsInStrings(t *testing.T) {
	src := []byte(`INSERT INTO audit (note) VALUES ('a;b;c');
CREATE TABLE kept (id INT);`)
	res := File("m.sql", src, nil)

	var sawKept bool
	for _, s := range res.Symbols {
		if s.Kind == model.KindTable && s.Name == "kept" {
			sawKept = true
		}
	}
	if !sawKept {
		t.Errorf("a semicolon inside a string literal split the file; symbols = %v",
			symbolNames(res.Symbols))
	}
}

// TestNoTablesFromProse guards the end-to-end path, not just db.LooksLikeSQL:
// a comment or a log message must not reach the graph as a table.
func TestNoTablesFromProse(t *testing.T) {
	src := []byte(`package main

// We should delete the old records from the archive at some point.
func work() {
	log.Printf("update failed for transaction %s", id)
	fmt.Println("select an option from the menu")
}`)
	res := File("main.go", src, nil)
	if len(res.Accesses) != 0 {
		t.Errorf("prose produced table accesses: %+v", res.Accesses)
	}
	if len(res.Symbols) != 0 {
		t.Errorf("prose produced symbols: %v", symbolNames(res.Symbols))
	}
}

func TestFileIsSafeOnHostileInput(t *testing.T) {
	for _, src := range []string{
		"\"" + repeat("a", 100000),
		repeat("'", 10000),
		repeat("`", 10000),
		"\"\"\"" + repeat("x", 50000),
		repeat("SELECT * FROM t;", 5000),
	} {
		for _, path := range []string{"a.java", "a.go", "a.py", "a.sql"} {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic on %s: %v", path, r)
					}
				}()
				_ = File(path, []byte(src), nil)
			}()
		}
	}
}

func rng(start, end int) model.Range {
	return model.Range{
		Start: model.Position{Line: start},
		End:   model.Position{Line: end},
	}
}

func assertAccess(t *testing.T, accesses []Access, table string, mode db.Access, src string) {
	t.Helper()
	for _, a := range accesses {
		if a.Table == table && a.Mode == mode && a.SrcSymbolFQN == src {
			return
		}
	}
	t.Errorf("no %s access to %q from %q; got %+v", mode, table, src, accesses)
}

func symbolNames(syms []model.Symbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, string(s.Kind)+":"+s.Name)
	}
	return out
}

func repeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}
