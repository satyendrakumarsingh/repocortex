package indexer

import (
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Action classifies an audited subprocess.
type Action string

const (
	// ActionProbe is a version query. Read-only, and the only action Plan is
	// allowed to produce.
	ActionProbe Action = "probe"

	// ActionProvision downloaded or installed software.
	ActionProvision Action = "provision"

	// ActionIndex ran an indexer, which for Java means running the
	// repository's build system.
	ActionIndex Action = "index"
)

// AuditRecord is one thing RepoCortex caused to happen outside its own process.
//
// The requirement this satisfies is simple to state and easy to get wrong: a
// user must be able to answer "what did this tool run on my machine, and what
// did it install" after the fact, without reading RepoCortex's source. So argv is
// recorded verbatim — including absolute paths, because an audit trail that
// elides which binary ran is not one.
//
// What is deliberately NOT recorded: the child environment. It is inherited
// from the user's shell and routinely holds NPM_TOKEN, GITHUB_TOKEN and cloud
// credentials, and an audit log that leaks those would be a worse problem than
// the one it solves. Subprocess output is not recorded either; it can contain
// source code. Result.Diagnostic exists for the failure path, where a bounded
// excerpt is worth its risk and goes to the user, not to a log file.
type AuditRecord struct {
	Action Action
	At     time.Time

	// Tool, Indexer and Language are set where they apply, so records can be
	// filtered without parsing argv.
	Tool     Tool
	Indexer  Indexer
	Language Language

	// Result carries argv, exit code and duration. Its captured output is
	// cleared before the record is stored; see record.
	Result Result

	// Err is the failure this attempt produced, rendered. Empty on success.
	Err string
}

// AuditSink receives audit records. Implementations must be safe for concurrent
// use and must not block: they sit on the path of every subprocess.
type AuditSink interface {
	Record(AuditRecord)
}

// MemoryAudit collects records in memory. Used by `repocortex indexers list` to
// show what a run did, and by tests to assert what it did not do.
type MemoryAudit struct {
	mu      sync.Mutex
	records []AuditRecord
}

// Record implements AuditSink.
func (m *MemoryAudit) Record(r AuditRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, r)
}

// Records returns a copy of everything recorded so far.
func (m *MemoryAudit) Records() []AuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditRecord(nil), m.records...)
}

// LogAudit writes records through a slog.Logger.
type LogAudit struct{ Logger *slog.Logger }

// Record implements AuditSink.
func (l LogAudit) Record(r AuditRecord) {
	if l.Logger == nil {
		return
	}
	attrs := []any{
		"action", string(r.Action),
		"argv", strings.Join(r.Result.Argv, " "),
		"exit_code", r.Result.ExitCode,
		"duration_ms", r.Result.Duration.Milliseconds(),
	}
	if r.Indexer != "" {
		attrs = append(attrs, "indexer", string(r.Indexer))
	}
	if r.Language != "" {
		attrs = append(attrs, "language", string(r.Language))
	}
	if r.Tool != "" {
		attrs = append(attrs, "tool", string(r.Tool))
	}
	if r.Result.TimedOut {
		attrs = append(attrs, "timed_out", true)
	}
	if r.Err != "" {
		l.Logger.Warn("indexer subprocess failed", append(attrs, "error", r.Err)...)
		return
	}
	l.Logger.Info("indexer subprocess", attrs...)
}

// record normalises and files one record.
//
// Stdout and Stderr are dropped here rather than at each call site, so that no
// future caller can accidentally persist subprocess output — which may be
// source code — into an audit trail. The byte counts survive, because "the
// build printed 400 MB" is itself diagnostic.
func record(sink AuditSink, action Action, r AuditRecord) {
	if sink == nil {
		return
	}
	r.Action = action
	if r.At.IsZero() {
		r.At = time.Now()
	}
	r.Result.Stdout = nil
	r.Result.Stderr = nil
	sink.Record(r)
}

// recordErr is record for an attempt that failed before or during the run.
func recordErr(sink AuditSink, action Action, r AuditRecord, err error) {
	if err != nil {
		r.Err = err.Error()
	}
	record(sink, action, r)
}
