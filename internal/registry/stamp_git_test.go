package registry

import (
	"context"
	"testing"
)

// TestStampGitRevision covers the provenance claim the portal and the MCP
// server both render.
//
// The bug this guards: only the FULL index path creates a run, so only it
// stamped a revision. An incremental pass updates the committed run in place,
// which left the run naming whichever commit it was first built at while its
// contents described a later one. `head_matches_build` then reported a mismatch
// for an index that was in fact current — a provenance claim that is wrong,
// which is worse than one that is absent.
func TestStampGitRevision(t *testing.T) {
	t.Parallel()

	const (
		first  = "1111111111111111111111111111111111111111"
		second = "2222222222222222222222222222222222222222"
	)

	tests := []struct {
		name  string
		begin string
		stamp string
		want  string
	}{
		{
			name:  "records the commit an incremental pass describes",
			begin: first,
			stamp: second,
			want:  second,
		},
		{
			name:  "re-stamping the same revision is a no-op in effect",
			begin: first,
			stamp: first,
			want:  first,
		},
		{
			name:  "a run begun outside a repository can be stamped later",
			begin: "",
			stamp: second,
			want:  second,
		},
		{
			name:  "an empty stamp clears rather than writing a blank string",
			begin: first,
			stamp: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			reg, _ := newTestRegistry(t)

			run, err := reg.BeginRun(ctx, "auto", tc.begin)
			if err != nil {
				t.Fatalf("BeginRun: %v", err)
			}
			if run.GitRevision != tc.begin {
				t.Fatalf("BeginRun recorded %q, want %q", run.GitRevision, tc.begin)
			}

			if err := reg.StampGitRevision(ctx, run.ID, tc.stamp); err != nil {
				t.Fatalf("StampGitRevision: %v", err)
			}

			var got string
			row := reg.db.QueryRowContext(ctx,
				`SELECT COALESCE(git_revision, '') FROM index_runs WHERE id = ?`, run.ID)
			if err := row.Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got != tc.want {
				t.Errorf("git_revision = %q, want %q", got, tc.want)
			}
		})
	}
}
