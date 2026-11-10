package parse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixtureExtraction runs the parser over the payment-platform fixture.
//
// Unit tests use hand-written snippets, which flatter a parser: real files have
// annotations, imports, generics, javadoc and nesting. This asserts that the
// entities the golden context test depends on (spec §43) are actually
// extractable before anything downstream is built on that assumption.
func TestFixtureExtraction(t *testing.T) {
	root := fixtureRoot(t)
	reg := NewRegistry()

	// symbol name -> files it was found in
	found := map[string][]string{}
	var parsed, failed int

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		p := reg.ForFile(path)
		if p == nil {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		res, err := p.Parse(context.Background(), path, src)
		if err != nil {
			t.Errorf("%s: run-level error: %v", path, err)
			return nil
		}
		parsed++
		if res.Status == StatusFailed {
			failed++
			t.Errorf("%s: parse failed: %v", path, res.Err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, s := range res.Symbols {
			found[s.Name] = append(found[s.Name], filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("parsed %d files, %d failures, %d distinct symbols", parsed, failed, len(found))
	if parsed == 0 {
		t.Fatal("fixture produced no parseable files")
	}

	// The entities the golden task context must be able to surface.
	required := []string{
		"CallbackController", "handleCallback",
		"TransactionService", "updateStatus",
		"TransactionRepository", "findById", "save",
		"NotificationService", "send",
		"CallbackControllerTest", "TransactionServiceTest",
	}
	for _, want := range required {
		if len(found[want]) == 0 {
			t.Errorf("golden entity %q not extracted from the fixture", want)
		}
	}

	// The unrelated module must be indexable but distinguishable — the golden
	// test asserts it is EXCLUDED from task context by relevance, not by being
	// invisible to the indexer.
	if len(found["MarketingReport"]) == 0 {
		t.Error("MarketingReport should still be indexed (exclusion is a ranking decision, not an indexing one)")
	}
	for _, f := range found["MarketingReport"] {
		if !strings.Contains(f, "unrelated/") {
			t.Errorf("MarketingReport found outside unrelated/: %s", f)
		}
	}
}

func TestFixtureSQLIsDetected(t *testing.T) {
	root := fixtureRoot(t)
	sqlPath := filepath.Join(root, "src/main/resources/db/migration/V1__create_transactions.sql")
	src, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Skipf("fixture SQL missing: %v", err)
	}
	p := NewRegistry().ForFile(sqlPath)
	if p == nil {
		t.Fatal("no parser for .sql — DBMind will need one")
	}
	res, err := p.Parse(context.Background(), sqlPath, src)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sql: status=%s symbols=%d", res.Status, len(res.Symbols))
	for _, s := range res.Symbols {
		t.Logf("  %s (%s)", s.Name, s.Kind)
	}
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "fixtures", "payment-platform")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("fixture not present: %v", err)
	}
	return root
}
