package mcp

import (
	"strings"
	"testing"
	"time"
)

// While a background pass is finishing a large edit, the answer says the index
// is busy and how many files remain; once it is done the notice goes away.
func TestAnAnswerDuringABackgroundRefreshSaysHowMuchRemains(t *testing.T) {
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

	p := &Provenance{}
	applyRefresh(p, RefreshOutcome{LastCheck: at, Refreshed: []string{"a.c", "b.c"}, Pending: 4000})
	if !p.IndexBusy || p.RefreshPending != 4000 {
		t.Errorf("busy %v, pending %d; want busy and 4000", p.IndexBusy, p.RefreshPending)
	}
	if !strings.Contains(p.Caveat, "4000 changed files are still being re-indexed") {
		t.Errorf("the caveat does not say how many files remain: %q", p.Caveat)
	}
	if len(p.RefreshedFiles) != 2 || p.EditsCheckedAt == "" {
		t.Errorf("what was refreshed is not reported: %+v", p)
	}

	one := &Provenance{}
	applyRefresh(one, RefreshOutcome{LastCheck: at, Pending: 1})
	if !strings.Contains(one.Caveat, "1 changed file is still being re-indexed") {
		t.Errorf("singular: %q", one.Caveat)
	}

	done := &Provenance{}
	applyRefresh(done, RefreshOutcome{LastCheck: at, Refreshed: []string{"c.c"}})
	if done.IndexBusy || done.RefreshPending != 0 || strings.Contains(done.Caveat, "still being re-indexed") {
		t.Errorf("a finished pass still reads as busy: %+v", done)
	}
}
