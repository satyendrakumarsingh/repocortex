package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

func TestLabelsUseTheSharedPathPrefixWhenThereIsOne(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{
			{Key: "internal/auth/login.go:Login"},
			{Key: "internal/auth/token.go:Mint"},
		},
		nil,
	)
	as := []Assignment{
		{Key: "internal/auth/login.go:Login", ClusterID: "internal/auth/login.go:Login"},
		{Key: "internal/auth/token.go:Mint", ClusterID: "internal/auth/login.go:Login"},
	}
	got, err := Labels(context.Background(), g, as, nil)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if !strings.Contains(got[0].Name, "internal/auth") {
		t.Errorf("Name = %q, want it to mention internal/auth", got[0].Name)
	}
	if got[0].Method != "path-prefix" {
		t.Errorf("Method = %q, want \"path-prefix\"", got[0].Method)
	}
	if got[0].Members != 2 {
		t.Errorf("Members = %d, want 2", got[0].Members)
	}
}

func TestLabelsFallBackToTheHighestRankedMember(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{{Key: "aaa/x.go:One"}, {Key: "zzz/y.go:Two"}},
		nil,
	)
	as := []Assignment{
		{Key: "aaa/x.go:One", ClusterID: "aaa/x.go:One"},
		{Key: "zzz/y.go:Two", ClusterID: "aaa/x.go:One"},
	}
	ranks := []metrics.Score{{Key: "zzz/y.go:Two", Value: 0.9}, {Key: "aaa/x.go:One", Value: 0.1}}
	got, err := Labels(context.Background(), g, as, ranks)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if got[0].Method != "top-ranked-member" {
		t.Errorf("Method = %q, want \"top-ranked-member\"", got[0].Method)
	}
	if !strings.Contains(got[0].Name, "Two") {
		t.Errorf("Name = %q, want it to name the highest-ranked member", got[0].Name)
	}
}

// Regression: a single-member cluster whose key has neither a ":" nor a "/"
// (e.g. a malformed key from an upstream parser bug) used to fall through
// sharedDirPrefix's split unstripped, so the whole raw key came back as a
// bogus one-segment "directory" and — because a single-member cluster never
// enters the keys[1:] disagreement loop — nothing ever falsified it. That let
// a guess-quality name be reported with Method "path-prefix", which claims a
// directory fact that does not exist. It must fall back to
// "top-ranked-member" instead.
func TestLabelsDoNotTreatABareKeyAsADirectory(t *testing.T) {
	g := metrics.NewGraph([]metrics.Node{{Key: "foo"}}, nil)
	as := []Assignment{{Key: "foo", ClusterID: "foo"}}
	got, err := Labels(context.Background(), g, as, nil)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Method == "path-prefix" {
		t.Errorf("Method = %q, want anything but \"path-prefix\" for a key with no directory", got[0].Method)
	}
	if got[0].Method != "top-ranked-member" {
		t.Errorf("Method = %q, want \"top-ranked-member\"", got[0].Method)
	}
}

// Regression: members whose keys share no directory component at all (one
// rooted under internal/, the other under cmd/, with nothing in common at
// any segment) must not be labelled "path-prefix" — sharedDirPrefix's
// disagreement loop should walk down to zero common segments and return "".
func TestLabelsWithNoSharedDirectoryFallBack(t *testing.T) {
	g := metrics.NewGraph(
		[]metrics.Node{
			{Key: "internal/auth/login.go:Login"},
			{Key: "cmd/api/handler.go:Get"},
		},
		nil,
	)
	as := []Assignment{
		{Key: "cmd/api/handler.go:Get", ClusterID: "cmd/api/handler.go:Get"},
		{Key: "internal/auth/login.go:Login", ClusterID: "cmd/api/handler.go:Get"},
	}
	got, err := Labels(context.Background(), g, as, nil)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Method != "top-ranked-member" {
		t.Errorf("Method = %q, want \"top-ranked-member\" (internal/auth and cmd/api share no directory segment at all)", got[0].Method)
	}
}

// Regression: two members whose keys have no "/" at all but happen to share
// a filename stem (e.g. "foo:A" and "foo:B") used to have that stem returned
// as their "shared directory prefix". "foo" is a filename, not a directory —
// there is no "/" in either key, so split had nothing to strip and returned
// the stem unstripped, same root cause as the single-member case above.
// Decision: this case is disqualified from "path-prefix" too, for the same
// reason a bare key is — see the doc comment on sharedDirPrefix. It falls
// back to "top-ranked-member" rather than getting an ad hoc third method
// name; there being only two members with ranks left equal (nil ranks), the
// deterministic tie-break picks the lexicographically smaller key.
func TestLabelsDoNotTreatASharedFilenameStemAsADirectory(t *testing.T) {
	g := metrics.NewGraph([]metrics.Node{{Key: "foo:A"}, {Key: "foo:B"}}, nil)
	as := []Assignment{
		{Key: "foo:A", ClusterID: "foo:A"},
		{Key: "foo:B", ClusterID: "foo:A"},
	}
	got, err := Labels(context.Background(), g, as, nil)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Method != "top-ranked-member" {
		t.Errorf("Method = %q, want \"top-ranked-member\" (\"foo\" is a filename stem, not a directory)", got[0].Method)
	}
	if got[0].Name != "foo:A" {
		t.Errorf("Name = %q, want \"foo:A\" (lexicographically smallest key, deterministic tie-break with no ranks)", got[0].Name)
	}
}
