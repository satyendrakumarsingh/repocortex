package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
)

// TestRegistryOf pins how an image reference maps to the credential key the
// Docker CLI looks up, since a wrong key means asking the wrong helper.
func TestRegistryOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"sourcegraph/scip-java":            dockerHubRegistryKey,
		"scip-java":                        dockerHubRegistryKey,
		"docker.io/sourcegraph/scip-java":  dockerHubRegistryKey,
		"index.docker.io/sourcegraph/x":    dockerHubRegistryKey,
		"ghcr.io/sourcegraph/scip-java":    "ghcr.io",
		"localhost:5000/team/image":        "localhost:5000",
		"registry.example.com:8443/team/x": "registry.example.com:8443",
		"localhost/team/image":             "localhost",
	}
	for image, want := range cases {
		if got := registryOf(image); got != want {
			t.Errorf("registryOf(%q) = %q, want %q", image, got, want)
		}
	}
}

// TestDockerCredentialHelperResolvesLikeTheCLI: a per-registry credHelpers
// entry wins over credsStore; no configuration means no helper; a missing
// file is not an error.
func TestDockerCredentialHelperResolvesLikeTheCLI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name, config, image, want string
	}{
		{"store only", `{"credsStore":"desktop"}`, "sourcegraph/scip-java", "docker-credential-desktop"},
		{"per-registry wins", `{"credsStore":"desktop","credHelpers":{"ghcr.io":"gh"}}`, "ghcr.io/x/y", "docker-credential-gh"},
		{"per-registry for hub", `{"credHelpers":{"https://index.docker.io/v1/":"osxkeychain"}}`, "sourcegraph/scip-java", "docker-credential-osxkeychain"},
		{"other registry falls back to store", `{"credsStore":"desktop","credHelpers":{"ghcr.io":"gh"}}`, "sourcegraph/scip-java", "docker-credential-desktop"},
		{"nothing configured", `{"auths":{}}`, "sourcegraph/scip-java", ""},
	}
	for _, c := range cases {
		got, err := dockerCredentialHelper(write(c.name+".json", c.config), c.image)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: helper = %q, want %q", c.name, got, c.want)
		}
	}
	if got, err := dockerCredentialHelper(filepath.Join(dir, "absent.json"), "x/y"); err != nil || got != "" {
		t.Errorf("missing config: helper=%q err=%v, want none", got, err)
	}
	if _, err := dockerCredentialHelper(write("bad.json", `{`), "x/y"); err == nil {
		t.Error("a malformed config should be reported, not silently treated as empty")
	}
}

// dockerPinFor returns a container-channel pin with a digest, so Ensure takes
// the pull path.
func dockerPinFor(t *testing.T) Pin {
	t.Helper()
	pin, ok := PinFor(LangJava)
	if !ok || pin.Channel != ChannelDocker || pin.ImageDigest == "" {
		t.Fatalf("the Java pin is expected to be a digest-pinned image: %+v", pin)
	}
	return pin
}

// TestDockerPullRefusesWhenTheCredentialHelperHangs is the measurement that
// motivated the preflight, replayed.
//
// `docker pull` asks the configured credential helper before it talks to the
// daemon. Docker Desktop's helper opens a connection to the Desktop app and,
// from a shell with no Desktop session, never returns — so the pull blocked
// for RepoCortex's whole provisioning timeout and the user was told "could not
// install <image>", which was true and named nothing. The helper is asked
// first, with a bound a working one cannot miss, and a hang is reported as
// what it is, with the workaround.
func TestDockerPullRefusesWhenTheCredentialHelperHangs(t *testing.T) {
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"credsStore":"desktop"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", cfg)

	r := newFakeRunner("docker", "docker-credential-desktop")
	r.handle("docker", func(c Command) (Result, error) {
		if len(c.Args) > 0 && c.Args[0] == "image" {
			return Result{ExitCode: 1}, nil // not present yet
		}
		return Result{}, nil
	})
	r.handle("docker-credential-desktop", func(Command) (Result, error) {
		return Result{TimedOut: true, ExitCode: -1}, errors.New("indexer: docker-credential-desktop timed out after 5s: " + context.DeadlineExceeded.Error())
	})
	p := &Provisioner{Runner: r, Policy: config.ProvisionAuto, RunnerKind: RunnerDocker}

	_, err := p.Ensure(context.Background(), dockerPinFor(t))
	if err == nil {
		t.Fatal("Ensure succeeded while the credential helper was hung")
	}
	msg := err.Error()
	for _, want := range []string{"docker-credential-desktop", "DOCKER_CONFIG", "Docker Desktop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q: %s", want, msg)
		}
	}
	for _, argv := range r.argvs() {
		if strings.Contains(argv, " pull ") {
			t.Errorf("docker pull was attempted despite the hung helper: %s", argv)
		}
	}
}

// TestDockerPullProceedsWhenTheHelperAnswers: a helper that answers — even
// with a failure — is not the preflight's business; docker reports those.
func TestDockerPullProceedsWhenTheHelperAnswers(t *testing.T) {
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"credsStore":"desktop"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", cfg)

	for _, tc := range []struct {
		name string
		res  Result
	}{
		{"answers", Result{Stdout: []byte("{}\n")}},
		{"fails fast", Result{ExitCode: 1, Stderr: []byte("credentials not found")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner("docker", "docker-credential-desktop")
			r.handle("docker", func(c Command) (Result, error) {
				if len(c.Args) > 0 && c.Args[0] == "image" {
					return Result{ExitCode: 1}, nil
				}
				return Result{}, nil
			})
			r.handle("docker-credential-desktop", func(Command) (Result, error) { return tc.res, nil })
			p := &Provisioner{Runner: r, Policy: config.ProvisionAuto, RunnerKind: RunnerDocker}

			if _, err := p.Ensure(context.Background(), dockerPinFor(t)); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			var pulled bool
			for _, argv := range r.argvs() {
				pulled = pulled || strings.Contains(argv, " pull ")
			}
			if !pulled {
				t.Errorf("docker pull did not run: %v", r.argvs())
			}
		})
	}
}

// TestDockerPullSkipsThePreflightWithoutAHelper: no helper configured, or a
// configured helper that is not on PATH, is docker's to report.
func TestDockerPullSkipsThePreflightWithoutAHelper(t *testing.T) {
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"credsStore":"desktop"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", cfg)

	r := newFakeRunner("docker") // the helper is configured but absent
	r.handle("docker", func(c Command) (Result, error) {
		if len(c.Args) > 0 && c.Args[0] == "image" {
			return Result{ExitCode: 1}, nil
		}
		return Result{}, nil
	})
	p := &Provisioner{Runner: r, Policy: config.ProvisionAuto, RunnerKind: RunnerDocker}
	if _, err := p.Ensure(context.Background(), dockerPinFor(t)); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, argv := range r.argvs() {
		if strings.Contains(argv, "docker-credential") {
			t.Errorf("the preflight ran a helper that is not on PATH: %s", argv)
		}
	}
}
