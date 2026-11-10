package cli

import (
	"strings"
	"testing"
)

func TestRenderThirdPartyIsDeterministicAndSorted(t *testing.T) {
	mods := []moduleInfo{
		{Path: "github.com/spf13/cobra", Version: "v1.8.0"},
		{Path: "github.com/odvcencio/gotreesitter", Version: "v0.1.0"},
	}

	got := renderThirdParty(mods)

	if !strings.Contains(got, "| `github.com/odvcencio/gotreesitter` | v0.1.0 |") {
		t.Fatalf("gotreesitter row missing from:\n%s", got)
	}
	iGot := strings.Index(got, "gotreesitter")
	iCobra := strings.Index(got, "cobra")
	if iGot > iCobra {
		t.Fatalf("rows are not sorted by module path:\n%s", got)
	}
	if strings.Contains(got, "TODO") || strings.Contains(got, "TBD") {
		t.Fatal("generated inventory contains a placeholder")
	}
	if renderThirdParty(mods) != got {
		t.Fatal("renderThirdParty is not deterministic")
	}
}
