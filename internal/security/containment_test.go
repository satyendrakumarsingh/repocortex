package security_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
	"github.com/satyendrakumarsingh/repocortex/internal/security"
)

func TestContainedPathAcceptsAFileInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "pkg", "file.go")
	if err := os.MkdirAll(filepath.Dir(inside), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(inside, []byte("package pkg\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := security.ContainedPath(root, "pkg/file.go")
	if err != nil {
		t.Fatalf("ContainedPath: %v", err)
	}
	want, err := filepath.EvalSymlinks(inside)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestContainedPathRejectsDotDotEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := security.ContainedPath(root, "../etc/passwd"); err == nil {
		t.Fatal("expected refusal for ../ escape, got nil")
	} else if !errs.Is(err, errs.KindInvalid) {
		t.Errorf("want KindInvalid, got kind %q", errs.KindOf(err))
	}
}

func TestContainedPathRejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	if _, err := security.ContainedPath(root, "/etc/passwd"); err == nil {
		t.Fatal("expected refusal for absolute path, got nil")
	}
}

// The case a naive check misses: the path has no ".." in it and is not
// absolute, but a symlink component points outside the root.
func TestContainedPathRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := security.ContainedPath(root, "link.txt"); err == nil {
		t.Fatal("expected refusal for symlink pointing outside the root, got nil")
	}
}

// A sibling directory whose name shares the root's prefix must not be treated
// as inside it. "/tmp/rootx" is not inside "/tmp/root".
func TestContainedPathRejectsPrefixSibling(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	sibling := filepath.Join(parent, "rootx")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	target := filepath.Join(sibling, "f.txt")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := security.ContainedPath(root, "../rootx/f.txt"); err == nil {
		t.Fatal("expected refusal for prefix-sibling escape, got nil")
	}
}

func TestOpenContainedRefusesASymlinkedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	f, err := security.OpenContained(root, "link.txt")
	if err == nil {
		_ = f.Close()
		t.Fatal("expected refusal opening a symlink, got nil")
	}
}
