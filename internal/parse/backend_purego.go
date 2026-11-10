//go:build !cgo_parser

package parse

// Default build: the pure-Go runtime is the only one compiled in.
//
// Nothing in this file imports CGO, which is the entire point. `go build ./...`
// with CGO_ENABLED=0 and all five cross-compile targets stay green because the
// C bindings live in backend_cgo.go behind the `cgo_parser` tag and are simply
// not part of this build.
//
// See BACKENDS.md for how and why to build the other one.

import "errors"

// ErrCGOBackendNotBuilt is returned by CGOBackend in the default build.
//
// A sentinel rather than a panic: `cortex doctor` reports which backend is
// compiled in, and asking for a backend that was not built is a fact to report,
// not a crash.
var ErrCGOBackendNotBuilt = errors.New("parse: CGO backend not compiled in; rebuild with -tags cgo_parser")

// ActiveBackendName is the runtime this binary parses with.
const ActiveBackendName = "purego"

// ActiveBackend returns the backend this binary was built with.
//
// This is the compile-time swap. Callers get whichever runtime the build
// selected and never branch on it, so no code path can accidentally depend on
// one particular tree-sitter implementation.
func ActiveBackend() Backend { return PureGoBackend() }

// CGOBackend returns the official C tree-sitter runtime, or an error when it
// was not compiled in.
func CGOBackend() (Backend, error) { return nil, ErrCGOBackendNotBuilt }
