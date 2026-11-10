package indexer

import (
	"strings"
	"testing"
)

// TestDockerJavaCommandGivesTheContainerUserAWritableHome pins a defect that
// made the SAFE path the one that did not work.
//
// The container runs as the invoking uid so the .scip file is not left
// root-owned. A numeric uid has no passwd entry inside the image, so HOME
// resolves to "/", and every tool that wants a cache directory fails on it.
// scip-java dies before the build even starts:
//
//	java.nio.file.AccessDeniedException: /.cache
//	  at coursier.bootstrap.launcher...
//
// RepoCortex surfaced that as "scip-java skipped: indexer failed" and nothing
// else, so the recommended container route for Java — the one the planner
// tells users to take instead of running an untrusted build on their host —
// could not index anything at all.
//
// HOME points at the output mount because that is the one directory this uid
// is guaranteed to be able to write: the index itself is written there.
func TestDockerJavaCommandGivesTheContainerUserAWritableHome(t *testing.T) {
	e := &Executor{Runner: &fakeRunner{present: map[string]bool{"docker": true}}}

	cmd, err := e.dockerJavaCommand(
		Installed{Indexer: SCIPJava, Image: "img@sha256:abc"},
		Project{Language: LangJava, Dir: ".", Markers: []string{"pom.xml"}},
		"", "/repo", "/tmp/out", "/tmp/out/java.scip")
	if err != nil {
		t.Fatalf("dockerJavaCommand: %v", err)
	}

	joined := strings.Join(cmd.Args, " ")

	// Only assert HOME when the command actually drops privileges; on Windows
	// the uid flag is omitted and so is this.
	if !strings.Contains(joined, "-u ") {
		t.Skip("no uid on this platform, so no HOME to set")
	}

	var home string
	for i, a := range cmd.Args {
		if a == "-e" && i+1 < len(cmd.Args) && strings.HasPrefix(cmd.Args[i+1], "HOME=") {
			home = strings.TrimPrefix(cmd.Args[i+1], "HOME=")
		}
	}
	if home == "" {
		t.Fatalf("no HOME set for the container user; scip-java will fail on /.cache\nargs: %s", joined)
	}
	if home == "/" {
		t.Errorf("HOME=/ is the broken default this exists to replace")
	}

	// It has to be a path this uid can write, which means one of the mounts.
	if !strings.Contains(joined, "-v /tmp/out:"+home) {
		t.Errorf("HOME=%s is not a writable mount; args: %s", home, joined)
	}
}

