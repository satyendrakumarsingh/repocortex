package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// Execution is the outcome of running one indexer over one project.
type Execution struct {
	Project Project
	Indexer Indexer

	// IndexPath is the .scip file, ready for scip.Ingester.IngestFile. Empty
	// when the run failed.
	IndexPath string

	// Bytes is the size of that file. Zero bytes with exit code 0 is a real
	// failure mode for these tools and the caller should treat it as one.
	Bytes int64

	Argv     []string
	ExitCode int
	Duration time.Duration
	TimedOut bool

	// Diagnostic is a bounded excerpt of the tool's output, kept only for a
	// failed run. It may contain fragments of source code, so it belongs in a
	// message shown to the user who asked for the index — never in a log file
	// and never in errs.Fields.
	Diagnostic string
}

// Executor runs indexers.
type Executor struct {
	Runner Runner
	Audit  AuditSink

	// MountLocalArtifacts gives a containerised build the invoking user's
	// artifact cache. Off unless user config asked for it; see
	// config.UserConfig for what it costs.
	MountLocalArtifacts bool

	// Timeout bounds one indexer. This is config.Config.IndexerTimeout: a
	// Gradle build on a cold cache genuinely takes minutes, and the user is the
	// only one who knows how many they are willing to wait.
	Timeout time.Duration

	// RunnerKind is "native" or "docker".
	RunnerKind string
}

// OutputDir creates the one directory this package writes to.
//
// It is under os.TempDir and owned by this process. Nothing RepoCortex produces
// is written into the repository, so indexing a repository never dirties a
// working tree or, worse, overwrites a file in it. (A Java build still writes
// its own target/ — that is Maven's doing and refusing it would mean refusing
// exact Java altogether.)
func OutputDir() (string, error) {
	dir, err := os.MkdirTemp("", "cortex-scip-")
	if err != nil {
		return "", errs.Wrap(errs.KindInternal, "indexer.OutputDir", err)
	}
	return dir, nil
}

// Run executes one indexer and returns the index it produced.
//
// The repository directory crosses into the subprocess as Command.Dir, never as
// an argument. Every argument below is either a compile-time constant or a path
// this package constructed inside its own temp directory.
func (e *Executor) Run(ctx context.Context, inst Installed, p Project, repoRoot, outDir string) (Execution, error) {
	if !filepath.IsAbs(repoRoot) || !filepath.IsAbs(outDir) {
		return Execution{}, errs.New(errs.KindInvalid, "indexer.Run",
			"repository root and output directory must be absolute")
	}
	rel, err := sanitizeRel(p.Dir)
	if err != nil {
		return Execution{}, errs.Wrap(errs.KindInvalid, "indexer.Run", err)
	}
	projectDir := repoRoot
	if rel != "" {
		projectDir = filepath.Join(repoRoot, filepath.FromSlash(rel))
	}
	indexPath := filepath.Join(outDir, slugFor(p)+".scip")

	cmd, err := e.command(inst, p, rel, projectDir, repoRoot, outDir, indexPath)
	if err != nil {
		return Execution{}, err
	}
	cmd.Timeout = e.timeout()

	res, runErr := e.Runner.Run(ctx, cmd)
	recordErr(e.Audit, ActionIndex, AuditRecord{
		Indexer: inst.Indexer, Language: p.Language, Result: res,
	}, runErr)

	ex := Execution{
		Project:  p,
		Indexer:  inst.Indexer,
		Argv:     res.Argv,
		ExitCode: res.ExitCode,
		Duration: res.Duration,
		TimedOut: res.TimedOut,
	}
	if runErr != nil {
		ex.Diagnostic = res.Diagnostic()
		// A timeout unwraps to context.DeadlineExceeded, which errs classifies
		// as KindCancelled — a run we stopped, not a fault to report as a bug.
		return ex, errs.Wrap(errs.KindOf(runErr), "indexer.Run",
			fmt.Errorf("%s on %s: %w", inst.Indexer, p, runErr)).
			WithField("language", string(p.Language)).
			WithField("project_dir", p.Dir)
	}
	if res.ExitCode != 0 {
		ex.Diagnostic = res.Diagnostic()
		// KindUnsupported: the tool ran and could not index this project. In
		// `auto` the caller degrades to heuristics; in `exact` it must fail.
		// Either way it is not a RepoCortex bug and not retryable.
		return ex, errs.Wrap(errs.KindUnsupported, "indexer.Run",
			fmt.Errorf("%s exited %d for %s", inst.Indexer, res.ExitCode, p)).
			WithField("language", string(p.Language)).
			WithField("project_dir", p.Dir).
			WithField("exit_code", res.ExitCode)
	}

	info, statErr := os.Stat(indexPath)
	if statErr != nil {
		// Exit 0 and no output. Reporting success here would hand the caller a
		// path that does not exist, and a later "file not found" would look
		// like a RepoCortex bug rather than an indexer that produced nothing.
		ex.Diagnostic = res.Diagnostic()
		return ex, errs.Wrap(errs.KindUnsupported, "indexer.Run",
			fmt.Errorf("%s reported success but wrote no index for %s", inst.Indexer, p)).
			WithField("language", string(p.Language)).
			WithField("project_dir", p.Dir)
	}
	// Exit 0 and an index with no documents. This is not a hypothetical: it is
	// how scip-go reports a module go/packages could not load, and it reached
	// import as "none of the index's 0 definition(s) matched a symbol in this
	// repository" — a message that blames the repository. See scipcontent.go.
	//
	// A file we cannot parse is reported as itself rather than as empty, and is
	// not treated as a reason to reject an index the tool says it wrote.
	hasDocs, scanErr := scipHasDocuments(indexPath)
	if scanErr != nil {
		ex.Diagnostic = res.Diagnostic()
		return ex, errs.Wrap(errs.KindUnsupported, "indexer.Run",
			fmt.Errorf("%s wrote an unreadable index for %s: %w", inst.Indexer, p, scanErr)).
			WithField("language", string(p.Language)).
			WithField("project_dir", p.Dir)
	}
	if !hasDocs {
		ex.Diagnostic = res.Diagnostic()
		return ex, emptyIndexError(inst.Indexer, p)
	}

	ex.IndexPath = indexPath
	ex.Bytes = info.Size()
	return ex, nil
}

