package indexer

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Tool is an executable RepoCortex may need on PATH.
//
// The set is closed and every member is a compile-time constant. Nothing
// derived from a repository can ever name a Tool, which is what stops a
// repository from choosing what gets executed.
type Tool string

const (
	ToolGo     Tool = "go"
	ToolNode   Tool = "node"
	ToolNPM    Tool = "npm"
	ToolJava   Tool = "java"
	ToolMaven  Tool = "mvn"
	ToolGradle Tool = "gradle"
	ToolPython Tool = "python3"
	ToolDocker Tool = "docker"
)

// probeTimeout bounds a version probe. A version flag that has not answered in
// ten seconds means something is wrong with the tool, not that it is slow.
const probeTimeout = 10 * time.Second

// probe describes how to ask a tool what it is.
type probe struct {
	// args produce a version banner.
	args []string

	// cheap says whether asking is fast enough to do on every plan.
	//
	// `mvn -v` and `gradle --version` each start a JVM, and Gradle's takes
	// seconds. `repocortex indexers list` is meant to be instant, so for those two
	// the probe stops at presence on PATH and the version is left unknown rather
	// than paying seconds for a cosmetic string.
	cheap bool

	// requiresSuccess marks tools whose probe proves more than presence on
	// PATH, so that a failing probe means the tool is unusable. See ToolDocker.
	requiresSuccess bool
}

var probes = map[Tool]probe{
	ToolGo:     {args: []string{"version"}, cheap: true},
	ToolNode:   {args: []string{"--version"}, cheap: true},
	ToolNPM:    {args: []string{"--version"}, cheap: true},
	ToolJava:   {args: []string{"-version"}, cheap: true},
	ToolPython: {args: []string{"--version"}, cheap: true},
	ToolMaven:  {args: []string{"-v"}, cheap: false},
	ToolGradle: {args: []string{"--version"}, cheap: false},

	// Docker is probed against the DAEMON, not the CLI, and the probe failing
	// means the tool is not usable.
	//
	// `docker --version` succeeds when Docker Desktop is installed but not
	// running, which is the single most common way `runner: docker` fails. A
	// plan that says "docker: present" and then dies at run time is worse than
	// no plan, so presence here means "a daemon answered".
	ToolDocker: {args: []string{"version", "--format", "{{.Server.Version}}"}, cheap: true, requiresSuccess: true},
}

// ToolStatus is what probing learned about one tool.
type ToolStatus struct {
	Tool Tool

	// Present means the tool can be used. For most tools that is "found on
	// PATH"; for Docker it additionally means a daemon answered.
	Present bool

	// Path is the absolute path it resolved to.
	Path string

	// Version is the first line of its version banner, bounded. Empty when the
	// probe was skipped as too expensive (see probe.cheap) or failed.
	Version string

	// ProbeErr explains a version probe that did not work. It is diagnostic
	// only: a tool whose banner could not be parsed is still present.
	ProbeErr string
}

// Toolchain is the result of probing.
type Toolchain struct {
	Tools map[Tool]ToolStatus
}

// Has reports whether t is usable.
func (tc Toolchain) Has(t Tool) bool { return tc.Tools[t].Present }

// HasAny reports whether at least one of tools is usable. Java projects need
// this: Maven or Gradle will do, depending on which the repository uses.
func (tc Toolchain) HasAny(tools ...Tool) bool {
	for _, t := range tools {
		if tc.Has(t) {
			return true
		}
	}
	return false
}

// Missing returns the subset of tools that are not usable, in the order given.
func (tc Toolchain) Missing(tools ...Tool) []Tool {
	var out []Tool
	for _, t := range tools {
		if !tc.Has(t) {
			out = append(out, t)
		}
	}
	return out
}

// Summary renders the probe result for `repocortex indexers list`.
func (tc Toolchain) Summary() string {
	names := make([]string, 0, len(tc.Tools))
	for t := range tc.Tools {
		names = append(names, string(t))
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		st := tc.Tools[Tool(n)]
		switch {
		case !st.Present:
			parts = append(parts, n+": missing")
		case st.Version != "":
			parts = append(parts, n+": "+st.Version)
		default:
			parts = append(parts, n+": present")
		}
	}
	return strings.Join(parts, ", ")
}

