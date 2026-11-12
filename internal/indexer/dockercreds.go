package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// credHelperTimeout bounds the credential-helper preflight. A helper answers
// `list` from a local store in milliseconds; one that has not answered in five
// seconds is not slow, it is stuck.
const credHelperTimeout = 5 * time.Second

// dockerHubRegistryKey is the config.json key Docker uses for Docker Hub —
// the one registry whose key is not its host name.
const dockerHubRegistryKey = "https://index.docker.io/v1/"

// dockerConfig is the subset of ~/.docker/config.json the preflight reads.
type dockerConfig struct {
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// dockerConfigPath is where the Docker CLI reads its client configuration:
// $DOCKER_CONFIG/config.json, else ~/.docker/config.json.
func dockerConfigPath() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

// registryOf reports the config.json credential key for an image reference:
// the first path segment when it names a host (a dot, a colon, or
// "localhost"), otherwise Docker Hub.
func registryOf(image string) string {
	i := strings.IndexByte(image, '/')
	if i < 0 {
		return dockerHubRegistryKey
	}
	first := image[:i]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		if first == "docker.io" || first == "index.docker.io" {
			return dockerHubRegistryKey
		}
		return first
	}
	return dockerHubRegistryKey
}

// dockerCredentialHelper names the helper binary the Docker CLI would consult
// for image, or "" when the configuration names none. A per-registry
// credHelpers entry wins over the global credsStore, exactly as the CLI
// resolves it.
func dockerCredentialHelper(configPath, image string) (string, error) {
	if configPath == "" {
		return "", nil
	}
	data, err := os.ReadFile(configPath) //nolint:gosec // G304: the Docker CLI's own config file, at the path the CLI itself reads
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", configPath, err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", configPath, err)
	}
	name := cfg.CredsStore
	if h, ok := cfg.CredHelpers[registryOf(image)]; ok && h != "" {
		name = h
	}
	if name == "" {
		return "", nil
	}
	return "docker-credential-" + name, nil
}

// credentialHelperPreflight fails fast on the one failure `docker pull` cannot
// report.
//
// Before the CLI talks to the daemon it asks the configured credential helper
// for the registry's credentials. Docker Desktop's helper opens a connection to
// the Desktop app, and that connection sometimes never completes: observed
// twice on one host, for stretches of minutes each time, from a shell with no
// Desktop session, with the trigger not pinned down — the same helper answered
// `list` in 170ms an hour later. While it lasts, the pull blocks before a byte
// is requested, RepoCortex's provisioning timeout expires ten minutes later, and
// the user is told "could not install <image>", which is true and useless: the
// image was never asked for.
//
// So the helper is asked first, with a bound a working helper cannot miss —
// `list` reads a local store and answers in milliseconds — at the cost of one
// short subprocess per pull. A helper that times out is the finding, and the
// error says what to do about it: a digest-pinned public image needs no
// credentials, so pointing DOCKER_CONFIG at an empty directory sidesteps the
// helper without weakening the pull. Every other outcome — no helper
// configured, helper not on PATH, helper exits non-zero — is left to
// `docker pull`, which reports those well.
func (p *Provisioner) credentialHelperPreflight(ctx context.Context, pin Pin) error {
	helper, err := dockerCredentialHelper(dockerConfigPath(), pin.Image)
	if err != nil {
		// An unreadable config is docker's problem to report, not ours to
		// refuse on.
		return nil //nolint:nilerr // deliberate: the preflight only ever refuses on a hang
	}
	if helper == "" {
		return nil
	}
	pathTo, err := p.Runner.LookPath(helper)
	if err != nil {
		return nil //nolint:nilerr // docker pull will say the helper is missing
	}
	res, runErr := p.Runner.Run(ctx, Command{
		Path: pathTo, Args: []string{"list"}, Env: os.Environ(), Timeout: credHelperTimeout,
	})
	recordErr(p.Audit, ActionProbe, AuditRecord{
		Indexer: pin.Indexer, Language: pin.Language, Tool: ToolDocker, Result: res,
	}, runErr)
	if !res.TimedOut {
		return nil
	}
	return errs.Wrap(errs.KindUnsupported, "indexer.Ensure", fmt.Errorf(
		"%s did not answer within %s, so `docker pull` would block before reaching the daemon. "+
			"Docker Desktop's helper hangs from a shell with no Desktop session. "+
			"The pinned image is public and verified by digest, so no credential is needed: "+
			"run with DOCKER_CONFIG pointing at an empty directory (`DOCKER_CONFIG=$(mktemp -d) repocortex indexers install`), "+
			"or open Docker Desktop and retry",
		helper, credHelperTimeout))
}
