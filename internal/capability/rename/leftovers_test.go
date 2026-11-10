package rename

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

var _ TextSearcher = (*registry.Registry)(nil)

type fakeSearch struct {
	paths  []string
	noText []string
	query  string
}

func (f *fakeSearch) FilesWithoutText(context.Context, int64) ([]model.File, error) {
	var out []model.File
	for _, p := range f.noText {
		out = append(out, model.File{Path: p})
	}
	return out, nil
}

func (f *fakeSearch) SearchFiles(_ context.Context, query string, limit int) ([]registry.FileMatch, error) {
	f.query = query
	var out []registry.FileMatch
	for _, p := range f.paths {
		if len(out) == limit {
			break
		}
		out = append(out, registry.FileMatch{Path: p})
	}
	return out, nil
}

func reader(files map[string]string) FileReader {
	return func(path string) ([]byte, error) {
		b, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(b), nil
	}
}

func TestFindLeftoversNoneIsAnEmptyScan(t *testing.T) {
	files := map[string]string{"a.go": "func InsertOrder() {}\n", "b.go": "x.InsertOrder()\n"}
	ts := &fakeSearch{paths: []string{"a.go", "b.go"}}
	got, err := FindLeftovers(context.Background(), ts, 1, reader(files), "CreateOrder", nil, LeftoverLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 0 || len(got.Items) != 0 || got.LowerBound || got.Unreadable != 0 {
		t.Errorf("scan = %+v, want nothing", got)
	}
	if ts.query != `"CreateOrder"` {
		t.Errorf("full-text query = %q, want the quoted name", ts.query)
	}
}

func TestFindLeftovers(t *testing.T) {
	files := map[string]string{
		"svc.go": "package app\n\nfunc f() { s.CreateOrder(1) }\n",
		"t.go": strings.Join([]string{
			"package app",
			"// CreateOrder is mocked here",          // 2: a comment still counts
			"verify(r).CreateOrder(\"x\")",           // 3
			"r.CreateOrderX(); r.preCreateOrder()",   // 4: substrings only
			"r.CreateOrder(r.CreateOrder(\x1b[31m))", // 5: two, one a site; escape sequence
		}, "\n") + "\n",
	}
	ts := &fakeSearch{paths: []string{"svc.go", "t.go", "gone.go"}}
	// Dry run: the sites still hold the old name and are excluded, one per
	// recorded site.
	sites := []Site{{Path: "svc.go", Line: 3}, {Path: "t.go", Line: 5}}
	got, err := FindLeftovers(context.Background(), ts, 1, reader(files), "CreateOrder", sites, LeftoverLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range got.Items {
		lines = append(lines, fmt.Sprintf("%s:%d", l.Path, l.Line))
	}
	if fmt.Sprint(lines) != "[t.go:2 t.go:3 t.go:5]" || got.Total != 3 {
		t.Fatalf("leftovers = %v (total %d), want t.go:2, :3, :5", lines, got.Total)
	}
	if got.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 (gone.go)", got.Unreadable)
	}
	if txt := got.Items[2].Text; strings.ContainsRune(txt, '\x1b') || !strings.Contains(txt, `\u001b`) {
		t.Errorf("line text not escaped for a terminal: %q", txt)
	}
}

func TestFindLeftoversIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("Old()\n")
	}
	files := map[string]string{"a.go": b.String(), "b.go": "Old\n", "c.go": "Old\n"}
	ts := &fakeSearch{paths: []string{"a.go", "b.go", "c.go"}}
	got, err := FindLeftovers(context.Background(), ts, 1, reader(files), "Old", nil,
		LeftoverLimits{MaxItems: 4, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 4 || got.Total != 11 || !got.LowerBound {
		t.Errorf("items=%d total=%d lowerBound=%v, want 4, 11, true", len(got.Items), got.Total, got.LowerBound)
	}
}

func TestFindLeftoversPropagatesSearchErrors(t *testing.T) {
	_, err := FindLeftovers(context.Background(), errSearch{}, 1, reader(nil), "Old", nil, LeftoverLimits{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

type errSearch struct{}

func (errSearch) SearchFiles(context.Context, string, int) ([]registry.FileMatch, error) {
	return nil, errors.New("boom")
}

func (errSearch) FilesWithoutText(context.Context, int64) ([]model.File, error) { return nil, nil }

// TestFindLeftoversReadsFilesTheSearchCannotSee: a file recorded with no
// full-text row (too large, binary, redacted) is read directly; a credential
// file is never read, only counted.
func TestFindLeftoversReadsFilesTheSearchCannotSee(t *testing.T) {
	files := map[string]string{
		"gen/Big.java": "class Big { void f() { s.CreateOrder(); } }\n",
		".env":         "CreateOrder=1\n",
	}
	ts := &fakeSearch{noText: []string{"gen/Big.java", ".env"}}
	got, err := FindLeftovers(context.Background(), ts, 1, reader(files), "CreateOrder", nil, LeftoverLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || len(got.Items) != 1 || got.Items[0].Path != "gen/Big.java" {
		t.Errorf("items = %+v, want gen/Big.java:1 only", got.Items)
	}
	if got.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 (the credential file, never read)", got.Unreadable)
	}
}

// TestFindLeftoversCannotSearchANameWithoutWordCharacters: `$` and `_` are
// identifiers, but neither the full-text index nor a reader can tell their
// uses apart from punctuation; the scan says so instead of reporting clear.
func TestFindLeftoversCannotSearchANameWithoutWordCharacters(t *testing.T) {
	for _, name := range []string{"$", "_", "$_"} {
		got, err := FindLeftovers(context.Background(), errSearch{}, 1, reader(nil), name, nil, LeftoverLimits{})
		if err != nil || !got.Unsearchable || got.Clear() {
			t.Errorf("%q: scan = %+v, err %v; want unsearchable and not clear", name, got, err)
		}
	}
	if !(LeftoverScan{}).Clear() {
		t.Error("an empty scan must be clear")
	}
}

func TestTerminalSafe(t *testing.T) {
	if got := TerminalSafe("a\x1b[2Jb\u202e.go"); got != `a\u001b[2Jb\u202e.go` {
		t.Errorf("TerminalSafe = %q", got)
	}
}
