package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/multirepo"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// TestEstateWithoutAManifestGuidesRatherThanBlanks is the empty path, and it is
// the one every single-repository checkout takes.
func TestEstateWithoutAManifestGuidesRatherThanBlanks(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := repoFixture(t)

	var got EstateResponse
	pillarGet(t, srv.handleEstate, "/api/system", &got)

	if got.Federated {
		t.Fatal("federated is true with no manifest")
	}
	if len(got.Edges) != 0 || len(got.Members) != 0 {
		t.Error("an unfederated screen returned members or edges")
	}
	if got.Help == nil {
		t.Fatal("no guidance for a missing manifest")
	}
	if !strings.Contains(got.Help.Title, "No estate manifest") {
		t.Errorf("guidance title = %q", got.Help.Title)
	}
	if !strings.Contains(got.Help.Detail, "not because the estate has no cross-repository edges") {
		t.Errorf("guidance does not distinguish an undeclared estate from an unconnected one: %q",
			got.Help.Detail)
	}
	steps := strings.Join(got.Help.Steps, "\n")
	if !strings.Contains(steps, "cortex system") {
		t.Errorf("guidance names no command: %q", steps)
	}
	if got.Manifest.Path != estateFile || !got.Manifest.Defaulted {
		t.Errorf("manifest = %+v, want the conventional name, marked defaulted", got.Manifest)
	}
	if got.Next.Command == "" && got.Next.Screen == "" {
		t.Error("the screen ends with no next action")
	}
}

// TestEstateWithOneMemberSaysWhyThatIsNotASystem.
func TestEstateWithOneMemberSaysWhyThatIsNotASystem(t *testing.T) {
	t.Parallel()
	srv, _, _, root := repoFixture(t)
	writeRepoFile(t, root, estateFile, "members:\n  - name: web\n    index: ./web.db\n")

	var got EstateResponse
	pillarGet(t, srv.handleEstate, "/api/system", &got)

	if got.Federated {
		t.Fatal("federated is true over a one-member estate")
	}
	if got.Counts.MembersDeclared != 1 {
		t.Errorf("members_declared = %d, want 1", got.Counts.MembersDeclared)
	}
	if got.Help == nil || !strings.Contains(got.Help.Title, "at least two") {
		t.Fatalf("guidance = %+v, want it to say a system graph needs two members", got.Help)
	}
}

