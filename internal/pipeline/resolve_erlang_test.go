package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestErlangResolution indexes a small OTP application and its EUnit and Common
// Test suites and checks every call against what the module system makes it
// reach.
//
// What it is built to catch: `shop_money:zero()` where another module
// (`other_money`) also has a `zero/0`; a bare call reaching a function that an
// `-import` brought in and one that it did not; `lists:map`, `io:format` and
// `gen_server:call`, which leave the repository; `length/1` and `is_list/1`,
// which are the compiler's; `?MODULE:f()`; a module chosen at run time
// (`Mod:f()`); a call at an arity no function of the module has (`add/1` where
// only `add/2` and `add/3` exist); a function defined at two arities; and tests
// that call the module under test.
func TestErlangResolution(t *testing.T) {
	got := langEdges(t, erlangFixture())
	high := model.ConfidenceHigh
	wants := []want{
		// A remote call names the module.
		{"src/shop_cart.erl:total", "src/shop_money.erl:zero", high},
		{"src/shop_cart.erl:total", "src/shop_money.erl:add", high},
		{"src/shop_cart.erl:total", "src/shop_tax.erl:rate", high},
		{"src/shop_cart.erl:checkout", "src/shop_money.erl:zero", high},
		// A bare call: the module's own function, and one an -import names.
		{"src/shop_cart.erl:total", "src/shop_cart.erl:helper", high},
		{"src/shop_cart.erl:total", "src/shop_util.erl:slug", high},
		{"src/shop_cart.erl:checkout", "src/shop_cart.erl:total", high},
		// `?MODULE:f()` is the module the call is written in.
		{"src/shop_cart.erl:checkout", "src/shop_cart.erl:helper", high},
		// A function written at two arities is two functions; the count chooses.
		{"src/shop_cart.erl:pad", "src/shop_cart.erl:pad", high},
		{"src/shop_cart.erl:checkout", "src/shop_cart.erl:pad", high},
		{"src/shop_cart.erl:arities", "src/shop_money.erl:add", high},
		// The server calls the cart, and the cart's own callbacks.
		{"src/shop_server.erl:handle_call", "src/shop_cart.erl:total", high},
		{"src/shop_server.erl:handle_call", "src/shop_server.erl:reply", high},
		{"src/shop_server.erl:init", "src/shop_money.erl:zero", high},
		// EUnit and Common Test call the module under test.
		{"test/shop_cart_tests.erl:total_test", "src/shop_cart.erl:total", high},
		{"test/shop_cart_tests.erl:total_test", "src/shop_money.erl:zero", high},
		{"test/shop_SUITE.erl:total_case", "src/shop_cart.erl:total", high},
		{"test/shop_SUITE.erl:total_case", "src/shop_cart.erl:checkout", high},
	}
	forbid := []want{
		// other_money is not the module that was named.
		{Src: "src/shop_cart.erl", Dst: "src/other_money.erl"},
		{Src: "src/shop_server.erl", Dst: "src/other_money.erl"},
		// shop_util:format/1 is not imported, and `format` is a name of io's.
		{Src: "src/shop_cart.erl", Dst: "src/shop_util.erl:format"},
		// Nothing calls the debug module, and its names are the language's.
		{Src: "src/", Dst: "src/shop_debug.erl"},
		// A module chosen at run time names nothing.
		{Src: "src/shop_server.erl:dispatch", Dst: "src/shop_money.erl"},
	}
	scoreEdges(t, got, wants, forbid, nil)

	// add/1 does not exist: of the three calls of shop_money:add in the cart,
	// two are at arities the module has and one is not, and only two bind.
	n := 0
	for _, e := range got {
		if e.Src == "src/shop_cart.erl:arities" && e.Dst == "src/shop_money.erl:add" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("arities -> shop_money:add edges = %d, want 2 (add/2 and add/3; add/1 is not defined)", n)
	}
}

func erlangFixture() map[string]string {
	return map[string]string{
		"rebar.config": `{erl_opts, [debug_info]}.
`,
		"src/shop_money.erl": `-module(shop_money).
-export([zero/0, add/2, add/3]).

zero() -> 0.

add(A, B) -> A + B.

add(A, B, C) -> A + B + C.
`,
		"src/other_money.erl": `-module(other_money).
-export([zero/0]).

zero() -> 0.0.
`,
		"src/shop_util.erl": `-module(shop_util).
-export([slug/1, format/1]).

slug(S) -> S.

format(S) -> S.
`,
		"src/shop_tax.erl": `-module(shop_tax).
-export([rate/1]).

rate(_C) -> 0.2.
`,
		// Names the language owns, defined by a module nobody calls.
		"src/shop_debug.erl": `-module(shop_debug).
-export([trace/1, log/2]).

trace(X) -> X.

log(_F, X) -> X.
`,
		"src/shop_cart.erl": `-module(shop_cart).
-export([total/1, checkout/1, pad/1, arities/0]).
-import(shop_util, [slug/1]).
-include("shop.hrl").

total(Items) ->
    Z = shop_money:zero(),
    T = shop_tax:rate(Items),
    lists:map(fun(I) -> helper(I) end, Items),
    io:format("~p~n", [Z]),
    slug(Items),
    helper(Items),
    length(Items),
    is_list(Items),
    {Z, T, shop_money:add(Z, T)}.

checkout(C) ->
    shop_money:zero(),
    ?MODULE:helper(C),
    pad(C),
    total(C).

pad(X) -> pad(X, 2).
pad(X, N) -> {X, N}.

arities() ->
    shop_money:add(1, 2),
    shop_money:add(1, 2, 3),
    shop_money:add(1).

helper(X) -> X.
`,
		"src/shop_server.erl": `-module(shop_server).
-behaviour(gen_server).
-export([init/1, handle_call/3, dispatch/2]).

init(_Args) ->
    {ok, shop_money:zero()}.

handle_call({total, Items}, _From, State) ->
    {reply, shop_cart:total(Items), reply(State)};
handle_call(_Other, _From, State) ->
    {reply, ok, State}.

dispatch(Mod, X) ->
    Mod:zero(),
    gen_server:call(?MODULE, X).

reply(S) -> S.
`,
		"test/shop_cart_tests.erl": `-module(shop_cart_tests).
-include_lib("eunit/include/eunit.hrl").

total_test() ->
    ?assertEqual(0, shop_cart:total([])),
    Z = shop_money:zero(),
    ?assertEqual(0, Z).
`,
		"test/shop_SUITE.erl": `-module(shop_SUITE).
-export([all/0, total_case/1]).

all() -> [total_case].

total_case(_Config) ->
    shop_cart:total([]),
    shop_cart:checkout([]),
    lists:foreach(fun(X) -> X end, [1]).
`,
	}
}
