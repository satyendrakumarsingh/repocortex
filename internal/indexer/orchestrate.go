package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// Options are the caller's decisions. Everything here comes from the CLI
// invocation or from trusted user config; nothing in this struct may be filled
// in from the repository being indexed.
type Options struct {
	// Runner reaches the operating system. Nil means ExecRunner.
	Runner Runner

	// Audit receives one record per subprocess. Nil discards them, which is
	// allowed but is not what the CLI should do.
	Audit AuditSink

	// Logger receives progress. Nil is silent.
	Logger *slog.Logger

	// OutputDir is where .scip files are written. Empty means a fresh temp
	// directory, which is the normal case; the caller owns removing it.
	OutputDir string

	// Prompt asks the user to approve one install. Nil means there is nobody to
	// ask, which under ProvisionPrompt refuses rather than assumes.
	Prompt PromptFunc

	// AssumeYes is the explicit non-interactive opt-in (the CLI's --yes). It
	// only has effect under ProvisionPrompt, and it exists so that "install
	// third-party binaries without asking" is always something a human typed.
	// A TTY check must never be used in its place: `cortex index` in CI has
	// no TTY and that is exactly the case that must not auto-install.
	AssumeYes bool

	// AllowRepoBuild authorises indexers that execute the repository's own
	// build system on the host.
	//
	// Today that is scip-java under the native runner: indexing a Maven or
	// Gradle project means running Maven or Gradle, and their plugins and build
	// scripts are arbitrary code by design. There is no version of that which is
	// safe on an unread repository, so it is off by default and Java projects
	// are reported as blocked with the reason spelled out. Under the docker
	// runner the flag is unnecessary — the build runs in a container — which is
	// the honest recommendation to give a user who hits this.
	AllowRepoBuild bool
}

func (o Options) withDefaults() Options {
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	return o
}

func (o Options) log() *slog.Logger {
	if o.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return o.Logger
}

// Step is one indexer that would run, or one that cannot.
type Step struct {
	Project Project
	Indexer Indexer
	Pin     Pin

	// Installed is set when the indexer is already usable. Preexisting tells
	// you whether it was found or would be installed.
	Installed Installed

	// NeedsProvision means the indexer is absent and would have to be
	// installed before this step could run.
	NeedsProvision bool

	// Advisory is a condition that degrades the index without stopping it.
	//
	// It exists because the worst outcome here is not a failure, it is a
	// SUCCESS-SHAPED one. scip-typescript on a project whose dependencies are
	// not installed exits 0, prints "done", and writes a smaller index with
	// external imports unresolved -- measured at 675 bytes against 732 for the
	// same project after `npm install`. Nothing in that run says the result is
	// worse; the user just gets fewer edges and no reason why.
	//
	// Advisory is not Blocked: a partial index is still worth having, and
	// refusing to build one would be the false-block failure the release check
	// is careful to avoid.
	Advisory string

	// MissingTools are the toolchain gaps blocking this step. A missing
	// toolchain is not fixable by provisioning: RepoCortex will install
	// scip-java, it will not install a JDK.
	MissingTools []Tool

	// Blocked means this step cannot run as configured, and Reason says why in
	// a sentence meant for a user.
	Blocked bool
	Reason  string

	// Excluded distinguishes "the user told us not to" from "we cannot".
	//
	// It exists because of exact mode. Failing a run because a language the
	// user explicitly removed from accuracy.indexers.languages was not indexed
	// would be obeying and then punishing the same instruction. An excluded
	// step is reported as a skip and never counts toward Plan.Err.
	Excluded bool
}

// Plan is what would happen. It is data: producing one changes nothing.
type Plan struct {
	RepoRoot string
	Accuracy config.AccuracyMode

	// ProvisionPolicy is carried from user config so Execute honours the same
	// policy the plan was computed under. A zero value means `never`; see
	// policyOrDefault.
	ProvisionPolicy config.ProvisionPolicy

	Runner    string
	Timeout   time.Duration
	Projects  []Project
	Toolchain Toolchain
	Steps     []Step

	// MountLocalArtifacts carries the user's opt-in to giving a containerised
	// build the host's artifact cache (~/.m2). USER config only: a repository
	// must never be able to ask for its own build to be handed the developer's
	// cache. Off by default.
	MountLocalArtifacts bool

	// Note explains a plan with no steps, so that `cortex indexers list` on a
	// fast-mode configuration says why rather than printing nothing.
	Note string
}

