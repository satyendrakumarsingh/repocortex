package registry

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// permute returns a copy of s reordered by idx.
func permute[T any](s []T, idx []int) []T {
	out := make([]T, len(idx))
	for i, j := range idx {
		out[i] = s[j]
	}
	return out
}

// digestOfRun writes content into a fresh run and returns its digest.
func digestOfRun(t *testing.T, r *Registry, files []fileSpec, edges []edgeSpec) string {
	t.Helper()
	ctx := context.Background()
	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	writeContent(t, r, run.ID, files, edges)
	dg, err := r.RegistryDigest(ctx, run.ID)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return dg
}

func richContent() ([]fileSpec, []edgeSpec) {
	files := []fileSpec{
		{path: "internal/a.go", lang: "go", hash: "ha", content: "func handleCallback() {}",
			syms: []symSpec{
				{fqn: "internal/a.go#handleCallback", name: "handleCallback", line: 10, kind: model.KindFunction},
				{fqn: "internal/a.go#Server", name: "Server", line: 3, kind: model.KindClass},
				{fqn: "internal/a.go#helper", name: "helper", line: 30, kind: model.KindFunction},
			}},
		{path: "internal/b.go", lang: "go", hash: "hb", content: "func updateStatus() {}",
			syms: []symSpec{
				{fqn: "internal/b.go#updateStatus", name: "updateStatus", line: 5, kind: model.KindFunction},
				{fqn: "internal/b.go#Store", name: "Store", line: 1, kind: model.KindInterface},
			}},
		{path: "web/c.ts", lang: "typescript", hash: "hc", content: "export function render() {}",
			syms: []symSpec{
				{fqn: "web/c.ts#render", name: "render", line: 7, kind: model.KindFunction},
			}},
	}
	edges := []edgeSpec{
		{srcFQN: "internal/a.go#handleCallback", dstFQN: "internal/b.go#updateStatus",
			kind: model.EdgeCalls, conf: model.ConfidenceHigh, source: model.SourceSourceCode,
			evFile: "internal/a.go", evLine: 11},
		{srcFQN: "internal/a.go#Server", dstFQN: "internal/b.go#Store",
			kind: model.EdgeImplements, conf: model.ConfidenceMedium, source: model.SourceSourceCode,
			evFile: "internal/a.go", evLine: 4},
		{srcFQN: "internal/a.go#helper", dstFQN: "web/c.ts#render",
			kind: model.EdgeReferences, conf: model.ConfidenceLow, source: model.SourceInferred,
			evFile: "internal/a.go", evLine: 31},
		{srcFQN: "web/c.ts#render", dstFQN: "internal/b.go#updateStatus",
			kind: model.EdgeCalls, conf: model.ConfidenceLow, source: model.SourceInferred,
			evFile: "web/c.ts", evLine: 8},
	}
	return files, edges
}

// TestDigestIsIndependentOfInsertionOrder is the determinism gate's own test.
//
// Both runs live in the same database, so their files/symbols/edges necessarily
// get different rowids — an equal digest therefore proves independence from
// autoincrement ids as well as from insertion order. Symbol order within a file
// and file order across the run are both shuffled, and mod_time_unix varies per
// insertion (see writeContent).
func TestDigestIsIndependentOfInsertionOrder(t *testing.T) {
	r, _ := newTestRegistry(t)
	files, edges := richContent()

	forward := digestOfRun(t, r, files, edges)

	shuffledFiles := permute(files, []int{2, 0, 1})
	shuffledFiles[1].syms = permute(shuffledFiles[1].syms, []int{2, 0, 1})
	shuffledFiles[2].syms = permute(shuffledFiles[2].syms, []int{1, 0})
	shuffledEdges := permute(edges, []int{3, 1, 0, 2})

	reordered := digestOfRun(t, r, shuffledFiles, shuffledEdges)

	if forward != reordered {
		t.Fatalf("digest depends on insertion order:\n  in-order  %s\n  shuffled  %s", forward, reordered)
	}
	if forward == "" {
		t.Fatal("digest is empty")
	}

	// A third pass in yet another order, to catch a scheme that happens to be
	// stable under one permutation.
	third := digestOfRun(t, r,
		permute(files, []int{1, 2, 0}),
		permute(edges, []int{2, 3, 0, 1}))
	if third != forward {
		t.Fatalf("digest differs on a third ordering: %s vs %s", third, forward)
	}
}

func TestDigestExcludesTimestampsAndRunIdentity(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	files, edges := richContent()

	first := digestOfRun(t, r, files, edges)

	// A later run with a different git revision, different started_unix and
	// different file mod times, but identical content.
	r.now = func() int64 { return 1_900_000_000 }
	run, err := r.BeginRun(ctx, "exact", "some-other-revision")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	writeContent(t, r, run.ID, files, edges)
	second, err := r.RegistryDigest(ctx, run.ID)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first != second {
		t.Fatalf("digest changed with run metadata that is not content:\n  %s\n  %s", first, second)
	}
}

