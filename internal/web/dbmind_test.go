package web

import (
	"net/http"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- populated

// TestWebDBNamesReadersAndWritersSeparatelyWithTierAndEvidence is the claim the
// screen exists to make: not "what touches payments", but "what WRITES it, and
// where", with the tier that says how much the answer is worth.
func TestWebDBNamesReadersAndWritersSeparatelyWithTierAndEvidence(t *testing.T) {
	t.Parallel()
	srv, _, _ := mindFixture(t)

	var got DBResponse
	rec := get(t, srv, "/api/db", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !got.Meta.Indexed {
		t.Fatal("meta.indexed is false over a committed run")
	}
	if got.Meta.Health.Status == "" {
		t.Error("meta.index_health is absent; every screen must carry the run's verdict on itself")
	}
	if len(got.Legend) != 4 {
		t.Errorf("confidence legend has %d tiers, want 4", len(got.Legend))
	}

	byTable := map[string]TableRow{}
	for _, row := range got.Tables {
		byTable[row.Table] = row
	}

	payments, ok := byTable["payments"]
	if !ok {
		t.Fatalf("payments is missing; tables = %+v", got.Tables)
	}
	if payments.SymbolsWriting != 1 || payments.SymbolsReading != 1 {
		t.Errorf("payments: %d writer(s) / %d reader(s), want 1 / 1", payments.SymbolsWriting, payments.SymbolsReading)
	}
	if len(payments.WrittenBy) != 1 || payments.WrittenBy[0].Name != "charge" {
		t.Fatalf("written_by = %+v, want exactly charge", payments.WrittenBy)
	}
	if len(payments.ReadBy) != 1 || payments.ReadBy[0].Name != "refund" {
		t.Fatalf("read_by = %+v, want exactly refund", payments.ReadBy)
	}

	// The two directions must not be interchangeable: a reader appearing in the
	// writer list is the failure this split exists to prevent.
	for _, w := range payments.WrittenBy {
		if w.Access != "WRITES" {
			t.Errorf("a row in written_by carries access %q", w.Access)
		}
	}
	for _, r := range payments.ReadBy {
		if r.Access != "READS" {
			t.Errorf("a row in read_by carries access %q", r.Access)
		}
	}

	cases := []struct {
		name      string
		accessor  TableAccessor
		wantTier  string
		wantOwner string
	}{
		{"writer", payments.WrittenBy[0], "high", "charge"},
		{"reader", payments.ReadBy[0], "medium", "refund"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.accessor.Name != tc.wantOwner {
				t.Errorf("name = %q, want %q", tc.accessor.Name, tc.wantOwner)
			}
			if tc.accessor.Confidence != tc.wantTier {
				t.Errorf("confidence = %q, want %q — a relationship without its tier is an assertion",
					tc.accessor.Confidence, tc.wantTier)
			}
			if tc.accessor.Evidence.FilePath == "" || tc.accessor.Evidence.Line == 0 {
				t.Errorf("evidence = %+v, want a real file:line — without it the reader cannot check the claim",
					tc.accessor.Evidence)
			}
			if tc.accessor.SymbolID == 0 {
				t.Error("symbol_id is zero; the row cannot be followed to the blast screen")
			}
		})
	}

	// A declared table nothing touches is a FINDING, not a row to drop, and it
	// must carry the sentence saying what its empty accessor list does not mean.
	audit, ok := byTable["audit_log"]
	if !ok {
		t.Fatal("audit_log is missing: a declared table with no accessor above the floor was dropped")
	}
	if audit.SymbolsReading != 0 || audit.SymbolsWriting != 0 {
		t.Errorf("audit_log has accessors at the default floor: %+v", audit)
	}
	if !strings.Contains(audit.Caveat, "not evidence") {
		t.Errorf("audit_log caveat does not state what the absence is not: %q", audit.Caveat)
	}
	if !audit.Declared {
		t.Error("audit_log is declared in db/schema.sql and reports declared_here false")
	}

	// The sub-floor write to audit_log must be counted, not silently dropped.
	hidden := omissionsFor(got.Omissions, ReasonBelowConfidenceFloor)
	if len(hidden) == 0 {
		t.Fatal("the `low` WRITES edge was excluded and no omission records it")
	}
	total := 0
	for _, om := range hidden {
		total += om.Count
		if om.Label == om.Reason {
			t.Errorf("omission label is the raw reason key %q; the API owns the words", om.Reason)
		}
		if om.Remedy == "" {
			t.Error("a floor omission carries no remedy, so the reader cannot undo it")
		}
	}
	if total != 1 {
		t.Errorf("hidden-by-floor total = %d, want 1", total)
	}
	if got.Omissions.Complete {
		t.Error("omissions.complete is true while an edge was hidden")
	}
	if got.Filters.MinConfidence != string(DefaultFloor) {
		t.Errorf("filters.min_confidence = %q, want the applied floor %q", got.Filters.MinConfidence, DefaultFloor)
	}
}

// TestWebDBLoweringTheFloorRecoversTheHiddenWrite proves the omission's remedy
// is real: the same query at `low` returns the row the default hid.
func TestWebDBLoweringTheFloorRecoversTheHiddenWrite(t *testing.T) {
	t.Parallel()
	srv, _, _ := mindFixture(t)

	var got DBResponse
	get(t, srv, "/api/db?min_confidence=low", &got)

	for _, row := range got.Tables {
		if row.Table != "audit_log" {
			continue
		}
		if row.SymbolsWriting != 1 {
			t.Fatalf("audit_log at floor low has %d writer(s), want 1", row.SymbolsWriting)
		}
		if row.WrittenBy[0].Confidence != "low" {
			t.Errorf("recovered writer tier = %q, want low", row.WrittenBy[0].Confidence)
		}
		return
	}
	t.Fatal("audit_log is missing at floor low")
}

// ---------------------------------------------------------------- empty

// TestWebDBOnARepositoryWithNoSQLExplainsItself is the empty case. An empty
// payload with no explanation would read as "this service touches no database",
// which is a claim a static SQL scan cannot make.
func TestWebDBOnARepositoryWithNoSQLExplainsItself(t *testing.T) {
	t.Parallel()
	srv, _, _ := bareFixture(t)

	var got DBResponse
	rec := get(t, srv, "/api/db", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(got.Tables) != 0 {
		t.Fatalf("expected no tables, got %+v", got.Tables)
	}
	if got.Help == nil {
		t.Fatal("an empty table list was returned with no guidance")
	}
	if len(got.Help.Steps) == 0 {
		t.Error("guidance names no next step")
	}
	if !strings.Contains(got.Help.Detail, "not that this repository has no database") {
		t.Errorf("guidance does not distinguish a coverage limit from a finding: %q", got.Help.Detail)
	}
	if got.Coverage == "" {
		t.Error("coverage_limit is empty; it must be present on every answer, not only the empty one")
	}
	if got.Next.Label == "" {
		t.Error("the empty screen ends in no next action")
	}
	// Empty is not the same as absent: the arrays must serialise as arrays.
	body := raw(t, srv, "/api/db")
	if _, ok := body["tables"].([]any); !ok {
		t.Errorf("tables did not serialise as an array: %T", body["tables"])
	}
	if _, ok := body["omissions"].(map[string]any); !ok {
		t.Error("omissions block is missing from an empty response")
	}
}

// TestWebDBWithoutAnIndexSaysSoRatherThanReportingZeroTables separates the two
// empties: no committed run at all, versus a run that found no SQL.
func TestWebDBWithoutAnIndexSaysSoRatherThanReportingZeroTables(t *testing.T) {
	t.Parallel()
	srv := unindexedServer(t)

	var got DBResponse
	get(t, srv, "/api/db", &got)
	if got.Meta.Indexed {
		t.Fatal("meta.indexed is true with no committed run")
	}
	if got.Help == nil || !strings.Contains(got.Help.Title, "no committed index") {
		t.Errorf("guidance does not name the absent index: %+v", got.Help)
	}
	if got.Meta.Health.Trustworthy {
		t.Error("index_health.trustworthy is true with no committed run")
	}
}
