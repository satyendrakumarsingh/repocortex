// Package config loads RepoCortex settings from two distinct trust domains.
//
// THE SECURITY BOUNDARY IN THIS FILE IS LOAD-BEARING.
//
// Repository config (.repocortex.yml, committed and therefore attacker-supplied
// when you clone an untrusted repo) may only influence what gets indexed.
// It may never influence what gets EXECUTED.
//
// Indexer provisioning runs third-party binaries and build systems — Maven and
// Gradle plugins execute arbitrary code by design. If a repo could name the
// indexer to run or the URL to fetch it from, then `git clone && repocortex
// index` would be remote code execution. So those settings live exclusively in
// user config (see UserConfigPath; ~/.config on Linux, ~/Library/Application
// Support on macOS), which the repo cannot write.
//
// Spec §33 lists "avoid arbitrary command execution" in the threat model; this
// package is where that is enforced.
package config

import (
	"github.com/satyendrakumarsingh/repocortex/internal/arch"

	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AccuracyMode selects how cross-file references are resolved.
type AccuracyMode string

const (
	// AccuracyFast uses tree-sitter only. No toolchain, seconds to index,
	// heuristic confidence.
	AccuracyFast AccuracyMode = "fast"

	// AccuracyAuto uses compiler-backed indexers where a toolchain is present
	// and falls back to heuristics elsewhere, labelling each edge accordingly.
	AccuracyAuto AccuracyMode = "auto"

	// AccuracyExact requires compiler-backed indexing for every detected
	// language and fails loudly when that is impossible. Slow — it runs real
	// builds — but every edge is compiler-proven.
	AccuracyExact AccuracyMode = "exact"
)

func (m AccuracyMode) Valid() bool {
	switch m {
	case AccuracyFast, AccuracyAuto, AccuracyExact:
		return true
	}
	return false
}

// ProvisionPolicy controls whether RepoCortex may install third-party indexers.
type ProvisionPolicy string

const (
	// ProvisionNever refuses to download anything. Safest; `exact` mode then
	// works only with already-installed indexers.
	ProvisionNever ProvisionPolicy = "never"

	// ProvisionPrompt asks before each download. The default: provisioning is
	// never silent.
	ProvisionPrompt ProvisionPolicy = "prompt"

	// ProvisionAuto downloads without asking. Requires an explicit opt-in and
	// still verifies checksums.
	ProvisionAuto ProvisionPolicy = "auto"
)

func (p ProvisionPolicy) Valid() bool {
	switch p {
	case ProvisionNever, ProvisionPrompt, ProvisionAuto:
		return true
	}
	return false
}

// MCPProfile bounds how many tools are exposed to an agent.
//
// This is a correctness setting, not cosmetics: every exposed tool's schema is
// injected into the agent's context on every request. A server exposing forty
// tools degrades tool selection and burns the very context RepoCortex exists to
// save.
type MCPProfile string

const (
	// Counts are deliberately not stated here. They were, and they were wrong
	// in both directions — "~6" for a set of 7 and "~15" for a set of 9 — because
	// a number in a comment beside a list in another package drifts the first
	// time either changes. internal/mcp.profileTools is the definition, and
	// `repocortex serve --list-tools` prints what a profile actually exposes.
	MCPMinimal   MCPProfile = "minimal"   // the default: lookup, search and task context
	MCPStandard  MCPProfile = "standard"  // adds blast radius and test discovery
	MCPFull      MCPProfile = "full"      // every enabled capability
	MCPCompact   MCPProfile = "compact"   // single front-door tool: saves prompt tokens
	MCPFrontdoor MCPProfile = "frontdoor" // alias for compact
)

func (p MCPProfile) Valid() bool {
	switch p {
	case MCPMinimal, MCPStandard, MCPFull, MCPCompact, MCPFrontdoor:
		return true
	}
	return false
}

// RepoConfig is the repository-scoped configuration (.repocortex.yml).
//
// UNTRUSTED. Everything here describes *what to index*, never *what to run*.
// Adding an execution-capable field to this struct reintroduces the RCE this
// package exists to prevent.
type RepoConfig struct {
	Project struct {
		Name string `yaml:"name"`
	} `yaml:"project"`

	Index struct {
		Include []string `yaml:"include"`
		Exclude []string `yaml:"exclude"`

		// LanguageOverrides maps a glob to the language its files are written
		// in, for the cases content heuristics cannot settle.
		//
		// SAFE IN UNTRUSTED CONFIG, for the reason architecture rules are: it
		// is declarative data. A glob is matched by the hand-written matcher
		// index.include uses, never compiled as a regular expression, and the
		// value can only name a grammar that is already compiled in. The worst
		// a hostile entry can do is route files to the wrong grammar, which
		// costs symbols from those files and is bounded by the per-file parse
		// timeout: the same reach a repository already has by committing a
		// file with a misleading extension.
		LanguageOverrides LanguageOverrides `yaml:"language_overrides"`

		// MaxFileBytes is the largest file that is read, parsed and
		// text-indexed. Larger files are recorded as skipped, with their size,
		// and named by `repocortex index` and `repocortex status`.
		//
		// SAFE IN UNTRUSTED CONFIG ONLY BECAUSE IT IS BOUNDED. Parsing costs
		// memory in proportion to the source, so an unbounded value here would
		// let a cloned repository choose how much memory `repocortex index`
		// uses. Validate rejects anything outside
		// MinMaxFileBytes..HardMaxFileBytes, and the pipeline admits large
		// files a few at a time whatever the limit is.
		MaxFileBytes int64 `yaml:"max_file_bytes"`
	} `yaml:"index"`

	Languages []string `yaml:"languages"`

	Capabilities []string `yaml:"capabilities"`

	Context struct {
		MaxFiles      int `yaml:"max_files"`
		MaxSymbols    int `yaml:"max_symbols"`
		MaxLines      int `yaml:"max_lines"`
		MaxGraphDepth int `yaml:"max_graph_depth"`
	} `yaml:"context"`

	MCP struct {
		Profile MCPProfile `yaml:"profile"`
	} `yaml:"mcp"`

	// There is deliberately no `security:` block here, and its absence is the
	// stronger version of what used to be in it.
	//
	// `security.redact_secrets` existed and could be set to `true` (which was
	// the default) or to `false` (which was parsed and then ignored, so that a
	// cloned repository could not switch off redaction of its own secrets).
	// Neither value ever reached a decision: redaction is unconditional in
	// pipeline.RegistrySink, which calls security.RedactFile on every file's
	// text before the search row is written. A key whose only permitted value
	// is the behaviour you already get is not a control, it is a claim — and it
	// invited the reading that redaction is optional.
	//
	// Removing it keeps the asymmetry and hardens it. KnownFields(true) now
	// makes `security:` an outright parse error, so a repository cannot express
	// an opinion about redaction at all, and a user who wrote `redact_secrets:
	// false` learns that it was never honoured instead of believing it was.
	// TestRepoCannotDisableRedaction guards exactly that.

	// Architecture holds component definitions and dependency rules.
	//
	// SAFE IN UNTRUSTED CONFIG, and the reasoning matters because everything
	// else in this struct is here on the grounds that it cannot cause
	// execution. Architecture rules are pure declarative data: path globs and
	// lists of component names. They are matched by a hand-written glob
	// matcher, never compiled as a regular expression, so a hostile pattern
	// cannot become a catastrophic backtrack either. Nothing here selects a
	// tool, a path to run, or an argument.
	//
	// And this IS the right place for them. Architecture rules describe the
	// repository they live in and change with it; putting them in user config
	// would mean every contributor maintaining their own copy of a shared
	// constraint, which is how the constraint stops being enforced.
	Architecture arch.Model `yaml:"architecture"`
}

// LanguageOverride routes the files a glob matches to one language.
type LanguageOverride struct {
	Pattern  string
	Language string
}

// LanguageOverrides is index.language_overrides: a YAML mapping from glob to
// language that keeps the order it was written in.
//
// Order matters because the last matching entry wins, as in .gitattributes: a
// broad rule is written first and narrowed by the lines after it. A Go map
// would lose that order, and with it the meaning of two overlapping globs, so
// the mapping node is decoded by hand.
type LanguageOverrides []LanguageOverride

// MaxLanguageOverrides bounds the list. Every entry is matched against every
// file in the repository, and this file is untrusted.
const MaxLanguageOverrides = 256

// UnmarshalYAML decodes the mapping in document order.
func (o *LanguageOverrides) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("index.language_overrides: want a mapping from glob to language, such as %q",
			`"*.h": cpp`)
	}
	if n := len(node.Content) / 2; n > MaxLanguageOverrides {
		return fmt.Errorf("index.language_overrides has %d entries, over the %d limit", n, MaxLanguageOverrides)
	}
	out := make(LanguageOverrides, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || val.Kind != yaml.ScalarNode {
			return fmt.Errorf("index.language_overrides line %d: want %q", key.Line, `"<glob>": <language>`)
		}
		pattern, language := strings.TrimSpace(key.Value), strings.TrimSpace(val.Value)
		if pattern == "" || language == "" {
			return fmt.Errorf("index.language_overrides line %d: the glob and the language must both be set", key.Line)
		}
		out = append(out, LanguageOverride{Pattern: pattern, Language: language})
	}
	*o = out
	return nil
}

