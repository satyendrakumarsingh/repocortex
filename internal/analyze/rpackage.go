package analyze

import (
	"bufio"
	"bytes"
	"path"
	"strings"
)

// RPackageFQN namespaces the node an R package's DESCRIPTION declares, so a
// package named like a function cannot collide with it.
func RPackageFQN(name string) string { return "rpackage:" + name }

// IsRPackageFQN reports whether an FQN names an R package declared by a
// DESCRIPTION file.
func IsRPackageFQN(fqn string) bool { return strings.HasPrefix(fqn, "rpackage:") }

// maxDescriptionBytes bounds a DESCRIPTION. A package's is a few hundred
// bytes; one this large is not one.
const maxDescriptionBytes = 256 << 10

// isRDescription reports whether a repository path is an R package's
// DESCRIPTION file.
func isRDescription(p string) bool { return path.Base(p) == "DESCRIPTION" }

// rPackageName reads the `Package:` field of a DESCRIPTION file.
//
// The field is a Debian control field: `Package: ledger` at the start of a
// line. The name must be what R accepts for a package (letters, digits and
// dots, starting with a letter), which is also what keeps another file that
// happens to be called DESCRIPTION from declaring one.
func rPackageName(p string, src []byte) (name string, line int, ok bool) {
	if !isRDescription(p) || len(src) == 0 || len(src) > maxDescriptionBytes {
		return "", 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for n := 1; sc.Scan(); n++ {
		rest, found := strings.CutPrefix(sc.Text(), "Package:")
		if !found {
			continue
		}
		name = strings.TrimSpace(rest)
		if !validRPackageName(name) {
			return "", 0, false
		}
		return name, n, true
	}
	return "", 0, false
}

func validRPackageName(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '.'):
		default:
			return false
		}
	}
	return !strings.HasSuffix(s, ".")
}
