package git

import (
	"context"
	"testing"
)

// TestResolveRevision covers the property the staleness check in
// capability/impact depends on: two DIFFERENT names for the same commit must
// resolve to the same SHA, or a freshness comparison built on it warns about
// every correct index.
func TestResolveRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("a.go", "package a\n")
	first := f.commit("first")
	f.write("b.go", "package b\n")
	second := f.commit("second")

	repo := f.open()

	tests := []struct {
		name    string
		rev     string
		want    string
		wantErr bool
	}{
		{name: "HEAD", rev: "HEAD", want: second.String()},
		{name: "branch name", rev: fixtureBranch, want: second.String()},
		{name: "full sha", rev: second.String(), want: second.String()},
		{name: "ancestor expression", rev: "HEAD~1", want: first.String()},
		{name: "empty is a position, not a name", rev: "", want: ""},
		{name: "unknown revision is an error, not empty", rev: "no-such-ref", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := repo.ResolveRevision(context.Background(), tc.rev)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveRevision(%q) = %q, want an error", tc.rev, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRevision(%q): %v", tc.rev, err)
			}
			if got != tc.want {
				t.Errorf("ResolveRevision(%q) = %q, want %q", tc.rev, got, tc.want)
			}
		})
	}
}

func TestResolveRevisionOnNonRepository(t *testing.T) {
	t.Parallel()
	repo, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := repo.ResolveRevision(context.Background(), "HEAD"); err == nil {
		t.Fatal("a non-repository resolved a revision")
	}
}