func (e *Executor) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return DefaultTimeout
}

// command builds the argv for one indexer.
//
// Read this function as a list of what is NOT here. No --config flag pointing
// at a repository file. No project name taken from .cortex.yml. No package
// manager install step: `npm install` and `mvn dependency:resolve` both execute
// repository-supplied code, so scip-typescript indexes whatever node_modules is
// already on disk and reports less rather than RepoCortex running the repository's
// install hooks. And no interpolation anywhere — every element is appended as
// its own argv slot.
func (e *Executor) command(inst Installed, p Project, rel, projectDir, repoRoot, outDir, indexPath string) (Command, error) {
	switch inst.Indexer {
	case SCIPGo:
		return Command{
			Path: inst.Path,
			Args: []string{"--output", indexPath},
			Dir:  projectDir,
			// scip-go drives go/packages, which reads the repository's go.mod.
			// GOTOOLCHAIN must therefore be chosen by us, or a
			// `toolchain go1.99.9` line in an untrusted repository would make
			// the Go command download and execute a compiler of the
			// repository's choosing. It is also the reason Go indexing produced
			// nothing at all until now: see gotoolchain.go for the failure and
			// for why raising the pin to the project's `go` directive — and to
			// nothing else — keeps the guarantee.
			Env: goEnv("GOTOOLCHAIN=" + goIndexToolchain(projectDir)),
		}, nil

	case SCIPTypeScript:
		args := []string{"index", "--output", indexPath}
		if !p.hasMarker("tsconfig.json") {
			// Decided by a file NAME, never by its contents.
			args = append(args, "--infer-tsconfig")
		}
		return Command{Path: inst.Path, Args: args, Dir: projectDir, Env: os.Environ()}, nil

	case SCIPPython:
		return Command{
			Path: inst.Path,
			Args: []string{
				"index", "--output", indexPath,
				// Fixed literals. scip-python wants a project name and version,
				// and the tempting sources for them — package.json's "name",
				// pyproject.toml's [project], .cortex.yml's project.name — are
				// all attacker-controlled strings that would land in an argv. The
				// name only labels the index, so a constant costs nothing.
				"--project-name", "cortex-repository",
				"--project-version", "0.0.0",
			},
			Dir: projectDir,
			Env: os.Environ(),
		}, nil

	case SCIPJava:
		if e.RunnerKind == RunnerDocker {
			return e.dockerJavaCommand(inst, p, rel, repoRoot, outDir, indexPath)
		}
		// Native scip-java drives Maven or Gradle over the repository's own
		// build files. See Options.AllowRepoBuild for why reaching this point
		// requires an explicit decision, and java.go for why the argv is not
		// simply "index --output".
		return Command{
			Path: inst.Path,
			Args: javaArgs(p, indexPath),
			Dir:  projectDir,
			Env:  os.Environ(),
		}, nil
	}
	return Command{}, errs.New(errs.KindInternal, "indexer.Run",
		"no command defined for indexer "+string(inst.Indexer))
}

