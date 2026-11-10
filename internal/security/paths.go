package security

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Errors returned by ValidatePath. They are sentinels so a caller can tell an
// attack apart from a typo — an MCP server may want to log the first loudly and
// the second not at all.
var (
	ErrEmptyPath    = errors.New("security: empty path")
	ErrAbsolutePath = errors.New("security: absolute paths are not accepted")
	ErrOutsideRoot  = errors.New("security: path escapes the repository root")
	ErrBadRoot      = errors.New("security: repository root is unusable")
)

// sensitiveNames are files whose entire content is a credential. Matching is on
// the base name, lower-cased.
var sensitiveNames = map[string]bool{
	".env": true, ".envrc": true,
	"credentials": true, "credential": true, "credentials.json": true,
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	".npmrc": true, ".netrc": true, "_netrc": true, ".pgpass": true,
	".htpasswd": true, ".git-credentials": true, ".dockercfg": true,
	".pypirc": true, ".boto": true, ".s3cfg": true,
	"kubeconfig": true, ".kubeconfig": true,
	"secrets.yml": true, "secrets.yaml": true, "secrets.json": true,
	"secring.gpg": true, "service-account.json": true,
	"terraform.tfvars": true, "terraform.tfvars.json": true,
	"master.key": true, "credentials.yml.enc": true,
}

// sensitiveExts are extensions that carry key material.
//
// .pem is here even though a .pem may hold a public certificate: the name does
// not say which, and guessing wrong in the permissive direction leaks a private
// key. .crt, .cer and .pub are deliberately ABSENT — they are public by
// definition, and suppressing them would hide real architecture (which CA signs
// what, which host key is pinned) for no protection at all.
var sensitiveExts = []string{
	".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".ppk",
	".gpg", ".asc", ".kdbx", ".tfvars",
}

// sensitiveDirs are directory components under which everything is suspect.
var sensitiveDirs = map[string]bool{
	"secrets": true, ".secrets": true, ".ssh": true, ".gnupg": true,
	".aws": true, ".gcloud": true, ".azure": true,
}

// envExampleSuffixes mark the placeholder twins of a secret file.
//
// .env.example is committed on purpose: it documents WHICH environment
// variables exist, with dummy values. That is exactly the "existence and
// location, not value" answer this package is trying to produce, so suppressing
// it would discard the one artefact that is already safe. Content redaction
// still runs over it, so a real secret checked into .env.example is still
// caught by pattern.
var envExampleSuffixes = []string{
	".example", ".sample", ".template", ".dist", ".defaults", ".tpl", ".local.example",
}

// IsSensitivePath reports whether a path's content should be suppressed
// wholesale rather than pattern-matched.
//
// Pattern matching is the wrong tool for these files. In a .env or an id_rsa
// every line is credential material; a detector that catches nine lines out of
// ten has still leaked. Classifying by path is a cheap, total answer for the
// cases where it applies.
func IsSensitivePath(path string) bool {
	p := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	if p == "" {
		return false
	}
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}

	// Public counterparts first: id_rsa.pub is a public key and cert.crt is a
	// certificate. Both are safe, and both sit next to files that are not.
	for _, ext := range []string{".pub", ".crt", ".cer", ".csr"} {
		if strings.HasSuffix(base, ext) {
			return false
		}
	}
	if isEnvExample(base) {
		return false
	}

	if sensitiveNames[base] {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return true // .env.production, staging.env
	}
	for _, ext := range sensitiveExts {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if sensitiveDirs[seg] {
			return true
		}
	}
	return false
}

func isEnvExample(base string) bool {
	for _, s := range envExampleSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	// Variants where the marker is not the final segment: ".env.example.local",
	// "env.sample.production". Deliberately narrowed to env-ish names — an
	// "example.pem" is still a .pem, and a key someone called an example is
	// still a key.
	if !strings.Contains(base, "env") {
		return false
	}
	return strings.Contains(base, "example") || strings.Contains(base, "sample") ||
		strings.Contains(base, "template")
}