// TestDockerJavaCommandKeepsItsContainmentGuarantees pins the properties the
// HOME change must not have loosened, since it edits the same argv.
func TestDockerJavaCommandKeepsItsContainmentGuarantees(t *testing.T) {
	e := &Executor{Runner: &fakeRunner{present: map[string]bool{"docker": true}}}
	cmd, err := e.dockerJavaCommand(
		Installed{Indexer: SCIPJava, Image: "img@sha256:abc"},
		Project{Language: LangJava, Dir: ".", Markers: []string{"pom.xml"}},
		"", "/repo", "/tmp/out", "/tmp/out/java.scip")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")

	for _, forbidden := range []string{"--privileged", "--network host", "/var/run/docker.sock"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("docker command contains %q: %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "--rm") {
		t.Errorf("container is not removed after the run: %s", joined)
	}
	if !strings.Contains(joined, "img@sha256:abc") {
		t.Errorf("image is not referenced by digest: %s", joined)
	}
}

// TestLocalArtifactCacheIsMountedOnlyWhenAskedFor pins both halves of an
// opt-in that trades away part of the container's whole purpose.
//
// Without it, a private dependency cannot resolve inside the container and
// compiler-backed indexing is impossible for exactly the repositories most
// worth indexing — the ones with internal libraries. With it, a hostile build
// can poison a cached artifact that every other project on the host then
// compiles against.
//
// So it is off unless asked for, and asking is possible only in user config.
func TestLocalArtifactCacheIsMountedOnlyWhenAskedFor(t *testing.T) {
	build := func(mount bool) string {
		e := &Executor{
			Runner:              &fakeRunner{present: map[string]bool{"docker": true}},
			MountLocalArtifacts: mount,
		}
		cmd, err := e.dockerJavaCommand(
			Installed{Indexer: SCIPJava, Image: "img@sha256:abc"},
			Project{Language: LangJava, Dir: ".", Markers: []string{"pom.xml"}},
			"", "/repo", "/tmp/out", "/tmp/out/java.scip")
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(cmd.Args, " ")
	}

	if off := build(false); strings.Contains(off, ".m2") {
		t.Errorf("the artifact cache was mounted without being asked for: %s", off)
	}
	on := build(true)
	if !strings.Contains(on, ".m2") {
		t.Skip("no ~/.m2 on this machine, so there is nothing to mount")
	}
	// It must land where the container's HOME is, or Maven will not look there.
	if !strings.Contains(on, ":/out/.m2") {
		t.Errorf("the cache is not mounted at the container HOME: %s", on)
	}
}

// TestDockerJavaCommandGivesTheJVMAHomeThatResolves pins the second half of
// the HOME defect, found on the first repository Java was measured on.
//
// HOME=/out is honoured by Maven and by everything that reads $HOME. It is
// NOT what the JVM's `user.home` property reads: that comes from the passwd
// entry for the running uid, and a numeric uid the image has never heard of
// resolves to "?". scip-java's wrapper fetches its own JDK through
// `coursier java`, coursier is a JVM program, and its cache root is
// `${user.home}/.cache/coursier` — so 611 MB of Temurin landed in a directory
// literally named `?` at the root of the indexed repository, untracked, not
// gitignored, and outside every mount the runner meant to contain it in.
//
// The fix is an environment variable every JDK 9+ launcher reads
// (JDK_JAVA_OPTIONS) rather than JAVA_TOOL_OPTIONS, because the image already
// sets JAVA_TOOL_OPTIONS for container memory limits and this runner must not
// have to know what those are to keep them.
func TestDockerJavaCommandGivesTheJVMAHomeThatResolves(t *testing.T) {
	e := &Executor{Runner: &fakeRunner{present: map[string]bool{"docker": true}}}
	cmd, err := e.dockerJavaCommand(
		Installed{Indexer: SCIPJava, Image: "img@sha256:abc"},
		Project{Language: LangJava, Dir: ".", Markers: []string{"pom.xml"}},
		"", "/repo", "/tmp/out", "/tmp/out/java.scip")
	if err != nil {
		t.Fatalf("dockerJavaCommand: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "-u ") {
		t.Skip("no uid on this platform, so no JVM home to set")
	}

	var home, jvmOpts string
	for i, a := range cmd.Args {
		if a != "-e" || i+1 >= len(cmd.Args) {
			continue
		}
		switch {
		case strings.HasPrefix(cmd.Args[i+1], "HOME="):
			home = strings.TrimPrefix(cmd.Args[i+1], "HOME=")
		case strings.HasPrefix(cmd.Args[i+1], "JDK_JAVA_OPTIONS="):
			jvmOpts = strings.TrimPrefix(cmd.Args[i+1], "JDK_JAVA_OPTIONS=")
		case strings.HasPrefix(cmd.Args[i+1], "JAVA_TOOL_OPTIONS="):
			t.Errorf("JAVA_TOOL_OPTIONS is set by the image for container memory limits; overriding it here silently drops them: %s", cmd.Args[i+1])
		}
	}
	if jvmOpts == "" {
		t.Fatalf("no JDK_JAVA_OPTIONS for the container user; the JVM's user.home resolves to \"?\" and coursier writes its JDK cache into the repository\nargs: %s", joined)
	}
	want := "-Duser.home=" + home
	if !strings.Contains(jvmOpts, want) {
		t.Errorf("JDK_JAVA_OPTIONS=%q does not carry %q, so the JVM's home and the shell's HOME disagree", jvmOpts, want)
	}
}
