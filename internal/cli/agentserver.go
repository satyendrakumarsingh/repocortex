package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/hotspots"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	devmcp "github.com/satyendrakumarsingh/repocortex/internal/mcp"
	"github.com/satyendrakumarsingh/repocortex/internal/parse"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// auditLogEnv names the environment variable that turns the audit log on for
// every `repocortex serve` and `repocortex tool call` on a machine, without a flag in
// each agent's configuration.
const auditLogEnv = "REPOCORTEX_AUDIT_LOG"

// agentServerOptions configures the tool server behind `serve` and `tool`.
type agentServerOptions struct {
	// Profile overrides mcp.profile from .repocortex.yml when set.
	Profile config.MCPProfile
	// AuditPath is where tool calls are recorded. Empty falls back to
	// REPOCORTEX_AUDIT_LOG; empty there too means no log.
	AuditPath string
}

// openAgentServer opens the committed index read-only and builds the tool
// server over it. The returned function releases everything it opened.
//
// `repocortex serve`, the REST endpoint and `repocortex tool` all come through here, so
// the three front ends cannot be wired differently: same index handle, same
// history source, same language table, same audit log.
//
// The audit log is a flag and an environment variable and deliberately NOT a
// key in .repocortex.yml. That file is repository content, and a repository must
// not be able to switch off the record of what an agent read from it.
func openAgentServer(ctx context.Context, g *Global, opts agentServerOptions) (*devmcp.Server, func(), error) {
	cfg := g.Config()
	if opts.Profile != "" {
		if !opts.Profile.Valid() {
			return nil, nil, fmt.Errorf("--profile %q: want minimal, standard or full", opts.Profile)
		}
		cfg.MCPProfile = opts.Profile
	}

	dbPath := g.DBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("no index at %s — run 'repocortex init' and 'repocortex index' first", dbPath)
	}

	// Read-only: an agent session must never be able to mutate the index it
	// is querying.
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return nil, nil, err
	}
	closers := []func(){func() { _ = db.Close() }}
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	deps := devmcp.Deps{
		Registry:  registry.New(db),
		Graph:     graph.New(db, graph.WithCSR()),
		Config:    cfg,
		Judgeable: bindsReferences,
	}
	if h := openHistory(g); h.repo != nil {
		deps.History = hotspotHistory{h}
		deps.Owners = h
	}

	auditPath := opts.AuditPath
	if auditPath == "" {
		auditPath = strings.TrimSpace(os.Getenv(auditLogEnv))
	}
	if auditPath != "" {
		f, err := openAuditFile(auditPath)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		closers = append(closers, func() { _ = f.Close() })
		deps.Audit = devmcp.NewAuditLog(f)
	}

	srv, err := devmcp.New(deps)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return srv, cleanup, nil
}

// openAuditFile opens the audit log for appending, readable only by the user:
// it records which symbols and tasks an agent asked about, which is a map of
// what someone is working on.
func openAuditFile(path string) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create audit log directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the path is the user's own flag or environment value
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	return f, nil
}

// bindsReferences reports whether the index binds cross-file references for a
// language. In the parsed tier it records definitions only, so every one of
// them has no caller by construction; see internal/parse/tiers.go.
func bindsReferences(language string) bool {
	return parse.TierFor(language) != parse.TierParsed
}

// hotspotHistory adapts the CLI's git history reader to hotspots.History.
//
// The reader already rebases commit paths from the worktree root onto the
// project root (see gitHistory), which is the translation hotspots.Commit
// requires and the reason it is a distinct type.
type hotspotHistory struct{ h *gitHistory }

func (a hotspotHistory) Commits(ctx context.Context, limit int) ([]hotspots.Commit, error) {
	cs, err := a.h.Commits(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]hotspots.Commit, 0, len(cs))
	for _, c := range cs {
		out = append(out, hotspots.Commit{SHA: c.SHA, When: c.When, Paths: c.Paths})
	}
	return out, nil
}
