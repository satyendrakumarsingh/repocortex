package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// npmPin builds a test pin whose tarball digest is whatever the caller says.
// The shipped pins deliberately have no recorded digests (see the comment on
// `pins`), so mismatch behaviour is exercised through a pin the test controls.
func npmPin(sha string) Pin {
	return Pin{
		Indexer:       SCIPTypeScript,
		Language:      LangTypeScript,
		Version:       "0.3.14",
		Binary:        "scip-typescript",
		VersionArgs:   []string{"--version"},
		Channel:       ChannelNPM,
		NPMPackage:    "@sourcegraph/scip-typescript",
		TarballSHA256: sha,
	}
}

// fakeNPM makes the fake `npm pack` write a tarball with the given contents.
func fakeNPM(r *fakeRunner, contents string) {
	r.handle("npm", func(c Command) (Result, error) {
		if len(c.Args) > 0 && c.Args[0] == "pack" {
			dest := flagValue(c.Args, "--pack-destination")
			if dest == "" {
				dest = c.Dir
			}
			name := filepath.Join(dest, "sourcegraph-scip-typescript-0.3.14.tgz")
			if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
				return Result{ExitCode: 1}, err
			}
		}
		return Result{}, nil
	})
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestProvisionRefusesOnChecksumMismatch is the supply-chain control. The
// registry served *something*; it is not what was pinned; nothing gets
// installed and nothing from the package has been executed, because npm pack
// runs with --ignore-scripts and the tarball is only ever hashed.
func TestProvisionRefusesOnChecksumMismatch(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("npm")
	fakeNPM(r, "malicious payload")

	p := &Provisioner{Runner: r, Policy: config.ProvisionAuto, Audit: &MemoryAudit{}}
	_, err := p.Ensure(context.Background(), npmPin(sha256Of("the expected package")))

	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
	// Never retryable: fetching the same wrong bytes again is the wrong instinct.
	if errs.IsRetryable(err) {
		t.Error("a checksum mismatch must not be reported as retryable")
	}
	if k := errs.KindOf(err); k != errs.KindCorrupt {
		t.Errorf("KindOf = %q, want %q", k, errs.KindCorrupt)
	}
	for _, argv := range r.argvs() {
		if strings.Contains(argv, "install") {
			t.Fatalf("install ran despite a digest mismatch: %s", argv)
		}
	}
}

func TestProvisionInstallsWhenTheDigestMatches(t *testing.T) {
	t.Parallel()
	const payload = "the expected package"
	r := newFakeRunner("npm")
	fakeNPM(r, payload)

	p := &Provisioner{Runner: r, Policy: config.ProvisionAuto, Audit: &MemoryAudit{}}
	pin := npmPin(sha256Of(payload))

	// The binary only appears on PATH once the install command has run.
	r.handle("npm", withInstallSideEffect(r, payload, pin))

	inst, err := p.Ensure(context.Background(), pin)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if inst.Preexisting {
		t.Error("a freshly installed indexer must not be reported as preexisting")
	}
	if inst.Version != pin.Version {
		t.Errorf("version = %q, want %q", inst.Version, pin.Version)
	}
	// Lifecycle scripts run as the invoking user at install time, which would
	// defeat verifying a payload we then never trusted enough to execute.
	sawPack, sawInstall := false, false
	for _, c := range r.calls() {
		if !contains(c.Args, "--ignore-scripts") {
			t.Errorf("npm invoked without --ignore-scripts: %v", c.Argv())
		}
		if contains(c.Args, "pack") {
			sawPack = true
		}
		if contains(c.Args, "install") {
			sawInstall = true
			// The install must be of the verified local tarball, not of a name
			// the registry gets to resolve a second time.
			if contains(c.Args, pin.NPMPackage+"@"+pin.Version) {
				t.Error("installed by name rather than from the verified tarball")
			}
		}
	}
	if !sawPack || !sawInstall {
		t.Fatalf("expected pack then install; pack=%v install=%v", sawPack, sawInstall)
	}
}