// Runnable returns the steps that would actually execute.
func (p Plan) Runnable() []Step {
	var out []Step
	for _, s := range p.Steps {
		if !s.Blocked {
			out = append(out, s)
		}
	}
	return out
}

// BlockedSteps returns the steps that cannot run and why.
func (p Plan) BlockedSteps() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Blocked {
			out = append(out, s)
		}
	}
	return out
}

// Err reports why this plan cannot be carried out as configured, or nil.
//
// Only AccuracyExact can produce an error here, and that is the entire point of
// the mode. `auto` skipping a language is correct behaviour — the run continues
// with heuristic edges, labelled as such. `exact` skipping a language would mean
// publishing a run marked exact whose Java edges were guessed, which is the one
// thing the confidence system exists to make impossible. So it fails, and the
// message names the language and the missing tool, because "exact mode failed"
// with no further detail is an error a user cannot act on.
func (p Plan) Err() error {
	if p.Accuracy != config.AccuracyExact {
		return nil
	}
	blocked := p.BlockedSteps()
	if len(blocked) == 0 {
		return nil
	}
	var (
		langs []string
		seen  = map[Language]bool{}
		parts []string
	)
	for _, s := range blocked {
		if s.Excluded {
			continue
		}
		if !seen[s.Project.Language] {
			seen[s.Project.Language] = true
			langs = append(langs, string(s.Project.Language))
		}
		parts = append(parts, fmt.Sprintf("%s (%s): %s", s.Project.Language, s.Project.Dir, s.Reason))
	}
	if len(parts) == 0 {
		return nil
	}
	sort.Strings(langs)
	err := errs.Wrap(errs.KindUnsupported, "indexer.Plan", fmt.Errorf(
		"accuracy.mode=exact requires compiler-backed indexing for every detected language, "+
			"but %s cannot be indexed here:\n  - %s\n"+
			"install the missing tools, or set accuracy.mode to auto to accept heuristic edges for them",
		strings.Join(langs, ", "), strings.Join(parts, "\n  - ")))
	// Languages and tool names are a fixed public vocabulary, so they are safe
	// in Fields; nothing here is repository content. See internal/errs.
	return err.WithField("languages", langs).WithField("mode", string(config.AccuracyExact))
}

// Results is what Execute did.
type Results struct {
	// Indexes are the .scip files produced, ready for scip.Ingester.
	Indexes []Execution

	// OutputDir holds them, and nothing else: Execute prunes every build cache
	// a contained run wrote beside them before returning, and removes the
	// directory outright when it produced no index. The caller removes what
	// is left (CleanupOutput) once the indexes have been ingested.
	OutputDir string

	// Provisioned lists indexers installed during this run. Empty is the good
	// case.
	Provisioned []Installed

	// Skipped explains everything that did not run. In `auto` this is normal
	// and must be surfaced: a user whose Java edges are heuristic deserves to
	// know it was because mvn is missing, not because RepoCortex decided Java was
	// unimportant.
	Skipped []Skip
}

// Skip is one thing that did not run, and why.
type Skip struct {
	Project      Project
	Indexer      Indexer
	Reason       string
	MissingTools []Tool
	Err          error

	// Diagnostic is the first lines the indexer printed before failing.
	//
	// Captured all along and dropped on the floor: a failed Java index
	// surfaced as "scip-java skipped: indexer failed" and nothing else, while
	// the actual cause sat one struct field away —
	// "Multiple build tools detected: Maven, Gradle" for one repository,
	// "AccessDeniedException: /.cache" for another. Neither is guessable, and
	// both name their own fix.
	Diagnostic string
}

// String renders "java at services/api: mvn is not installed".
func (s Skip) String() string { return s.Project.String() + ": " + s.Reason }

