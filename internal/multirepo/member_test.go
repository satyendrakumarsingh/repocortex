package multirepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEstate(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "repocortex.system.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadEstateResolvesRelativeToTheFile is the property that makes the file
// worth having: `repocortex system --members ../estate.yml` must name the same
// repositories from every directory, or the answer depends on where the caller
// was standing.
func TestLoadEstateResolvesRelativeToTheFile(t *testing.T) {
	path := writeEstate(t, `
members:
  - name: orders
    index: orders/.repocortex/index.db
  - name: web
    index: ../web/.repocortex/index.db
`)
	dir := filepath.Dir(path)

	got, err := LoadEstate(path)
	if err != nil {
		t.Fatalf("LoadEstate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 members, got %+v", got)
	}
	// Sorted by name, so the report is the same however the file is ordered.
	if got[0].Name != "orders" || got[1].Name != "web" {
		t.Errorf("members are not name-sorted: %+v", got)
	}
	wantOrders := filepath.Join(dir, "orders/.repocortex/index.db")
	if got[0].Index != wantOrders {
		t.Errorf("index = %q, want %q", got[0].Index, wantOrders)
	}
	if !filepath.IsAbs(got[1].Index) || strings.Contains(got[1].Index, "..") {
		t.Errorf("a parent-relative entry did not resolve against the file: %q", got[1].Index)
	}
}

// TestLoadEstateRefusesSilentWrongAnswers. Each of these would otherwise
// produce a report that is quietly about a different system than the one the
// operator described.
func TestLoadEstateRefusesSilentWrongAnswers(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty list", "members: []\n", "lists no members"},
		{"missing name", "members:\n  - index: a/index.db\n", "has no name"},
		{"missing index", "members:\n  - name: web\n", "has no index path"},
		{
			"duplicate names",
			"members:\n  - name: web\n    index: a\n  - name: web\n    index: b\n",
			"both named",
		},
		{"not yaml", "members: [\n", "parse estate file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadEstate(writeEstate(t, tc.body))
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}

	if _, err := LoadEstate(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Error("a missing estate file must be an error, not an empty system")
	}
}

// TestParseMemberFlag covers the inline spelling, including a path that
// contains the separator.
func TestParseMemberFlag(t *testing.T) {
	m, err := ParseMemberFlag("orders=../orders/.repocortex/index.db")
	if err != nil {
		t.Fatalf("ParseMemberFlag: %v", err)
	}
	if m.Name != "orders" || m.Index != "../orders/.repocortex/index.db" {
		t.Errorf("parsed %+v", m)
	}

	// Split on the FIRST '=' only: a name cannot contain one, a path can.
	m, err = ParseMemberFlag("weird=/tmp/a=b/index.db")
	if err != nil {
		t.Fatalf("ParseMemberFlag: %v", err)
	}
	if m.Index != "/tmp/a=b/index.db" {
		t.Errorf("path was split on a later '=': %q", m.Index)
	}

	for _, bad := range []string{"noequals", "=/tmp/index.db", "name=", ""} {
		if _, err := ParseMemberFlag(bad); err == nil {
			t.Errorf("%q should not parse as a member", bad)
		}
	}
}
