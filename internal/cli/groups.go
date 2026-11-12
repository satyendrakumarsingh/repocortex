package cli

import "github.com/spf13/cobra"

// ---------------------------------------------------------------- help layout
//
// `repocortex --help` is the first screen a new user reads, and with fifty
// commands in one alphabetical list it answers "what can this do?" with a
// wall. So the commands are grouped by what the reader is trying to get done,
// in the order a new user meets them: set up, ask, change, check health, serve.
//
// The grouping lives in ONE table rather than as a GroupID on each command, so
// the whole layout can be read and reordered in one place, and so
// TestEveryCommandHasAGroup can fail the build when a command is added without
// being placed. An unplaced command falls into cobra's "Additional Commands"
// heading, which is where things go to be overlooked.
//
// # Naming rules for commands (enforced by TestCommandNamesFollowTheConvention)
//
//   - lower case, words joined by hyphens: dead-code, plan-change
//   - a noun for a thing you look at (callers, hotspots, blindspots), a verb or
//     verb-noun for a thing you do (index, rename, plan-change, minimize-diff)
//   - never noun-verb: it is minimize-diff, not diff-minimize
//   - the name an agent tool uses, minus its verb, where one exists:
//     list_dead_code -> dead-code, list_blindspots -> blindspots
//   - one name per command, and no aliases: a second spelling is a second
//     thing to document, complete and keep working

const (
	groupSetup  = "setup"
	groupAsk    = "ask"
	groupChange = "change"
	groupHealth = "health"
	groupServe  = "serve"
)

// commandGroups lists the groups in display order.
func commandGroups() []*cobra.Group {
	return []*cobra.Group{
		{ID: groupSetup, Title: "Set up and keep current:"},
		{ID: groupAsk, Title: "Ask the graph:"},
		{ID: groupChange, Title: "Before and after a change:"},
		{ID: groupHealth, Title: "Check the repository and the index:"},
		{ID: groupServe, Title: "Serve agents and integrate:"},
	}
}

// commandGroupOf places every top-level command. Keyed by canonical name.
func commandGroupOf() map[string]string {
	return map[string]string{
		// Set up and keep current.
		"init": groupSetup, "index": groupSetup, "resolve": groupSetup,
		"scip": groupSetup, "indexers": groupSetup, "install": groupSetup,
		"agent": groupSetup, "hooks": groupSetup, "watch": groupSetup,

		// Ask the graph.
		"context": groupAsk, "search": groupAsk, "symbol": groupAsk,
		"references": groupAsk, "callers": groupAsk, "callees": groupAsk,
		"implementations": groupAsk, "path": groupAsk, "tests": groupAsk,
		"outline": groupAsk, "api": groupAsk, "db": groupAsk,
		"config": groupAsk, "deps": groupAsk, "events": groupAsk, "infra": groupAsk,
		"query": groupAsk,

		// Before and after a change.
		"impact": groupChange, "plan-change": groupChange, "rename": groupChange,
		"owners": groupChange, "compile": groupChange, "verify": groupChange,
		"diff": groupChange, "minimize-diff": groupChange,

		// Check the repository and the index.
		"status": groupHealth, "doctor": groupHealth, "languages": groupHealth,
		"blindspots": groupHealth, "dead-code": groupHealth, "hotspots": groupHealth,
		"arch": groupHealth, "accuracy": groupHealth, "bench": groupHealth,

		// Serve agents and integrate.
		"serve": groupServe, "tool": groupServe, "ui": groupServe,
		"system": groupServe, "trace": groupServe, "export": groupServe,
		"docs": groupServe, "report": groupServe,
	}
}

// ungroupedCommands are deliberately left under "Additional Commands":
// housekeeping a reader does not need in order to understand the product.
func ungroupedCommands() map[string]bool {
	return map[string]bool{"version": true, "licenses": true, "help": true, "completion": true}
}

// applyCommandGroups registers the groups and assigns each command to its own.
func applyCommandGroups(root *cobra.Command) {
	root.AddGroup(commandGroups()...)
	placed := commandGroupOf()
	for _, c := range root.Commands() {
		if id, ok := placed[c.Name()]; ok {
			c.GroupID = id
		}
	}
}