// withInstallSideEffect wraps the npm fake so a successful install puts the
// binary on PATH, the way a real one would.
func withInstallSideEffect(r *fakeRunner, payload string, pin Pin) func(Command) (Result, error) {
	return func(c Command) (Result, error) {
		switch {
		case contains(c.Args, "pack"):
			dest := flagValue(c.Args, "--pack-destination")
			name := filepath.Join(dest, "pkg.tgz")
			if err := os.WriteFile(name, []byte(payload), 0o600); err != nil {
				return Result{ExitCode: 1}, err
			}
		case contains(c.Args, "install"):
			r.mu.Lock()
			r.present[pin.Binary] = true
			r.mu.Unlock()
		}
		return Result{}, nil
	}
}

// TestProvisionRefusesWithoutRecordedDigest is the fail-closed guarantee: an
// indexer with no recorded digest is never fetched, and the refusal happens
// BEFORE npm or docker is invoked at all.
//
// It builds synthetic pins with the digest stripped rather than relying on the
// shipped table. The shipped pins now carry real, verified digests, and an
// earlier version of this test asserted against them — so filling those in
// broke it. A test that only holds while a value is missing is testing the
// wrong thing: the behaviour under test is the REFUSAL, not the absence.
func TestProvisionRefusesWithoutRecordedDigest(t *testing.T) {
	t.Parallel()
	for _, shipped := range Pins() {
		if shipped.Channel == ChannelGoInstall {
			continue // verified by the Go module checksum database instead
		}
		pin := shipped
		pin.TarballSHA256 = ""
		pin.ImageDigest = ""
		t.Run(string(pin.Indexer), func(t *testing.T) {
			r := newFakeRunner("npm", "docker")
			r.handle("docker", func(Command) (Result, error) { return Result{ExitCode: 1}, nil })
			p := &Provisioner{Runner: r, Policy: config.ProvisionAuto, RunnerKind: RunnerDocker}

			_, err := p.Ensure(context.Background(), pin)
			if !errors.Is(err, ErrNoRecordedDigest) {
				t.Fatalf("err = %v, want ErrNoRecordedDigest", err)
			}
			for _, argv := range r.argvs() {
				if strings.Contains(argv, "pull") || strings.Contains(argv, "install") || strings.Contains(argv, "pack") {
					t.Fatalf("fetched something unverifiable: %s", argv)
				}
			}
		})
	}
}

// TestProvisionPolicyNever: the safest setting means nothing is downloaded, and
// the refusal names what is missing so the user can install it themselves.
func TestProvisionPolicyNever(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("npm")
	p := &Provisioner{Runner: r, Policy: config.ProvisionNever}

	_, err := p.Ensure(context.Background(), npmPin(sha256Of("x")))
	if !errors.Is(err, ErrProvisionNotPermitted) {
		t.Fatalf("err = %v, want ErrProvisionNotPermitted", err)
	}
	if len(r.calls()) != 0 {
		t.Fatalf("ran commands under provision=never: %v", r.argvs())
	}
}

