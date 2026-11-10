package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestWebConfigClassifiesEveryKeyIntoOneFinding is the table the screen turns
// on: a key read and declared nowhere is a start-up failure, a key declared and
// read by nothing is dead configuration or an invisible lookup, and the two
// must never share a bucket.
func TestWebConfigClassifiesEveryKeyIntoOneFinding(t *testing.T) {
	t.Parallel()

	keys := []registry.ConfigKeyUsage{
		{SymbolID: 7, Name: "DATABASE_URL", FilePath: ".env", Line: 1, Readers: 3, Declared: true},
		{SymbolID: 8, Name: "API_SECRET_KEY", FilePath: ".env", Line: 2, Readers: 0, Declared: true, Secret: true},
		{Name: "STRIPE_TOKEN", FilePath: "internal/billing/charge.go", Line: 42, Readers: 2, Declared: false, Secret: true},
		{Name: "FEATURE_FLAG_X", FilePath: "internal/web/flags.go", Line: 9, Readers: 1, Declared: false},
	}
	_, deps, _ := mindFixture(t)
	srv := newServerWithConfigKeys(t, deps, keys)

	var got ConfigResponse
	rec := get(t, srv, "/api/config", &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	byName := map[string]ConfigKeyRow{}
	for _, k := range got.Keys {
		byName[k.Name] = k
	}

	cases := []struct {
		key           string
		wantFinding   string
		wantSensitive bool
		wantEvidence  string
	}{
		{"DATABASE_URL", ConfigDeclaredAndRead, false, "the declaration"},
		{"API_SECRET_KEY", ConfigDeclaredNotRead, true, "the declaration"},
		{"STRIPE_TOKEN", ConfigReadNotDeclared, true, "read site"},
		{"FEATURE_FLAG_X", ConfigReadNotDeclared, false, "read site"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			row, ok := byName[tc.key]
			if !ok {
				t.Fatalf("%s is missing from the inventory", tc.key)
			}
			if row.Finding != tc.wantFinding {
				t.Errorf("finding = %q, want %q", row.Finding, tc.wantFinding)
			}
			if row.Sensitive != tc.wantSensitive {
				t.Errorf("sensitive_by_name = %v, want %v", row.Sensitive, tc.wantSensitive)
			}
			if row.Evidence.FilePath == "" || row.Evidence.Line == 0 {
				t.Errorf("evidence = %+v, want a real file:line", row.Evidence)
			}
			if !strings.Contains(row.EvidenceIs, tc.wantEvidence) {
				t.Errorf("evidence_is = %q, want it to name %q — the same file:line means two "+
					"different things depending on `declared`", row.EvidenceIs, tc.wantEvidence)
			}
		})
	}

	// The two mismatch lists lead, and they must agree with the inventory.
	if got.Counts.ReadNotDeclared != 2 {
		t.Errorf("counts.read_but_not_declared = %d, want 2", got.Counts.ReadNotDeclared)
	}
	if len(got.ReadNotDeclared) != 2 {
		t.Errorf("read_but_not_declared has %d row(s), want 2", len(got.ReadNotDeclared))
	}
	if got.Counts.DeclaredNotRead != 1 || len(got.DeclaredNotRead) != 1 {
		t.Errorf("declared_but_never_read = %d / %d rows, want 1 / 1",
			got.Counts.DeclaredNotRead, len(got.DeclaredNotRead))
	}
	if got.Counts.SensitiveByName != 2 {
		t.Errorf("counts.sensitive_by_name = %d, want 2", got.Counts.SensitiveByName)
	}
	if got.Counts.Keys != len(keys) {
		t.Errorf("counts.keys = %d, want %d", got.Counts.Keys, len(keys))
	}

	// The guarantee is in the payload, not only in the docs.
	if !strings.Contains(got.ValuePolicy, "never read") {
		t.Errorf("value_policy does not state the guarantee: %q", got.ValuePolicy)
	}
	if !strings.Contains(got.Coverage, "Kubernetes") {
		t.Errorf("coverage_limit does not name the sources that are NOT read: %q", got.Coverage)
	}
	if got.Next.Label == "" {
		t.Error("the screen ends in no next action")
	}

	// Read sites are counted, not listed with evidence — and that has to be
	// stated rather than left as an unexplained absence.
	ev := omissionsFor(got.Omissions, ReasonEvidenceBudget)
	if len(ev) != 1 {
		t.Fatalf("expected one evidence-budget omission for the unlisted read sites, got %d", len(ev))
	}
	if ev[0].Count != 6 {
		t.Errorf("evidence-budget count = %d, want 6 (3+2+1 read sites)", ev[0].Count)
	}
}

