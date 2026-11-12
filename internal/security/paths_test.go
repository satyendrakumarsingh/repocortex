package security

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsSensitivePath(t *testing.T) {
	sensitive := []string{
		".env",
		".env.production",
		".env.local",
		"apps/api/.env",
		"config/staging.env",
		".envrc",
		"certs/server.pem",
		"certs/server.key",
		"deploy/id_rsa",
		"home/.ssh/id_ed25519",
		"home/.ssh/config", // everything under .ssh
		".npmrc",
		".netrc",
		".pgpass",
		".git-credentials",
		"~/.aws/credentials",
		"infra/secrets/database.yaml",
		"k8s/kubeconfig",
		"terraform.tfvars",
		"config/master.key",
		"keystore.jks",
		"backup.kdbx",
		"service-account.json",
		`windows\path\.env`,
	}
	for _, p := range sensitive {
		if !IsSensitivePath(p) {
			t.Errorf("IsSensitivePath(%q) = false, want true", p)
		}
	}

	insensitive := []string{
		"main.go",
		"internal/config/config.go",
		"README.md",
		"package.json",
		"deploy/id_rsa.pub", // a public key is public
		"certs/server.crt",  // a certificate is public
		"certs/server.csr",  // a signing request is public
		".env.example",      // documents which variables exist, by design
		".env.sample",
		".env.template",
		"config/env.example.local",
		"docs/environment.md",
		"src/keyboard.ts",
		"pkg/keys/keys.go",
		"",
	}
	for _, p := range insensitive {
		if IsSensitivePath(p) {
			t.Errorf("IsSensitivePath(%q) = true, want false", p)
		}
	}
}

func TestValidatePathAcceptsPathsInsideRoot(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "internal", "db"), "conn.go", "package db\n")

	for _, rel := range []string{
		"internal/db/conn.go",
		"./internal/db/conn.go",
		"internal/../internal/db/conn.go",
		"internal",
		".",
		"does/not/exist/yet.go", // existence is the caller's problem, not ours
	} {
		got, err := ValidatePath(root, rel)
		if err != nil {
			t.Errorf("ValidatePath(root, %q) = %v, want success", rel, err)
			continue
		}
		if !within(mustEval(t, root), got) {
			t.Errorf("ValidatePath(root, %q) = %q, which is outside %q", rel, got, root)
		}
	}
}

func TestValidatePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()

	cases := []struct {
		requested string
		want      error
	}{
		{"../../etc/passwd", ErrOutsideRoot},
		{"..", ErrOutsideRoot},
		{"../", ErrOutsideRoot},
		{"internal/../../etc/passwd", ErrOutsideRoot},
		{"a/b/../../../..", ErrOutsideRoot},
		{"/etc/passwd", ErrAbsolutePath},
		{"/", ErrAbsolutePath},
		{"", ErrEmptyPath},
		{"   ", ErrEmptyPath},
		{"secrets.txt\x00/../../etc/passwd", ErrOutsideRoot},
	}
	for _, tc := range cases {
		got, err := ValidatePath(root, tc.requested)
		if !errors.Is(err, tc.want) {
			t.Errorf("ValidatePath(root, %q) = (%q, %v), want %v", tc.requested, got, err, tc.want)
		}
	}

	if runtime.GOOS == "windows" {
		for _, p := range []string{`C:\Windows\System32`, `C:foo`, `\\server\share`} {
			if _, err := ValidatePath(root, p); !errors.Is(err, ErrAbsolutePath) {
				t.Errorf("ValidatePath(root, %q) accepted a volume-qualified path", p)
			}
		}
	}
}

// TestValidatePathRejectsSymlinkEscape is the attack ValidatePath exists for: a
// path that is textually innocent and resolves outside the repository. Nothing
// in filepath.Clean catches this, which is why EvalSymlinks is not optional.
func TestValidatePathRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	mustWrite(t, root, "ok.go", "package main\n")
	mustWrite(t, outside, "id_rsa", "-----BEGIN RSA PRIVATE KEY-----\n")

	if err := os.Symlink(filepath.Join(outside, "id_rsa"), filepath.Join(root, "innocent.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "vendor")); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{
		"innocent.go",          // symlinked file pointing out
		"vendor/id_rsa",        // via a symlinked directory
		"vendor/not-there.txt", // …to a path that does not exist yet
	} {
		if got, err := ValidatePath(root, rel); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("ValidatePath(root, %q) = (%q, %v), want ErrOutsideRoot", rel, got, err)
		}
	}

	// A symlink that stays inside the root is fine — repositories use them.
	if err := os.Symlink(filepath.Join(root, "ok.go"), filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePath(root, "alias.go"); err != nil {
		t.Errorf("ValidatePath(root, alias.go) = %v, want success", err)
	}
}

// TestValidatePathSymlinkedRoot is the macOS /tmp case, which is the specific
// bug this API is written to avoid: /tmp is a symlink to /private/tmp, so a
// repository rooted under /tmp has an unevaluated root and an evaluated child.
// Comparing those two directly rejects every legitimate path in the repo.
func TestValidatePathSymlinkedRoot(t *testing.T) {
	// Portable form: a root reached through a symlink of our own making.
	real := t.TempDir()
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "repo-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mustWrite(t, filepath.Join(real, "internal"), "a.go", "package a\n")

	got, err := ValidatePath(link, "internal/a.go")
	if err != nil {
		t.Fatalf("ValidatePath through a symlinked root = %v, want success", err)
	}
	if want := filepath.Join(mustEval(t, real), "internal", "a.go"); got != want {
		t.Errorf("ValidatePath = %q, want %q", got, want)
	}

	// The real thing, where the platform provides it.
	if resolved, err := filepath.EvalSymlinks("/tmp"); err != nil || resolved == "/tmp" {
		t.Skip("/tmp is not a symlink on this platform")
	}
	tmpRoot, err := os.MkdirTemp("/tmp", "repocortex-security-")
	if err != nil {
		t.Skipf("cannot create a repo under /tmp: %v", err)
	}
	defer os.RemoveAll(tmpRoot)
	mustWrite(t, filepath.Join(tmpRoot, "cmd"), "main.go", "package main\n")

	got, err = ValidatePath(tmpRoot, "cmd/main.go")
	if err != nil {
		t.Fatalf("ValidatePath(%q, cmd/main.go) = %v; the /tmp -> /private/tmp symlink was not handled", tmpRoot, err)
	}
	if !strings.HasSuffix(got, filepath.Join("cmd", "main.go")) {
		t.Errorf("ValidatePath = %q, want a path ending in cmd/main.go", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("returned path is not usable: %v", err)
	}
}

func TestValidatePathRejectsUnusableRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-repo")
	if _, err := ValidatePath(missing, "a.go"); !errors.Is(err, ErrBadRoot) {
		t.Errorf("ValidatePath with a missing root = %v, want ErrBadRoot", err)
	}
}

func mustWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
