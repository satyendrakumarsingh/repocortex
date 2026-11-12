package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubReindexer records calls and returns what the test asks for.
type stubReindexer struct {
	mu     sync.Mutex
	calls  int
	block  chan struct{} // when non-nil, Reindex waits on it
	result ReindexResult
	err    error
}

func (s *stubReindexer) Reindex(ctx context.Context) (ReindexResult, error) {
	s.mu.Lock()
	s.calls++
	block := s.block
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ReindexResult{}, ctx.Err()
		}
	}
	return s.result, s.err
}

func (s *stubReindexer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func reindexRequest(t *testing.T, mutate func(*http.Request)) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/reindex", nil)
	r.Host = "127.0.0.1:7777"
	r.Header.Set(ReindexHeader, "1")
	if mutate != nil {
		mutate(r)
	}
	return r
}

// TestRefreshIsAbsentUnlessEnabled is the load-bearing test for this feature.
// The portal's promise is that a browser pointed at localhost cannot make
// RepoCortex do anything; that promise holds only while the route does not exist.
func TestRefreshIsAbsentUnlessEnabled(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, reindexRequest(t, nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d — a portal without --allow-refresh must refuse POST at the protocol level",
			rec.Code, http.StatusMethodNotAllowed)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

func TestRefreshRejectsCrossSiteRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
		reason string
	}{
		{
			name:   "a cross-site form POST has no custom header",
			mutate: func(r *http.Request) { r.Header.Del(ReindexHeader) },
			want:   http.StatusForbidden,
			reason: "a <form> on another site can POST but cannot set headers",
		},
		{
			name:   "an Origin from another site is refused even with the header",
			mutate: func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
			want:   http.StatusForbidden,
			reason: "belt and braces for a browser that stops preflighting",
		},
		{
			name:   "the browser reporting cross-site is refused",
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
			want:   http.StatusForbidden,
			reason: "Sec-Fetch-Site is the browser's own account of provenance",
		},
		{
			name:   "a non-loopback Host is refused before any of this",
			mutate: func(r *http.Request) { r.Host = "repocortex.example.com" },
			want:   http.StatusForbidden,
			reason: "DNS rebinding reaches a loopback socket with a public Host",
		},
		{
			name:   "same-origin with the header is allowed",
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") },
			want:   http.StatusOK,
			reason: "the portal's own page must still work",
		},
		{
			name:   "a loopback Origin is allowed",
			mutate: func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:7777") },
			want:   http.StatusOK,
			reason: "this is what the portal's own fetch sends",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubReindexer{}
			srv, _, _ := fixture(t, WithReindexer(stub))

			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, reindexRequest(t, tc.mutate))

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)\nbody: %s", rec.Code, tc.want, tc.reason, rec.Body.String())
			}
			// A refused request must not have started an indexer.
			if tc.want != http.StatusOK && stub.count() != 0 {
				t.Errorf("a refused request still called Reindex %d time(s)", stub.count())
			}
		})
	}
}

func TestRefreshSerialisesConcurrentRequests(t *testing.T) {
	t.Parallel()
	stub := &stubReindexer{block: make(chan struct{})}
	srv, _, _ := fixture(t, WithReindexer(stub))
	h := srv.Handler()

	// First request parks inside Reindex.
	started := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		close(started)
		h.ServeHTTP(rec, reindexRequest(t, nil))
		done <- rec.Code
	}()
	<-started

	// Wait for the gate to actually be claimed, without sleeping blindly.
	deadline := time.After(2 * time.Second)
	for {
		if running, _ := srv.reindexes.snapshot(); running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the first refresh never claimed the gate")
		default:
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, reindexRequest(t, nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("second concurrent refresh = %d, want %d — two indexers must not write one SQLite file",
			rec.Code, http.StatusConflict)
	}

	close(stub.block)
	if code := <-done; code != http.StatusOK {
		t.Errorf("first refresh = %d, want 200", code)
	}
	if stub.count() != 1 {
		t.Errorf("Reindex called %d times, want 1", stub.count())
	}
}

func TestRefreshReportsFailureWithoutClaimingSuccess(t *testing.T) {
	t.Parallel()
	stub := &stubReindexer{err: errors.New("parser exploded")}
	srv, _, _ := fixture(t, WithReindexer(stub))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, reindexRequest(t, nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "parser exploded") {
		t.Errorf("error = %q, want the underlying cause named", msg)
	}
	if !strings.Contains(msg, "left as it was") {
		t.Errorf("error = %q, want it to say the index was not damaged", msg)
	}
}