// NewPlan reports what indexing this repository would involve.
//
// It is pure inspection: it stats marker files, probes tool versions, and asks
// whether each indexer is already present. It creates no directories, installs
// nothing, prompts nobody and runs no build. That is what makes it safe to call
// from `cortex indexers list` and from `cortex doctor`.
//
// The only inputs are repoRoot and cfg. cfg's execution fields — accuracy mode,
// provisioning policy, runner, language filter, timeout — can only have come
// from ~/.config/cortex/config.yml, because internal/config's repo-side struct
// has no fields that could set them. Nothing is read out of the repository here
// beyond the names of marker files.
//
// (The obvious name for this function is Plan, which is taken by the type it
// returns. NewPlan is the Go-shaped compromise.)
func NewPlan(ctx context.Context, repoRoot string, cfg config.Config) (Plan, error) {
	return NewPlanWith(ctx, repoRoot, cfg, Options{})
}

// NewPlanWith is NewPlan with an injected Runner and audit sink. It exists so
// that planning can be tested without a JVM, a network or a Docker daemon.
func NewPlanWith(ctx context.Context, repoRoot string, cfg config.Config, opts Options) (Plan, error) {
	opts = opts.withDefaults()

	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return Plan{}, errs.Wrap(errs.KindInvalid, "indexer.Plan", err)
	}

	plan := Plan{
		RepoRoot:            abs,
		Accuracy:            cfg.Accuracy,
		ProvisionPolicy:     cfg.ProvisionPolicy,
		Runner:              cfg.IndexerRunner,
		Timeout:             cfg.IndexerTimeout,
		MountLocalArtifacts: cfg.IndexerMountLocalArtifacts,
	}
	if plan.Runner == "" {
		plan.Runner = RunnerNative
	}
	if plan.Timeout <= 0 {
		plan.Timeout = DefaultTimeout
	}

	if cfg.Accuracy == config.AccuracyFast {
		// Deliberately before detection. `fast` promises tree-sitter only and
		// seconds to index, so it must not pay for a repository walk or a
		// single subprocess.
		plan.Note = "accuracy.mode=fast: no compiler-backed indexer runs"
		return plan, nil
	}

	projects, err := Detect(ctx, abs)
	if err != nil {
		return plan, err
	}
	plan.Projects = projects
	if len(projects) == 0 {
		plan.Note = "no Java, TypeScript, Go or Python project markers found"
		return plan, nil
	}

	plan.Toolchain = Probe(ctx, opts.Runner, opts.Audit, toolsFor(projects, plan.Runner)...)

	prov := &Provisioner{
		Runner:     opts.Runner,
		Policy:     cfg.ProvisionPolicy,
		Prompt:     opts.Prompt,
		Audit:      opts.Audit,
		Timeout:    plan.Timeout,
		AssumeYes:  opts.AssumeYes,
		RunnerKind: plan.Runner,
	}

	allowed := languageFilter(cfg.IndexerLanguages)
	for _, p := range projects {
		plan.Steps = append(plan.Steps, planStep(ctx, p, plan, cfg, allowed, prov, opts))
	}
	return plan, nil
}