// UserConfig is the user-scoped configuration.
//
// The path is OS-dependent and comes from UserConfigPath -- NOT always
// ~/.config. On macOS os.UserConfigDir resolves to ~/Library/Application
// Support, so a file written to ~/.config/repocortex/config.yml there is read by
// nothing. This comment used to name that path unconditionally, which cost a
// real debugging session: the indexer told a user to set
// accuracy.indexers.runner, they set it, and the same message came back.
//
// TRUSTED. Only the machine's owner can write it, so it alone may authorise
// downloading and executing indexers.
type UserConfig struct {
	Accuracy struct {
		Mode     AccuracyMode `yaml:"mode"`
		Indexers struct {
			Provision   ProvisionPolicy `yaml:"provision"`
			Runner      string          `yaml:"runner"` // native | docker
			Languages   []string        `yaml:"languages"`
			Timeout     string          `yaml:"timeout"`
			MaxParallel int             `yaml:"max_parallel"`
			NodeHeapMB  int             `yaml:"node_heap_mb"`

			// MountLocalArtifacts lets a containerised build read and write the
			// invoking user's local artifact cache (~/.m2).
			//
			// USER CONFIG ONLY, and off by default. The container exists so an
			// untrusted repository's build cannot touch the developer's
			// machine; this hands it one directory of that machine, read-write
			// because Maven writes there. A hostile build could poison a cached
			// artifact that every other project on the host then compiles
			// against, and read any credentials a settings.xml holds.
			//
			// It is offered because for a repository you already build locally
			// the risk is one you have already accepted, and without it a
			// private dependency makes compiler-backed indexing impossible —
			// which is the difference between exact confidence and none.
			MountLocalArtifacts bool `yaml:"mount_local_artifacts"`
		} `yaml:"indexers"`
	} `yaml:"accuracy"`
}

