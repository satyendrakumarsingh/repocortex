package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// requiredRelease reports the highest Java language level a project declares,
// or 0 when it declares none.
//
// This is a PREFLIGHT signal, and its whole value is being cheap: it reads two
// build files and compares an integer, where the alternative is discovering the
// same fact from a seven-minute Maven build that ends in
// `error: release version 25 not supported`.
//
// It is deliberately textual rather than a real POM parse. A full parse would
// have to resolve parent POMs, profiles and property indirection -- a
// dependency resolution, i.e. exactly the expensive thing this exists to avoid.
// The cost of that choice is bounded and one-directional, see below.
func requiredRelease(dir string) int {
	best := 0
	for _, name := range []string{"pom.xml", "build.gradle", "build.gradle.kts"} {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: project dir the user named + a fixed filename
		if err != nil {
			continue
		}
		if n := scanRelease(string(b)); n > best {
			best = n
		}
	}
	return best
}

var releasePatterns = []*regexp.Regexp{
	// Maven: <maven.compiler.release>, <maven.compiler.source>,
	// <maven.compiler.target>, and the Spring Boot convention <java.version>.
	regexp.MustCompile(`(?i)<(?:maven\.compiler\.(?:release|source|target)|java\.version)>\s*(?:1\.)?(\d+)\s*</`),
	// Gradle: sourceCompatibility = '21', JavaVersion.VERSION_21,
	// languageVersion = JavaLanguageVersion.of(21).
	regexp.MustCompile(`(?i)(?:source|target)Compatibility\s*=?\s*['"]?(?:1\.)?(\d+)`),
	regexp.MustCompile(`JavaVersion\.VERSION_(?:1_)?(\d+)`),
	regexp.MustCompile(`JavaLanguageVersion\.of\(\s*(\d+)\s*\)`),
}

func scanRelease(text string) int {
	best := 0
	for _, re := range releasePatterns {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			n, err := strconv.Atoi(strings.TrimSpace(m[1]))
			// Bounds, not decoration: a version-like number outside the range
			// of real Java releases is something else that happened to match,
			// and must not be allowed to block a project that would have built.
			if err != nil || n < 5 || n > 99 {
				continue
			}
			if n > best {
				best = n
			}
		}
	}
	return best
}

// releaseMismatch returns the reason this indexer cannot compile this project,
// or "" when it can or when the question cannot be answered.
//
// FAILS OPEN, in both directions, and that asymmetry is the design:
//
//   - A project that declares nothing we recognise (a parent POM, a property
//     indirection, a profile) reads as 0 and is never blocked. It may still
//     fail at build time -- which is exactly where it fails today, so an
//     undetected case is no worse than the status quo.
//   - A pin with no declared ceiling blocks nothing.
//
// The only case that blocks is one where BOTH numbers are known and the project
// is definitively out of range. A false block would be worse than the bug this
// fixes: it would refuse a project that works.
func releaseMismatch(p Pin, dir string) string {
	if p.MaxLanguageRelease == 0 {
		return ""
	}
	need := requiredRelease(dir)
	if need == 0 || need <= p.MaxLanguageRelease {
		return ""
	}
	return "this project declares Java " + strconv.Itoa(need) +
		", and the pinned " + string(p.Indexer) + " image compiles at most Java " +
		strconv.Itoa(p.MaxLanguageRelease) +
		"; its build would fail with \"release version " + strconv.Itoa(need) +
		" not supported\". Pin a newer image, or index this project elsewhere."
}