// planStep decides the fate of one project. It calls Provisioner.Available,
// which looks the binary up and asks its version — read-only, and the reason
// "already installed" never turns into a download here.
func planStep(ctx context.Context, p Project, plan Plan, cfg config.Config,
	allowed map[Language]bool, prov *Provisioner, opts Options) Step {

	step := Step{Project: p}

	pin, ok := PinFor(p.Language)
	if !ok {
		step.Blocked = true
		step.Reason = "no SCIP indexer is wired up for " + string(p.Language)
		return step
	}
	step.Pin = pin
	step.Indexer = pin.Indexer

	// A user who restricted accuracy.indexers.languages made a deliberate
	// choice in their own config file, so an excluded language is a skip rather
	// than a failure — including under `exact`, where honouring the restriction
	// is what the user asked for. It is still reported.
	if allowed != nil && !allowed[p.Language] {
		step.Blocked = true
		step.Excluded = true
		step.Reason = "excluded by accuracy.indexers.languages"
		return step
	}

	if missing := plan.Toolchain.Missing(RequiredTools(p, plan.Runner)...); len(missing) > 0 {
		step.MissingTools = missing
		step.Blocked = true
		step.Reason = missingToolsReason(missing, plan.Runner)
		return step
	}

	// Preflight before consent, deliberately. Asking a user to accept running
	// an untrusted build, and only then discovering the indexer could never
	// have compiled it, spends their trust on nothing.
	step.Advisory = dependencyAdvisory(p, filepath.Join(plan.RepoRoot, p.Dir))

	if why := releaseMismatch(pin, filepath.Join(plan.RepoRoot, p.Dir)); why != "" {
		step.Blocked = true
		step.Reason = why
		return step
	}

	if p.Language == LangJava && plan.Runner != RunnerDocker && !opts.AllowRepoBuild {
		step.Blocked = true
		step.Reason = "indexing Java runs the repository's own Maven or Gradle build, which executes " +
			"arbitrary code from the repository; re-run with the explicit build opt-in, " +
			"or set accuracy.indexers.runner to docker to run it in a container"
		return step
	}

	if inst, available := prov.Available(ctx, pin); available {
		step.Installed = inst
		return step
	}

	step.NeedsProvision = true
	if pin.Channel == ChannelDocker && plan.Runner != RunnerDocker {
		// Installing scip-java natively means letting coursier fetch JVM
		// artifacts and wiring it into the build — a host mutation with a much
		// larger surface than `go install` or `npm i -g`, and one this package
		// declines to perform on the user's behalf.
		step.Blocked = true
		step.Reason = pin.Binary + " is not installed; RepoCortex installs it only as a container image, " +
			"so either install it yourself or set accuracy.indexers.runner to docker"
		return step
	}
	if cfg.ProvisionPolicy == config.ProvisionNever {
		step.Blocked = true
		step.Reason = fmt.Sprintf("%s is not installed and accuracy.indexers.provision is never", pin.Ref())
		return step
	}
	if err := pin.verifiable(); err != nil {
		// No recorded digest means provisioning would refuse anyway; saying so
		// in the plan is better than promising an install that cannot happen.
		step.Blocked = true
		step.Reason = fmt.Sprintf("%s is not installed and has no pinned digest recorded, so it cannot be verified", pin.Ref())
		return step
	}
	return step
}

func missingToolsReason(missing []Tool, runner string) string {
	names := make([]string, len(missing))
	for i, t := range missing {
		names[i] = string(t)
	}
	if runner == RunnerDocker && len(missing) == 1 && missing[0] == ToolDocker {
		return "accuracy.indexers.runner is docker but no Docker daemon answered"
	}
	verb := "is"
	if len(missing) > 1 {
		verb = "are"
	}
	return strings.Join(names, ", ") + " " + verb + " not installed"
}

// languageFilter turns accuracy.indexers.languages into a set. Nil means no
// restriction, which is different from an empty set.
func languageFilter(langs []string) map[Language]bool {
	if len(langs) == 0 {
		return nil
	}
	set := make(map[Language]bool, len(langs))
	for _, l := range langs {
		set[Language(strings.ToLower(strings.TrimSpace(l)))] = true
	}
	// A user writing `languages: [ts]` or `[js]` means TypeScript. Accepting
	// the obvious aliases beats silently indexing nothing.
	if set["ts"] || set["javascript"] || set["js"] {
		set[LangTypeScript] = true
	}
	if set["py"] {
		set[LangPython] = true
	}
	if set["golang"] {
		set[LangGo] = true
	}
	if set["jvm"] || set["kotlin"] || set["scala"] {
		set[LangJava] = true
	}
	return set
}

