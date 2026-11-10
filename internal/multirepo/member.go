package multirepo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Member is one repository in the system, as handed to a [Federator].
//
// Source is nil when the caller could not open this member's index, and
// Unavailable then says why. That combination is a first-class input rather
// than an error the caller should have returned: a system graph that drops a
// member it could not read reports a smaller system than the one that exists,
// and reads as completeness. The member appears in the output either way.
type Member struct {
	// Name is how the member is identified in every line of the report. It is
	// operator-supplied because a directory name is not a service name, and
	// because two checkouts of the same repository at different revisions are a
	// legitimate thing to federate.
	Name string

	// IndexPath is the database this member was read from, carried so the
	// report can name the file somebody has to go and rebuild.
	IndexPath string

	// Source reads the member's committed run. Nil when unavailable.
	Source MemberSource

	// RunID is the committed run inside that index.
	RunID int64

	// Unavailable is the reason Source is nil, written for the person who has
	// to fix it. Empty when the member opened.
	Unavailable string
}

// Available reports whether this member contributed any facts.
func (m Member) Available() bool { return m.Source != nil && m.Unavailable == "" }

// EstateFile is the on-disk list of member repositories.
//
// WHY A FILE AND NOT ONLY FLAGS. The member list is a property of the ESTATE,
// not of an invocation: it is the same for everyone on the team, it changes
// when a service is added or retired, and it is exactly the kind of thing that
// should be reviewed in a pull request. Retyping eight --member flags is how a
// member silently goes missing, and a missing member is the precise failure
// this report exists to prevent. --member survives for the ad-hoc two-repo
// question, which is the other real case.
//
// WHY NOT .cortex.yml. That file is per-repository indexing configuration
// owned by internal/config, and the estate is not a property of any one of its
// members — putting it there would mean N copies that drift.
type EstateFile struct {
	// Members are listed in file order; the report sorts by name regardless, so
	// the order here is documentation rather than semantics.
	Members []EstateMember `yaml:"members"`
}

// EstateMember is one entry in an [EstateFile].
type EstateMember struct {
	Name string `yaml:"name"`
	// Index is the path to that repository's index database, absolute or
	// relative to the estate file itself. Relative to the FILE and not to the
	// working directory, so `cortex system --members ../estate.yml` means the
	// same thing from every directory.
	Index string `yaml:"index"`
}

// LoadEstate reads and validates an estate file.
//
// Strict about two things, because both are silent-wrong-answer generators: an
// entry missing a name or an index path is refused rather than defaulted, and a
// duplicate name is refused rather than last-one-wins. Two members sharing a
// name would make every "which repository breaks" line ambiguous, which is the
// one property the output cannot lose.
func LoadEstate(path string) ([]EstateMember, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied
	if err != nil {
		return nil, fmt.Errorf("multirepo: read estate file %s: %w", path, err)
	}
	var f EstateFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("multirepo: parse estate file %s: %w", path, err)
	}
	if len(f.Members) == 0 {
		return nil, fmt.Errorf(
			"multirepo: %s lists no members under 'members:' — a system graph of "+
				"nothing is not a shorter answer, it is a wrong one", path)
	}

	dir := filepath.Dir(path)
	seen := map[string]int{}
	out := make([]EstateMember, 0, len(f.Members))
	for i, m := range f.Members {
		name := strings.TrimSpace(m.Name)
		idx := strings.TrimSpace(m.Index)
		if name == "" {
			return nil, fmt.Errorf("multirepo: %s: member %d has no name", path, i+1)
		}
		if idx == "" {
			return nil, fmt.Errorf("multirepo: %s: member %q has no index path", path, name)
		}
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf(
				"multirepo: %s: two members are both named %q (entries %d and %d); "+
					"names identify repositories in every line of the report and must be distinct",
				path, name, prev, i+1)
		}
		seen[name] = i + 1
		if !filepath.IsAbs(idx) {
			idx = filepath.Join(dir, idx)
		}
		out = append(out, EstateMember{Name: name, Index: filepath.Clean(idx)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ParseMemberFlag reads one `--member name=path` value.
//
// Split on the FIRST '=' only: a Windows path or a query-ish filename can
// contain another, and a name cannot.
func ParseMemberFlag(raw string) (EstateMember, error) {
	name, path, ok := strings.Cut(raw, "=")
	name, path = strings.TrimSpace(name), strings.TrimSpace(path)
	if !ok || name == "" || path == "" {
		return EstateMember{}, fmt.Errorf(
			"--member %q is not name=path (for example: --member orders-api=../orders-api/.cortex/index.db)",
			raw)
	}
	return EstateMember{Name: name, Index: path}, nil
}
