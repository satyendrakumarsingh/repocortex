package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestProbeToleratesAMissingTool is the contract that makes honest degradation
// possible: probing answers "what can this machine do", and "nothing" is a
// legitimate answer rather than a failure. Probe therefore returns no error at
// all, which is why it has no error result to forget to check.
func TestProbeToleratesAMissingTool(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("go") // node, mvn, java, docker all absent
	audit := &MemoryAudit{}

	tc := Probe(context.Background(), r, audit, ToolGo, ToolNode, ToolMaven, ToolJava, ToolDocker)

	if !tc.Has(ToolGo) {
		t.Error("go should be present")
	}
	for _, absent := range []Tool{ToolNode, ToolMaven, ToolJava, ToolDocker} {
		if tc.Has(absent) {
			t.Errorf("%s should be absent", absent)
		}
		if got := tc.Tools[absent].ProbeErr; got == "" {
			t.Errorf("%s: expected an explanation of its absence", absent)
		}
	}
	want := []Tool{ToolNode, ToolMaven}
	if got := tc.Missing(ToolGo, ToolNode, ToolMaven); !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %v, want %v", got, want)
	}
	// A tool that is not there is not executed.
	for _, c := range r.calls() {
		if c.Path != fakeBin("go") {
			t.Errorf("probed a tool that is not installed: %v", c.Argv())
		}
	}
}

func TestProbeRecordsVersions(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("go", "mvn")
	r.handle("go", func(Command) (Result, error) {
		return Result{Stdout: []byte("go version go1.25.0 darwin/arm64\nnoise\n")}, nil
	})

	tc := Probe(context.Background(), r, nil, ToolGo, ToolMaven)

	if got := tc.Tools[ToolGo].Version; got != "go version go1.25.0 darwin/arm64" {
		t.Errorf("go version = %q", got)
	}
	// mvn starts a JVM, so presence is established without paying for the
	// banner. Present but versionless is the documented outcome.
	mvn := tc.Tools[ToolMaven]
	if !mvn.Present || mvn.Version != "" {
		t.Errorf("mvn = %+v, want present with no version", mvn)
	}
	for _, c := range r.calls() {
		if c.Path == fakeBin("mvn") {
			t.Error("mvn was executed during a plan-time probe")
		}
	}
}

// TestProbeVersionOnStderr covers java, which writes its banner to stderr.
func TestProbeVersionOnStderr(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("java")
	r.handle("java", func(Command) (Result, error) {
		return Result{Stderr: []byte("openjdk version \"21.0.2\" 2024-01-16\n")}, nil
	})
	tc := Probe(context.Background(), r, nil, ToolJava)
	if got := tc.Tools[ToolJava].Version; got != `openjdk version "21.0.2" 2024-01-16` {
		t.Errorf("java version = %q", got)
	}
}

// TestProbeDockerRequiresADaemon: `docker --version` succeeds with Docker
// Desktop installed but stopped, which is the most common way runner=docker
// fails. Reporting it present would produce a plan that dies at run time.
func TestProbeDockerRequiresADaemon(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("docker")
	r.handle("docker", func(Command) (Result, error) {
		return Result{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon\n")}, nil
	})
	tc := Probe(context.Background(), r, nil, ToolDocker)
	if tc.Has(ToolDocker) {
		t.Fatal("docker reported present with no daemon answering")
	}

	r = newFakeRunner("docker")
	r.handle("docker", func(Command) (Result, error) {
		return Result{Stdout: []byte("27.1.1\n")}, nil
	})
	if tc = Probe(context.Background(), r, nil, ToolDocker); !tc.Has(ToolDocker) {
		t.Fatal("docker should be present when the daemon answers")
	}
}

// TestProbeSurvivesARunnerError: a probe that cannot run must degrade to
// "unknown version", not take the plan down.
func TestProbeSurvivesARunnerError(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("node")
	r.handle("node", func(Command) (Result, error) {
		return Result{}, errors.New("exec format error")
	})
	tc := Probe(context.Background(), r, nil, ToolNode)
	if !tc.Has(ToolNode) {
		t.Error("a failed version probe should not unmake a tool that is on PATH")
	}
	if tc.Tools[ToolNode].ProbeErr == "" {
		t.Error("the probe failure should be recorded")
	}
}

func TestRequiredToolsPerProject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		project Project
		runner  string
		want    []Tool
	}{
		{Project{Language: LangGo}, RunnerNative, []Tool{ToolGo}},
		{Project{Language: LangTypeScript}, RunnerNative, []Tool{ToolNode}},
		{Project{Language: LangPython}, RunnerNative, []Tool{ToolNode}},
		{Project{Language: LangJava, Markers: []string{"pom.xml"}}, RunnerNative, []Tool{ToolJava, ToolMaven}},
		{Project{Language: LangJava, Markers: []string{"build.gradle"}}, RunnerNative, []Tool{ToolJava, ToolGradle}},
		// Under docker the host needs nothing but docker: that is the reason to
		// prefer it.
		{Project{Language: LangJava, Markers: []string{"pom.xml"}}, RunnerDocker, []Tool{ToolDocker}},
	}
	for _, tc := range cases {
		if got := RequiredTools(tc.project, tc.runner); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("RequiredTools(%v, %s) = %v, want %v", tc.project, tc.runner, got, tc.want)
		}
	}
}

func TestProbeAuditsOnlyReadOnlyActions(t *testing.T) {
	t.Parallel()
	audit := &MemoryAudit{}
	Probe(context.Background(), newFakeRunner("go", "node"), audit, ToolGo, ToolNode)
	recs := audit.Records()
	if len(recs) == 0 {
		t.Fatal("probing recorded nothing")
	}
	for _, r := range recs {
		if r.Action != ActionProbe {
			t.Errorf("probe recorded a %q action", r.Action)
		}
		// Audit records must never carry captured output: it can contain source.
		if r.Result.Stdout != nil || r.Result.Stderr != nil {
			t.Error("audit record retained subprocess output")
		}
	}
}

func TestFirstLineIsBounded(t *testing.T) {
	t.Parallel()
	long := make([]byte, 4096)
	for i := range long {
		long[i] = 'a'
	}
	if got := firstLine(string(long)); len(got) > maxVersionChars+4 {
		t.Fatalf("firstLine returned %d chars, want it bounded", len(got))
	}
	if got := firstLine("one\ntwo"); got != "one" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("  \r\n"); got != "" {
		t.Fatalf("firstLine = %q, want empty", got)
	}
}

func TestMemoryAuditIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	audit := &MemoryAudit{}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				audit.Record(AuditRecord{Action: ActionProbe})
				_ = audit.Records()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := len(audit.Records()); got != 400 {
		t.Fatalf("recorded %d, want 400", got)
	}
}

func TestLookPathReturnsAbsolutePaths(t *testing.T) {
	t.Parallel()
	// Look up the one executable guaranteed to exist wherever these tests are
	// compiled and run.
	p, err := ExecRunner{}.LookPath("go")
	if err != nil {
		t.Skipf("go not on PATH: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("LookPath returned a relative path %q, which exec would resolve against the repository", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("LookPath returned an unusable path: %v", err)
	}
}