// dockerJavaCommand builds the container invocation.
//
// The container is the security feature: the repository's build system executes
// inside it, so a malicious Maven plugin gets a container filesystem rather than
// the developer's home directory. That property is only real if the flags below
// hold, which is why each is spelled out rather than inherited from a helper:
//
//   - The repository is mounted read-write, because Maven and Gradle write
//     target/ and .gradle/ and fail without it. This is the one concession, and
//     it is bounded to the repository the user asked to index.
//   - The output directory is a separate mount, so the index cannot be written
//     into the tree.
//   - --rm leaves nothing behind, and the image is always referenced by digest.
//   - No --privileged, no docker socket, no --network host.
//   - The container runs as the invoking uid so the .scip file is readable
//     afterwards and is not left root-owned in the user's temp directory.
func (e *Executor) dockerJavaCommand(inst Installed, p Project, rel, repoRoot, outDir, indexPath string) (Command, error) {
	docker, err := e.Runner.LookPath(string(ToolDocker))
	if err != nil {
		return Command{}, errs.Wrap(errs.KindUnsupported, "indexer.Run",
			fmt.Errorf("runner is docker but docker is unavailable: %w", err))
	}
	if inst.Image == "" {
		return Command{}, errs.New(errs.KindInvalid, "indexer.Run",
			"docker runner requires a pinned image digest")
	}

	const workMount, outMount = "/work", "/out"
	workdir := workMount
	if rel != "" {
		// rel has been through sanitizeRel: no "..", no leading dash, no
		// absolute path. It is a single argv element either way.
		workdir = workMount + "/" + rel
	}

	args := []string{
		"run", "--rm",
		"-v", repoRoot + ":" + workMount,
		"-v", outDir + ":" + outMount,
		"-w", workdir,
	}
	if uid := currentUser(); uid != "" {
		args = append(args, "-u", uid)
		// A numeric uid has no passwd entry inside the image, so HOME resolves
		// to "/" and every tool that wants a cache directory fails on it.
		// scip-java dies immediately: coursier tries to create /.cache and
		// gets AccessDeniedException before the build starts, which surfaced
		// only as "indexer failed".
		//
		// The output mount is already writable by this uid — it has to be, the
		// index is written there — so it is the one directory guaranteed to
		// work. Caches land beside the index and are discarded with it, which
		// costs a cold dependency download per run and is the right trade for
		// a container whose whole point is leaving no trace on the host.
		args = append(args, "-e", "HOME="+outMount)

		// HOME is not what a JVM reads. Its `user.home` property comes from
		// the passwd entry for the running uid, and a numeric uid the image
		// has never heard of resolves it to "?". scip-java's wrapper fetches
		// its own JDK through `coursier java`, coursier is a JVM program, and
		// its cache root is `${user.home}/.cache/coursier` — so on the first
		// repository Java was measured on, 611 MB of Temurin landed in a
		// directory literally named `?` at the root of the indexed
		// repository: untracked, not gitignored, and outside every mount
		// above. The output mount is the home the shell was just given, so
		// the JVM gets the same one.
		//
		// JDK_JAVA_OPTIONS rather than JAVA_TOOL_OPTIONS: every JDK 9+
		// launcher reads it, and it does not displace the JAVA_TOOL_OPTIONS
		// the image sets for container memory limits — which this runner
		// would otherwise have to know and repeat to keep.
		args = append(args, "-e", "JDK_JAVA_OPTIONS=-Duser.home="+outMount)

		// The local artifact cache, when the user has asked for it.
		//
		// Without this a private dependency cannot resolve inside the
		// container and Java indexing is impossible for exactly the
		// repositories most worth indexing — the ones with internal libraries.
		// Mounted at the HOME the container was just given, which is where
		// Maven looks.
		if e.MountLocalArtifacts {
			if home, err := os.UserHomeDir(); err == nil {
				if m2 := filepath.Join(home, ".m2"); dirExists(m2) {
					args = append(args, "-v", m2+":"+outMount+"/.m2")
				}
			}
		}
	}
	args = append(args, inst.Image)
	args = append(args, inst.DockerEntryOr("scip-java")...)
	args = append(args, javaArgs(p, outMount+"/"+filepath.Base(indexPath))...)

	return Command{Path: docker, Args: args, Env: os.Environ()}, nil
}

// DockerEntryOr returns the in-image argv prefix, defaulting to the binary name.
func (i Installed) DockerEntryOr(def string) []string {
	if pin, ok := PinFor(languageOf(i.Indexer)); ok && len(pin.DockerEntry) > 0 {
		return pin.DockerEntry
	}
	return []string{def}
}

func languageOf(ix Indexer) Language {
	for _, p := range pins {
		if p.Indexer == ix {
			return p.Language
		}
	}
	return ""
}

// currentUser renders "uid:gid" for `docker run -u`, or "" where that is
// meaningless. On Windows Getuid returns -1 and containers do not take numeric
// ids, so the flag is simply omitted there.
func currentUser() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