func TestDigestChangesWithContent(t *testing.T) {
	baseFiles, baseEdges := richContent()

	cases := []struct {
		name   string
		mutate func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec)
	}{
		{"renamed symbol", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			f[0].syms[0].name = "handleCallbackV2"
			return f, e
		}},
		{"moved symbol", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			f[0].syms[0].line = 11
			return f, e
		}},
		{"changed symbol kind", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			f[1].syms[1].kind = model.KindClass
			return f, e
		}},
		{"changed file content hash", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			f[2].hash = "hc-modified"
			return f, e
		}},
		{"dropped a file", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			return f[:2], e[:2]
		}},
		{"weakened edge confidence", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			e[0].conf = model.ConfidenceMedium
			return f, e
		}},
		{"changed edge kind", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			e[1].kind = model.EdgeExtends
			return f, e
		}},
		{"moved edge evidence", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			e[0].evLine = 12
			return f, e
		}},
		{"dropped an edge", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			return f, e[:3]
		}},
		{"repointed an edge", func(f []fileSpec, e []edgeSpec) ([]fileSpec, []edgeSpec) {
			e[0].dstFQN = "internal/b.go#Store"
			return f, e
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One store per subtest: OpenMemory instances are shared while both
			// are open, so these cannot run in parallel or overlap.
			r, _ := newTestRegistry(t)
			base := digestOfRun(t, r, baseFiles, baseEdges)

			f, e := richContent()
			f, e = tc.mutate(f, e)
			got := digestOfRun(t, r, f, e)
			if got == base {
				t.Fatalf("digest is blind to %s: both %s", tc.name, base)
			}
		})
	}
}

// TestDigestIsInjectiveAcrossFieldBoundaries guards the length-prefixed
// encoding. With naive separator-joined fields these two graphs would hash the
// same, because "ab"+"c" and "a"+"bc" concatenate identically. Doc comments come
// from repository source, which is attacker-controlled in the threat model.
func TestDigestIsInjectiveAcrossFieldBoundaries(t *testing.T) {
	r, _ := newTestRegistry(t)

	mk := func(name, fqn string) []fileSpec {
		return []fileSpec{{path: "a.go", lang: "go", hash: "h",
			syms: []symSpec{{fqn: fqn, name: name, line: 1, kind: model.KindFunction}}}}
	}
	a := digestOfRun(t, r, mk("ab", "c"), nil)
	b := digestOfRun(t, r, mk("a", "bc"), nil)
	if a == b {
		t.Fatal("digest collides when a field boundary shifts; the encoding is not injective")
	}
}

// TestDigestTreatsNullAndEmptyAlike asserts that "no doc comment" and "an empty
// doc comment" are the same fact to the digest. The writer normalises "" to
// NULL, so the alternative encoding has to be injected with raw SQL.
//
// Honest scope: this pins the hashed values (the COALESCE in each SELECT). It
// does not exercise the matching COALESCE in each ORDER BY, which guards a
// related but currently unreachable case — SQLite sorts NULL before ”, so a
// run mixing both encodings could sort differently than it hashes, but only if
// an earlier sort key tied, and the leading keys are unique per run today. That
// COALESCE is there so the determinism argument in digest.go holds without
// depending on constraints declared in another package.
func TestDigestTreatsNullAndEmptyAlike(t *testing.T) {
	ctx := context.Background()
	r, db := newTestRegistry(t)
	files, edges := richContent()

	withNull := digestOfRun(t, r, files, edges)

	run, err := r.BeginRun(ctx, "auto", "rev")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	writeContent(t, r, run.ID, files, edges)
	for _, stmt := range []string{
		`UPDATE symbols SET signature = '', doc = '' WHERE index_run_id = ?`,
		`UPDATE edges   SET detail = ''             WHERE index_run_id = ?`,
		`UPDATE files   SET parse_error = ''        WHERE index_run_id = ?`,
	} {
		if _, err := db.ExecContext(ctx, stmt, run.ID); err != nil {
			t.Fatalf("inject empty strings: %v", err)
		}
	}
	withEmpty, err := r.RegistryDigest(ctx, run.ID)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if withNull != withEmpty {
		t.Fatalf("digest distinguishes NULL from '' for optional text:\n  NULL  %s\n  empty %s",
			withNull, withEmpty)
	}
}

func TestCommitRecordsTheDigest(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	files, edges := richContent()

	run, _ := r.BeginRun(ctx, "auto", "rev")
	writeContent(t, r, run.ID, files, edges)
	want, err := r.RegistryDigest(ctx, run.ID)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := r.CommitRun(ctx, run.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := r.CommittedDigest(ctx)
	if err != nil {
		t.Fatalf("committed digest: %v", err)
	}
	if got != want {
		t.Errorf("stored digest %s, computed %s", got, want)
	}
}

func TestDigestOfEmptyRunIsStable(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	a, _ := r.BeginRun(ctx, "auto", "x")
	b, _ := r.BeginRun(ctx, "auto", "y")
	da, err := r.RegistryDigest(ctx, a.ID)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	db, err := r.RegistryDigest(ctx, b.ID)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if da != db {
		t.Errorf("two empty runs digest differently: %s vs %s", da, db)
	}
	if da == "" {
		t.Error("empty run produced an empty digest")
	}
}