// Config is the resolved configuration the rest of the system consumes.
type Config struct {
	ProjectName  string
	Include      []string
	Exclude      []string
	Languages    []string
	Capabilities []string

	// LanguageOverrides is index.language_overrides, in file order. Language
	// names are checked where the grammars are known: fs.NewWalker refuses a
	// name no grammar exists for.
	LanguageOverrides []LanguageOverride

	// MaxFileBytes is the size limit for indexing: index.max_file_bytes, or
	// DefaultMaxFileBytes.
	MaxFileBytes int64

	MaxFiles      int
	MaxSymbols    int
	MaxLines      int
	MaxGraphDepth int

	MCPProfile MCPProfile

	// There is no RedactSecrets field. Redaction is not configurable: see the
	// note in RepoConfig, and pipeline.RegistrySink for where it happens.

	// Architecture is the declared component model and its dependency rules.
	// Empty when the repository declares none, which is the normal case:
	// architecture governance is opt-in.
	Architecture arch.Model

	// Execution-domain settings. These come from user config only; see the
	// package comment.
	Accuracy        AccuracyMode
	ProvisionPolicy ProvisionPolicy
	IndexerRunner   string

	// IndexerMountLocalArtifacts mirrors
	// UserConfig.Accuracy.Indexers.MountLocalArtifacts. Only user config can
	// set it — a repository must never be able to ask for its own build to be
	// given the host's artifact cache.
	IndexerMountLocalArtifacts bool
	IndexerLanguages           []string
	IndexerTimeout             time.Duration
	IndexerMaxParallel         int
	IndexerNodeHeapMB          int
	RepoRoot                   string
	RepoConfigFile             string
	UserConfigPresent          bool
}