func TestIndexStateDescribesRefreshAndHook(t *testing.T) {
	t.Parallel()

	t.Run("disabled portal explains how to enable it", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := fixture(t)
		body := getIndexState(t, srv)

		refresh, _ := body["refresh"].(map[string]any)
		if refresh["enabled"] != false {
			t.Fatalf("refresh.enabled = %v, want false", refresh["enabled"])
		}
		cmd, _ := refresh["command"].(string)
		if !strings.Contains(cmd, "repocortex index") {
			t.Errorf("a portal that cannot refresh must name the command that can; got %q", cmd)
		}
	})

	t.Run("enabled portal names the header the page must send", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := fixture(t, WithReindexer(&stubReindexer{}))
		body := getIndexState(t, srv)

		refresh, _ := body["refresh"].(map[string]any)
		if refresh["enabled"] != true {
			t.Fatalf("refresh.enabled = %v, want true", refresh["enabled"])
		}
		if refresh["header"] != ReindexHeader {
			t.Errorf("refresh.header = %v, want %q", refresh["header"], ReindexHeader)
		}
	})

	t.Run("hook state is reported when a reporter is wired", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := fixture(t, WithHookReporter(HookReporterFunc(func(string) any {
			return map[string]any{"installed": true, "path": "/repo/.git/hooks/post-commit"}
		})))
		body := getIndexState(t, srv)

		hook, _ := body["commit_hook"].(map[string]any)
		if hook["installed"] != true {
			t.Errorf("commit_hook.installed = %v, want true", hook["installed"])
		}
	})

	t.Run("an unwired reporter reports unknown, never a confident false", func(t *testing.T) {
		t.Parallel()
		srv, _, _ := fixture(t)
		body := getIndexState(t, srv)
		hook, _ := body["commit_hook"].(map[string]any)
		if hook["checked"] != false {
			t.Errorf("commit_hook.checked = %v, want false — unknown must not render as \"not installed\"", hook["checked"])
		}
		detail, _ := hook["detail"].(string)
		if !strings.Contains(detail, "Not checked") {
			t.Errorf("detail = %q, want it to say the state was not checked", detail)
		}
	})
}

func getIndexState(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/indexstate", nil)
	r.Host = "127.0.0.1:7777"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestOtherWritesStayRefusedWhenRefreshIsEnabled(t *testing.T) {
	t.Parallel()
	srv, _, _ := fixture(t, WithReindexer(&stubReindexer{}))

	// Enabling refresh must not open the portal to writes generally.
	for _, target := range []string{"/api/overview", "/api/graph", "/", "/api/reindex/../overview"} {
		r := httptest.NewRequest(http.MethodPost, target, nil)
		r.Host = "127.0.0.1:7777"
		r.Header.Set(ReindexHeader, "1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want %d — only /api/reindex may be written to",
				target, rec.Code, http.StatusMethodNotAllowed)
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := httptest.NewRequest(method, "/api/reindex", nil)
		r.Host = "127.0.0.1:7777"
		r.Header.Set(ReindexHeader, "1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/reindex = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// TestReadOnlyClaimTracksReality guards a claim a reader may act on. A portal
// that can rebuild the index is not read-only, and saying so anyway is the kind
// of small dishonesty this product exists to avoid.
func TestReadOnlyClaimTracksReality(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []Option
		want bool
	}{
		{name: "a portal with no reindexer is read-only", opts: nil, want: true},
		{
			name: "a portal that can refresh is not",
			opts: []Option{WithReindexer(&stubReindexer{})},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _, _ := fixture(t, tc.opts...)
			var body map[string]any
			get(t, srv, "/api/overview", &body)
			meta, _ := body["meta"].(map[string]any)
			if meta["read_only"] != tc.want {
				t.Errorf("meta.read_only = %v, want %v", meta["read_only"], tc.want)
			}
		})
	}
}

// TestHealthEndpointReadOnlyClaimTracksReality covers the same claim on the one
// endpoint an operator is most likely to curl, which builds its payload as a map
// literal and so does not inherit Meta's field.
func TestHealthEndpointReadOnlyClaimTracksReality(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts []Option
		want bool
	}{
		{name: "default", opts: nil, want: true},
		{name: "refresh enabled", opts: []Option{WithReindexer(&stubReindexer{})}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, _, _ := fixture(t, tc.opts...)
			var body map[string]any
			get(t, srv, "/api/health", &body)
			if body["read_only"] != tc.want {
				t.Errorf("/api/health read_only = %v, want %v", body["read_only"], tc.want)
			}
		})
	}
}
