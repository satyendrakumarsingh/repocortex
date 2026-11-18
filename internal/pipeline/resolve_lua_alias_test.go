package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestLuaRequireAliasRebindingAndShadowing checks that a name bound by
// `local m = require("a.mod")` is the module only where it still is: a name
// the file assigns again (`m = require("b.mod")`), declares twice, or binds as
// a parameter names no module, and a call through it binds to none. An alias
// assigned a second time gives no module at all (the first require is not
// preferred over the second, and so is the one before the assignment), which
// is the safe answer where the order of two requires is not read.
func TestLuaRequireAliasRebindingAndShadowing(t *testing.T) {
	high := model.ConfidenceHigh
	got := langEdges(t, map[string]string{
		"a/mod.lua": "local M = {}\nfunction M.run() end\nfunction M.only_a() end\nreturn M\n",
		"b/mod.lua": "local M = {}\nfunction M.run() end\nfunction M.only_b() end\nreturn M\n",
		"main.lua": `local m = require("a.mod")
m = require("b.mod")
function rebound()
  m.run()
  m.only_a()
end
local function f(m)
  m.only_b()
  m.run()
end
`,
		"clean.lua": `local m = require("a.mod")
local function g()
  m.run()
end
local function h(x)
  x.run()
end
`,
		"twice.lua": `local m2 = require("a.mod")
do
  local m2 = require("b.mod")
  m2.only_b()
end
m2.only_a()
`,
	})
	// The alias nothing disturbs still binds.
	requireEdge(t, got, "clean.lua:g", "a/mod.lua:run", high)
	// Reassigned: neither module.
	forbidEdge(t, got, "main.lua:rebound", "a/mod.lua:run")
	forbidEdge(t, got, "main.lua:rebound", "b/mod.lua:run")
	forbidEdge(t, got, "main.lua:rebound", "a/mod.lua:only_a")
	// A parameter is not the alias.
	forbidEdge(t, got, "main.lua:f", "b/mod.lua:only_b")
	forbidEdge(t, got, "main.lua:f", "a/mod.lua:run")
	forbidEdge(t, got, "main.lua:f", "b/mod.lua:run")
	// Declared twice, one in a nested block: neither is picked for a call
	// that could be either.
	forbidEdge(t, got, "twice.lua:(top level)", "a/mod.lua:only_a")
	forbidEdge(t, got, "twice.lua:(top level)", "b/mod.lua:only_b")
}