// DefaultMaxFileBytes is the size limit when a repository sets none: 2 MiB.
//
// It was effectively 256 KiB (the parser refused anything larger while
// discovery admitted 2 MiB), which dropped hand-written files at the centre of
// real programs: seven of redis's core sources are between 256 KiB and 700 KiB.
// The measurements behind the new figure are beside
// parse.DefaultMaxSourceBytes.
const DefaultMaxFileBytes int64 = 2 << 20

// MinMaxFileBytes and HardMaxFileBytes bound index.max_file_bytes.
//
// The floor exists so a typo (a limit of 10, meaning megabytes) is an error and
// not an index of nothing. The ceiling is the security control: .repocortex.yml
// arrives with the repository, and a file at 8 MiB costs on the order of a
// gigabyte to parse. To index something larger, split it or exclude it.
const (
	MinMaxFileBytes  int64 = 1 << 10
	HardMaxFileBytes int64 = 8 << 20
)

// Defaults returns a Config with safe defaults applied.
//
// The defaults are deliberately conservative: heuristics rather than executing
// builds, prompting rather than downloading, minimal tool surface.
func Defaults() Config {
	return Config{
		Include: []string{"**"},
		Exclude: []string{
			".git/**", "node_modules/**", "vendor/**", "target/**",
			"build/**", "dist/**", "coverage/**", ".repocortex/**", ".repocortex/**",
			"**/*.min.js", "**/*.lock", "**/testdata/**",
		},
		Capabilities:    []string{"code"},
		MaxFileBytes:    DefaultMaxFileBytes,
		MaxFiles:        20,
		MaxSymbols:      100,
		MaxLines:        5000,
		MaxGraphDepth:   3,
		MCPProfile:      MCPMinimal,
		Accuracy:        AccuracyAuto,
		ProvisionPolicy: ProvisionPrompt,
		IndexerRunner:   "native",
		IndexerTimeout:  10 * time.Minute,
	}
}

// RepoConfigName is the per-repository config filename.
const RepoConfigName = ".repocortex.yml"