// TestProvisionPromptIsNotSilent is the non-interactive rule. `prompt` is the
// default, so a CI job that never sees a question must refuse rather than
// assume yes; only the caller's explicit AssumeYes changes that.
func TestProvisionPromptIsNotSilent(t *testing.T) {
	t.Parallel()
	payload := "pkg"
	pin := npmPin(sha256Of(payload))

	t.Run("no prompt available", func(t *testing.T) {
		r := newFakeRunner("npm")
		fakeNPM(r, payload)
		p := &Provisioner{Runner: r, Policy: config.ProvisionPrompt}
		_, err := p.Ensure(context.Background(), pin)
		if !errors.Is(err, ErrProvisionNeedsConsent) {
			t.Fatalf("err = %v, want ErrProvisionNeedsConsent", err)
		}
		if len(r.calls()) != 0 {
			t.Fatalf("downloaded without consent: %v", r.argvs())
		}
	})

	t.Run("user declines", func(t *testing.T) {
		r := newFakeRunner("npm")
		fakeNPM(r, payload)
		asked := 0
		p := &Provisioner{
			Runner: r, Policy: config.ProvisionPrompt,
			Prompt: func(context.Context, ProvisionRequest) (bool, error) { asked++; return false, nil },
		}
		_, err := p.Ensure(context.Background(), pin)
		if !errors.Is(err, ErrProvisionDeclined) {
			t.Fatalf("err = %v, want ErrProvisionDeclined", err)
		}
		if asked != 1 {
			t.Fatalf("asked %d times, want 1", asked)
		}
		if len(r.calls()) != 0 {
			t.Fatalf("downloaded after being declined: %v", r.argvs())
		}
	})

	t.Run("explicit opt-in", func(t *testing.T) {
		r := newFakeRunner("npm")
		r.handle("npm", withInstallSideEffect(r, payload, pin))
		p := &Provisioner{Runner: r, Policy: config.ProvisionPrompt, AssumeYes: true}
		if _, err := p.Ensure(context.Background(), pin); err != nil {
			t.Fatalf("Ensure with AssumeYes: %v", err)
		}
	})
}

// TestProvisionPromptDescribesOnlyPinnedFacts: the sentence a user is asked to
// approve is built from compile-time constants. A prompt whose text an attacker
// could write is not a control.
func TestProvisionPromptDescribesOnlyPinnedFacts(t *testing.T) {
	t.Parallel()
	var seen ProvisionRequest
	r := newFakeRunner("npm")
	pin := npmPin(sha256Of("pkg"))
	r.handle("npm", withInstallSideEffect(r, "pkg", pin))
	p := &Provisioner{
		Runner: r, Policy: config.ProvisionPrompt,
		Prompt: func(_ context.Context, req ProvisionRequest) (bool, error) { seen = req; return true, nil },
	}
	if _, err := p.Ensure(context.Background(), pin); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if seen.Ref != "@sourcegraph/scip-typescript@0.3.14" {
		t.Errorf("prompt ref = %q", seen.Ref)
	}
	if !seen.Mutates {
		t.Error("a global npm install does mutate the host and must say so")
	}
	if !strings.Contains(seen.String(), "on this machine") {
		t.Errorf("prompt text = %q", seen.String())
	}
}

// TestPreexistingToolIsUsedAndVersioned: "if a tool is already on PATH, use it
// and record the version rather than installing". The recorded version is what
// the binary reports, not what the pin claims — asserting the pinned version for
// a binary we did not install would be a provenance lie.
func TestPreexistingToolIsUsedAndVersioned(t *testing.T) {
	t.Parallel()
	r := newFakeRunner("scip-typescript")
	r.handle("scip-typescript", func(Command) (Result, error) {
		return Result{Stdout: []byte("0.3.11\n")}, nil
	})
	p := &Provisioner{Runner: r, Policy: config.ProvisionNever}

	inst, err := p.Ensure(context.Background(), npmPin(sha256Of("x")))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !inst.Preexisting {
		t.Error("expected the tool to be reported as preexisting")
	}
	if inst.Version != "0.3.11" {
		t.Errorf("version = %q, want the version the binary reports", inst.Version)
	}
	if inst.Path != fakeBin("scip-typescript") {
		t.Errorf("path = %q", inst.Path)
	}
}

