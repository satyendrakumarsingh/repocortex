// Package indexer orchestrates compiler-backed SCIP indexers: it decides which
// ones apply to a repository, whether the machine can run them, installs them
// when the user has authorised that, runs them, and hands the resulting .scip
// files to internal/resolve/scip.
//
// It is the only package in RepoCortex that downloads third-party software and
// executes build systems, so it is the package where the threat model bites
// hardest.
//
// # Why this package is dangerous, stated plainly
//
// `repocortex index` is expected to be run immediately after `git clone` on code
// nobody has read yet. Everything in the working tree is therefore attacker-
// controlled. Two of the four indexers here execute the repository's own build
// system: scip-java runs Maven or Gradle, and a Maven plugin or a Gradle build
// script is arbitrary code by design — that is what a build system IS. So the
// question this package must answer at every decision point is not "does this
// work?" but "can the repository influence this?"
//
// The controls, and the reason each exists:
//
//  1. NOTHING is read from the repository except the NAMES of marker files.
//     Detection stats pom.xml, go.mod, package.json and friends; it never opens
//     them. A `scripts` entry in package.json, a Makefile target, a
//     .repocortex.yml key — none of them reach this package, because the only
//     configuration input is config.Config, whose execution fields can only
//     come from ~/.config/repocortex/config.yml. See the package comment in
//     internal/config: that trust split is the security model, and this package
//     is its main consumer. TestRepoCannotInfluenceThePlan proves it by making
//     every marker file unreadable and checking that planning is unaffected.
//
//  2. Provisioning is never silent and never unpinned. Versions and digests are
//     compile-time constants (see Pins). ProvisionPrompt is the default; under
//     it a non-interactive caller must pass Options.AssumeYes explicitly, which
//     is the CLI's --yes flag and nothing else. A missing or mismatched digest
//     refuses the install rather than proceeding.
//
//  3. There is no shell. Every subprocess is an explicit argv passed to
//     exec.CommandContext through Runner, with an absolute executable path — a
//     relative path would be resolved against Cmd.Dir, which is the hostile
//     repository. Command.validate enforces that.
//
//  4. Every subprocess is timeboxed and its output is bounded. A build that
//     hangs must not hang RepoCortex, and a build that prints forever must not
//     exhaust memory. Both limits are enforced in ExecRunner, not left to the
//     caller to remember.
//
//  5. The only path this package writes to is an output directory it creates
//     itself under os.TempDir. It does not follow that the repository is
//     read-only — a Maven build writes target/, and refusing to let it would
//     mean refusing exact Java entirely — but nothing RepoCortex itself writes
//     lands in the tree.
//
//  6. Repository-derived strings never become command arguments. The only
//     repository-derived value that must cross into a subprocess is a
//     sub-project directory, and it crosses as Command.Dir, not as an argument.
//     Where the docker runner has no choice but to put it in an argv (-w), it
//     goes through sanitizeRel first. Values that look tempting to forward —
//     project.name from .repocortex.yml is the obvious one — are replaced by
//     fixed literals; see scip-python's --project-name.
//
// # Why this is what makes `exact` real
//
// Every edge RepoCortex has recorded until now came from a syntax tree, so the
// honest ceiling is ConfidenceHigh. model.Edge.Validate and a SQL CHECK both
// refuse ConfidenceExact unless Evidence.Source is SourceCompiler, and the only
// legitimate producer of that is internal/resolve/scip ingesting a real .scip
// index. Nothing was producing .scip indexes. This package does, which is why
// AccuracyExact stops being a label recorded for provenance and starts being a
// promise: if a detected language cannot be indexed by a compiler here, Execute
// fails and names the language and the missing tool. It never degrades quietly,
// because a silent downgrade from "proven" to "guessed" is precisely the lie
// the confidence system exists to prevent.
//
// # Shape of the API
//
// Plan is pure inspection and backs `repocortex indexers list`: it detects
// projects, probes the toolchain, and reports what would run, what is missing
// and what would have to be installed. It creates no files and installs
// nothing. Execute takes a Plan and carries it out.
//
// Runner exists so that none of this needs a network, a JVM or Docker to test.
// Everything that reaches the operating system goes through it, so a fake
// Runner turns the whole package into a pure function of its inputs.
package indexer