// ValidatePath resolves requested against repoRoot and refuses anything that
// escapes it, returning the cleaned absolute path.
//
// This is a real attack surface, not a formality. MCP tools take path arguments
// from an agent, and an agent's arguments are derived from content it read —
// an issue description, a README, a code comment in a cloned repository. Any of
// those can be attacker-influenced, so "../../../../.ssh/id_rsa" arrives here
// as an ordinary-looking tool call.
//
// Three escapes are handled:
//
//	absolute paths     /etc/passwd, C:\Windows, and drive-relative C:foo
//	dot-dot            ../../etc/passwd, a/../../etc/passwd
//	symlinks           a link inside the repo pointing anywhere outside it
//
// The symlink case is why both sides are passed through EvalSymlinks. Comparing
// a resolved target against an UNRESOLVED root is the classic bug: on macOS
// /tmp is a symlink to /private/tmp, so a root of /tmp/repo resolves to
// /private/tmp/repo and a naive prefix test rejects every legitimate path in
// the repository — or, with the operands the other way round, accepts paths
// outside it. Both sides are evaluated, and the comparison is done with
// filepath.Rel so separators and case are handled by the standard library.
//
// A path that does not exist yet is NOT an error: existence is the caller's
// business (its os.Open will say so), while containment is ours. The deepest
// existing ancestor is resolved and checked, so a non-existent path under a
// symlinked-out directory is still rejected.
func ValidatePath(repoRoot, requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return "", ErrEmptyPath
	}
	if strings.ContainsRune(requested, 0) {
		// A NUL truncates the path in any syscall that sees it, so "a.txt\x00/../.."
		// can mean two different things to two different layers.
		return "", fmt.Errorf("%w: NUL byte in path", ErrOutsideRoot)
	}
	if filepath.IsAbs(requested) || filepath.VolumeName(requested) != "" ||
		strings.HasPrefix(requested, "/") || strings.HasPrefix(requested, `\`) {
		return "", fmt.Errorf("%w: %q", ErrAbsolutePath, requested)
	}

	rel := filepath.Clean(filepath.FromSlash(requested))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrOutsideRoot, requested)
	}

	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrBadRoot, repoRoot, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrBadRoot, repoRoot, err)
	}

	joined := filepath.Join(root, rel)
	resolved, err := resolveExisting(joined)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %w", ErrOutsideRoot, requested, err)
	}
	if !within(root, resolved) {
		return "", fmt.Errorf("%w: %q", ErrOutsideRoot, requested)
	}
	return resolved, nil
}

// resolveExisting evaluates symlinks as far down the path as it can, then
// re-attaches the components that do not exist yet. Every existing component is
// therefore resolved — which is what matters, since only an existing component
// can be a symlink.
func resolveExisting(path string) (string, error) {
	const maxDanglingHops = 40 // same order as the OS's own symlink-loop limit
	rest := ""
	cur := path
	hops := 0
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		// EvalSymlinks reports NotExist both for a missing component and for a
		// symlink whose target is missing (a dangling link). Stripping a dangling
		// link as if it were a plain missing name would let it vouch for itself:
		// "repo/link" -> "/outside/new" would validate as "repo/link", and a
		// later create-or-write through it would land outside the root. So a
		// component that exists as a symlink is followed to its target instead,
		// and the caller's containment check judges where that target is.
		if target, ok := danglingTarget(cur); ok {
			if hops++; hops > maxDanglingHops {
				return "", fmt.Errorf("too many levels of dangling symlinks at %q", cur)
			}
			cur = target
			continue
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err // reached the filesystem root without finding anything
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// danglingTarget returns where the symlink at p points, as an absolute path,
// when p exists as a symlink. The link's directory is resolved first so a
// relative target containing ".." is interpreted against the real directory.
func danglingTarget(p string) (string, bool) {
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(p)
	if err != nil {
		return "", false
	}
	if filepath.IsAbs(target) {
		return filepath.Clean(target), true
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, target), true
}

// within reports whether p is root or lives underneath it. filepath.Rel does
// the separator and cleaning work; anything that has to climb out shows up as a
// leading "..", and an unrelated volume returns an error. Both fail closed.
func within(root, p string) bool {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator)))
}
