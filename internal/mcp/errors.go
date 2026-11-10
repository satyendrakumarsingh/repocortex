package mcp

// Errors an agent can act on without a human.
//
// A free-text MCP error leaves the model guessing, and the usual guess is to
// retry the identical call. Every error therefore carries a stable code (so the
// model can branch) and a hint naming a concrete next call (so it can recover).
//
// The mapping is deliberately derived from errs.Kind rather than invented here:
// the kinds already encode the remedy. errs documents that KindCorrupt needs a
// reindex and that telling the user to retry would be actively harmful — this
// is where that documented distinction becomes something the agent receives.

import (
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/satyendrakumarsingh/repocortex/internal/errs"
)

// hintForKind maps a kind onto a wire code and a recovery instruction.
func hintForKind(k errs.Kind) (code string, hint string) {
	switch k {
	case errs.KindInvalid:
		return "invalid_argument",
			"The arguments were rejected before anything ran. Correct them and call again; retrying unchanged will fail identically."

	case errs.KindNotFound:
		return "not_found",
			"Nothing matched. This is often normal rather than a fault. Try find_symbol with a shorter name, or search_code to locate it by text first."

	case errs.KindConflict:
		return "conflict",
			"The request was well-formed but the current index state forbids it. Call get_project_overview to see the run's state, then retry once the state allows it."

	case errs.KindCorrupt:
		return "index_corrupt",
			"The stored index violates an invariant and cannot be repaired in place. Run `cortex index` to rebuild it."

	case errs.KindTransient:
		return "transient",
			"A transient condition, usually lock contention between the index run, the watcher and this server. Retry the identical call once; if it fails again, treat it as a real failure."

	case errs.KindCancelled:
		return "cancelled",
			"The call was cancelled by the caller. This is not a failure and should not be reported as one; re-issue the call if the result is still needed."

	case errs.KindUnsupported:
		return "unsupported",
			"The request is valid but this build or input cannot be served (e.g. no parser for this file type). Nothing is wrong with the index; degrade gracefully or fall back to search_code rather than retrying."

	default:
		return "internal",
			"RepoCortex failed internally. The index is intact; fall back to search_code for this question and report the failure."
	}
}

// failWith renders an error result carrying a code, a message and a hint.
//
// RESERVED, AND THIS IS NOW THE ONLY PLACE THE RESERVATION IS WRITTEN DOWN.
// server.go's fail() carried this policy until it lost its last caller; the
// note moved here rather than dying with it, because failWith is the sole
// remaining constructor of a raw IsError result and therefore the place a
// future author will look.
//
// isError is for SECURITY REFUSALS, MALFORMED CALLS AND GENUINE MALFUNCTION
// ONLY. A recoverable condition routed through here costs the whole session's
// remaining tool calls, because a model that sees a tool fail early stops
// calling it. So before adding a caller, ask: could the agent fix this by
// asking a better question? If yes it belongs in Server.guide, which returns a
// success-shaped answer with zero items and notes saying what to do — not
// here. "No index yet" and "no symbol by that name" are guidance; a corrupt
// index is not.
//
// It stays a RESULT rather than a protocol error because a protocol error
// aborts the call and gives the model nothing at all to reason about.
//
// THE NIL ERROR IS THE CONTRACT, NOT AN OVERSIGHT. The go-sdk's ToolHandlerFor
// wrapper DISCARDS a handler's crafted result whenever the handler also
// returns a non-nil error, substituting its own bare errRes.SetError(err) —
// which would strip the code, the message and the hint from every call site at
// once. Returning the error here instead of the result has already been shipped
// once as a Critical bug. Do not "fix" it.
func failWith(code, hint, format string, args ...any) (*sdk.CallToolResult, *Response, error) {
	msg := fmt.Sprintf(format, args...)
	var b strings.Builder
	b.WriteString("=== " + sectionMetadata + " ===\n")
	b.WriteString("error_code: " + code + "\n")
	b.WriteString(msg + "\n")
	if hint != "" {
		b.WriteString("hint: " + hint + "\n")
	}
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: b.String()}},
	}, nil, nil
}

// failFrom classifies a Go error and renders it with the matching hint.
func failFrom(err error, format string, args ...any) (*sdk.CallToolResult, *Response, error) {
	code, hint := hintForKind(errs.KindOf(err))
	return failWith(code, hint, format, args...)
}
