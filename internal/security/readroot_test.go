package security

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileContained(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, c string) {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "sub", "a.txt"), "inside")
	write(filepath.Join(outside, "secret.txt"), "OUTSIDE")
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, _, err := ReadFileContained(root, "sub/a.txt")
	if err != nil || string(got) != "inside" {
		t.Fatalf("in-root read = %q, %v", got, err)
	}
	for _, p := range []string{"linkdir/secret.txt", "../outside/secret.txt", "sub/../../outside/secret.txt"} {
		if got, _, err := ReadFileContained(root, p); err == nil {
			t.Errorf("%q: escape was read: %q", p, got)
		}
	}
}

// TestRootRefusesSymlinkedDirectory proves the os.Root layer rejects a
// symlinked directory component on its own, independent of ValidatePath, which
// is what closes the check-then-open race.
func TestRootRefusesSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := r.ReadFile("linkdir/secret.txt"); err == nil {
		t.Fatal("os.Root followed a directory symlink out of the root")
	}
}

func TestValidatePathDanglingSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"dangling-out":     filepath.Join(outside, "new.txt"), // absolute, missing, outside
		"dangling-out-rel": "../outside/new.txt",              // relative, missing, outside
		"dangling-in":      "new-inside.txt",                  // relative, missing, inside
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	tests := []struct {
		name      string
		requested string
		wantErr   bool
	}{
		{"dangling link to outside (absolute)", "dangling-out", true},
		{"dangling link to outside (relative)", "dangling-out-rel", true},
		{"dangling link to inside", "dangling-in", false},
		{"non-existent plain path inside", "no/such/file.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidatePath(root, tt.requested)
			if tt.wantErr && !errors.Is(err, ErrOutsideRoot) {
				t.Fatalf("want ErrOutsideRoot, got %v", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
		})
	}
}

func TestReadFileContainedRefusesWhatIsNotARegularFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFileContained(root, "dir"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("directory: err = %v, want ErrNotRegular", err)
	}
}

func TestReadFileContainedSizeCap(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		opts    []ReadOption
		wantErr error
	}{
		{name: "no cap", opts: nil},
		{name: "cap equals size", opts: []ReadOption{WithMaxBytes(10)}},
		{name: "cap below size", opts: []ReadOption{WithMaxBytes(9)}, wantErr: ErrTooLarge},
		{name: "non-positive cap is ignored", opts: []ReadOption{WithMaxBytes(0)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := ReadFileContained(root, "f.txt", tc.opts...)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || got != nil {
					t.Fatalf("got %q, err %v, want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || string(got) != "0123456789" {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}