// TestGoInstallRunsOutsideTheRepository: `go install` inside a hostile
// repository reads that repository's go.mod, including a `toolchain` directive
// that would have the Go command download and execute a compiler of the
// repository's choosing.
func TestGoInstallRunsOutsideTheRepository(t *testing.T) {
	t.Parallel()
	repo := writeRepo(t, map[string]string{"go.mod": "module x\ntoolchain go1.99.9\n"})
	r := newFakeRunner("go")
	r.handle("go", func(c Command) (Result, error) {
		if strings.HasPrefix(c.Dir, repo) {
			t.Errorf("go install ran inside the repository: %s", c.Dir)
		}
		// The property that matters is DETERMINISM, not the literal value:
		// GOTOOLCHAIN must never be "auto" or empty, because that is what lets
		// a dependency's go.mod name a toolchain and have it downloaded and
		// executed. Pinning to our own running version keeps that guarantee
		// while allowing installs that need a current Go — "local" resolved to
		// the older `go` on PATH and broke them.
		if env := envValue(c.Env, "GOTOOLCHAIN"); env == "" || env == "auto" {
			t.Errorf("GOTOOLCHAIN = %q; must be pinned so no third party can choose the compiler", env)
		}
		if env := envValue(c.Env, "GOFLAGS"); env != "" {
			t.Errorf("GOFLAGS = %q, want empty", env)
		}
		r.mu.Lock()
		r.present["scip-go"] = true
		r.mu.Unlock()
		return Result{}, nil
	})

	p := &Provisioner{Runner: r, Policy: config.ProvisionAuto}
	pin, _ := PinFor(LangGo)
	if _, err := p.Ensure(context.Background(), pin); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	var installed bool
	for _, c := range r.calls() {
		if contains(c.Args, "install") {
			installed = true
			if !contains(c.Args, pin.Module+"@"+pin.Version) {
				t.Errorf("installed %v, want the pinned version", c.Args)
			}
			for _, a := range c.Args {
				if strings.Contains(a, "latest") {
					t.Errorf("unpinned install: %v", c.Args)
				}
			}
		}
	}
	if !installed {
		t.Fatal("nothing was installed")
	}
}

func TestPinsAreFullySpecified(t *testing.T) {
	t.Parallel()
	seen := map[Language]bool{}
	for _, p := range Pins() {
		if p.Version == "" || strings.Contains(p.Version, "latest") {
			t.Errorf("%s: version %q must be an exact pin", p.Indexer, p.Version)
		}
		if p.Binary == "" || p.Language == "" || p.Channel == "" {
			t.Errorf("%s: incomplete pin %+v", p.Indexer, p)
		}
		if seen[p.Language] {
			t.Errorf("%s: two pins claim language %s", p.Indexer, p.Language)
		}
		seen[p.Language] = true
	}
	for _, l := range []Language{LangGo, LangJava, LangTypeScript, LangPython} {
		if !seen[l] {
			t.Errorf("no pin for %s", l)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// TestShippedPinsCarryDigests guards the gap that made the refusal test
// misleading: every non-go-install pin must record a digest, or provisioning
// silently degrades to "refuses everything" and the feature does not work at
// all while every test still passes.
func TestShippedPinsCarryDigests(t *testing.T) {
	t.Parallel()
	for _, pin := range Pins() {
		switch pin.Channel {
		case ChannelGoInstall:
			// go install verifies through the module checksum database.
			if pin.Module == "" {
				t.Errorf("%s: go-install pin has no module path", pin.Indexer)
			}
		case ChannelNPM:
			if pin.TarballSHA256 == "" {
				t.Errorf("%s: no TarballSHA256; provisioning will refuse and the "+
					"indexer can never be installed", pin.Indexer)
			}
			if len(pin.TarballSHA256) != 64 {
				t.Errorf("%s: TarballSHA256 %q is not a 64-char hex digest", pin.Indexer, pin.TarballSHA256)
			}
		case ChannelDocker:
			if pin.ImageDigest == "" {
				t.Errorf("%s: no ImageDigest; provisioning will refuse", pin.Indexer)
			}
			if !strings.HasPrefix(pin.ImageDigest, "sha256:") {
				t.Errorf("%s: ImageDigest %q lacks the sha256: prefix", pin.Indexer, pin.ImageDigest)
			}
			// Docker Hub tags for this repository carry no "v" prefix; an
			// earlier pin of "v0.10.5" would have failed with manifest unknown.
			if strings.HasPrefix(pin.Version, "v") {
				t.Errorf("%s: version %q has a v prefix; this registry's tags do not", pin.Indexer, pin.Version)
			}
		}
	}
}
