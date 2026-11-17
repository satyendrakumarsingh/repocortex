// Package autorun records the outcome of the last automatic re-index: the
// background pass a git hook starts with `repocortex auto-index`.
//
// A hook's run has no terminal. Without this record a failed pass was visible
// only in .repocortex/logs/auto-index.log, which nobody reads, so `status` and
// an agent's provenance kept describing an index that had silently stopped
// updating. The record is one small file beside the index, written by the
// pass and read by `status` and the MCP server.
package autorun

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FileName is the record's name inside the .repocortex directory.
const FileName = "reindex.status"

// Run is the outcome of one automatic pass.
type Run struct {
	// At is when the pass started, RFC3339.
	At string `json:"at"`
	// DurationMS is how long it took.
	DurationMS int64 `json:"duration_ms"`
	// OK is true when both the index and the resolve step succeeded.
	OK bool `json:"ok"`
	// Error is the failure, empty when OK.
	Error string `json:"error,omitempty"`
}

// Write records a pass. Best effort by design: the pass already happened, and
// failing to note it must not fail the hook.
func Write(metaDir string, started time.Time, took time.Duration, failure error) error {
	status := "ok"
	if failure != nil {
		status = strings.ReplaceAll(failure.Error(), "\n", " ")
	}
	body := fmt.Sprintf("timestamp=%s\nduration_ms=%d\nstatus=%s\n",
		started.Format(time.RFC3339), took.Milliseconds(), status)
	return os.WriteFile(filepath.Join(metaDir, FileName), []byte(body), 0o600)
}

// Read returns the last recorded pass, or nil when none was ever recorded or
// the record cannot be read.
func Read(metaDir string) *Run {
	b, err := os.ReadFile(filepath.Clean(filepath.Join(metaDir, FileName)))
	if err != nil {
		return nil
	}
	var r Run
	var seen bool
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "timestamp":
			r.At, seen = v, true
		case "duration_ms":
			r.DurationMS, _ = strconv.ParseInt(v, 10, 64)
		case "status":
			if v == "ok" {
				r.OK = true
			} else {
				r.Error = v
			}
		}
	}
	if !seen {
		return nil
	}
	return &r
}
