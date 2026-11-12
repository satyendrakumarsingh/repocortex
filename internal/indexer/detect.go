package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// Language names a language RepoCortex has a compiler-backed indexer for.
//
// The set is closed and matches Pins. A repository written in something else is
// not "undetected because we failed" — there is simply no SCIP indexer wired up
// for it, so `exact` makes no promise about it. See Plan.Err for why that
// distinction is load-bearing.
type Language string

const (
	LangGo         Language = "go"
	LangTypeScript Language = "typescript"
	LangJava       Language = "java"
	LangPython     Language = "python"
)

// Project is one buildable unit found in the repository.
type Project struct {
	// Language is what the marker files say this directory is.
	Language Language

	// Dir is the sub-project root, slash-separated and relative to the
	// repository root; "." for the repository root itself. Relative on purpose:
	// it is safe to log (see internal/errs on Fields) and it is the only
	// repository-derived value that reaches a subprocess.
	Dir string

	// Markers are the base names that identified the project, sorted. They are
	// evidence for `repocortex indexers list` — "why did you decide this is a
	// Gradle project" has to have an answer.
	Markers []string
}

// String renders "java at services/api".
func (p Project) String() string {
	if p.Dir == "." {
		return string(p.Language) + " at repository root"
	}
	return string(p.Language) + " at " + p.Dir
}

// marker maps a file name to the language its presence implies.
//
// The whole of detection is this table plus os.Stat. No marker file is ever
// OPENED, which is the point: package.json's `scripts`, a Makefile's targets
// and pom.xml's plugin list are all attacker-supplied program text, and the
// moment any of them influences what RepoCortex runs, cloning a repository
// becomes code execution. Existence of a name cannot carry a payload.
var markerLanguages = map[string]Language{
	"pom.xml":          LangJava,
	"build.gradle":     LangJava,
	"build.gradle.kts": LangJava,

	"tsconfig.json": LangTypeScript,
	// package.json without a tsconfig.json still gets scip-typescript, which
	// type-checks plain JavaScript with inferred settings. Detecting it does not
	// mean reading it.
	"package.json": LangTypeScript,

	"go.mod": LangGo,

	"pyproject.toml":   LangPython,
	"requirements.txt": LangPython,
	"setup.py":         LangPython,
}

// skipDirs are never descended into.
//
// node_modules is the important one: a mid-sized frontend repository has tens of
// thousands of package.json files under it and not one of them is a project of
// this repository. The rest are build output, which contains generated copies of
// marker files.
//
// Dot-directories are skipped wholesale by the walk in addition to this list.
// That loses a project living in a hidden directory, which no build system does
// on purpose, and gains not walking .git, .cache or a committed .venv.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"target": true, "build": true, "dist": true, "out": true,
	".gradle": true, ".mvn": true, ".venv": true, "venv": true,
	"__pycache__": true, ".tox": true, ".repocortex": true,
	".idea": true, ".vscode": true, "testdata": true,
}

// maxDetectDepth bounds how deep a sub-project may sit below the repository
// root. Real monorepos nest two or three levels (services/api, packages/ui);
// eight is generous. The bound exists so that a pathological or deliberately
// deep tree cannot turn detection into an unbounded walk.
const maxDetectDepth = 8