// Load resolves configuration for repoRoot.
//
// Order: defaults, then user config (trusted), then repo config (untrusted,
// and structurally unable to set execution settings).
func Load(repoRoot string) (Config, error) {
	cfg := Defaults()
	cfg.RepoRoot = repoRoot

	userPath, err := UserConfigPath()
	if err == nil {
		uc, found, err := loadUserConfig(userPath)
		if err != nil {
			return cfg, fmt.Errorf("user config %s: %w", userPath, err)
		}
		if found {
			cfg.UserConfigPresent = true
			applyUser(&cfg, uc)
		}
	}

	var repoConfigFile string
	for _, name := range []string{RepoConfigName, ".cortex.yml", ".devintel.yml"} {
		p := filepath.Join(repoRoot, name)
		if _, err := os.Stat(p); err == nil {
			repoConfigFile = p
			break
		}
	}
	if repoConfigFile == "" {
		repoConfigFile = filepath.Join(repoRoot, RepoConfigName)
	}

	rc, found, err := loadRepoConfig(repoConfigFile)
	if err != nil {
		return cfg, fmt.Errorf("repo config: %w", err)
	}
	if found {
		cfg.RepoConfigFile = repoConfigFile
		applyRepo(&cfg, rc)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// UserConfigPath returns the trusted config location for this OS.
func UserConfigPath() (string, error) {
	if v := os.Getenv("REPOCORTEX_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "config.yml"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		if runtime.GOOS == "windows" {
			return "", errors.New("cannot determine user config directory")
		}
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", herr
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "repocortex", "config.yml"), nil
}

func loadUserConfig(path string) (UserConfig, bool, error) {
	var uc UserConfig
	b, err := os.ReadFile(path) //nolint:gosec // path is UserConfigPath(), derived from the OS config dir, never from user input
	if err != nil {
		if os.IsNotExist(err) {
			return uc, false, nil
		}
		return uc, false, err
	}
	if err := yaml.Unmarshal(b, &uc); err != nil {
		return uc, false, fmt.Errorf("parse: %w", friendlyYAMLError(err, b))
	}
	return uc, true, nil
}

// maxRepoConfigBytes bounds .repocortex.yml.
//
// This file arrives WITH the repository and is therefore untrusted, and it is
// read by every single subcommand — so an unbounded one is a denial of service
// against the whole tool, not against one command. Measured before this cap: a
// 17 MB .repocortex.yml drove `repocortex status` to 495 MB of resident memory,
// roughly 29x amplification, on a command that only prints counts.
//
// 1 MiB matches the ceilings already used for .gitignore and for configuration
// files elsewhere in the analysis layer. A hand-written project config is
// measured in kilobytes; one that is not is generated or hostile.
const maxRepoConfigBytes = 1 << 20

func loadRepoConfig(path string) (RepoConfig, bool, error) {
	var rc RepoConfig
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return rc, false, nil
		}
		return rc, false, err
	}
	if info.Size() > maxRepoConfigBytes {
		return rc, false, fmt.Errorf("%s is %d bytes, over the %d limit; "+
			"a project config this large is generated or hostile, and parsing it would "+
			"cost far more memory than the file",
			RepoConfigName, info.Size(), maxRepoConfigBytes)
	}
	b, err := os.ReadFile(path) //nolint:gosec // path is repoRoot + a fixed filename; repoRoot is validated by the CLI
	if err != nil {
		return rc, false, err
	}
	// KnownFields makes an unrecognised key an error rather than a silent
	// no-op. If a repo tries to set an execution field, the user finds out.
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&rc); err != nil {
		// An empty or comment-only file has no YAML document, which the
		// decoder reports as EOF. That is a config that sets nothing, i.e.
		// the defaults, not a parse failure.
		if errors.Is(err, io.EOF) {
			return RepoConfig{}, true, nil
		}
		return rc, false, fmt.Errorf("parse %s: %w", RepoConfigName, friendlyYAMLError(err, b))
	}
	return rc, true, nil
}

// yamlTypeRe matches the decoder's "cannot unmarshal !!int `5` into
// config.X" wording.
var yamlTypeRe = regexp.MustCompile("^(line \\d+): cannot unmarshal !!(\\w+) (?:`(.*)` )?into (.+)$")

// friendlyYAMLError rewrites a yaml.TypeError so it no longer names Go struct
// types. The decoder only knows the Go type, so the message says what value
// was found and what kind of value the key wants.
func friendlyYAMLError(err error, src []byte) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, m := range te.Errors {
		sm := yamlTypeRe.FindStringSubmatch(m)
		if sm == nil {
			msgs = append(msgs, m)
			continue
		}
		found := yamlKindNames[sm[2]]
		if found == "" {
			found = sm[2]
		}
		want := goTypeWant(sm[4])
		where := sm[1]
		if k := yamlKeyAtLine(src, sm[1]); k != "" {
			where += ", key " + k
		}
		if sm[3] != "" {
			msgs = append(msgs, fmt.Sprintf("%s: got %s (%s), want %s", where, found, sm[3], want))
		} else {
			msgs = append(msgs, fmt.Sprintf("%s: got %s, want %s", where, found, want))
		}
	}
	return errors.New("invalid value: " + strings.Join(msgs, "; "))
}

