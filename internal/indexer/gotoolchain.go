package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
)

// Why this file exists.
//
// scip-go drives go/packages, which shells out to the `go` command, which
// refuses to load a module whose `go` directive names a release newer than the
// toolchain it is running as. goEnv pins GOTOOLCHAIN to the release RepoCortex
// was ITSELF built with, so that no third party chooses the compiler — and that
// pin is what silently emptied Go indexing:
//
//	$ cd pulse/backend/go/gateway-service   # go.mod: go 1.26.5
//	$ GOTOOLCHAIN=go1.25.13 scip-go --output x.scip
//	Resolving module name
//	Loading Packages
//	error: err: exit status 1: stderr: go: go.mod requires go >= 1.26.5
//	  (running go 1.25.13; GOTOOLCHAIN=go1.25.13)
//
// Six of seven Go modules in that repository declare `go 1.26.5` and produced
// no file at all. That is not a property of those modules: ANY repository whose
// `go` directive is newer than the RepoCortex binary's own build is unindexable,
// and the newer the repository, the more certain the failure. Go is the
// language this tool resolves best, so the compiler-proven tier was empty for
// exactly the repositories most worth proving.
//
// THE FIX AND ITS LIMIT. GOTOOLCHAIN is raised to the release the project's
// go.mod asks for, and only when that is newer than ours. Three properties keep
// the original guarantee intact:
//
//  1. Only the `go` directive is read. The `toolchain` directive is ignored,
//     and ignoring it is the whole point: `toolchain go1.99.9` in a hostile
//     go.mod is what GOTOOLCHAIN=auto would download and execute. An explicit
//     GOTOOLCHAIN=go1.X.Y overrides that directive rather than obeying it —
//     verified directly, see TestGoToolchainIgnoresToolchainDirective.
//
//  2. The value is RE-RENDERED from three parsed integers, never passed through
//     from the file. A go.mod cannot put a string of its choosing into this
//     package's environment, only three bounded numbers.
//
//  3. It only ever raises. A repository cannot select an OLDER compiler than
//     the one we would have used, so this cannot be used to downgrade onto a
//     known-vulnerable release.
//
// What remains is that a repository can cause a newer OFFICIAL Go release to be
// fetched — a golang.org/toolchain module, verified against the checksum
// database that goEnv pins GOSUMDB to and clears GONOSUMDB/GOPRIVATE/GOINSECURE
// to protect. That is the same trust anchor `go install` of a pinned indexer
// already rests on. The alternative on offer was indexing nothing.
//
// FAILS CLOSED, deliberately: anything unparseable leaves the pin exactly where
// goEnv put it, so a malformed go.mod degrades to today's behaviour rather than
// to a guess.

// goDirectivePattern matches the `go` line of a go.mod. Anchored to the start of
// a line so a `go 1.99` inside a comment or a require block cannot match, and
// the version is captured as separate digit groups so nothing else can be.
var goDirectivePattern = regexp.MustCompile(`(?m)^go\s+(\d{1,3})\.(\d{1,3})(?:\.(\d{1,3}))?\s*$`)

// goVersion is a parsed Go release.
type goVersion struct{ major, minor, patch int }

func (v goVersion) String() string {
	s := "go" + strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor)
	if v.patch > 0 {
		s += "." + strconv.Itoa(v.patch)
	}
	return s
}

// newer reports whether v is a strictly later release than other.
func (v goVersion) newer(other goVersion) bool {
	switch {
	case v.major != other.major:
		return v.major > other.major
	case v.minor != other.minor:
		return v.minor > other.minor
	default:
		return v.patch > other.patch
	}
}

// parseGoVersion reads "go1.25.13" or "go1.26". It rejects anything else,
// including the "devel ..." and "go1.25.13 X:nocoverageredesign" forms
// runtime.Version() can return on a non-release build.
var goVersionPattern = regexp.MustCompile(`^go(\d{1,3})\.(\d{1,3})(?:\.(\d{1,3}))?$`)

func parseGoVersion(s string) (goVersion, bool) {
	return fromMatch(goVersionPattern.FindStringSubmatch(s))
}

func fromMatch(m []string) (goVersion, bool) {
	if m == nil {
		return goVersion{}, false
	}
	// Errors are impossible here — the patterns admit 1-3 digits and nothing
	// else — but a silently-zero version would pick the wrong toolchain, so
	// they are checked rather than discarded.
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return goVersion{}, false
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return goVersion{}, false
	}
	patch := 0
	if m[3] != "" {
		if patch, err = strconv.Atoi(m[3]); err != nil {
			return goVersion{}, false
		}
	}
	// Bounds, not decoration. Only Go 1.x exists; a directive naming anything
	// else is either a future this code cannot reason about or a value chosen
	// to make us fetch something strange, and both must fall through to the pin.
	if major != 1 || minor < 1 || minor > 99 || patch > 999 {
		return goVersion{}, false
	}
	return goVersion{major, minor, patch}, true
}

// goModRelease reports the release dir's go.mod requires, or ok=false when
// there is no go.mod, no readable `go` line, or a line that does not parse.
func goModRelease(dir string) (goVersion, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: project dir the user named + a fixed filename
	if err != nil {
		return goVersion{}, false
	}
	return fromMatch(goDirectivePattern.FindStringSubmatch(string(b)))
}

// goIndexToolchain is the GOTOOLCHAIN value for a scip-go run over projectDir:
// the release RepoCortex itself runs, raised to the project's `go` directive when
// that is newer. See the file comment for why raising is safe and why lowering
// is not on offer.
func goIndexToolchain(projectDir string) string {
	self, ok := parseGoVersion(runtime.Version())
	if !ok {
		// A devel or otherwise unrecognised build. goEnv's pin is still a pin;
		// leave it alone rather than replace it with a number we cannot compare.
		return runtime.Version()
	}
	want, ok := goModRelease(projectDir)
	if !ok || !want.newer(self) {
		return self.String()
	}
	return want.String()
}
