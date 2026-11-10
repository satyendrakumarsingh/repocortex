package scip

import (
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// Strategy names how a SCIP document path was reconciled with a repo-relative
// path. It is reported per index so that "SCIP produced no edges" always comes
// with an explanation of what was tried.
type Strategy string

const (
	// StrategyExplicit means the caller supplied Options.PathPrefix and it was
	// used verbatim. No fallback is attempted: if the caller states where the
	// sub-project sits, silently ignoring them hides their mistake.
	StrategyExplicit Strategy = "explicit-prefix"

	// StrategyProjectRoot means the prefix was derived by locating
	// Metadata.project_root underneath Options.RepoRoot. This is the normal
	// path for a monorepo sub-project or a Gradle module.
	StrategyProjectRoot Strategy = "project-root"

	// StrategyDocument means the document path already was repo-relative — the
	// usual case when the indexer ran at the repository root, and the fallback
	// when project_root is an absolute path from some other machine or
	// container and so cannot be related to RepoRoot at all.
	StrategyDocument Strategy = "document-path"

	// StrategySuffix means the document path matched exactly one known file as
	// a path suffix. This rescues indexes whose project_root is unusable and
	// whose documents are relative to a directory RepoCortex cannot name, at the
	// cost of being a match rather than a derivation — which is why it is last
	// and why ambiguous matches are refused rather than picked from.
	StrategySuffix Strategy = "suffix-match"

	// StrategyUnverified means the caller supplied no file list, so the
	// computed path was accepted without checking that it names a file RepoCortex
	// indexed. Mismatch counts are meaningless in this mode, and saying so is
	// better than reporting a confident zero.
	StrategyUnverified Strategy = "unverified"
)

// pathMapper reconciles indexer-relative document paths with RepoCortex's
// repo-relative ones.
//
// The problem it solves: Document.relative_path is relative to
// Metadata.project_root, which is the *indexer's* notion of the project. For a
// Gradle module that is `services/billing`, so the index calls a file
// `src/main/java/Foo.java` while RepoCortex knows it as
// `services/billing/src/main/java/Foo.java`. Joining the two is easy; noticing
// when the join produced a path that names nothing is the part that matters,
// because an unnoticed mismatch presents as an index that produced no edges
// with no indication of why.
type pathMapper struct {
	repoRoot string
	explicit string
	prefix   string // derived from Metadata.project_root, set during ingest

	known  map[string]struct{}
	byBase map[string][]string // lazily built, only when suffix matching is reached
}

func newPathMapper(opts Options) *pathMapper {
	m := &pathMapper{
		repoRoot: normalizePath(opts.RepoRoot),
		explicit: strings.Trim(filepath.ToSlash(opts.PathPrefix), "/"),
	}
	if opts.KnownPaths != nil {
		m.known = make(map[string]struct{}, len(opts.KnownPaths))
		for _, p := range opts.KnownPaths {
			if c, ok := cleanDocumentPath(p); ok {
				m.known[c] = struct{}{}
			}
		}
	}
	return m
}

// verifying reports whether the mapper can check a candidate against reality.
func (m *pathMapper) verifying() bool { return m.known != nil }

// setProjectRoot derives the sub-project prefix from the index metadata.
//
// Failure here is expected and not an error: indexers frequently run in a
// container or CI checkout whose absolute paths have nothing to do with the
// caller's, and the document-path and suffix strategies exist for exactly that
// case.
func (m *pathMapper) setProjectRoot(root string) {
	m.prefix = derivePrefix(m.repoRoot, root)
}

// resolve maps one document path, reporting which strategy succeeded.
func (m *pathMapper) resolve(docPath string) (string, Strategy, bool) {
	clean, ok := cleanDocumentPath(docPath)
	if !ok {
		return "", "", false
	}

	if m.explicit != "" {
		cand := path.Join(m.explicit, clean)
		if !m.verifying() {
			return cand, StrategyExplicit, true
		}
		if m.isKnown(cand) {
			return cand, StrategyExplicit, true
		}
		return "", "", false
	}

	if !m.verifying() {
		return path.Join(m.prefix, clean), StrategyUnverified, true
	}
	if m.prefix != "" {
		if cand := path.Join(m.prefix, clean); m.isKnown(cand) {
			return cand, StrategyProjectRoot, true
		}
	}
	if m.isKnown(clean) {
		return clean, StrategyDocument, true
	}
	if cand, ok := m.suffixMatch(clean); ok {
		return cand, StrategySuffix, true
	}
	return "", "", false
}

func (m *pathMapper) isKnown(p string) bool {
	_, ok := m.known[p]
	return ok
}

// suffixMatch finds the unique known file ending in "/"+clean.
//
// Indexed by base name so the cost is one map lookup plus a walk of the files
// sharing that name, rather than a scan of the whole repository per document.
// The index is built on first use because most indexes never reach this
// strategy.
func (m *pathMapper) suffixMatch(clean string) (string, bool) {
	if m.byBase == nil {
		m.byBase = make(map[string][]string, len(m.known))
		for p := range m.known {
			b := path.Base(p)
			m.byBase[b] = append(m.byBase[b], p)
		}
	}
	suffix := "/" + clean
	var found string
	for _, cand := range m.byBase[path.Base(clean)] {
		if !strings.HasSuffix(cand, suffix) {
			continue
		}
		if found != "" {
			// Two files match. Picking one would attribute compiler facts to
			// the wrong file, so refuse; the caller sees an unmatched document
			// and can supply PathPrefix.
			return "", false
		}
		found = cand
	}
	return found, found != ""
}

// derivePrefix returns the repo-relative directory that projectRoot names, or
// "" if it cannot be placed inside repoRoot.
func derivePrefix(repoRoot, projectRoot string) string {
	if repoRoot == "" || projectRoot == "" {
		return ""
	}
	pr := normalizeProjectRoot(projectRoot)
	if pr == "" || pr == repoRoot {
		return ""
	}
	rel, err := filepath.Rel(filepath.FromSlash(repoRoot), filepath.FromSlash(pr))
	if err != nil {
		return ""
	}
	s := filepath.ToSlash(rel)
	if s == "." || s == ".." || strings.HasPrefix(s, "../") {
		return ""
	}
	return s
}

// normalizeProjectRoot decodes Metadata.project_root, which the SCIP spec
// defines as a URI-encoded absolute path — in practice either a file:// URI or
// a bare path, depending on the indexer.
func normalizeProjectRoot(root string) string {
	s := strings.TrimSpace(root)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "file://") {
		if u, err := url.Parse(s); err == nil {
			s = u.Path
		} else {
			s = strings.TrimPrefix(s, "file://")
		}
	} else if unescaped, err := url.PathUnescape(s); err == nil {
		s = unescaped
	}
	// A Windows file URI yields "/C:/src/repo"; the leading slash is part of
	// the URI syntax, not of the path.
	if len(s) >= 3 && s[0] == '/' && s[2] == ':' {
		s = s[1:]
	}
	return normalizePath(s)
}

func normalizePath(p string) string {
	s := strings.TrimSpace(filepath.ToSlash(p))
	if s == "" {
		return ""
	}
	s = path.Clean(s)
	if s != "/" {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// cleanDocumentPath normalises a document path and rejects anything that
// escapes the repository.
//
// A .scip file is untrusted input: it arrives with the repository, and a
// hostile one can name `../../../etc/passwd` or `/etc/shadow` as a document.
// Nothing here opens files, but the path is stored and later handed to an agent
// as a location to read, so it must never leave the tree. Rejected paths are
// counted as unmatched rather than dropped quietly.
func cleanDocumentPath(p string) (string, bool) {
	s := strings.TrimSpace(filepath.ToSlash(p))
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "/") {
		return "", false
	}
	// Windows drive-absolute ("C:/x") and UNC-ish forms.
	if len(s) >= 2 && s[1] == ':' {
		return "", false
	}
	if strings.ContainsRune(s, 0) {
		return "", false
	}
	c := path.Clean(s)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}
