package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/autorun"
)

// An agent is told when the last automatic re-index failed: a commit that
// matches the build says nothing about a hook whose update never landed.
// And the resolve time is when the last pass finished, not when it began.
func TestProvenanceCarriesTheLastAutoIndexAndTheResolveTime(t *testing.T) {
	dir := t.TempDir()
	s := &Server{deps: Deps{MetaDir: dir}, runMeta: map[int64]runMeta{}}
	started, finished := int64(1_790_000_000), int64(1_790_000_090)
	s.rememberRunMeta(7, "", started, finished, 0.5)

	p := s.provenanceFor(context.Background(), 7)
	if p.LastAutoIndex != nil {
		t.Fatalf("no pass recorded, got %+v", p.LastAutoIndex)
	}
	if want := time.Unix(finished, 0).UTC().Format(time.RFC3339); p.LastResolvedAt != want {
		t.Errorf("last_resolve_at = %q, want the pass's finish time %q", p.LastResolvedAt, want)
	}

	if err := autorun.Write(dir, time.Now(), time.Second, errors.New("index: disk full")); err != nil {
		t.Fatal(err)
	}
	p = s.provenanceFor(context.Background(), 7)
	if p.LastAutoIndex == nil || p.LastAutoIndex.OK || p.LastAutoIndex.Error != "index: disk full" {
		t.Fatalf("last_auto_index = %+v, want the failed pass", p.LastAutoIndex)
	}
}