// Execute carries out a plan: provisions what policy allows, runs what can run.
//
// The accuracy modes differ only here, and each behaves as documented:
//
//   - fast returns immediately having run nothing.
//   - auto runs every step whose toolchain is present and reports the rest as
//     Skips. The caller falls back to heuristic resolution for those languages
//     and labels the edges accordingly.
//   - exact refuses to start if any detected language is blocked. It does not
//     index the languages it can and quietly omit the rest, because a run
//     published as exact with silent gaps is a false claim about the graph.
func Execute(ctx context.Context, plan Plan, opts Options) (res Results, err error) {
	opts = opts.withDefaults()
	log := opts.log()

	// Whatever way this returns, the output directory holds only the indexes
	// that were produced, and holds nothing at all if none were. The
	// container route points HOME and the JVM's user.home at that directory
	// precisely so the build's caches land there instead of in the
	// repository; that is containment, not a promise to keep 700 MB of
	// somebody else's JDK under the system temp directory until the OS gets
	// round to it. Results.OutputDir outlives this call only so that
	// `scip import` can read the index later, and the index is the only
	// thing that has to survive.
	created := false
	defer func() {
		if res.OutputDir == "" {
			return
		}
		if len(res.Indexes) == 0 && created {
			CleanupOutput(res.OutputDir)
			res.OutputDir = ""
			return
		}
		if pruneErr := pruneOutput(res.OutputDir, res.Indexes); pruneErr != nil {
			log.Warn("could not prune the indexer output directory", "dir", res.OutputDir, "err", pruneErr)
		}
	}()

	if plan.Accuracy == config.AccuracyFast {
		return Results{}, nil
	}
	// Checked before anything is created, installed or run: exact failing must
	// be free of side effects, so that a user who fixes their toolchain and
	// re-runs is not recovering from a half-done state.
	if err := plan.Err(); err != nil {
		return Results{}, err
	}

	res = Results{OutputDir: opts.OutputDir}
	for _, s := range plan.BlockedSteps() {
		res.Skipped = append(res.Skipped, Skip{
			Project: s.Project, Indexer: s.Indexer,
			Reason: s.Reason, MissingTools: s.MissingTools,
		})
	}

	runnable := plan.Runnable()
	if len(runnable) == 0 {
		return res, nil
	}

	if res.OutputDir == "" {
		dir, dirErr := OutputDir()
		if dirErr != nil {
			return res, dirErr
		}
		res.OutputDir = dir
		created = true
	}

	prov := &Provisioner{
		Runner:     opts.Runner,
		Policy:     policyOrDefault(plan),
		Prompt:     opts.Prompt,
		Audit:      opts.Audit,
		Timeout:    plan.Timeout,
		AssumeYes:  opts.AssumeYes,
		RunnerKind: plan.Runner,
	}
	exec := &Executor{
		Runner:              opts.Runner,
		Audit:               opts.Audit,
		Timeout:             plan.Timeout,
		RunnerKind:          plan.Runner,
		MountLocalArtifacts: plan.MountLocalArtifacts,
	}

	for _, s := range runnable {
		if err := ctx.Err(); err != nil {
			return res, errs.Wrap(errs.KindCancelled, "indexer.Execute", err)
		}

		inst := s.Installed
		if inst.Path == "" && inst.Image == "" {
			var err error
			inst, err = prov.Ensure(ctx, s.Pin)
			if err != nil {
				// In exact mode a failed install is fatal; the plan said this
				// step was required and it did not happen.
				if plan.Accuracy == config.AccuracyExact {
					return res, errs.Wrap(errs.KindOf(err), "indexer.Execute",
						fmt.Errorf("accuracy.mode=exact needs %s for %s: %w", s.Pin.Indexer, s.Project, err))
				}
				res.Skipped = append(res.Skipped, Skip{
					Project: s.Project, Indexer: s.Indexer,
					Reason: "could not install " + s.Pin.Ref(), Err: err,
				})
				continue
			}
			if !inst.Preexisting {
				res.Provisioned = append(res.Provisioned, inst)
			}
		}

		log.Info("running indexer",
			"indexer", string(s.Indexer),
			"language", string(s.Project.Language),
			"project_dir", s.Project.Dir)

		out, err := exec.Run(ctx, inst, s.Project, plan.RepoRoot, res.OutputDir)
		if err != nil {
			if plan.Accuracy == config.AccuracyExact {
				return res, errs.Wrap(errs.KindOf(err), "indexer.Execute", err)
			}
			res.Skipped = append(res.Skipped, Skip{
				Project: s.Project, Indexer: s.Indexer,
				Reason: failureReason(err), Err: err,
				Diagnostic: out.Diagnostic,
			})
			continue
		}
		res.Indexes = append(res.Indexes, out)
	}
	return res, nil
}

