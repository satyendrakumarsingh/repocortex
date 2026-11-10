package cli

import (
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/missed"
)

func TestMissedRejectsUnknownKind(t *testing.T) {
	root := indexedProject(t)
	_, err := run(t, root, "missed", "--kind", "unresolvd_call")
	if err == nil || !strings.Contains(err.Error(), "unresolved_call") {
		t.Fatalf("a mistyped --kind must be rejected and list the valid kinds; got %v", err)
	}
}

func TestMissedRejectsUnknownFailOn(t *testing.T) {
	root := indexedProject(t)
	if _, err := run(t, root, "missed", "--fail-on", "critical"); err == nil {
		t.Fatal("--fail-on accepted a severity that does not exist")
	}
}

func TestMissedWritesToTheCommandOutput(t *testing.T) {
	root := indexedProject(t)
	out, err := run(t, root, "missed")
	if err != nil {
		t.Fatalf("missed: %v", err)
	}
	if !strings.Contains(out, "Blindspot Report") {
		t.Errorf("report went somewhere other than the command's writer:\n%q", out)
	}
}

func TestBlindspotGate(t *testing.T) {
	found := []missed.Blindspot{{Severity: "low"}, {Severity: "medium"}}
	cases := []struct {
		failOn  string
		wantErr bool
	}{
		{"", false},
		{"low", true},
		{"medium", true},
		{"high", false},
	}
	for _, c := range cases {
		t.Run("fail-on="+c.failOn, func(t *testing.T) {
			err := blindspotGate(found, c.failOn)
			if (err != nil) != c.wantErr {
				t.Errorf("blindspotGate(%q) = %v, wantErr %v", c.failOn, err, c.wantErr)
			}
		})
	}
	if err := blindspotGate(nil, "low"); err != nil {
		t.Errorf("no blindspots must pass any gate; got %v", err)
	}
}
