package arch

import (
	"strings"
	"testing"
)

func classified() Model {
	return Model{
		Components: []Component{
			{Name: "payments", Include: []string{"src/payments/**"}},
			{Name: "reporting", Include: []string{"src/reporting/**"}},
		},
		Data: []DataClass{
			{Name: "pci", Include: []string{"card_*", "payment_token"}, Exclude: []string{"card_bin_reference"}},
			{Name: "pii", Include: []string{"customer*"}},
		},
		DataRules: []DataRule{{
			ID: "pci-origin", Classification: []string{"pci"},
			AllowedFrom: []string{"payments"}, Forbidden: []string{"reporting"},
			Severity: "high",
		}},
	}
}

func TestClassesOfLabelsTables(t *testing.T) {
	m := classified()
	cases := []struct {
		name  string
		table string
		want  []string
	}{
		{"a glob labels its family", "card_tokens", []string{"pci"}},
		{"a literal pattern labels one table", "payment_token", []string{"pci"}},
		{"an exclude carves a hole an include cannot win back", "card_bin_reference", nil},
		{"an unmatched table carries no label", "audit_log", nil},
		{"config case does not matter", "CARD_TOKENS", []string{"pci"}},
		{"an empty name is not a table", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := m.ClassesOf(tc.table)
			if len(got) != len(tc.want) {
				t.Fatalf("ClassesOf(%q) = %v, want %v", tc.table, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ClassesOf(%q) = %v, want %v", tc.table, got, tc.want)
				}
			}
		})
	}
}

// TestATableCanCarryTwoClassifications. A customer card table genuinely holds
// both personal and cardholder data, and reporting only one of them drops half
// of a compliance finding.
func TestATableCanCarryTwoClassifications(t *testing.T) {
	m := classified()
	got := m.ClassesOf("customer_cards")
	if len(got) != 1 || got[0] != "pii" {
		t.Fatalf("baseline ClassesOf(customer_cards) = %v, want [pii]", got)
	}

	m.Data = append(m.Data, DataClass{Name: "pci", Include: []string{"customer_cards"}})
	got = m.ClassesOf("customer_cards")
	if len(got) != 2 || got[0] != "pci" || got[1] != "pii" {
		t.Fatalf("ClassesOf(customer_cards) = %v, want [pci pii] sorted and deduplicated", got)
	}
}

// TestDuplicateClassNamesUnion. Unlike components, two blocks with one name are
// a documented way to split a large classification across teams.
func TestDuplicateClassNamesUnion(t *testing.T) {
	m := Model{
		Components: []Component{{Name: "c", Include: []string{"**"}}},
		Data: []DataClass{
			{Name: "pci", Include: []string{"card_*"}},
			{Name: "pci", Include: []string{"settlement_*"}},
		},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("two data blocks with one name were rejected: %v", err)
	}
	for _, table := range []string{"card_tokens", "settlement_batches"} {
		if got := m.ClassesOf(table); len(got) != 1 || got[0] != "pci" {
			t.Errorf("ClassesOf(%q) = %v, want [pci]", table, got)
		}
	}
}

func TestEvaluateDataPrecedence(t *testing.T) {
	m := classified()
	cases := []struct {
		name    string
		class   string
		from    string
		want    Verdict
		wantsID string
	}{
		{"an allow-listed origin is allowed", "pci", "payments", VerdictAllowed, "pci-origin"},
		{"a forbidden origin is forbidden", "pci", "reporting", VerdictForbidden, "pci-origin"},
		{"an origin off the allow-list is gated", "pci", "unknown-but-named", VerdictNotAllowed, "pci-origin"},
		{"a classification no rule governs is allowed", "pii", "reporting", VerdictAllowed, ""},
		{"an unassigned origin is not judged", "pci", "", VerdictAllowed, ""},
		{"classification case does not matter", "PCI", "reporting", VerdictForbidden, "pci-origin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rule := m.EvaluateData(tc.class, tc.from)
			if got != tc.want {
				t.Fatalf("EvaluateData(%q, %q) = %v, want %v", tc.class, tc.from, got, tc.want)
			}
			id := ""
			if rule != nil {
				id = rule.ID
			}
			if id != tc.wantsID {
				t.Errorf("decided by rule %q, want %q", id, tc.wantsID)
			}
		})
	}
}

// TestForbiddenBeatsAnAllowListElsewhere. Same precedence as Evaluate, on
// purpose: two rule kinds that resolved conflicts differently would be two rule
// kinds nobody could reason about together.
func TestForbiddenBeatsAnAllowListElsewhere(t *testing.T) {
	m := classified()
	m.DataRules = append(m.DataRules, DataRule{
		ID: "reporting-exception", Classification: []string{"pci"},
		AllowedFrom: []string{"reporting"},
	})
	got, rule := m.EvaluateData("pci", "reporting")
	if got != VerdictForbidden {
		t.Fatalf("an allow-list cancelled a forbid: got %v via %+v", got, rule)
	}
}

