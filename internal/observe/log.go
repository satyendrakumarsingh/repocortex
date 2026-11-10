// Package observe provides structured logging and telemetry setup.
//
// Logging rule that matters for this product: RepoCortex indexes source code and
// configuration, so log records must carry identifiers (paths, symbol names,
// counts) and never source content or configuration values. Spec §35.
package observe

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Level maps a human string to a slog level.
func Level(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Options configures the logger.
type Options struct {
	Level  string
	JSON   bool
	Quiet  bool
	Writer io.Writer // defaults to stderr
}

// New builds a logger.
//
// Human output goes to stderr so that stdout stays clean for --json payloads;
// piping `cortex search --json` into jq must never be polluted by log lines.
func New(opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	if opts.Quiet {
		w = io.Discard
	}

	handlerOpts := &slog.HandlerOptions{Level: Level(opts.Level)}

	var h slog.Handler
	if opts.JSON {
		h = slog.NewJSONHandler(w, handlerOpts)
	} else {
		h = slog.NewTextHandler(w, handlerOpts)
	}
	// Correlation is applied unconditionally. When tracing is off it costs one
	// atomic load per record and adds no fields; when it is on, every log line
	// emitted inside a span becomes traceable. See slogtrace.go.
	return slog.New(WithTrace(h))
}

// Discard returns a logger that drops everything. For tests.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