var yamlKindNames = map[string]string{
	"int": "a number", "float": "a number", "str": "a string", "bool": "a boolean",
	"seq": "a list", "map": "a mapping", "null": "null",
}

// yamlKeyAtLine returns the mapping key written on a "line N" location, or "".
func yamlKeyAtLine(src []byte, loc string) string {
	n, err := strconv.Atoi(strings.TrimPrefix(loc, "line "))
	lines := strings.Split(string(src), "\n")
	if err != nil || n < 1 || n > len(lines) {
		return ""
	}
	key, _, ok := strings.Cut(strings.TrimSpace(lines[n-1]), ":")
	key = strings.Trim(strings.TrimPrefix(key, "- "), `"' `)
	if !ok || key == "" {
		return ""
	}
	return key
}

// goTypeWant describes a Go target type in config terms.
func goTypeWant(t string) string {
	switch {
	case strings.HasPrefix(t, "[]"):
		return "a list"
	case strings.HasPrefix(t, "map["):
		return "a mapping"
	case t == "string" || t == "config.AccuracyMode" || t == "config.MCPProfile" || t == "config.ProvisionPolicy":
		return "a string"
	case strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint") || strings.HasPrefix(t, "float"):
		return "a number"
	case t == "bool":
		return "true or false"
	default:
		return "a mapping of nested keys"
	}
}

// checkGlob reports a malformed glob component, such as an unclosed "[". The
// walker matches each path component with path.Match and treats a pattern it
// cannot parse as never matching, so without this check a typo excludes
// nothing and says nothing.
func checkGlob(g string) error {
	g = strings.TrimPrefix(g, "!")
	for _, part := range strings.Split(g, "/") {
		if part == "**" {
			continue
		}
		if _, err := path.Match(part, ""); err != nil {
			return err
		}
	}
	return nil
}

func applyUser(cfg *Config, uc UserConfig) {
	if uc.Accuracy.Mode != "" {
		cfg.Accuracy = uc.Accuracy.Mode
	}
	if uc.Accuracy.Indexers.Provision != "" {
		cfg.ProvisionPolicy = uc.Accuracy.Indexers.Provision
	}
	if uc.Accuracy.Indexers.MountLocalArtifacts {
		cfg.IndexerMountLocalArtifacts = true
	}
	if uc.Accuracy.Indexers.Runner != "" {
		cfg.IndexerRunner = uc.Accuracy.Indexers.Runner
	}
	if len(uc.Accuracy.Indexers.Languages) > 0 {
		cfg.IndexerLanguages = uc.Accuracy.Indexers.Languages
	}
	// Previously parsed and then dropped on the floor: the YAML key was
	// accepted, so a user setting it saw no error and no effect. A silently
	// ignored setting is worse than an unsupported one.
	if uc.Accuracy.Indexers.Timeout != "" {
		if d, err := time.ParseDuration(uc.Accuracy.Indexers.Timeout); err == nil {
			cfg.IndexerTimeout = d
		}
	}
	if uc.Accuracy.Indexers.MaxParallel > 0 {
		cfg.IndexerMaxParallel = uc.Accuracy.Indexers.MaxParallel
	}
	if uc.Accuracy.Indexers.NodeHeapMB > 0 {
		cfg.IndexerNodeHeapMB = uc.Accuracy.Indexers.NodeHeapMB
	}
}

