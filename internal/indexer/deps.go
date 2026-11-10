package indexer

import (
	"os"
	"path/filepath"
)

// dependencyAdvisory reports that a project's third-party dependencies are
// declared but not installed.
//
// WHY THIS IS WORTH A CHECK. Both npm-channel indexers resolve imports through
// the dependency tree on disk. Without it they do not fail -- they succeed
// smaller. Measured on a two-file TypeScript project: 675 bytes of index
// without node_modules, 732 with, both exiting 0 with "done". The missing 57
// bytes are the resolved external symbol, and nothing in the output
// distinguishes the two runs.
//
// That is the failure mode this codebase treats as the serious one: not an
// error, but a quiet reduction in what the graph knows, indistinguishable from
// a repository that genuinely has fewer edges.
//
// Deliberately NOT the Java-style version ceiling. That check exists because a
// container digest freezes a JDK; the npm indexers install the current compiler
// at provision time (scip-typescript depends on typescript ^5.6.2, resolved
// fresh), and neither package declares an `engines` floor. Inventing a version
// ceiling for them would block projects that work.
func dependencyAdvisory(p Project, dir string) string {
	switch p.Language {
	case LangTypeScript:
		if !exists(filepath.Join(dir, "package.json")) || exists(filepath.Join(dir, "node_modules")) {
			return ""
		}
		return "dependencies are declared in package.json but node_modules is absent; " +
			"imports into third-party packages will not resolve and the index will be " +
			"quietly smaller. Run your package manager's install first."

	case LangPython:
		// A virtualenv inside the project is the common layout; an interpreter
		// managed elsewhere is equally valid, so this only fires when the
		// project declares dependencies AND carries no local environment.
		if !declaresPythonDeps(dir) || hasLocalEnv(dir) {
			return ""
		}
		return "dependencies are declared but no virtualenv is present in the project; " +
			"imports into third-party packages may not resolve and the index will be " +
			"quietly smaller. Activate the environment these dependencies are installed into."
	}
	return ""
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func declaresPythonDeps(dir string) bool {
	for _, f := range []string{"requirements.txt", "pyproject.toml", "Pipfile", "setup.py"} {
		if exists(filepath.Join(dir, f)) {
			return true
		}
	}
	return false
}

func hasLocalEnv(dir string) bool {
	for _, d := range []string{".venv", "venv", "env", ".tox"} {
		if exists(filepath.Join(dir, d)) {
			return true
		}
	}
	return false
}
