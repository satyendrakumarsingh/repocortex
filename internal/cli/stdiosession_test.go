package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type rpcErr struct {
	ID    json.RawMessage `json:"id"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
}

func repliesFor(t *testing.T, input string) (replies []rpcErr, passed string) {
	t.Helper()
	var out lockedBuf
	sess := newStdioSession(&out)
	got, err := io.ReadAll(newSessionFilter(strings.NewReader(input), sess, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var r rpcErr
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("reply is not JSON: %q", line)
		}
		replies = append(replies, r)
	}
	return replies, string(got)
}

func TestSessionAnswersWhatTheSDKWouldHangUpOn(t *testing.T) {
	t.Run("empty batch", func(t *testing.T) {
		r, passed := repliesFor(t, "[]\n")
		if len(r) != 1 || r[0].Error.Code != -32600 || string(r[0].ID) != "null" || strings.TrimSpace(passed) != "" {
			t.Errorf("replies %+v passed %q", r, passed)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		r, _ := repliesFor(t, "{not json\n")
		if len(r) != 1 || r[0].Error.Code != -32700 || string(r[0].ID) != "null" {
			t.Errorf("replies %+v", r)
		}
	})
	t.Run("no method", func(t *testing.T) {
		r, passed := repliesFor(t, `{"jsonrpc":"2.0","id":5}`+"\n")
		if len(r) != 1 || r[0].Error.Code != -32600 || string(r[0].ID) != "5" || strings.TrimSpace(passed) != "" {
			t.Errorf("replies %+v passed %q", r, passed)
		}
	})
	t.Run("valid frames pass through", func(t *testing.T) {
		// The notification has no id, so nothing waits for it.
		r, passed := repliesFor(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
		if len(r) != 0 || !strings.Contains(passed, "initialized") {
			t.Errorf("replies %+v passed %q", r, passed)
		}
	})
}

// End of input must not end the session while a request is unanswered.
func TestSessionEndOfInputWaitsForAnswers(t *testing.T) {
	var out lockedBuf
	sess := newStdioSession(&out)
	f := newSessionFilter(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), sess, nil)

	buf := make([]byte, 4096)
	if _, err := f.Read(buf); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, err := f.Read(buf) // end of input
		if !errors.Is(err, io.EOF) {
			t.Errorf("want EOF, got %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("end of input was reported with a request unanswered")
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := sess.Writer().Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("end of input was never reported after the answer")
	}
}