// Probe reports which of tools this machine can use.
//
// A missing tool is not an error. That is the whole contract: probing exists to
// answer "is exact achievable here", and the honest answer for a machine with no
// JVM is "no, because java and mvn are absent" — delivered as data, so that
// `auto` can skip Java and `exact` can fail with a message naming exactly what
// to install. Probe therefore returns no error at all.
//
// Probing runs subprocesses, so it goes through Runner and is fake-able. It has
// no side effects: every command here is a version flag.
func Probe(ctx context.Context, r Runner, audit AuditSink, tools ...Tool) Toolchain {
	tc := Toolchain{Tools: make(map[Tool]ToolStatus, len(tools))}
	for _, t := range tools {
		if _, done := tc.Tools[t]; done {
			continue
		}
		tc.Tools[t] = probeOne(ctx, r, audit, t)
	}
	return tc
}

func probeOne(ctx context.Context, r Runner, audit AuditSink, t Tool) ToolStatus {
	st := ToolStatus{Tool: t}
	pathTo, err := r.LookPath(string(t))
	if err != nil {
		st.ProbeErr = "not found on PATH"
		return st
	}
	st.Path = pathTo
	st.Present = true

	p, known := probes[t]
	if !known || !p.cheap {
		return st
	}

	cmd := Command{Path: pathTo, Args: p.args, Timeout: probeTimeout, Env: nil}
	res, runErr := r.Run(ctx, cmd)
	recordErr(audit, ActionProbe, AuditRecord{Tool: t, Result: res}, runErr)
	if runErr != nil {
		st.ProbeErr = runErr.Error()
		if p.requiresSuccess {
			st.Present = false
		}
		return st
	}
	if res.ExitCode != 0 {
		st.ProbeErr = "version probe exited " + itoa(res.ExitCode)
		if p.requiresSuccess {
			// Docker: the CLI exists, the daemon does not answer. Reporting it
			// as present would produce a plan that cannot run.
			st.Present = false
		}
		return st
	}
	// java writes its banner to stderr; go and node to stdout. Take whichever
	// spoke rather than encoding the difference per tool.
	st.Version = firstLine(string(res.Stdout))
	if st.Version == "" {
		st.Version = firstLine(string(res.Stderr))
	}
	return st
}

// maxVersionChars bounds a version banner. It is untrusted only in the weak
// sense — the tool is one we chose — but an unbounded string from a subprocess
// has no business being copied into a plan that gets printed.
const maxVersionChars = 120

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > maxVersionChars {
		s = s[:maxVersionChars] + "…"
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// RequiredTools lists what a project needs before its indexer can run.
//
// Two notes on the trust boundary, because this function looks like the place
// where a repository gets a say:
//
//   - Which JVM build tool is required IS decided by a marker file name:
//     pom.xml means Maven, build.gradle means Gradle. That is a choice between
//     two hardcoded constants, and there is no third option a repository could
//     name. It is the narrowest possible influence and it is unavoidable — you
//     cannot index a Gradle project with Maven.
//   - Under the docker runner the answer is always just Docker, because
//     everything the build needs lives in the pinned image. That is the reason
//     to prefer it: the host is never mutated and the repository's build system
//     runs inside a container instead of on the developer's machine.
func RequiredTools(p Project, runner string) []Tool {
	if runner == RunnerDocker && p.Language == LangJava {
		return []Tool{ToolDocker}
	}
	switch p.Language {
	case LangGo:
		return []Tool{ToolGo}
	case LangTypeScript:
		// scip-typescript is a Node program and drives the TypeScript compiler
		// from the repository's own node_modules when present.
		return []Tool{ToolNode}
	case LangPython:
		// scip-python is likewise a Node program — it embeds Pyright's type
		// checker — so Node is the hard requirement and python3 is not needed to
		// analyse the source.
		return []Tool{ToolNode}
	case LangJava:
		if p.hasMarker("pom.xml") {
			return []Tool{ToolJava, ToolMaven}
		}
		return []Tool{ToolJava, ToolGradle}
	}
	return nil
}

// toolsFor collects every tool a set of projects could need, so that Probe runs
// once per tool rather than once per project.
func toolsFor(projects []Project, runner string) []Tool {
	seen := map[Tool]bool{}
	var out []Tool
	add := func(t Tool) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, p := range projects {
		for _, t := range RequiredTools(p, runner) {
			add(t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