// applyRepo merges untrusted repository settings.
//
// Note what is absent: nothing here touches Accuracy, ProvisionPolicy or
// IndexerRunner. That omission is the security control.
func applyRepo(cfg *Config, rc RepoConfig) {
	if rc.Project.Name != "" {
		cfg.ProjectName = rc.Project.Name
	}
	if len(rc.Index.Include) > 0 {
		cfg.Include = rc.Index.Include
	}
	if len(rc.Index.Exclude) > 0 {
		// Repo excludes ADD to the defaults rather than replacing them, so a
		// repo cannot un-exclude .git or node_modules.
		cfg.Exclude = append(cfg.Exclude, rc.Index.Exclude...)
	}
	if rc.Index.MaxFileBytes != 0 {
		// Taken as written, including a nonsensical value: Validate rejects
		// what is out of range, so a bad limit is an error the user sees and
		// not a setting that silently did nothing.
		cfg.MaxFileBytes = rc.Index.MaxFileBytes
	}
	if len(rc.Languages) > 0 {
		cfg.Languages = rc.Languages
	}
	if len(rc.Index.LanguageOverrides) > 0 {
		cfg.LanguageOverrides = rc.Index.LanguageOverrides
	}
	if len(rc.Capabilities) > 0 {
		cfg.Capabilities = rc.Capabilities
	}
	if rc.Context.MaxFiles > 0 {
		cfg.MaxFiles = rc.Context.MaxFiles
	}
	if rc.Context.MaxSymbols > 0 {
		cfg.MaxSymbols = rc.Context.MaxSymbols
	}
	if rc.Context.MaxLines > 0 {
		cfg.MaxLines = rc.Context.MaxLines
	}
	if rc.Context.MaxGraphDepth > 0 {
		cfg.MaxGraphDepth = rc.Context.MaxGraphDepth
	}
	if rc.MCP.Profile != "" {
		cfg.MCPProfile = rc.MCP.Profile
	}
	if len(rc.Architecture.Components) > 0 || len(rc.Architecture.Rules) > 0 {
		cfg.Architecture = rc.Architecture
	}
	// Nothing here touches redaction either, and now it cannot: the repo struct
	// has no security block, so `security: {redact_secrets: false}` is rejected
	// by the decoder rather than accepted and ignored.
}

// Validate rejects incoherent configuration.
func (c Config) Validate() error {
	// Architecture rules are validated here rather than where they are used,
	// so a typo in a component name is reported when the file is read — with
	// every problem at once — instead of surfacing later as a rule that
	// silently matches nothing.
	if err := c.Architecture.Validate(); err != nil {
		return fmt.Errorf("architecture: %w", err)
	}
	if !c.Accuracy.Valid() {
		return fmt.Errorf("accuracy.mode %q: want fast, auto or exact", c.Accuracy)
	}
	if !c.ProvisionPolicy.Valid() {
		return fmt.Errorf("accuracy.indexers.provision %q: want never, prompt or auto", c.ProvisionPolicy)
	}
	if !c.MCPProfile.Valid() {
		return fmt.Errorf("mcp.profile %q: want minimal, standard, full, compact or frontdoor", c.MCPProfile)
	}
	if c.IndexerRunner != "native" && c.IndexerRunner != "docker" {
		return fmt.Errorf("accuracy.indexers.runner %q: want native or docker", c.IndexerRunner)
	}
	if c.MaxGraphDepth < 1 || c.MaxGraphDepth > 10 {
		return fmt.Errorf("context.max_graph_depth %d: want 1..10 (unbounded traversal is a DoS on large repos)", c.MaxGraphDepth)
	}
	for _, key := range []struct {
		name string
		list []string
	}{{"index.include", c.Include}, {"index.exclude", c.Exclude}} {
		for _, g := range key.list {
			if err := checkGlob(g); err != nil {
				return fmt.Errorf("%s %q: not a valid glob (%w)", key.name, g, err)
			}
		}
	}
	if c.MaxFiles < 1 {
		return fmt.Errorf("context.max_files %d: must be >= 1", c.MaxFiles)
	}
	// Zero is "not set" for a Config built by hand (tests, embedders); Load
	// always fills the default in.
	if c.MaxFileBytes != 0 && (c.MaxFileBytes < MinMaxFileBytes || c.MaxFileBytes > HardMaxFileBytes) {
		return fmt.Errorf("index.max_file_bytes %d: want %d..%d (parsing costs memory in proportion to file size, "+
			"so the limit is bounded; exclude or split a larger file)",
			c.MaxFileBytes, MinMaxFileBytes, HardMaxFileBytes)
	}
	return nil
}
