package watch

import (
	"testing"
	"time"
)

// fakeFS answers the coalescer's existence question from a set, so the
// classification rules can be tested without a filesystem at all.
type fakeFS map[string]bool // rel -> isDir

func (f fakeFS) exists(rel string) (bool, bool) {
	isDir, ok := f[rel]
	return ok, isDir
}

func newTestCoalescer(fs fakeFS) *coalescer {
	return newCoalescer(100*time.Millisecond, fs.exists)
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestCoalesceClassification(t *testing.T) {
	tests := []struct {
		name  string
		ops   []op
		after fakeFS
		want  []Change
	}{
		{
			name:  "write to existing file is a modify",
			ops:   []op{opWrite},
			after: fakeFS{"a.go": false},
			want:  []Change{{Path: "a.go", Kind: ChangeModified}},
		},
		{
			name:  "ten writes coalesce to one modify",
			ops:   []op{opWrite, opWrite, opWrite, opWrite, opWrite, opWrite, opWrite, opWrite, opWrite, opWrite},
			after: fakeFS{"a.go": false},
			want:  []Change{{Path: "a.go", Kind: ChangeModified}},
		},
		{
			name:  "create then write is a single create",
			ops:   []op{opCreate, opWrite, opWrite},
			after: fakeFS{"a.go": false},
			want:  []Change{{Path: "a.go", Kind: ChangeCreated}},
		},
		{
			name:  "write then delete is a delete",
			ops:   []op{opWrite, opRemove},
			after: fakeFS{},
			want:  []Change{{Path: "a.go", Kind: ChangeDeleted}},
		},
		{
			name:  "create then delete cancels out",
			ops:   []op{opCreate, opWrite, opRemove},
			after: fakeFS{},
			want:  nil,
		},
		{
			// The atomic-save shape: the editor removes the target and renames
			// a temp file over it. The consumer already has this file indexed,
			// so calling it "created" would be wrong.
			name:  "delete then create is a modify",
			ops:   []op{opRemove, opCreate},
			after: fakeFS{"a.go": false},
			want:  []Change{{Path: "a.go", Kind: ChangeModified}},
		},
		{
			name:  "chmod alone is not a change",
			ops:   []op{opChmod, opChmod},
			after: fakeFS{"a.go": false},
			want:  nil,
		},
		{
			name:  "chmod after a write does not suppress the write",
			ops:   []op{opWrite, opChmod},
			after: fakeFS{"a.go": false},
			want:  []Change{{Path: "a.go", Kind: ChangeModified}},
		},
		{
			// An unpaired rename-out is indistinguishable from a delete, and
			// must not be dropped just because we could not find its other half.
			name:  "unpaired rename out is a delete",
			ops:   []op{opRename},
			after: fakeFS{},
			want:  []Change{{Path: "a.go", Kind: ChangeDeleted}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoalescer(tc.after)
			for _, o := range tc.ops {
				c.add("a.go", o, false, epoch)
			}
			got := c.due(epoch.Add(time.Second))
			assertChanges(t, got, tc.want)
			if !c.empty() {
				t.Errorf("pending not drained after flush: %d entries left", len(c.pending))
			}
		})
	}
}

func TestCoalesceRenamePairing(t *testing.T) {
	t.Run("same base name pairs across directories", func(t *testing.T) {
		c := newTestCoalescer(fakeFS{"lib/util.go": false})
		c.add("src/util.go", opRename, false, epoch)
		c.add("lib/util.go", opCreate, false, epoch)

		assertChanges(t, c.due(epoch.Add(time.Second)), []Change{
			{Path: "lib/util.go", OldPath: "src/util.go", Kind: ChangeRenamed},
		})
	})

	t.Run("one out and one in pair even when renamed", func(t *testing.T) {
		c := newTestCoalescer(fakeFS{"new.go": false})
		c.add("old.go", opRename, false, epoch)
		c.add("new.go", opCreate, false, epoch)

		assertChanges(t, c.due(epoch.Add(time.Second)), []Change{
			{Path: "new.go", OldPath: "old.go", Kind: ChangeRenamed},
		})
	})

	t.Run("ambiguous renames degrade to delete plus create", func(t *testing.T) {
		// Two renames out and two unrelated creates: guessing a pairing here
		// would be a coin flip, and a wrong OldPath makes the consumer move
		// index rows that belong to a different file.
		c := newTestCoalescer(fakeFS{"c.go": false, "d.go": false})
		c.add("a.go", opRename, false, epoch)
		c.add("b.go", opRename, false, epoch)
		c.add("c.go", opCreate, false, epoch)
		c.add("d.go", opCreate, false, epoch)

		assertChanges(t, c.due(epoch.Add(time.Second)), []Change{
			{Path: "a.go", Kind: ChangeDeleted},
			{Path: "b.go", Kind: ChangeDeleted},
			{Path: "c.go", Kind: ChangeCreated},
			{Path: "d.go", Kind: ChangeCreated},
		})
	})

	t.Run("a directory is never paired with a file", func(t *testing.T) {
		c := newTestCoalescer(fakeFS{"new.go": false})
		c.add("olddir", opRename, true, epoch)
		c.add("new.go", opCreate, false, epoch)

		assertChanges(t, c.due(epoch.Add(time.Second)), []Change{
			{Path: "new.go", Kind: ChangeCreated},
			{Path: "olddir", Kind: ChangeDeleted, IsDir: true},
		})
	})
}

// TestCoalesceHoldsBackStillNoisyPaths proves the flush is per-path: a file
// that is still being written must not be reported half-done just because a
// quieter file next to it became due.
func TestCoalesceHoldsBackStillNoisyPaths(t *testing.T) {
	c := newTestCoalescer(fakeFS{"quiet.go": false, "noisy.go": false})
	c.add("quiet.go", opWrite, false, epoch)
	c.add("noisy.go", opWrite, false, epoch.Add(80*time.Millisecond))

	got := c.due(epoch.Add(150 * time.Millisecond))
	assertChanges(t, got, []Change{{Path: "quiet.go", Kind: ChangeModified}})

	if _, ok := c.nextDeadline(); !ok {
		t.Fatal("noisy.go should still be pending")
	}
	assertChanges(t, c.due(epoch.Add(200*time.Millisecond)), []Change{
		{Path: "noisy.go", Kind: ChangeModified},
	})
}

// TestCoalesceIsDirIsSticky guards the one fact the filesystem cannot re-tell
// us: a deleted directory cannot be stat'ed, so its directory-ness has to
// survive from the event that reported it.
func TestCoalesceIsDirIsSticky(t *testing.T) {
	c := newTestCoalescer(fakeFS{})
	c.add("pkg", opWrite, true, epoch)
	c.add("pkg", opRemove, false, epoch)

	assertChanges(t, c.due(epoch.Add(time.Second)), []Change{
		{Path: "pkg", Kind: ChangeDeleted, IsDir: true},
	})
}

func assertChanges(t *testing.T, got, want []Change) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d changes %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