// failureReason renders why one indexer produced nothing.
//
// This was the constant "indexer failed", and the constant is why a real defect
// stayed invisible: scip-go exited 0 having loaded no packages, the tools print
// their progress only to a TTY so Diagnostic was empty, and the user was left
// with "scip-go skipped: indexer failed" and no thread to pull. The error
// already names the tool, the project and what happened; that sentence is the
// entire value of having caught it, and dropping it in favour of two generic
// words is the same mistake as letting failure look like an empty result.
//
// The op prefix ("indexer.Run: ") is stripped because it names a function in
// this package, which tells a user nothing about their repository.
func failureReason(err error) string {
	if err == nil {
		return "indexer failed"
	}
	var e *errs.Error
	if errors.As(err, &e) && e.Err != nil {
		return e.Err.Error()
	}
	return err.Error()
}

// policyOrDefault reads the provisioning policy off the plan, defaulting to the
// most restrictive value.
//
// The default is `never` rather than `prompt` because a Plan is an ordinary
// struct a caller can build by hand, and a zero value must not be able to
// authorise a download. Fail closed on the field that decides whether
// third-party code gets installed.
func policyOrDefault(plan Plan) config.ProvisionPolicy {
	if plan.ProvisionPolicy.Valid() {
		return plan.ProvisionPolicy
	}
	return config.ProvisionNever
}

// CleanupOutput removes an output directory produced by Execute. Callers should
// defer it once the indexes have been ingested.
func CleanupOutput(dir string) {
	owned, ok := ownedOutputDir(dir)
	if !ok {
		return
	}
	_ = os.RemoveAll(owned)
}

// ownedOutputDir reports whether dir is a directory this package created with
// OutputDir — and returns the path to act on if so.
//
// The name prefix alone was the whole check once, and a name is not ownership:
// a repository can contain a directory called cortex-scip-anything, and a
// symlink under the temp root can carry the prefix while pointing at a home
// directory. Both would have been removed. So the path is resolved through
// every symlink first, and it counts as ours only if what it resolves to
// carries the prefix AND sits directly under the resolved temp root — the two
// facts OutputDir establishes and nothing else can fake. The resolved path is
// what gets acted on, so a link is never followed into somewhere else.
func ownedOutputDir(dir string) (string, bool) {
	if dir == "" || !strings.HasPrefix(filepath.Base(dir), "cortex-scip-") {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return "", false
	}
	if !strings.HasPrefix(filepath.Base(resolved), "cortex-scip-") || filepath.Dir(resolved) != tmp {
		return "", false
	}
	return resolved, true
}

// pruneOutput removes from an output directory everything that is not one of
// the indexes in keep: the build caches a contained run was told to write there
// (Maven's .m2, coursier's JDK archives), a tool's stray log, anything.
//
// Only ever a directory this package created under os.TempDir — the same
// ownership check CleanupOutput applies, see ownedOutputDir — so a
// caller-supplied Options.OutputDir that was not made by OutputDir() is left
// exactly as it was. Errors are collected rather
// than stopping at the first, because a cache that could not be removed is a
// smaller problem than a cache that was skipped because an earlier one failed.
func pruneOutput(dir string, keep []Execution) error {
	owned, ok := ownedOutputDir(dir)
	if !ok {
		return nil
	}
	dir = owned
	kept := make(map[string]bool, len(keep))
	for _, ix := range keep {
		if ix.IndexPath != "" {
			kept[filepath.Base(ix.IndexPath)] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read output directory %s: %w", dir, err)
	}
	var failures []error
	for _, e := range entries {
		if kept[e.Name()] {
			continue
		}
		if rmErr := os.RemoveAll(filepath.Join(dir, e.Name())); rmErr != nil {
			failures = append(failures, fmt.Errorf("remove %s: %w", e.Name(), rmErr))
		}
	}
	return errors.Join(failures...)
}
