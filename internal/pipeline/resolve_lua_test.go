package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestLuaResolution indexes a small application and its busted spec and checks
// every call against what `require` makes it reach.
//
// What it is built to catch: `util.log` where logger.lua, which cart.lua does
// not require, has a `log` too; `string.format` and `tostring`, which share
// names with util.lua's `format`; `json.encode` of a rock (`cjson`) where the
// repository has its own `encode`; a method called on a value of unknown type
// where two modules define `save`; and a spec whose calls are made in
// describe/it callbacks outside any definition.
func TestLuaResolution(t *testing.T) {
	got := langEdges(t, luaFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"app/cart.lua:checkout", "app/models/order.lua:total", high},
		{"app/cart.lua:checkout", "app/util.lua:log", high},
		// require("app.models") is app/models/init.lua.
		{"app/cart.lua:checkout", "app/models/init.lua:load", high},
		{"app/cart.lua:recalc", "app/cart.lua:helper", high},
		{"app/models/order.lua:save", "app/util.lua:log", high},
		{"app/models/order.lua:total", "app/util.lua:log", high},
		{"app/models/order.lua:total", "app/money.lua:zero", high},
		{"spec/cart_spec.lua:(top level)", "app/cart.lua:new", high},
	}
	forbid := []want{
		{Src: "app/cart.lua", Dst: "app/logger.lua"},
		{Src: "app/models/order.lua", Dst: "app/logger.lua"},
		{Src: "app/cart.lua", Dst: "app/util.lua:format"},
		{Src: "app/models/order.lua", Dst: "app/util.lua:format"},
		{Src: "app/cart.lua", Dst: "app/codec.lua"},
		// A second util.lua under another root is not the module `app.util`.
		{Src: "app/", Dst: "lib/util.lua"},
		{Src: "app/cart.lua:checkout", Dst: "app/models/order.lua:save"},
		{Src: "app/cart.lua:checkout", Dst: "app/cache.lua:save"},
	}
	// A method called on `self`, and on a value, where one module declares the
	// name: bound at low, because the receiver's class is not something a Lua
	// file states.
	extraOK := []want{
		{"app/cart.lua:checkout", "app/cart.lua:recalc", model.ConfidenceLow},
		{"spec/cart_spec.lua:(top level)", "app/cart.lua:checkout", model.ConfidenceLow},
	}
	scoreEdges(t, got, wants, forbid, extraOK)
}

func luaFixture() map[string]string {
	return map[string]string{
		"src/app/util.lua": `local M = {}

function M.log(msg)
  print(msg)
end

function M.format(n)
  return tostring(n)
end

return M
`,
		"src/app/logger.lua": `local M = {}

function M.log(msg)
  io.write(msg)
end

return M
`,
		"lib/util.lua": `local M = {}

function M.log(msg)
  io.stderr:write(msg)
end

return M
`,
		"src/app/codec.lua": `local M = {}

function M.encode(v)
  return tostring(v)
end

return M
`,
		"src/app/money.lua": `local Money = {}
Money.__index = Money

function Money.new(cents)
  return setmetatable({ cents = cents }, Money)
end

function Money.zero()
  return Money.new(0)
end

function Money:add(other)
  return Money.new(self.cents + other.cents)
end

return Money
`,
		"src/app/models/init.lua": `local models = {}

function models.load(name)
  return require("app.models." .. name)
end

return models
`,
		"src/app/models/order.lua": `local Money = require("app.money")
local util = require "app.util"

local Order = {}
Order.__index = Order

function Order.new()
  return setmetatable({ lines = {} }, Order)
end

function Order:save()
  util.log("saved")
end

function Order.total(o)
  local sum = Money.zero()
  for _, l in ipairs(o.lines) do
    sum = sum:add(l)
  end
  util.log(string.format("%d", sum.cents))
  return sum
end

return Order
`,
		"src/app/cache.lua": `local Cache = {}

function Cache:save(k)
  return k
end

return Cache
`,
		"src/app/cart.lua": `local Order = require("app.models.order")
local models = require("app.models")
local util = require "app.util"
local json = require("cjson")

local Cart = {}
Cart.__index = Cart

local function helper(t)
  return t
end

function Cart.new()
  return setmetatable({}, Cart)
end

function Cart:recalc()
  return helper(1)
end

function Cart:checkout(o)
  local t = Order.total(o)
  util.log(t)
  models.load("order")
  self:recalc()
  print(string.format("%d", t))
  local blob = json.encode(t)
  o:save()
  return blob
end

return Cart
`,
		"spec/cart_spec.lua": `local Cart = require("app.cart")

describe("cart", function()
  it("checks out", function()
    local c = Cart.new()
    c:checkout({ lines = {} })
  end)
end)
`,
	}
}
