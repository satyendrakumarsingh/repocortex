package cli

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/web"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"

	"github.com/spf13/cobra"
)

// Refresh is the one thing `repocortex ui` can do that changes the repository's
// index, so the tests here are mostly about it NOT happening: the flag is off,
// the route is absent, the header is required, a cancelled request stops.

func TestUIAllowRefreshFlagIsOffAndSaysWhy(t *testing.T) {
	t.Parallel()
	flag := newUICmd(&Global{}).Flags().Lookup("allow-refresh")
	if flag == nil {
		t.Fatal("ui has no --allow-refresh flag")
	}
	// The default IS the security decision. A portal that could rebuild an
	// index without being asked to would make "read-only" a claim rather than a
	// property.
	if flag.DefValue != "false" {
		t.Errorf("--allow-refresh defaults to %q, want false", flag.DefValue)
	}
	// The help has to answer the three questions a reader has before typing it:
	// what it permits, what the portal is without it, and what still refuses it.
	for _, want := range []string{"rebuild the index", "read-only", "loopback"} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("--allow-refresh help does not mention %q: %s", want, flag.Usage)
		}
	}
}

func TestUIPortalWritesOnlyWhenRefreshIsAllowed(t *testing.T) {
	t.Parallel()

	okRuns := fakeRuns{
		run:    &model.IndexRun{ID: 42, GitRevision: "cafef00d"},
		counts: registry.Counts{Files: 7, Symbols: 91, Edges: 13},
	}

	cases := []struct {
		name         string
		allowRefresh bool
		sendHeader   bool
		origin       string
		wantStatus   int
		wantCalls    int
	}{{
		// The read-only guarantee, asserted where it is actually enforced: no
		// Reindexer means no route, so the method allow-list refuses the POST
		// before any handler could run.
		name:       "without the flag there is no write route at all",
		wantStatus: http.StatusMethodNotAllowed,
	}, {
		name:         "with the flag a same-origin POST rebuilds",
		allowRefresh: true,
		sendHeader:   true,
		wantStatus:   http.StatusOK,
		wantCalls:    2,
	}, {
		// A cross-site <form> can POST but cannot set a header, which is the
		// whole reason the header is the gate.
		name:         "a POST without the header is refused",
		allowRefresh: true,
		wantStatus:   http.StatusForbidden,
	}, {
		name:         "a POST from another origin is refused even with the header",
		allowRefresh: true,
		sendHeader:   true,
		origin:       "https://evil.example",
		wantStatus:   http.StatusForbidden,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			seedIndex(t, dir)
			g := newTestGlobal(t, dir)

			// Atomic because the stages run on the server's goroutine while
			// the assertion reads on the test's, with only a socket between
			// them — exactly the edge -race is there to catch.
			var calls atomic.Int64
			step := func(stdctx.Context) error { calls.Add(1); return nil }

			var extra []web.Option
			if tc.allowRefresh {
				// The real adapter, with only its two pipeline stages stubbed:
				// running tree-sitter and SQLite here would test those, not the
				// wiring this file is about.
				extra = append(extra, web.WithReindexer(
					newUIReindexer(g, okRuns, withRefreshSteps(step, step))))
			}

			srv, closePortal, err := newPortal(stdctx.Background(), g, uiOptions{
				Host: defaultUIHost, MaxNodes: web.DefaultMaxNodes, AllowRefresh: tc.allowRefresh,
			}, extra...)
			if err != nil {
				t.Fatalf("newPortal: %v", err)
			}
			defer closePortal()

			// httptest binds 127.0.0.1, so the Host header the portal's guard
			// reads is a loopback address without the test forging one.
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/reindex", nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if tc.sendHeader {
				req.Header.Set(web.ReindexHeader, "1")
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			res, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("POST /api/reindex: %v", err)
			}
			defer func() { _ = res.Body.Close() }()
			body, _ := io.ReadAll(res.Body)

			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", res.StatusCode, tc.wantStatus, body)
			}
			if got := int(calls.Load()); got != tc.wantCalls {
				t.Errorf("pipeline ran %d stage(s), want %d", got, tc.wantCalls)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var got struct {
				Status string            `json:"status"`
				Result web.ReindexResult `json:"result"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			if got.Status != "ok" {
				t.Errorf("status = %q", got.Status)
			}
			// The reported numbers must come from the run the refresh
			// produced. A refresh that answered with zeroes would look
			// identical to one that indexed an empty repository.
			if got.Result.Revision != 42 || got.Result.GitRevision != "cafef00d" ||
				got.Result.Files != 7 || got.Result.Symbols != 91 || got.Result.Edges != 13 {
				t.Errorf("result = %+v, want the seeded run's counts", got.Result)
			}
		})
	}
}

func TestUIPortalReportsCommitHookState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedIndex(t, dir)
	g := newTestGlobal(t, dir)

	srv, closePortal, err := newPortal(stdctx.Background(), g, uiOptions{
		Host: defaultUIHost, MaxNodes: web.DefaultMaxNodes,
	})
	if err != nil {
		t.Fatalf("newPortal: %v", err)
	}
	defer closePortal()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var state struct {
		CommitHook map[string]any `json:"commit_hook"`
		Refresh    map[string]any `json:"refresh"`
	}
	getJSON(t, ts.URL+"/api/indexstate", &state)

	// "not installed" is a fact the reporter established. Without the reporter
	// wired the portal answers "not checked", which is a different claim and
	// the one this test exists to keep from creeping back.
	if _, ok := state.CommitHook["installed"]; !ok {
		t.Errorf("commit_hook has no installed field: %v", state.CommitHook)
	}
	if detail, _ := state.CommitHook["detail"].(string); strings.Contains(detail, "Not checked") {
		t.Errorf("the portal was built without a hook reporter: %v", state.CommitHook)
	}
	if state.Refresh["enabled"] != false {
		t.Errorf("refresh reported enabled=%v on a portal built without --allow-refresh", state.Refresh["enabled"])
	}
}

func TestUIReindexerStagesAndCancellation(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	okRuns := fakeRuns{
		run:    &model.IndexRun{ID: 9, GitRevision: "abc123"},
		counts: registry.Counts{Files: 3, Symbols: 40, Edges: 5},
	}
	boom := errors.New("parser exploded")

	cases := []struct {
		name string
		// cancelAt: "" none, "before" the first stage, "during-index" while it runs.
		cancelAt  string
		indexErr  error
		runs      runReader
		wantErr   bool
		wantIs    error
		wantIndex bool // did the index stage run?
		wantResol bool // did the resolve stage run?
		wantMsg   string
	}{{
		name:      "a healthy refresh runs both stages and reports the run",
		runs:      okRuns,
		wantIndex: true, wantResol: true,
	}, {
		// The portal bounds a refresh with a timeout. A request already
		// abandoned must not start an indexer nobody is waiting on.
		name:     "an already-cancelled request never starts the indexer",
		cancelAt: "before",
		runs:     okRuns,
		wantErr:  true, wantIs: stdctx.Canceled,
		wantMsg: "not started",
	}, {
		// Indexing publishes atomically, so stopping here leaves a complete
		// index with stale edges — a state worth naming rather than reporting
		// as success.
		name:     "cancellation between the stages stops before resolving",
		cancelAt: "during-index",
		runs:     okRuns,
		wantErr:  true, wantIs: stdctx.Canceled,
		wantIndex: true,
		wantMsg:   "before resolution",
	}, {
		name:     "a failing index stage is wrapped, not swallowed",
		indexErr: boom,
		runs:     okRuns,
		wantErr:  true, wantIs: boom,
		wantIndex: true,
		wantMsg:   "repocortex index",
	}, {
		name:    "an unreadable run is reported rather than answered with zeroes",
		runs:    fakeRuns{err: registry.ErrNoCommittedRun},
		wantErr: true, wantIs: registry.ErrNoCommittedRun,
		wantIndex: true, wantResol: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := stdctx.WithCancel(stdctx.Background())
			defer cancel()
			if tc.cancelAt == "before" {
				cancel()
			}

			var ranIndex, ranResolve bool
			index := func(stdctx.Context) error {
				ranIndex = true
				if tc.cancelAt == "during-index" {
					cancel()
				}
				return tc.indexErr
			}
			resolve := func(stdctx.Context) error { ranResolve = true; return nil }

			// Two ticks, 1500ms apart: the adapter reads the clock once at the
			// start and once at the end.
			ticks, i := []time.Time{base, base.Add(1500 * time.Millisecond)}, 0
			clock := func() time.Time {
				if i >= len(ticks) {
					i = len(ticks) - 1
				}
				now := ticks[i]
				i++
				return now
			}

			r := newUIReindexer(&Global{}, tc.runs,
				withRefreshSteps(index, resolve), withRefreshClock(clock))
			res, err := r.Reindex(ctx)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Reindex succeeded, want an error; result %+v", res)
				}
				if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
					t.Errorf("error %v does not wrap %v — %%w was dropped somewhere", err, tc.wantIs)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("error %q does not say %q", err, tc.wantMsg)
				}
			} else if err != nil {
				t.Fatalf("Reindex: %v", err)
			}
			if ranIndex != tc.wantIndex {
				t.Errorf("index stage ran = %v, want %v", ranIndex, tc.wantIndex)
			}
			if ranResolve != tc.wantResol {
				t.Errorf("resolve stage ran = %v, want %v", ranResolve, tc.wantResol)
			}
			if tc.wantErr {
				return
			}
			want := web.ReindexResult{
				Revision: 9, GitRevision: "abc123",
				Files: 3, Symbols: 40, Edges: 5, DurationMS: 1500,
			}
			if res != want {
				t.Errorf("result = %+v, want %+v", res, want)
			}
		})
	}
}

// ---------------------------------------------------------------- helpers

// newTestGlobal resolves configuration the way the root command would, which is
// what `ui` and everything it builds read.
func newTestGlobal(t *testing.T, project string) *Global {
	t.Helper()
	g := &Global{Project: project}
	if err := g.setup(&cobra.Command{Use: "repocortex-test"}); err != nil {
		t.Fatalf("resolve project %s: %v", project, err)
	}
	return g
}

// fakeRuns stands in for the portal's read-only registry. Two methods is the
// whole port, which is the test this design was supposed to make cheap.
type fakeRuns struct {
	run    *model.IndexRun
	counts registry.Counts
	err    error
}

func (f fakeRuns) CurrentRun(stdctx.Context) (*model.IndexRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.run, nil
}

func (f fakeRuns) Counts(_ stdctx.Context, _ int64) (registry.Counts, error) {
	if f.err != nil {
		return registry.Counts{}, f.err
	}
	return f.counts, nil
}