// TestEstateFederatesAndReportsEveryMemberItCouldNotRead is the populated case.
//
// The federation really runs: internal/multirepo builds a report over two
// declared members, one of which is an index that exists but holds no committed
// run and the other of which is not there at all. Both are ROWS, `complete` is
// false, and every count is stated as a floor — which is the property a
// federated view cannot lose without becoming worse than no view.
func TestEstateFederatesAndReportsEveryMemberItCouldNotRead(t *testing.T) {
	t.Parallel()
	srv, _, _, root := repoFixture(t)

	empty := newEmptyIndexFile(t)
	writeRepoFile(t, root, estateFile, fmt.Sprintf(
		"members:\n  - name: orders\n    index: %s\n  - name: web\n    index: %s\n",
		empty, filepath.Join(t.TempDir(), "absent.db")))

	var got EstateResponse
	pillarGet(t, srv.handleEstate, "/api/system", &got)

	if !got.Federated {
		t.Fatalf("federated is false: manifest %+v, guidance %+v", got.Manifest, got.Help)
	}
	if got.Complete {
		t.Fatal("complete is true with two unreadable members; every count here is a floor")
	}
	if len(got.Members) != 2 {
		t.Fatalf("members = %d, want 2 — a member that could not be read is still a row", len(got.Members))
	}

	byName := map[string]multirepo.MemberStatus{}
	for _, m := range got.Members {
		byName[m.Name] = m
	}
	orders, ok := byName["orders"]
	if !ok {
		t.Fatalf("member `orders` is missing: %+v", got.Members)
	}
	if orders.Available {
		t.Error("an index with no committed run reported itself available")
	}
	if !strings.Contains(orders.Unavailable, "no committed run") {
		t.Errorf("orders.unavailable = %q, want it to distinguish an uncommitted index from a "+
			"missing one — the two look identical and have different fixes", orders.Unavailable)
	}
	if !strings.Contains(orders.Unavailable, "cortex index") {
		t.Errorf("orders.unavailable names no command that fixes it: %q", orders.Unavailable)
	}
	web := byName["web"]
	if !strings.Contains(web.Unavailable, "no index at") {
		t.Errorf("web.unavailable = %q, want it to say the index is absent", web.Unavailable)
	}

	if len(got.Unjoined.UnavailableMembers) != 2 {
		t.Errorf("unavailable_members = %v, want both", got.Unjoined.UnavailableMembers)
	}
	if !strings.Contains(got.Counts.Scope, "lower bound") {
		t.Errorf("counts scope does not state that the numbers are a lower bound: %q", got.Counts.Scope)
	}
	if len(got.Caveats) == 0 {
		t.Error("no caveats: the confidence ceiling on this screen is the reading rule")
	}

	// The unreadable members are an OMISSION, counted and remediable.
	var noted bool
	for _, om := range got.Omissions.Omitted {
		if strings.Contains(om.Detail, "could not be read") {
			noted = true
			if om.Count != 2 {
				t.Errorf("the unreadable-member omission counts %d, want 2", om.Count)
			}
		}
	}
	if !noted {
		t.Errorf("unreadable members are not in the omission block: %+v", got.Omissions.Omitted)
	}
	if got.Omissions.Complete {
		t.Error("omissions.complete is true on a report missing two members")
	}

	// Every list is non-null: on this screen an empty list is a finding.
	if got.Unjoined.Calls == nil || got.Unjoined.Endpoints == nil || got.Edges == nil {
		t.Errorf("a null list reached the client: %+v", got.Unjoined)
	}
}

// TestEstateRejectsAnUnreadableManifest.
func TestEstateRejectsAnUnreadableManifest(t *testing.T) {
	t.Parallel()
	srv, _, _, root := repoFixture(t)
	writeRepoFile(t, root, estateFile, "members: this is not a list\n")

	var got EstateResponse
	pillarGet(t, srv.handleEstate, "/api/system", &got)

	if got.Federated {
		t.Fatal("federated is true over an unreadable manifest")
	}
	if got.Help == nil || !strings.Contains(got.Help.Title, "not a readable estate file") {
		t.Fatalf("guidance = %+v", got.Help)
	}
}

// TestMemberIndexFailuresAreDataWithARemedy pins the opener itself.
//
// Each failure is written for the person who has to fix it, and that person is
// usually not the person reading the screen — so each names the command.
func TestMemberIndexFailuresAreDataWithARemedy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notAnIndex := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notAnIndex, []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct {
		name     string
		path     string
		wantSays string
	}{
		{name: "absent", path: filepath.Join(dir, "missing.db"), wantSays: "no index at"},
		{name: "not a database", path: notAnIndex, wantSays: notAnIndex},
		{name: "a directory", path: dir, wantSays: dir},
		{name: "initialised but never populated", path: newEmptyIndexFile(t), wantSays: "no committed run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			got := openMemberIndex(ctx, tc.path)
			if got.closeFn != nil {
				got.closeFn()
			}
			if got.failure == "" {
				t.Fatalf("%s opened successfully", tc.path)
			}
			if !strings.Contains(got.failure, tc.wantSays) {
				t.Errorf("failure = %q, want it to mention %q", got.failure, tc.wantSays)
			}
			if got.source != nil {
				t.Error("a failed member returned a source anyway")
			}
		})
	}
}

// newEmptyIndexFile creates a migrated index database holding no committed run.
//
// A real store rather than an empty file, so the "initialised and never
// populated" case is reached through the same open path production uses.
func newEmptyIndexFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	ctx := context.Background()

	db, err := store.Open(ctx, store.Options{Path: path})
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := store.Migrate(ctx, db.DB, func() int64 { return 0 }); err != nil {
		_ = db.Close()
		t.Fatalf("migrate %s: %v", path, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
	return path
}
