package capability

import "testing"

func TestMarkedCommentsAreFoundOnlyInComments(t *testing.T) {
	src := `// HACK: the vendor API returns 200 on failure, so the body is checked too.
func charge() error {
	note := "DO NOT REMOVE" // a string, not a marker
	// note that this is ordinary prose
	x := 1 // NOTE(race): read under mu, see Close
	# XXX keep until v3 clients are gone
	/* INVARIANT: balance never goes negative */
	DO_NOT_REMOVE := 2
	-- DO NOT CHANGE the column order: the report reads it by position
	return nil
}`
	got := MarkedComments(src, 40)
	want := []Marked{
		{40, "HACK: the vendor API returns 200 on failure, so the body is checked too."},
		{44, "NOTE(race): read under mu, see Close"},
		{45, "XXX keep until v3 clients are gone"},
		{46, "INVARIANT: balance never goes negative"},
		{48, "DO NOT CHANGE the column order: the report reads it by position"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d marked comments, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