// TestARuleWithNoAllowListOnlyForbids. Writing one `forbidden` entry must not
// silently cut every other component off from the data.
func TestARuleWithNoAllowListOnlyForbids(t *testing.T) {
	m := classified()
	m.DataRules = []DataRule{{ID: "no-reporting", Forbidden: []string{"reporting"}}}
	if got, _ := m.EvaluateData("pci", "payments"); got != VerdictAllowed {
		t.Errorf("a bare forbid gated an unrelated component: %v", got)
	}
	if got, _ := m.EvaluateData("pci", "reporting"); got != VerdictForbidden {
		t.Errorf("the forbid did not fire: %v", got)
	}
}

// TestAnEmptyClassificationListGovernsEverything.
func TestAnEmptyClassificationListGovernsEverything(t *testing.T) {
	m := classified()
	m.DataRules = []DataRule{{ID: "all-classes", Forbidden: []string{"reporting"}}}
	for _, class := range []string{"pci", "pii"} {
		if got, _ := m.EvaluateData(class, "reporting"); got != VerdictForbidden {
			t.Errorf("class %q escaped a rule that named no classification: %v", class, got)
		}
	}
}

func TestValidateRejectsBrokenDataDeclarations(t *testing.T) {
	cases := []struct {
		name  string
		model Model
		want  string
	}{
		{
			"an unnamed data class",
			Model{Data: []DataClass{{Include: []string{"a"}}}},
			"name is empty",
		},
		{
			"a data class matching nothing",
			Model{Data: []DataClass{{Name: "pci"}}},
			"no include patterns",
		},
		{
			"a table pattern copied from a component",
			Model{Data: []DataClass{{Name: "pci", Include: []string{"src/**/card_*"}}}},
			"have no path separator",
		},
		{
			"a rule naming a classification nobody declared",
			Model{
				Components: []Component{{Name: "c", Include: []string{"**"}}},
				Data:       []DataClass{{Name: "pci", Include: []string{"card_*"}}},
				DataRules:  []DataRule{{ID: "r", Classification: []string{"pii"}, Forbidden: []string{"c"}}},
			},
			"undeclared classification",
		},
		{
			"a rule naming a component nobody declared",
			Model{
				Components: []Component{{Name: "c", Include: []string{"**"}}},
				Data:       []DataClass{{Name: "pci", Include: []string{"card_*"}}},
				DataRules:  []DataRule{{ID: "r", Classification: []string{"pci"}, Forbidden: []string{"typo"}}},
			},
			"unknown component",
		},
		{
			"a rule that constrains nothing",
			Model{
				Components: []Component{{Name: "c", Include: []string{"**"}}},
				Data:       []DataClass{{Name: "pci", Include: []string{"card_*"}}},
				DataRules:  []DataRule{{ID: "r", Classification: []string{"pci"}}},
			},
			"neither `allowed_from` nor `forbidden`",
		},
		{
			"an invalid severity",
			Model{
				Components: []Component{{Name: "c", Include: []string{"**"}}},
				Data:       []DataClass{{Name: "pci", Include: []string{"card_*"}}},
				DataRules:  []DataRule{{ID: "r", Forbidden: []string{"c"}, Severity: "critical"}},
			},
			"severity \"critical\"",
		},
		{
			"a data rule reusing an ordinary rule's id",
			Model{
				Components: []Component{{Name: "c", Include: []string{"**"}}},
				Rules:      []Rule{{ID: "dup", From: []string{"c"}, Forbidden: []string{"c"}}},
				Data:       []DataClass{{Name: "pci", Include: []string{"card_*"}}},
				DataRules:  []DataRule{{ID: "dup", Forbidden: []string{"c"}}},
			},
			"duplicate rule id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.model.Validate()
			if err == nil {
				t.Fatalf("accepted a model that would silently enforce nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestAModelWithOnlyDataIsStillValidated. The empty-model shortcut must not let
// a repository that declares only a `data` block skip validation entirely.
func TestAModelWithOnlyDataIsStillValidated(t *testing.T) {
	m := Model{Data: []DataClass{{Name: "", Include: nil}}}
	if err := m.Validate(); err == nil {
		t.Fatal("a model consisting only of a broken data block validated clean")
	}
}

func TestValidDataModelValidates(t *testing.T) {
	if err := classified().Validate(); err != nil {
		t.Fatalf("a well-formed data model was rejected: %v", err)
	}
}

// TestDeclaresDataGatesTheTraversal. The reachability walk is the most
// expensive thing the capability does; a repository that classified nothing
// must pay nothing for this rule kind existing.
func TestDeclaresDataGatesTheTraversal(t *testing.T) {
	if (&Model{}).DeclaresData() {
		t.Error("a model with no data block claimed to declare data")
	}
	m := classified()
	if !m.DeclaresData() {
		t.Error("a model with a data block did not declare data")
	}
}