// TestWebConfigCarriesTheTierOnEveryConfiguresEdge is the relationship half:
// an infrastructure unit that declares it is configured by a key is a real edge
// and must arrive with its tier and its file:line.
func TestWebConfigCarriesTheTierOnEveryConfiguresEdge(t *testing.T) {
	t.Parallel()
	srv, _, _ := mindFixture(t)

	var got ConfigResponse
	get(t, srv, "/api/config", &got)

	var found bool
	for _, key := range got.Keys {
		if key.Name != "DATABASE_URL" {
			continue
		}
		if len(key.ConfiguredBy) != 1 {
			t.Fatalf("DATABASE_URL configured_by = %+v, want exactly the compose service", key.ConfiguredBy)
		}
		c := key.ConfiguredBy[0]
		found = true
		if c.Name != "api" {
			t.Errorf("consumer name = %q, want api", c.Name)
		}
		if c.Relation != string(model.EdgeConfigures) {
			t.Errorf("relation = %q, want the recorded edge kind %q", c.Relation, model.EdgeConfigures)
		}
		if c.Confidence != "high" {
			t.Errorf("confidence = %q, want high", c.Confidence)
		}
		if c.Evidence.FilePath == "" || c.Evidence.Line == 0 {
			t.Errorf("evidence = %+v, want a real file:line", c.Evidence)
		}
	}
	if !found {
		t.Fatalf("DATABASE_URL is missing from the inventory: %+v", got.Keys)
	}
	if got.Counts.ConfiguresEdges != 1 {
		t.Errorf("counts.configures_edges = %d, want 1", got.Counts.ConfiguresEdges)
	}
}

// TestWebConfigEmptyAndUnwiredAreDifferentAnswers pins the three ways this
// screen can have nothing to show. Each has a different remedy, so each must
// have a different explanation — and none of them is a bare empty list.
func TestWebConfigEmptyAndUnwiredAreDifferentAnswers(t *testing.T) {
	t.Parallel()

	_, deps, _ := bareFixture(t)

	cases := []struct {
		name       string
		server     func(t *testing.T) *Server
		wantTitle  string
		wantDetail string
	}{
		{
			name:       "indexed but nothing found",
			server:     func(t *testing.T) *Server { s, _, _ := bareFixture(t); return s },
			wantTitle:  "recorded no configuration keys",
			wantDetail: "not that this service is configuration-free",
		},
		{
			name:       "no committed index",
			server:     func(t *testing.T) *Server { return unindexedServer(t) },
			wantTitle:  "no committed index",
			wantDetail: "no indexing pass has been committed",
		},
		{
			name:       "read port not wired",
			server:     func(t *testing.T) *Server { return serverWithoutNarrowPorts(t, deps) },
			wantTitle:  "without a configuration read port",
			wantDetail: "missing wiring",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.server(t)
			var got ConfigResponse
			rec := get(t, srv, "/api/config", &got)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if len(got.Keys) != 0 {
				t.Fatalf("expected no keys, got %+v", got.Keys)
			}
			if got.Help == nil {
				t.Fatal("an empty payload was returned with no guidance")
			}
			if !strings.Contains(got.Help.Title, tc.wantTitle) {
				t.Errorf("guidance title = %q, want it to contain %q", got.Help.Title, tc.wantTitle)
			}
			if !strings.Contains(strings.ToLower(got.Help.Detail), strings.ToLower(tc.wantDetail)) {
				t.Errorf("guidance detail = %q, want it to contain %q", got.Help.Detail, tc.wantDetail)
			}
			if len(got.Help.Steps) == 0 {
				t.Error("guidance names no next step")
			}
			if got.Next.Label == "" {
				t.Error("the empty screen ends in no next action")
			}
		})
	}
}

// TestWebConfigArraysAreNeverNull: a client must not have to tell null from
// empty, on any of the three lists.
func TestWebConfigArraysAreNeverNull(t *testing.T) {
	t.Parallel()
	srv, _, _ := bareFixture(t)

	body := raw(t, srv, "/api/config")
	for _, field := range []string{"keys", "read_but_not_declared", "declared_but_never_read"} {
		if _, ok := body[field].([]any); !ok {
			t.Errorf("%s did not serialise as an array: %T", field, body[field])
		}
	}
}

// newServerWithConfigKeys wires a stub ConfigReader through the functional
// option, which is also the only test of that option.
func newServerWithConfigKeys(t *testing.T, deps capability.Deps, keys []registry.ConfigKeyUsage) *Server {
	t.Helper()
	reg, ok := deps.Registry.(*registry.Registry)
	if !ok {
		t.Fatalf("captest returned %T, not *registry.Registry", deps.Registry)
	}
	return newServer(t, reg, deps, config.Config{ProjectName: "config-stub"},
		WithConfigReader(stubConfigReader{keys: keys}))
}