// Detect finds every project in repoRoot.
//
// A monorepo yields several Projects, one per (directory, language) pair — a
// directory holding both package.json and pyproject.toml is genuinely two
// projects and gets two indexers. The result is sorted by directory then
// language so that a plan is deterministic; `repocortex indexers list` printing a
// different order on each run would be a bug report waiting to happen.
//
// Symbolic links are not followed. fs.WalkDir does not follow them, and that is
// relied upon rather than merely tolerated: a repository can commit a symlink to
// /, and following it would walk the machine.
func Detect(ctx context.Context, repoRoot string) ([]Project, error) {
	info, err := os.Stat(repoRoot)
	if err != nil {
		return nil, errs.Wrap(errs.KindNotFound, "indexer.Detect", err)
	}
	if !info.IsDir() {
		return nil, errs.New(errs.KindInvalid, "indexer.Detect", "repository root is not a directory")
	}

	// Keyed by directory, then language, so several markers for one language
	// collapse into one project carrying all of them as evidence.
	found := map[string]map[Language][]string{}
	var jsSourceDirs []string

	walkErr := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		// Checked first, and on every entry. This walks a whole repository, so
		// without it a Ctrl-C during `repocortex indexers list` on a large tree
		// is simply ignored until the walk finishes. internal/acquire/fs does
		// the same thing in its own walker; this one had been missed.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			// An unreadable subdirectory is normal on a developer machine and
			// is not a reason to abandon detection of everything else.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, p)
		if relErr != nil {
			// Unreachable in practice — WalkDir only ever hands back paths
			// under repoRoot — and a path we cannot place relative to the root
			// is one we must not index. Skipping it is the safe reading.
			return nil //nolint:nilerr // deliberately skip, not abort the walk
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if rel == "." {
				return nil
			}
			name := path.Base(rel)
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if strings.Count(rel, "/")+1 > maxDetectDepth {
				return fs.SkipDir
			}
			return nil
		}

		if d.Type()&fs.ModeSymlink == 0 && tsSourceExt[path.Ext(rel)] {
			jsSourceDirs = append(jsSourceDirs, path.Dir(rel))
		}

		lang, ok := markerLanguages[path.Base(rel)]
		if !ok {
			return nil
		}
		// A marker reached through a symlink is not evidence about this
		// repository, and resolving it could point anywhere.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		dir := path.Dir(rel)
		byLang := found[dir]
		if byLang == nil {
			byLang = map[Language][]string{}
			found[dir] = byLang
		}
		byLang[lang] = append(byLang[lang], path.Base(rel))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("indexer: walk %s: %w", repoRoot, walkErr)
	}

	var projects []Project
	for dir, byLang := range found {
		for lang, markers := range byLang {
			sort.Strings(markers)
			// A bare package.json (no tsconfig.json) only means "JavaScript may be
			// here". scip-typescript indexes nothing in a directory with no .ts,
			// .tsx, .js or .jsx files, and that run is a failed indexer, not a
			// project. Keep the project only where such a file exists beneath it.
			if lang == LangTypeScript && len(markers) == 1 && markers[0] == "package.json" &&
				!hasSourceUnder(dir, jsSourceDirs) {
				continue
			}
			projects = append(projects, Project{Language: lang, Dir: dir, Markers: markers})
		}
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Dir != projects[j].Dir {
			return projects[i].Dir < projects[j].Dir
		}
		return projects[i].Language < projects[j].Language
	})
	return projects, nil
}

// tsSourceExt are the extensions scip-typescript indexes as program input.
var tsSourceExt = map[string]bool{".ts": true, ".tsx": true, ".js": true, ".jsx": true}

// hasSourceUnder reports whether any recorded source directory is dir or lies
// beneath it. "." is the repository root and contains everything.
func hasSourceUnder(dir string, sourceDirs []string) bool {
	for _, sd := range sourceDirs {
		if dir == "." || sd == dir || strings.HasPrefix(sd, dir+"/") {
			return true
		}
	}
	return false
}

// hasMarker reports whether the project was identified by this exact file name.
// Used for the handful of decisions a marker name legitimately drives, such as
// whether scip-typescript needs --infer-tsconfig.
func (p Project) hasMarker(name string) bool {
	for _, m := range p.Markers {
		if m == name {
			return true
		}
	}
	return false
}

// Languages returns the distinct languages present in projects, sorted.
func Languages(projects []Project) []Language {
	seen := map[Language]bool{}
	var out []Language
	for _, p := range projects {
		if !seen[p.Language] {
			seen[p.Language] = true
			out = append(out, p.Language)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sanitizeRel vets a repository-relative directory before it is allowed to
// become part of an argv or a file name.
//
// Only the docker runner needs this — every native invocation passes the
// directory as Command.Dir, where it cannot be confused with a flag. Inside a
// `docker run -w /work/<rel>` argument the value is still a single argv element,
// so it cannot introduce a new flag, but "cannot" is a claim worth checking
// rather than asserting: `..` would escape the mount and a leading dash would
// become an option if the argument construction were ever refactored.
func sanitizeRel(rel string) (string, error) {
	if rel == "" || rel == "." {
		return "", nil
	}
	clean := path.Clean(rel)
	if clean != rel {
		return "", fmt.Errorf("indexer: sub-project path %q is not in normal form", rel)
	}
	if path.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("indexer: sub-project path %q escapes the repository", rel)
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, "-") {
			return "", fmt.Errorf("indexer: sub-project path %q has an unusable segment", rel)
		}
	}
	return clean, nil
}

// slugFor turns a project into a file name stem for its .scip output.
//
// The directory is repository-derived, so it is reduced to a conservative
// character class rather than trusted. A directory called `../../etc` never
// reaches here — sanitizeRel rejects it first — but the output path is created
// by RepoCortex inside its own temp directory, and a path traversal there would be
// the one place this package writes outside the sandbox it promised.
func slugFor(p Project) string {
	var b strings.Builder
	b.WriteString(string(p.Language))
	if p.Dir != "" && p.Dir != "." {
		b.WriteByte('-')
		for _, r := range p.Dir {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
	}
	s := b.String()
	if len(s) > 96 {
		s = s[:96]
	}
	return s
}
