package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
)

// TestFirstIndexInProgressIsNotAbsent: an agent told "no index, run cortex
// index" while one is running will start a second, competing indexer.
func TestFirstIndexInProgressIsNotAbsent(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	if _, err := reg.BeginRun(ctx, "fast", ""); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Deps{Registry: reg, Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	clientT, serverT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), serverT) }()
	sess, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: "get_project_overview", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(res)
	if res.IsError {
		t.Errorf("indexing in progress was flagged isError:\n%s", text)
	}
	if !strings.Contains(text, "still being built") {
		t.Errorf("response does not say indexing is in progress:\n%s", text)
	}
	if !strings.Contains(text, "SYSTEM METADATA") {
		t.Errorf("guidance is not framed like a normal answer:\n%s", text)
	}
}

// TestNewerRunInProgressIsNoted: run 1 is committed and answers; run 2 is
// being built. The answer is valid for run 1 and must say it may be stale.
func TestNewerRunInProgressIsNoted(t *testing.T) {
	sess := newTestServer(t, func(ctx context.Context, reg *registry.Registry, _ int64) {
		if _, err := reg.BeginRun(ctx, "fast", ""); err != nil {
			t.Fatal(err)
		}
	})
	res := callTool(t, sess, "get_project_overview", map[string]any{})
	text := resultText(res)
	if !strings.Contains(text, "newer index") {
		t.Errorf("answer served during a newer build does not say so:\n%s", text)
	}
	for _, want := range []string{"run 2", "from run 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("note does not name %q:\n%s", want, text)
		}
	}
}

// TestStaleLowerIDStagingRunIsIgnored: an abandoned 'indexing' row with an id
// below the committed run is debris, not a newer build, and must not warn.
func TestStaleLowerIDStagingRunIsIgnored(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	stale, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, committed.ID); err != nil {
		t.Fatal(err)
	}
	if staging, err := reg.StagingRun(ctx); err != nil || staging == nil || staging.ID != stale.ID {
		t.Fatalf("setup: want stale run %d still staging, got (%v, %v)", stale.ID, staging, err)
	}

	srv, err := New(Deps{Registry: reg, Config: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	if note := srv.stagingNote(ctx, committed.ID); note != "" {
		t.Errorf("stale lower-id staging run produced a note: %q", note)
	}
}

// TestNoStagingRunNoNote guards the common case from noise.
func TestNoStagingRunNoNote(t *testing.T) {
	sess := newTestServer(t, nil)
	text := resultText(callTool(t, sess, "get_project_overview", map[string]any{}))
	if strings.Contains(text, "newer index") {
		t.Errorf("staleness note shown with no staging run:\n%s", text)
	}
}
