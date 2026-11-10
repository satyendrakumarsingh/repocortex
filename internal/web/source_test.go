package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSource serves a fixed text per path and records every path it was asked
// for, so a test can prove the handler never asks for a path the index lacks.
type fakeSource struct {
	mu    sync.Mutex
	texts map[string]string
	err   error
	asked []string
}

func (f *fakeSource) FileExcerpt(_ context.Context, _ int64, path string, startLine, lines int) (string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, path)
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	text, ok := f.texts[path]
	if !ok {
		return "", fmt.Errorf("fake: excerpt for %s: %w", path, sql.ErrNoRows)
	}
	all := strings.Split(text, "\n")
	start := startLine - 1
	if start >= len(all) {
		return "", nil
	}
	end := len(all)
	if lines > 0 && start+lines < end {
		end = start + lines
	}
	return strings.Join(all[start:end], "\n"), nil
}

func numberedLines(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %d", i+1)
	}
	return strings.Join(out, "\n")
}

func TestWebSourceServesIndexedTextWithDeclarations(t *testing.T) {
	t.Parallel()

	const hub = "internal/core/hub.go"
	cases := []struct {
		name        string
		query       string
		src         *fakeSource
		wantLines   int
		wantFrom    int
		wantMore    bool
		wantNext    int
		wantHelp    string // substring of the guidance title; "" means no guidance
		wantCallers int    // callers of `hub`, or -1 to skip
		wantAsked   bool   // whether the reader may be consulted at all
	}{
		{
			name: "whole file at the default floor", query: "?path=" + hub,
			src:       &fakeSource{texts: map[string]string{hub: numberedLines(12)}},
			wantLines: 12, wantFrom: 1, wantCallers: 4, wantAsked: true,
		},
		{
			name: "a lower floor counts the guess too", query: "?path=" + hub + "&min_confidence=low",
			src:       &fakeSource{texts: map[string]string{hub: numberedLines(12)}},
			wantLines: 12, wantFrom: 1, wantCallers: 5, wantAsked: true,
		},
		{
			name: "a window starting mid-file", query: "?path=" + hub + "&from=5",
			src:       &fakeSource{texts: map[string]string{hub: numberedLines(12)}},
			wantLines: 8, wantFrom: 5, wantCallers: -1, wantAsked: true,
		},
		{
			name: "a long file is cut and the remainder counted", query: "?path=" + hub,
			src:       &fakeSource{texts: map[string]string{hub: numberedLines(sourceMaxLines + 7)}},
			wantLines: sourceMaxLines, wantFrom: 1, wantMore: true, wantNext: sourceMaxLines + 1,
			wantCallers: -1, wantAsked: true,
		},
		{
			name: "no path asks for one", query: "",
			src:      &fakeSource{},
			wantHelp: "Name a file", wantCallers: -1,
		},
		{
			name: "a path the index does not hold is never read", query: "?path=../../etc/passwd",
			src:      &fakeSource{texts: map[string]string{"../../etc/passwd": "root:x:0:0"}},
			wantHelp: "is not a file in this index", wantCallers: -1,
		},
		{
			name: "an indexed file without stored text says so", query: "?path=" + hub,
			src:      &fakeSource{},
			wantHelp: "symbols but not its text", wantCallers: -1, wantAsked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _, _ := fixture(t, WithSourceReader(tc.src))

			var got SourceResponse
			rec := get(t, srv, "/api/source"+tc.query, &got)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
			}
			if tc.wantHelp != "" {
				if got.Help == nil || !strings.Contains(got.Help.Title, tc.wantHelp) {
					t.Fatalf("guidance = %+v, want a title containing %q", got.Help, tc.wantHelp)
				}
				if len(got.Lines) != 0 {
					t.Errorf("guidance response carried %d lines", len(got.Lines))
				}
			} else if got.Help != nil {
				t.Fatalf("unexpected guidance: %+v", got.Help)
			}
			if asked := len(tc.src.asked) > 0; asked != tc.wantAsked {
				t.Errorf("reader consulted = %v (paths %v), want %v", asked, tc.src.asked, tc.wantAsked)
			}
			if len(got.Lines) != tc.wantLines {
				t.Errorf("lines = %d, want %d", len(got.Lines), tc.wantLines)
			}
			if tc.wantLines > 0 {
				if got.From != tc.wantFrom || got.To != tc.wantFrom+tc.wantLines-1 {
					t.Errorf("window = %d..%d, want %d..%d", got.From, got.To, tc.wantFrom, tc.wantFrom+tc.wantLines-1)
				}
				if got.Lines[0] != fmt.Sprintf("line %d", tc.wantFrom) {
					t.Errorf("first line = %q, want line %d", got.Lines[0], tc.wantFrom)
				}
			}
			if got.HasMore != tc.wantMore || got.NextFrom != tc.wantNext {
				t.Errorf("has_more/next_from = %v/%d, want %v/%d", got.HasMore, got.NextFrom, tc.wantMore, tc.wantNext)
			}
			if tc.wantMore {
				if n := got.Omissions.totalFor(ReasonRowBudget); n != 7 {
					t.Errorf("row_budget omission counts %d lines, want 7", n)
				}
			}
			if tc.wantCallers >= 0 {
				var found bool
				for _, d := range got.Declarations {
					if d.Name == "hub" {
						found = true
						if d.Callers != tc.wantCallers {
							t.Errorf("hub callers = %d at %s, want %d", d.Callers, got.Floor, tc.wantCallers)
						}
					}
				}
				if !found {
					t.Errorf("hub is not among the declarations: %+v", got.Declarations)
				}
			}
		})
	}
}

func TestWebSourceIsReadOnly(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t, WithSourceReader(&fakeSource{}))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/source?path=internal/core/hub.go", nil)
		req.Host = "127.0.0.1:7777"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/source = %d, want 405", method, rec.Code)
		}
	}
}
